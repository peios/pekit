package pekit

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The static rules read only committed files: the recipe, its package
// files, pekit.lock and the patch series. Nothing here resolves a source or
// touches the network, so `pekit lint` with no version flag is safe to run
// anywhere, including a checkout with no build tools.

var (
	reverseDNSNameRE  = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9][a-z0-9+-]*)+$`)
	lowercaseNameRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9.+_-]*$`)
	hex40RE           = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	hex64RE           = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	fullFingerprintRE = regexp.MustCompile(`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	fixedGitRefRE     = regexp.MustCompile(`^([0-9a-f]{40}|refs/tags/.+|v?[0-9].*)$`)
)

func lintStatic(l *linter, recipe RecipeConfig, workspace *WorkspaceConfig) error {
	if workspace != nil && l.on("package.name.style") {
		base := filepath.Base(recipe.Root)
		if !lintNameMatchesStyle(base, l.cfg.Str("package.name.style")) {
			l.report("package.name.style", "", recipe.Root, "recipe directory %q is not a %s name", base, l.cfg.Str("package.name.style"))
		}
	}
	lintSourceRules(l, recipe)
	lintBuildRules(l, recipe)
	packages, err := loadEffectivePackages(recipe, workspace, cleanSourceState(recipe))
	if err != nil {
		// A recipe without package definitions has nothing for the package
		// rules to check; a delegate's live in the source it has not
		// resolved. Any other load error is a broken package file.
		if diagCode(err) == "missing_package" {
			return nil
		}
		return err
	}
	for _, pkg := range packages {
		lintPackageMeta(l, pkg)
	}
	return nil
}

func lintNameMatchesStyle(name, style string) bool {
	switch style {
	case "reverse-dns":
		return reverseDNSNameRE.MatchString(name)
	case "lowercase":
		return lowercaseNameRE.MatchString(name)
	}
	return true
}

// effectivePackageName mirrors makePackageInstance: a member without a
// declared name is named after its selector.
func effectivePackageName(pkg EffectivePackage) string {
	if pkg.Config.Package.Name != "" {
		return pkg.Config.Package.Name
	}
	return pkg.Selector
}

func lintPackageMeta(l *linter, pkg EffectivePackage) {
	name := effectivePackageName(pkg)
	meta := pkg.Config.Package
	path := ""
	if n := len(pkg.Layers); n > 0 {
		path = pkg.Layers[n-1].Path
	}
	if l.on("package.name.style") {
		style := l.cfg.Str("package.name.style")
		if !lintNameMatchesStyle(name, style) {
			l.report("package.name.style", name, path, "package name %q is not a %s name", name, style)
		}
	}
	if l.on("package.license") {
		switch {
		case strings.TrimSpace(meta.License) == "":
			l.report("package.license", name, path, "package declares no license")
		case l.cfg.Str("package.license") == "spdx":
			if err := validateSPDXExpression(meta.License); err != nil {
				l.report("package.license", name, path, "license %q is not a valid SPDX expression: %s", meta.License, err)
			}
		}
	}
	if l.on("package.license_class") && pkg.Config.Format == "peipkg" && meta.LicenseClass == "" {
		l.report("package.license_class", name, path, "peipkg package declares no license_class (free, firmware, proprietary or unknown)")
	}
	if l.on("package.homepage") {
		switch {
		case meta.Homepage == "":
			l.report("package.homepage", name, path, "package declares no homepage")
		case l.cfg.Str("package.homepage") == "https" && !strings.HasPrefix(meta.Homepage, "https://"):
			l.report("package.homepage", name, path, "homepage %q is not an https URL", meta.Homepage)
		}
	}
	if l.on("package.description") {
		lintDescription(l, name, path, meta.Description)
	}
	if l.on("package.dependencies") {
		lintDependencyConsistency(l, name, path, meta)
	}
}

func lintDescription(l *linter, name, path, desc string) {
	max, ok := l.cfg.Int("package.description")
	if !ok {
		max = 80
	}
	desc = strings.TrimSpace(desc)
	if desc == "" {
		l.report("package.description", name, path, "package has no description")
		return
	}
	if strings.ContainsAny(desc, "\n\r") {
		l.report("package.description", name, path, "description spans more than one line")
	}
	if len(desc) > max {
		l.report("package.description", name, path, "description is %d characters; the limit is %d", len(desc), max)
	}
	if strings.HasSuffix(desc, ".") {
		l.report("package.description", name, path, "description ends with a full stop; it is a summary, not a sentence")
	}
	lower := strings.ToLower(desc)
	short := name
	if i := strings.LastIndex(name, "."); i >= 0 {
		short = name[i+1:]
	}
	for _, prefix := range []string{name, short} {
		if prefix != "" && strings.HasPrefix(lower, strings.ToLower(prefix)+" ") {
			l.report("package.description", name, path, "description starts with the package name; say what it is, not what it is called")
			break
		}
	}
}

func lintDependencyConsistency(l *linter, name, path string, meta PackageMeta) {
	for dep := range meta.Dependencies {
		if dep == name {
			l.report("package.dependencies", name, path, "package depends on itself")
		}
		if _, ok := meta.Provides[dep]; ok {
			l.report("package.dependencies", name, path, "package depends on %q, which it provides itself", dep)
		}
		if _, ok := meta.Conflicts[dep]; ok {
			l.report("package.dependencies", name, path, "package both depends on and conflicts with %q", dep)
		}
		if _, ok := meta.OptionalDependencies[dep]; ok {
			l.report("package.dependencies", name, path, "%q is listed as both a dependency and an optional dependency", dep)
		}
	}
	for dep := range meta.OptionalDependencies {
		if _, ok := meta.Conflicts[dep]; ok {
			l.report("package.dependencies", name, path, "package both optionally depends on and conflicts with %q", dep)
		}
	}
	if _, ok := meta.Provides[name]; ok {
		l.report("package.dependencies", name, path, "package provides its own name, which every package does implicitly")
	}
	if _, ok := meta.Conflicts[name]; ok {
		l.report("package.dependencies", name, path, "package conflicts with itself")
	}
}

// --- source ----------------------------------------------------------------

func lintSourceRules(l *linter, recipe RecipeConfig) {
	s := recipe.Source
	path := recipe.Path
	if l.on("source.reproducible") && s.Local.Path != "" && !s.HasReproducible() {
		l.report("source.reproducible", "", path, "source is [source.local] only; a build from it cannot be reproduced — add a git, url or pypi source")
	}
	discovers := s.PyPI.Project != "" || (s.URL.URL != "" && s.URL.ListingURL != "") || (s.Git.URL != "" && s.Git.TagRegex != "")
	if l.on("source.discovery") {
		if s.URL.URL != "" && s.URL.ListingURL == "" {
			l.report("source.discovery", "", path, "[source.url] has no listing_url, so new upstream releases cannot be discovered")
		}
		if s.Git.URL != "" && s.Git.TagRegex == "" && s.Git.TrackedPath == "" {
			l.report("source.discovery", "", path, "[source.git] has no tag_regex, so new upstream releases cannot be discovered")
		}
	}
	if l.on("source.versions.floor") && discovers {
		constraint := s.URL.Versions
		if s.Git.URL != "" {
			constraint = s.Git.Versions
		}
		if s.PyPI.Project != "" {
			constraint = s.PyPI.Versions
		}
		if !constraintHasFloor(constraint) {
			l.report("source.versions.floor", "", path, "release discovery has no lower bound: set versions = \">= <first version this recipe was written for>\" so it never follows an older release")
		}
	}
	if l.on("source.versions.ceiling") {
		for _, c := range []string{s.URL.Versions, s.Git.Versions, s.PyPI.Versions} {
			if term := constraintCeiling(c); term != "" {
				l.report("source.versions.ceiling", "", path, "versions = %q bounds discovery from above with %q; unattended updates stop at that release — drop the ceiling and review the new major when it arrives", c, term)
			}
		}
	}
	if l.on("source.ref") && s.Git.URL != "" && s.Git.TrackedPath == "" {
		if ref := s.Git.Ref; !strings.Contains(ref, "{{") && !fixedGitRefRE.MatchString(ref) {
			l.report("source.ref", "", path, "git ref %q is a moving branch; use a tag template such as \"v{{version}}\" so the lock can pin it", ref)
		}
	}
	if l.on("source.url.scheme") {
		lintURLSchemes(l, path, s)
	}
	if l.on("source.lock") && s.HasReproducible() {
		lintLock(l, recipe)
	}
	if l.on("source.signature.required") && s.URL.URL != "" && !s.URL.Signature.Configured() {
		l.report("source.signature.required", "", path, "[source.url] verifies no upstream signature; add [source.url.signature] with the release key")
	}
	if l.on("source.signature.fingerprint") {
		lintSignatureFingerprints(l, path, "source.url.signature", s.URL.Signature)
		lintSignatureFingerprints(l, path, "source.url.patch_series.signature", s.URL.PatchSeries.Signature)
	}
	if l.on("source.signature.keys") {
		lintSignatureKeys(l, recipe, "source.url.signature", s.URL.Signature)
		lintSignatureKeys(l, recipe, "source.url.patch_series.signature", s.URL.PatchSeries.Signature)
	}
	if l.on("source.patches.headers") && s.Patches != "" {
		lintPatchHeaders(l, recipe)
	}
}

// constraintHasFloor reports whether a versions constraint bounds from
// below: a `>=`, `>`, `=`, `^` or `~` term. `< x` alone, `*` and an empty
// constraint follow anything upstream ever published.
func constraintHasFloor(raw string) bool {
	for _, part := range splitConstraintParts(raw) {
		part = strings.TrimSpace(part)
		if part == "" || part == "*" || strings.HasPrefix(part, "<") || strings.HasPrefix(part, "!=") {
			continue
		}
		return true
	}
	return false
}

// constraintCeiling returns the first term of a versions constraint that
// bounds from above, or "". `<` and `<=` are explicit ceilings; `=` pins
// one release; `^` and `~` imply the next major or minor as a ceiling.
func constraintCeiling(raw string) string {
	for _, part := range splitConstraintParts(raw) {
		part = strings.TrimSpace(part)
		switch {
		case part == "" || part == "*":
		case strings.HasPrefix(part, "<"), strings.HasPrefix(part, "^"), strings.HasPrefix(part, "~"):
			return part
		case strings.HasPrefix(part, "=") && !strings.HasPrefix(part, "=>"):
			return part
		}
	}
	return ""
}

func lintURLSchemes(l *linter, path string, s SourceConfig) {
	check := func(field, url string, git bool) {
		if url == "" || strings.HasPrefix(url, "{{") {
			return
		}
		if strings.HasPrefix(url, "https://") {
			return
		}
		if git && (strings.HasPrefix(url, "ssh://") || strings.HasPrefix(url, "git+ssh://") || scpLikeGitURL(url)) {
			return
		}
		l.report("source.url.scheme", "", path, "%s = %q is not fetched over https; a plain-text transport lets anyone on the path substitute the bytes", field, url)
	}
	check("source.url.url", s.URL.URL, false)
	check("source.url.listing_url", s.URL.ListingURL, false)
	check("source.url.signature.url", s.URL.Signature.URL, false)
	check("source.url.patch_series.url", s.URL.PatchSeries.URL, false)
	check("source.url.patch_series.signature.url", s.URL.PatchSeries.Signature.URL, false)
	check("source.git.url", s.Git.URL, true)
}

func scpLikeGitURL(url string) bool {
	if strings.Contains(url, "://") {
		return false
	}
	at := strings.Index(url, "@")
	colon := strings.Index(url, ":")
	return at > 0 && colon > at
}

func lintLock(l *linter, recipe RecipeConfig) {
	path := lockFilePath(recipe.Root)
	lock, err := LoadLockFile(recipe.Root)
	if err != nil {
		l.report("source.lock", "", path, "lockfile cannot be read: %s", err)
		return
	}
	if len(lock.Sources) == 0 {
		l.report("source.lock", "", path, "no lock entries: the source has never been pinned (run pekit lock --latest)")
		return
	}
	signed := recipe.Source.URL.Signature.Configured()
	for _, entry := range lock.Sources {
		switch entry.kind() {
		case "url":
			if !hex64RE.MatchString(entry.SHA256) {
				l.report("source.lock", "", path, "version %s is locked without a sha256", entry.Version)
			}
			if signed && entry.SignatureKey == "" {
				l.report("source.lock", "", path, "version %s was locked before the signature block existed, so its bytes were never verified against the upstream key (pekit lock --repin --version %s)", entry.Version, entry.Version)
			}
			for _, patch := range entry.Patches {
				if !hex64RE.MatchString(patch.SHA256) {
					l.report("source.lock", "", path, "version %s patch %s is locked without a sha256", entry.Version, patch.URL)
				}
			}
		case "git":
			if !hex40RE.MatchString(entry.Commit) {
				l.report("source.lock", "", path, "version %s is locked to %q, which is not a commit hash", entry.Version, entry.Commit)
			}
		}
	}
}

func lintSignatureFingerprints(l *linter, path, block string, sig URLSignatureConfig) {
	if !sig.Configured() {
		return
	}
	if len(sig.Fingerprints) == 0 {
		l.report("source.signature.fingerprint", "", path, "[%s] pins key files but no fingerprints; a key file alone says nothing about which key was meant", block)
		return
	}
	for _, fp := range sig.Fingerprints {
		compact := strings.ReplaceAll(strings.TrimPrefix(fp, "0x"), " ", "")
		if !fullFingerprintRE.MatchString(compact) {
			l.report("source.signature.fingerprint", "", path, "[%s] fingerprint %q is not a full 40- or 64-hex-digit fingerprint; a short key id can be collided", block, fp)
		}
	}
}

func lintSignatureKeys(l *linter, recipe RecipeConfig, block string, sig URLSignatureConfig) {
	for _, key := range sig.KeyFiles {
		resolved := key
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(recipe.Root, key)
		}
		st, err := os.Stat(resolved)
		if err != nil || st.IsDir() || st.Size() == 0 {
			l.report("source.signature.keys", "", resolved, "[%s] key file %q is missing or empty", block, key)
		}
	}
}

// lintPatchHeaders wants every patch to say what it does and where it came
// from, DEP-3 style: a Description or Subject line and an Origin, Author or
// From line ahead of the first hunk. git format-patch output satisfies it
// as written.
func lintPatchHeaders(l *linter, recipe RecipeConfig) {
	ps, err := loadPatchSet(recipe, true)
	if err != nil {
		l.report("source.patches.headers", "", filepath.Join(recipe.Root, recipe.Source.Patches), "patch series cannot be read: %s", err)
		return
	}
	if ps == nil {
		return
	}
	for _, entry := range ps.Entries {
		path := filepath.Join(ps.Dir, filepath.FromSlash(entry))
		desc, origin, err := patchHeaderFields(path)
		if err != nil {
			l.report("source.patches.headers", "", path, "patch cannot be read: %s", err)
			continue
		}
		var missing []string
		if !desc {
			missing = append(missing, "Description or Subject")
		}
		if !origin {
			missing = append(missing, "Origin, Author or From")
		}
		if len(missing) > 0 {
			l.report("source.patches.headers", "", path, "patch header lacks a %s line", strings.Join(missing, " line and a "))
		}
	}
}

func patchHeaderFields(path string) (desc, origin bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "Index: ") || strings.HasPrefix(line, "+++ ") {
			break
		}
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "description:"), strings.HasPrefix(lower, "subject:"):
			desc = true
		case strings.HasPrefix(lower, "origin:"), strings.HasPrefix(lower, "author:"), strings.HasPrefix(lower, "from:"):
			origin = true
		}
	}
	return desc, origin, sc.Err()
}

// --- build -----------------------------------------------------------------

func lintBuildRules(l *linter, recipe RecipeConfig) {
	builds := recipe.Targets[CommandBuild]
	delegated := recipe.Delegate.AllowsBuild()
	if l.on("build.dependencies.providers") && len(builds) > 0 {
		providers := l.cfg.Strings("build.dependencies.providers")
		for _, name := range sortedKeys(builds) {
			target := builds[name]
			for _, provider := range providers {
				if _, ok := target.Dependencies[provider]; !ok {
					l.report("build.dependencies.providers", "", target.Path, "build target %q declares no [build.%s.dependencies.%s] table; an empty table says \"none\" explicitly", name, name, provider)
				}
			}
		}
	}
	if l.on("build.test") && len(recipe.Targets[CommandTest]) == 0 && !delegated {
		l.report("build.test", "", recipe.Path, "recipe defines no test target")
	}
}

// --- SPDX ------------------------------------------------------------------

var spdxIDRE = regexp.MustCompile(`^[A-Za-z0-9.+-]+$`)

// validateSPDXExpression checks the grammar of an SPDX license expression:
// identifiers joined by AND / OR, WITH for exceptions, parentheses, and a
// trailing + for or-later. It does not consult the SPDX license list.
func validateSPDXExpression(expr string) error {
	p := &spdxParser{tokens: tokenizeSPDX(expr)}
	if len(p.tokens) == 0 {
		return spdxError("empty expression")
	}
	if err := p.expression(); err != nil {
		return err
	}
	if p.pos < len(p.tokens) {
		return spdxError("unexpected %q", p.tokens[p.pos])
	}
	return nil
}

type spdxParser struct {
	tokens []string
	pos    int
}

type spdxErr string

func (e spdxErr) Error() string { return string(e) }

func spdxError(format string, args ...any) error {
	return spdxErr(strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func tokenizeSPDX(expr string) []string {
	var tokens []string
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range expr {
		switch r {
		case '(', ')':
			flush()
			tokens = append(tokens, string(r))
		case ' ', '\t', '\n', '\r':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}

func (p *spdxParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *spdxParser) expression() error {
	if err := p.operand(); err != nil {
		return err
	}
	for {
		switch tok := p.peek(); tok {
		case "AND", "OR":
			p.pos++
			if err := p.operand(); err != nil {
				return err
			}
		case "and", "or", "And", "Or":
			return spdxError("operator %q must be upper-case", tok)
		default:
			return nil
		}
	}
}

func (p *spdxParser) operand() error {
	tok := p.peek()
	switch {
	case tok == "":
		return spdxError("expression ends where a license was expected")
	case tok == "(":
		p.pos++
		if err := p.expression(); err != nil {
			return err
		}
		if p.peek() != ")" {
			return spdxError("missing closing parenthesis")
		}
		p.pos++
		return nil
	case tok == ")" || tok == "AND" || tok == "OR" || tok == "WITH":
		return spdxError("unexpected %q", tok)
	case !spdxIDRE.MatchString(tok):
		return spdxError("%q is not a license identifier", tok)
	}
	p.pos++
	if p.peek() == "WITH" {
		p.pos++
		exc := p.peek()
		if exc == "" || !spdxIDRE.MatchString(exc) || exc == "AND" || exc == "OR" {
			return spdxError("WITH must be followed by an exception identifier")
		}
		p.pos++
	}
	return nil
}
