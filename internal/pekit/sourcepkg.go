package pekit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peios/peipkg/pack"
)

// planSourcePackage decides whether a packaging run emits a
// corresponding-source package and synthesizes its instance. One source
// package covers every peipkg-format package the recipe produces: the
// members are builds of the same source, so their corresponding source
// is one artifact. Nil (with no error) means no source package: emission
// opted out, no reproducible external source, or no peipkg members.
func planSourcePackage(recipe RecipeConfig, source SourceState, instances []PackageInstance) (*PackageInstance, error) {
	if !recipe.SourcePackage.IsEnabled() {
		return nil, nil
	}
	if source.Local || (source.Kind != "git" && source.Kind != "url" && source.Kind != "pypi") {
		return nil, nil
	}
	// A git source without a resolved commit is a bare branch ref — a
	// deliberately moving target (see the lock rules) with no stable
	// corresponding source to package.
	if source.Kind == "git" && source.Commit == "" {
		return nil, nil
	}
	var members []PackageInstance
	for _, inst := range instances {
		if inst.Format == "peipkg" {
			members = append(members, inst)
		}
	}
	if len(members) == 0 {
		return nil, nil
	}
	versionText := members[0].Version
	homepage := members[0].Config.Package.Homepage
	var licenses []string
	licenseClass := members[0].Config.Package.LicenseClass
	for i, m := range members {
		if m.Version != versionText {
			return nil, diag("source_package_version_conflict",
				"source package needs one version but members disagree (%s vs %s); align member versions or set source_package.enabled = false",
				versionText, m.Version)
		}
		if m.Config.Package.Homepage != homepage {
			homepage = ""
		}
		if l := m.Config.Package.License; l != "" {
			licenses = append(licenses, l)
		}
		if i > 0 {
			licenseClass = worseLicenseClass(licenseClass, m.Config.Package.LicenseClass)
		}
	}
	name := recipe.SourcePackage.Name
	if name == "" {
		name = filepath.Base(recipe.Root) + "-source"
	}
	// The source package contains the members' licensed material, plus
	// whatever only the source carries, so its license is the conjunction
	// of theirs and the recipe's source_package.license.
	if recipe.SourcePackage.License != "" {
		licenses = append(licenses, recipe.SourcePackage.License)
	}
	license := conjoinLicenses(licenses)
	base := strings.TrimSuffix(name, "-source")
	cfg := PackageConfig{
		Format: "peipkg",
		Package: PackageMeta{
			Name:         name,
			Version:      versionText,
			Architecture: "noarch",
			Description:  "Corresponding source for " + base + " " + versionText,
			License:      license,
			LicenseClass: licenseClass,
			Homepage:     homepage,
		},
		Publish: members[0].Config.Publish,
	}
	stageID := "source-" + shortHash(name, versionText, "noarch", "peipkg")
	stage := filepath.Join(source.WorkBase, "package", stageID)
	return &PackageInstance{
		DefinitionSelector: name,
		Config:             cfg,
		Name:               name,
		Version:            versionText,
		Architecture:       "noarch",
		Format:             "peipkg",
		Stage:              stage,
		Artifact:           filepath.Join(stage, fmt.Sprintf("%s_%s_noarch.peipkg", name, versionText)),
		SourcePkg:          true,
	}, nil
}

// sourcePayloadRoot is the archive-path prefix all of a source package's
// content lands under (PSD-009 §3.4.1 reserves usr/src/dist/ for it).
func sourcePayloadRoot(inst PackageInstance) string {
	base := strings.TrimSuffix(inst.Name, "-source")
	return "usr/src/dist/" + base + "-" + inst.Version
}

// writeSourcePackage stages and writes one corresponding-source package:
// upstream/ carries the pristine source input (the exact bytes the lock
// hash covers, or a git-archive export of the locked commit), recipe/
// and workspace/ carry captured build inputs; source/ is the prepared tree.
// Compatibility recipe/ and patches/ views remain available.
func writeSourcePackage(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version, inst PackageInstance, member string, run packRun, captured *sourceInputs) error {
	if err := removeStage(inst.Stage); err != nil {
		return wrapDiag("clean_stage", inst.Stage, err)
	}
	if err := os.MkdirAll(inst.Stage, 0o755); err != nil {
		return wrapDiag("mkdir", inst.Stage, err)
	}
	root := sourcePayloadRoot(inst)
	base := strings.TrimSuffix(inst.Name, "-source")
	var entries []payloadEntry
	switch source.Kind {
	case "url", "pypi":
		if !fileExists(source.Artifact) {
			return diag("missing_source_artifact", "cached source artifact %s does not exist", source.Artifact)
		}
		entries = append(entries, payloadEntry{Source: source.Artifact, Dest: root + "/upstream/" + filepath.Base(source.Artifact)})
		for _, artifact := range source.AdditionalArtifacts {
			if !fileExists(artifact) {
				return diag("missing_source_artifact", "cached source artifact %s does not exist", artifact)
			}
			entries = append(entries, payloadEntry{Source: artifact, Dest: root + "/upstream/patches/" + filepath.Base(artifact)})
		}
	case "git":
		archive := filepath.Join(inst.Stage, base+"-"+inst.Version+".tar")
		if source.TrackedPath != "" {
			if err := writeTrackedGitSourceArchive(archive, base+"-"+inst.Version, source); err != nil {
				return err
			}
		} else {
			// Ordinary git sources retain git archive semantics: they export
			// the exact commit from the mirror clone, including Git's normal
			// attribute handling and deterministic commit metadata.
			args := []string{"archive", "--format=tar", "--prefix=" + base + "-" + inst.Version + "/", "-o", archive, source.Commit}
			if err := runSimple(source.GitRepo, "git", args...); err != nil {
				return wrapDiag("git_archive", "export source tree", err)
			}
		}
		entries = append(entries, payloadEntry{Source: archive, Dest: root + "/upstream/" + filepath.Base(archive)})
	default:
		return diag("unsupported_source", "source package cannot be built from a %s source", source.Kind)
	}
	// Declared inputs are upstream bytes the build consumed, so corresponding
	// source carries them too, each under its own name and byte-for-byte as
	// fetched. Their hashes are in pekit.lock, so a recipient can verify them
	// independently of us.
	for _, input := range source.Inputs {
		if input.Artifact == "" {
			continue
		}
		if !fileExists(input.Artifact) {
			return diag("missing_source_artifact", "cached input artifact %s does not exist", input.Artifact)
		}
		entries = append(entries, payloadEntry{Source: input.Artifact, Dest: root + "/upstream/" + input.Name + "/" + filepath.Base(input.Artifact)})
	}
	entries, err := bundleEntries(ctx, captured, source, version, root, inst.Stage, entries)
	if err != nil {
		return err
	}
	if err := validatePayloadDestinations(entries); err != nil {
		return err
	}
	if err := writePeipkg(ctx, workspace, inst, source, entries, run); err != nil {
		return err
	}
	if run.SignKey != nil {
		ctx.Renderer.Event(Event{Type: "sign", Member: member, Package: instanceID(inst), Path: inst.Artifact, Message: "signed with key " + pack.SigningKeyFingerprint(run.SignKey)})
	}
	if err := writePackageStamp(source, inst, run); err != nil {
		return err
	}
	ctx.Renderer.Event(Event{Type: "artifact", Member: member, Package: instanceID(inst), Version: version.Raw, Path: inst.Artifact, Message: "wrote source package"})
	return nil
}

// writeTrackedGitSourceArchive writes the one-file pristine upstream archive
// for a tracked-path snapshot without invoking git archive. A blob:none mirror
// may deliberately lack .gitattributes blobs that git archive consults even
// when restricted to one path; attempting that traversal would introduce a
// hidden lazy fetch into an otherwise offline exact-lock build.
func writeTrackedGitSourceArchive(archive, prefix string, source SourceState) error {
	tracked, err := cleanRelPath(source.TrackedPath)
	if err != nil || tracked != source.TrackedPath {
		if err == nil {
			err = fmt.Errorf("path is not canonical")
		}
		return wrapDiag("invalid_source_path", source.TrackedPath, err)
	}
	prefix, err = cleanRelPath(prefix)
	if err != nil {
		return wrapDiag("invalid_source_path", prefix, err)
	}
	if source.TrackedRoot == "" || len(source.TrackedSHA256) != sha256.Size*2 {
		return diag("missing_source_artifact", "tracked git source is missing its pristine root or digest")
	}
	if _, err := hex.DecodeString(source.TrackedSHA256); err != nil {
		return diag("missing_source_artifact", "tracked git source has an invalid pristine digest")
	}

	file := filepath.Join(source.TrackedRoot, filepath.FromSlash(tracked))
	info, err := os.Lstat(file)
	if err != nil {
		return wrapDiag("stat", file, err)
	}
	if !info.Mode().IsRegular() {
		return diag("unsupported_source", "tracked git source %s is not a regular file", file)
	}
	if info.Mode().Perm() != source.TrackedMode.Perm() || (source.TrackedMode.Perm() != 0o644 && source.TrackedMode.Perm() != 0o755) {
		return diag("source_mode_mismatch", "tracked git source %s mode no longer matches its locked regular-file mode", file)
	}
	f, err := os.Open(file)
	if err != nil {
		return wrapDiag("open", file, err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil {
		return wrapDiag("read", file, copyErr)
	}
	if closeErr != nil {
		return wrapDiag("close", file, closeErr)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != source.TrackedSHA256 {
		return diag("lock_mismatch", "tracked git pristine source digest %s does not match locked %s", got, source.TrackedSHA256)
	}

	tarEntries := []payloadEntry{{Source: file, Dest: prefix + "/" + tracked}}
	if err := validatePayloadDestinations(tarEntries); err != nil {
		return err
	}
	if err := writeTar(archive, tarEntries); err != nil {
		return wrapDiag("git_archive", "export tracked source file", err)
	}
	return nil
}

func sourceTreeEntries(dir, destPrefix string) ([]payloadEntry, error) {
	var entries []payloadEntry
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		entries = append(entries, payloadEntry{Source: path, Dest: destPrefix + "/" + filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		return nil, wrapDiag("walk", dir, err)
	}
	return entries, nil
}

// worseLicenseClass folds two members' licence classes into the class of
// a package carrying both: the source package contains every member's
// material, so it is as encumbered as its most encumbered member.
// proprietary > firmware > unknown > free — unknown outranks free because
// a member nobody has classified cannot vouch for the whole. An undeclared
// class ("") is unknown, and stays "" so the manifest omits the key.
func worseLicenseClass(a, b string) string {
	rank := map[string]int{"": 2, "unknown": 2, "free": 1, "firmware": 3, "proprietary": 4}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// conjoinLicenses renders the conjunction of SPDX expressions as one valid,
// deterministic expression. Each expression is split into its top-level AND
// terms; a term containing OR keeps its parentheses so the surrounding ANDs
// cannot rebind it (AND binds tighter than OR). Equal terms appear once, and
// terms are sorted.
func conjoinLicenses(exprs []string) string {
	seen := map[string]bool{}
	var terms []string
	for _, expr := range exprs {
		for _, term := range spdxConjuncts(tokenizeSPDX(expr)) {
			if !seen[term] {
				seen[term] = true
				terms = append(terms, term)
			}
		}
	}
	sort.Strings(terms)
	return strings.Join(terms, " AND ")
}

// spdxConjuncts returns the rendered top-level AND terms of a token stream,
// flattening parenthesised conjunctions. An expression with a top-level OR is
// one term, parenthesised.
func spdxConjuncts(tokens []string) []string {
	for len(tokens) >= 2 && tokens[0] == "(" && spdxClosingParen(tokens, 0) == len(tokens)-1 {
		tokens = tokens[1 : len(tokens)-1]
	}
	if len(tokens) == 0 {
		return nil
	}
	if len(spdxSplitTopLevel(tokens, "OR")) > 1 {
		return []string{"(" + renderSPDX(tokens) + ")"}
	}
	parts := spdxSplitTopLevel(tokens, "AND")
	if len(parts) == 1 {
		return []string{renderSPDX(tokens)}
	}
	var terms []string
	for _, part := range parts {
		terms = append(terms, spdxConjuncts(part)...)
	}
	return terms
}

// spdxClosingParen returns the index of the parenthesis closing the one at
// open, or -1 when the stream is unbalanced.
func spdxClosingParen(tokens []string, open int) int {
	depth := 0
	for i := open; i < len(tokens); i++ {
		switch tokens[i] {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// spdxSplitTopLevel splits tokens at every op outside parentheses.
func spdxSplitTopLevel(tokens []string, op string) [][]string {
	var parts [][]string
	depth, start := 0, 0
	for i, tok := range tokens {
		switch tok {
		case "(":
			depth++
		case ")":
			depth--
		case op:
			if depth == 0 {
				parts = append(parts, tokens[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, tokens[start:])
}

// renderSPDX joins tokens with single spaces, attaching parentheses to what
// they enclose.
func renderSPDX(tokens []string) string {
	var b strings.Builder
	for i, tok := range tokens {
		if i > 0 && tok != ")" && tokens[i-1] != "(" {
			b.WriteByte(' ')
		}
		b.WriteString(tok)
	}
	return b.String()
}
