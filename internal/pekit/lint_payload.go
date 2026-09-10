package pekit

import (
	"bytes"
	"debug/elf"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// The payload rules look at what each package would pack: the file set
// `package` resolves from an existing build stage, attributed to the package
// that ships it. They run only when `pekit lint` is given a version, and they
// never build — a missing stage is an error that names the build to run.

// Defaults for the parameters a payload rule reads. Every one can be
// replaced in lint.pekit.toml; none of them names anything Peios-specific
// that another distribution would not also use.
var (
	lintDefaultBinDirs      = []string{"bin", "sbin", "usr/bin", "usr/sbin"}
	lintDefaultLibDirs      = []string{"lib", "lib64", "usr/lib", "usr/lib64", "lib/*-linux-*", "usr/lib/*-linux-*"}
	lintDefaultManDirs      = []string{"usr/share/man", "share/man"}
	lintDefaultInterpreters = []string{
		"/bin/sh", "/usr/bin/sh", "/bin/bash", "/usr/bin/bash", "/usr/bin/env",
		"/usr/bin/python3", "/usr/bin/perl",
	}
	lintDefaultJunk = []string{
		"**/*.la", "**/*.orig", "**/*.rej", "**/*.o", "**/*~", "**/*.swp",
		"**/.git", "**/.git/**", "**/.gitignore", "**/CMakeCache.txt", "**/.DS_Store",
		"usr/share/info/dir", "**/perllocal.pod", "**/.packlist",
	}
	lintDefaultDevelFiles = []string{
		"usr/include/**", "include/**", "**/pkgconfig/*.pc", "**/cmake/**",
		"usr/share/aclocal/**", "usr/lib/**/*.cmake",
	}
	lintDebugDirs       = []string{"usr/lib/debug/**", "usr/src/**"}
	lintManCompression  = []string{".gz", ".xz", ".zst", ".bz2", ".lz"}
	lintSharedLibNameRE = regexp.MustCompile(`^lib[^/]+\.so(\.\d+)*$`)
	lintShellNames      = map[string]bool{"sh": true, "dash": true, "ash": true, "bash": true}
)

// lintPackage is one package instance and its resolved payload, keyed by
// clean destination path.
type lintPackage struct {
	Inst  PackageInstance
	Files map[string]payloadEntry
	Dests []string // sorted
}

// lintPayloadSet is every package the recipe would pack for one version,
// with the union of their payloads for cross-package lookups (a -devel
// symlink pointing into -libs, a man page shipped beside its binary).
type lintPayloadSet struct {
	Packages []lintPackage
	byName   map[string]*lintPackage
	union    map[string]string // dest -> package name
	claims   map[string]bool   // claim slot paths, cross-package by design
	workBase string
	elfCache map[string]*elfInfo
}

func (s *lintPayloadSet) has(dest string) bool {
	if _, ok := s.union[dest]; ok {
		return true
	}
	prefix := dest + "/"
	for d := range s.union {
		if strings.HasPrefix(d, prefix) {
			return true
		}
	}
	return false
}

func (s *lintPayloadSet) elf(source string) *elfInfo {
	if info, ok := s.elfCache[source]; ok {
		return info
	}
	info, _ := analyzeELF(source)
	s.elfCache[source] = info
	return info
}

func dirMatches(dir string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := doublestar.Match(p, dir); ok {
			return true
		}
	}
	return false
}

func pathMatches(p string, patterns []string) bool {
	for _, pat := range patterns {
		if ok, _ := doublestar.Match(pat, p); ok {
			return true
		}
	}
	return false
}

func lintPayload(l *linter, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version) error {
	packages, err := loadEffectivePackages(recipe, workspace, source)
	if err != nil {
		if diagCode(err) == "missing_package" {
			l.ctx.Renderer.Event(Event{Type: "lint_skipped", Member: l.member, Version: version.Raw, Message: "recipe defines no packages; payload rules have nothing to check"})
			return nil
		}
		return err
	}
	buildNames, err := inferPackageBuilds(packages)
	if err != nil {
		return err
	}
	for _, name := range buildNames {
		stage := targetStage(source, CommandBuild, name)
		if !dirExists(stage) {
			return diag("stage_missing", "build target %q has no stage at %s; lint never builds — run `pekit build --version %s` first", name, stage, version.Raw)
		}
	}
	set := &lintPayloadSet{byName: map[string]*lintPackage{}, union: map[string]string{}, claims: map[string]bool{}, workBase: source.WorkBase, elfCache: map[string]*elfInfo{}}
	for _, def := range packages {
		instances, err := expandPackageInstances(def, source, recipe, workspace, version)
		if err != nil {
			return err
		}
		for _, inst := range instances {
			entries, err := resolvePayloadEntries(inst, source, recipe, workspace, version)
			if err != nil {
				return err
			}
			pkg := lintPackage{Inst: inst, Files: map[string]payloadEntry{}}
			for _, entry := range entries {
				dest, err := cleanRelPath(entry.Dest)
				if err != nil {
					l.report("payload.filenames", inst.Name, entry.Dest, "destination is not a clean relative path: %s", err)
					continue
				}
				entry.Dest = dest
				pkg.Files[dest] = entry
				set.union[dest] = inst.Name
			}
			pkg.Dests = sortedKeys(pkg.Files)
			for _, side := range []map[string]map[string]ClaimSlot{inst.Config.Package.Claims.Provides, inst.Config.Package.Claims.Dependencies} {
				for _, slots := range side {
					for _, slot := range slots {
						if slot.Path != "" {
							if p, err := cleanRelPath(slot.Path); err == nil {
								set.claims[p] = true
							}
						}
					}
				}
			}
			set.Packages = append(set.Packages, pkg)
		}
	}
	for i := range set.Packages {
		set.byName[set.Packages[i].Inst.Name] = &set.Packages[i]
	}
	for _, pkg := range set.Packages {
		lintPackagePayload(l, set, pkg, version)
	}
	return nil
}

func lintPackagePayload(l *linter, set *lintPayloadSet, pkg lintPackage, version Version) {
	name := pkg.Inst.Name
	binDirs := l.cfg.StringsOr("payload.dirs.bin", lintDefaultBinDirs)
	libDirs := l.cfg.StringsOr("payload.dirs.lib", lintDefaultLibDirs)
	manDirs := l.cfg.StringsOr("payload.dirs.man", lintDefaultManDirs)

	if l.on("payload.filenames") {
		for _, dest := range pkg.Dests {
			for _, r := range dest {
				if r < 0x21 || r > 0x7e {
					l.report("payload.filenames", name, dest, "path contains %q; portable names are printable ASCII without whitespace", r)
					break
				}
			}
		}
	}
	if l.on("payload.special_files") {
		for _, dest := range pkg.Dests {
			if fi, err := os.Lstat(pkg.Files[dest].Source); err == nil && fi.Mode()&(os.ModeDevice|os.ModeCharDevice|os.ModeNamedPipe|os.ModeSocket) != 0 {
				l.report("payload.special_files", name, dest, "%s is a device, fifo or socket; a package payload carries files, directories and symlinks", dest)
			}
		}
	}
	if l.on("payload.symlinks.dangling") || l.on("payload.symlinks.absolute") {
		lintSymlinks(l, set, pkg)
	}
	if l.on("payload.junk") {
		lintJunk(l, set, pkg)
	}
	if l.on("payload.scripts") {
		lintScripts(l, set, pkg, binDirs)
	}
	if l.on("payload.manpages.required") || l.on("payload.manpages.compression") {
		lintManpages(l, set, pkg, binDirs, manDirs)
	}
	if l.on("payload.pkgconfig") {
		for _, dest := range pkg.Dests {
			if strings.HasSuffix(dest, ".pc") && isRegular(pkg.Files[dest].Source) && fileContainsBytes(pkg.Files[dest].Source, []byte(set.workBase)) {
				l.report("payload.pkgconfig", name, dest, "pkg-config file refers to the build directory %s; consumers would look for headers and libraries there", set.workBase)
			}
		}
	}
	if l.on("payload.license_file") {
		if dir, err := renderLintTemplate(l.cfg.Str("payload.license_file"), pkg.Inst, version); err != nil {
			l.report("payload.license_file", name, "", "payload.license_file template: %s", err)
		} else if !pkg.hasRegularUnder(dir) {
			l.report("payload.license_file", name, dir, "package ships no licence file under %s/", dir)
		}
	}
	if l.on("package.architecture") {
		lintArchitecture(l, set, pkg, libDirs)
	}
	if l.on("split.devel.packages") {
		lintDevelSplit(l, set, pkg, libDirs)
	}
	lintELFRules(l, set, pkg, libDirs, version)
}

// hasRegularUnder reports whether the package ships a regular file at dir or
// anywhere beneath it.
func (p lintPackage) hasRegularUnder(dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	for _, dest := range p.Dests {
		if dest == dir || strings.HasPrefix(dest, dir+"/") {
			if isRegular(p.Files[dest].Source) {
				return true
			}
		}
	}
	return false
}

func isRegular(source string) bool {
	fi, err := os.Lstat(source)
	return err == nil && fi.Mode().IsRegular()
}

func isSymlink(source string) bool {
	fi, err := os.Lstat(source)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// renderLintTemplate renders a lint template: {{name}} is the package, the
// rest are the version tokens every recipe template has.
func renderLintTemplate(tmpl string, inst PackageInstance, version Version) (string, error) {
	out := strings.ReplaceAll(tmpl, "{{name}}", inst.Name)
	if strings.Contains(out, "{{") {
		rendered, err := RenderTemplate(out, TemplateContext{Version: version, Multipack: inst.Multipack})
		if err != nil {
			return "", err
		}
		out = rendered
	}
	return strings.TrimSuffix(out, "/"), nil
}

func lintSymlinks(l *linter, set *lintPayloadSet, pkg lintPackage) {
	name := pkg.Inst.Name
	for _, dest := range pkg.Dests {
		src := pkg.Files[dest].Source
		if !isSymlink(src) {
			continue
		}
		target, err := os.Readlink(src)
		if err != nil {
			continue
		}
		var resolved string
		if path.IsAbs(target) {
			if l.on("payload.symlinks.absolute") {
				l.report("payload.symlinks.absolute", name, dest, "symlink points at absolute %s; it breaks when the tree is mounted anywhere but /", target)
			}
			resolved = strings.TrimPrefix(path.Clean(target), "/")
		} else {
			resolved = path.Clean(path.Join(path.Dir(dest), target))
		}
		if !l.on("payload.symlinks.dangling") || set.claims[dest] {
			continue
		}
		if resolved == ".." || strings.HasPrefix(resolved, "../") {
			l.report("payload.symlinks.dangling", name, dest, "symlink target %s escapes the payload root", target)
			continue
		}
		if !set.has(resolved) {
			l.report("payload.symlinks.dangling", name, dest, "symlink target %s is shipped by no package of this recipe", target)
		}
	}
}

func lintJunk(l *linter, set *lintPayloadSet, pkg lintPackage) {
	name := pkg.Inst.Name
	patterns := l.cfg.Strings("payload.junk")
	if patterns == nil {
		patterns = lintDefaultJunk
	} else {
		var expanded []string
		for _, p := range patterns {
			if p == "@default" {
				expanded = append(expanded, lintDefaultJunk...)
			} else {
				expanded = append(expanded, p)
			}
		}
		patterns = expanded
	}
	for _, dest := range pkg.Dests {
		if pathMatches(dest, patterns) {
			l.report("payload.junk", name, dest, "build leftover shipped in the payload")
			continue
		}
		if strings.HasSuffix(dest, ".pyc") {
			py := pycSource(dest)
			if !set.has(py) {
				l.report("payload.junk", name, dest, "compiled Python without its source %s", py)
			}
		}
	}
}

// pycSource maps a .pyc destination to the .py it was compiled from:
// pkg/__pycache__/mod.cpython-312.pyc -> pkg/mod.py, or mod.pyc -> mod.py.
func pycSource(dest string) string {
	dir, base := path.Split(dest)
	base = strings.TrimSuffix(base, ".pyc")
	if i := strings.Index(base, ".cpython-"); i >= 0 {
		base = base[:i]
	} else if i := strings.Index(base, ".opt-"); i >= 0 {
		base = base[:i]
	}
	dir = strings.TrimSuffix(dir, "/")
	if path.Base(dir) == "__pycache__" {
		dir = path.Dir(dir)
	}
	return path.Join(dir, base+".py")
}

func lintScripts(l *linter, set *lintPayloadSet, pkg lintPackage, binDirs []string) {
	name := pkg.Inst.Name
	interpreters := l.cfg.StringsOr("payload.interpreters", lintDefaultInterpreters)
	knownInterp := map[string]bool{}
	knownBase := map[string]bool{}
	for _, i := range interpreters {
		knownInterp[i] = true
		knownBase[path.Base(i)] = true
	}
	for _, dest := range pkg.Dests {
		src := pkg.Files[dest].Source
		fi, err := os.Lstat(src)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		line, ok := shebangLine(src)
		if !ok {
			continue
		}
		if fi.Mode()&0o111 == 0 {
			l.report("payload.scripts", name, dest, "script has a #! line but is not executable")
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			l.report("payload.scripts", name, dest, "empty #! line")
			continue
		}
		interp := fields[0]
		if !path.IsAbs(interp) {
			l.report("payload.scripts", name, dest, "interpreter %q is not an absolute path", interp)
			continue
		}
		effective := interp
		if path.Base(interp) == "env" {
			if len(fields) < 2 {
				l.report("payload.scripts", name, dest, "#!%s names no program", interp)
				continue
			}
			prog := fields[1]
			if !knownBase[prog] && !set.hasProgram(prog, binDirs) {
				l.report("payload.scripts", name, dest, "interpreter %s is neither shipped by this recipe nor in payload.interpreters", prog)
			}
			effective = prog
		} else if !knownInterp[interp] && !set.has(strings.TrimPrefix(interp, "/")) {
			l.report("payload.scripts", name, dest, "interpreter %s is neither shipped by this recipe nor in payload.interpreters", interp)
		}
		if shell := path.Base(effective); lintShellNames[shell] {
			if msg := shellSyntaxError(shell, src); msg != "" {
				l.report("payload.scripts", name, dest, "shell syntax check failed: %s", msg)
			}
		}
	}
}

func (s *lintPayloadSet) hasProgram(prog string, binDirs []string) bool {
	for dest := range s.union {
		if path.Base(dest) == prog && dirMatches(path.Dir(dest), binDirs) {
			return true
		}
	}
	return false
}

func shebangLine(src string) (string, bool) {
	f, err := os.Open(src)
	if err != nil {
		return "", false
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	buf = buf[:n]
	if !bytes.HasPrefix(buf, []byte("#!")) {
		return "", false
	}
	// Rust inner attributes use the same two-byte prefix as a Unix shebang
	// (for example, #![no_std]) but do not name an interpreter. Treating them
	// as scripts produces false findings for debugsource packages.
	if len(buf) >= 3 && buf[2] == '[' {
		return "", false
	}
	line := buf[2:]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return string(line), true
}

// shellSyntaxError runs the host's `<shell> -n` over a script and returns
// the first line of its complaint, or "" when the script parses or no such
// shell is installed here.
func shellSyntaxError(shell, src string) string {
	host, err := exec.LookPath(shell)
	if err != nil {
		if shell == "sh" {
			return ""
		}
		if host, err = exec.LookPath("sh"); err != nil {
			return ""
		}
	}
	cmd := exec.Command(host, "-n", src)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if first == "" {
		first = err.Error()
	}
	return first
}

func lintManpages(l *linter, set *lintPayloadSet, pkg lintPackage, binDirs, manDirs []string) {
	name := pkg.Inst.Name
	sectionDirs := make([]string, 0, 2*len(manDirs))
	for _, m := range manDirs {
		sectionDirs = append(sectionDirs, m+"/man[1-9]*", m+"/*/man[1-9]*")
	}
	pages := map[string]bool{}
	for dest := range set.union {
		if dirMatches(path.Dir(dest), sectionDirs) {
			pages[manPageName(path.Base(dest))] = true
		}
	}
	if l.on("payload.manpages.required") {
		for _, dest := range pkg.Dests {
			if !dirMatches(path.Dir(dest), binDirs) {
				continue
			}
			fi, err := os.Lstat(pkg.Files[dest].Source)
			if err != nil || fi.IsDir() {
				continue
			}
			if !pages[path.Base(dest)] {
				l.report("payload.manpages.required", name, dest, "program has no manual page")
			}
		}
	}
	if mode := l.cfg.Str("payload.manpages.compression"); mode != "" {
		all := make([]string, 0, len(manDirs))
		for _, m := range manDirs {
			all = append(all, m+"/**")
		}
		for _, dest := range pkg.Dests {
			if !pathMatches(dest, all) || !isRegular(pkg.Files[dest].Source) {
				continue
			}
			compressed := hasAnySuffix(dest, lintManCompression)
			switch {
			case mode == "gzip" && !strings.HasSuffix(dest, ".gz"):
				l.report("payload.manpages.compression", name, dest, "manual page is not gzip-compressed")
			case mode == "none" && compressed:
				l.report("payload.manpages.compression", name, dest, "manual page is compressed; this tree ships them plain")
			}
		}
	}
}

// manPageName strips a page's compression and section suffixes:
// zstd.1.gz -> zstd, foo.3pm -> foo, foo.bar.1 -> foo.bar.
func manPageName(base string) string {
	for hasAnySuffix(base, lintManCompression) {
		base = base[:strings.LastIndex(base, ".")]
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	return base
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// lintArchitecture wants the declared architecture to agree with the
// payload: an object file, or a path in an architecture-specific directory,
// makes a package architecture-specific; nothing of the kind makes it
// architecture-independent.
func lintArchitecture(l *linter, set *lintPayloadSet, pkg lintPackage, libDirs []string) {
	name := pkg.Inst.Name
	noarch := l.cfg.Str("package.noarch")
	if noarch == "" {
		noarch = "noarch"
	}
	var archDirs []string
	for _, d := range libDirs {
		if strings.Contains(d, "*") {
			archDirs = append(archDirs, d, d+"/**")
		}
	}
	archDirs = append(archDirs, "usr/lib/debug/**", "usr/src/debug/**")
	evidence := ""
	for _, dest := range pkg.Dests {
		if pathMatches(dest, archDirs) {
			evidence = dest
			break
		}
		if isRegular(pkg.Files[dest].Source) && set.elf(pkg.Files[dest].Source) != nil {
			evidence = dest
			break
		}
	}
	switch {
	case pkg.Inst.Architecture == noarch && evidence != "":
		l.report("package.architecture", name, evidence, "package is declared %s but ships architecture-specific content", noarch)
	case pkg.Inst.Architecture != noarch && evidence == "" && len(pkg.Dests) > 0:
		l.report("package.architecture", name, "", "package is declared %s but ships nothing architecture-specific; declare it %s", pkg.Inst.Architecture, noarch)
	}
}

func lintDevelSplit(l *linter, set *lintPayloadSet, pkg lintPackage, libDirs []string) {
	name := pkg.Inst.Name
	if pathMatches(name, l.cfg.Strings("split.devel.packages")) {
		return
	}
	files := l.cfg.StringsOr("split.devel.files", lintDefaultDevelFiles)
	const limit = 10
	n := 0
	for _, dest := range pkg.Dests {
		develFile := pathMatches(dest, files)
		if !develFile && strings.HasSuffix(dest, ".so") && dirMatches(path.Dir(dest), libDirs) && isSymlink(pkg.Files[dest].Source) {
			develFile = true
		}
		if !develFile {
			continue
		}
		n++
		if n <= limit {
			l.report("split.devel.packages", name, dest, "development file in a package not matching split.devel.packages")
		}
	}
	if n > limit {
		l.report("split.devel.packages", name, "", "%d more development files in this package", n-limit)
	}
}

// --- [elf] -----------------------------------------------------------------

func lintELFRules(l *linter, set *lintPayloadSet, pkg lintPackage, libDirs []string, version Version) {
	name := pkg.Inst.Name
	anyELF := false
	for _, id := range []string{"elf.pie", "elf.stack", "elf.relro", "elf.relr", "elf.cet", "elf.rpath", "elf.textrel", "elf.stripped", "elf.debuginfo", "elf.build_paths", "elf.soname"} {
		if l.on(id) {
			anyELF = true
			break
		}
	}
	if !anyELF {
		return
	}
	debugPkg := (*lintPackage)(nil)
	debugPkgName := ""
	debugChecked := false
	for _, dest := range pkg.Dests {
		src := pkg.Files[dest].Source
		if !isRegular(src) || pathMatches(dest, lintDebugDirs) {
			continue
		}
		info := set.elf(src)
		if info == nil || info.Type == elf.ET_REL {
			continue
		}
		if l.on("elf.pie") && info.Type == elf.ET_EXEC {
			l.report("elf.pie", name, dest, "program is not position-independent (ET_EXEC); build with -fPIE -pie so ASLR applies to it")
		}
		if l.on("elf.stack") {
			switch {
			case !info.HasGnuStack:
				l.report("elf.stack", name, dest, "no PT_GNU_STACK header; the loader may map the stack executable")
			case info.StackExec:
				l.report("elf.stack", name, dest, "executable stack requested by PT_GNU_STACK")
			}
		}
		if l.on("elf.relro") && info.Dynamic {
			switch {
			case !info.Relro:
				l.report("elf.relro", name, dest, "no PT_GNU_RELRO segment; link with -z relro")
			case l.cfg.Str("elf.relro") == "full" && !info.BindNow:
				l.report("elf.relro", name, dest, "partial RELRO only; link with -z now so the GOT is read-only after load")
			}
		}
		if l.on("elf.relr") && info.Dynamic && !info.Relr && info.RelativeRelocs > 0 {
			l.report("elf.relr", name, dest, "%d relative relocations in .rela.dyn and no DT_RELR; link with -z pack-relative-relocs", info.RelativeRelocs)
		}
		if l.on("elf.cet") && (info.Machine == elf.EM_X86_64 || info.Machine == elf.EM_386) && !info.CET {
			l.report("elf.cet", name, dest, "no IBT+SHSTK property note; compile with -fcf-protection (an assembly object without the note drops it for the whole link)")
		}
		if l.on("elf.rpath") && info.Rpath != "" {
			l.report("elf.rpath", name, dest, "carries a run path %q", info.Rpath)
		}
		if l.on("elf.textrel") && info.Textrel {
			l.report("elf.textrel", name, dest, "has text relocations; the code segment cannot be shared read-only")
		}
		if l.on("elf.stripped") && (info.HasDebugSections || info.HasSymtab) {
			what := "debug sections"
			if !info.HasDebugSections {
				what = "a symbol table"
			}
			l.report("elf.stripped", name, dest, "still carries %s; strip it (and keep the debug data in the debuginfo package)", what)
		}
		if l.on("elf.debuginfo") {
			if !debugChecked {
				debugChecked = true
				if rendered, err := renderLintTemplate(l.cfg.Str("elf.debuginfo"), pkg.Inst, version); err != nil {
					l.report("elf.debuginfo", name, "", "elf.debuginfo template: %s", err)
				} else {
					debugPkgName = rendered
					debugPkg = set.byName[rendered]
					if debugPkg == nil {
						l.report("elf.debuginfo", name, "", "ships objects but this recipe has no %s package for their debug data", rendered)
					}
				}
			}
			if debugPkg != nil {
				switch {
				case info.BuildID == "":
					l.report("elf.debuginfo", name, dest, "has no build-id note, so no debug file can be matched to it; link with --build-id")
				default:
					expected := "usr/lib/debug/.build-id/" + info.BuildID[:2] + "/" + info.BuildID[2:] + ".debug"
					if _, ok := debugPkg.Files[expected]; !ok {
						l.report("elf.debuginfo", name, dest, "%s ships no %s for it", debugPkgName, expected)
					}
				}
			}
		}
		if l.on("elf.build_paths") && fileContainsBytes(src, []byte(set.workBase)) {
			l.report("elf.build_paths", name, dest, "embeds the build directory %s; use -fmacro-prefix-map / debugedit so the object does not depend on where it was built", set.workBase)
		}
		if l.on("elf.soname") && info.Type == elf.ET_DYN && dirMatches(path.Dir(dest), libDirs) && lintSharedLibNameRE.MatchString(path.Base(dest)) {
			base := path.Base(dest)
			switch {
			case info.Soname == "":
				l.report("elf.soname", name, dest, "shared library has no DT_SONAME; nothing can depend on it by ABI name")
			case base != info.Soname && !strings.HasPrefix(base, info.Soname+"."):
				l.report("elf.soname", name, dest, "file name does not match its DT_SONAME %q", info.Soname)
			}
		}
	}
}
