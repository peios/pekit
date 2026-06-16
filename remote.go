package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// remoteVerbs are the per-recipe verbs that accept a remote-repo argument:
// pekit clones the named recipe and runs the verb inside the checkout, as if
// the user had cloned it and cd'd in themselves. `workspace` is handled
// separately and isn't included.
var remoteVerbs = map[string]bool{
	"build": true, "test": true, "install": true,
	"package": true, "publish": true, "clean": true,
}

// isRemoteSpec reports whether a verb argument names a remote recipe repo
// rather than a local target. A target name is a bare word; a remote spec is a
// git URL ("https://…", "git@…:…", "scheme://…") or a host/path ("github.com/
// owner/repo"). Local filesystem paths (absolute, ./, ../, ~) are never remote.
func isRemoteSpec(arg string) bool {
	if strings.Contains(arg, "://") || strings.HasPrefix(arg, "git@") {
		return true
	}
	if arg == "" || strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, ".") || strings.HasPrefix(arg, "~") {
		return false
	}
	host, _, hasPath := strings.Cut(arg, "/")
	// A leading segment with a dot is a hostname (github.com, git.example.org);
	// a bare word with no slash is a local target.
	return hasPath && strings.Contains(host, ".")
}

// extractRemoteSpec finds a remote-recipe argument among a verb's args and
// returns the args with it removed, the spec, and whether one was found. Only
// the per-recipe verbs are eligible; the value following --version/-V is
// skipped so a versionish value is never mistaken for a spec.
func extractRemoteSpec(args []string) (rest []string, spec string, ok bool) {
	if len(args) == 0 || !remoteVerbs[args[0]] {
		return args, "", false
	}
	for i := 1; i < len(args); i++ {
		if args[i] == "--version" || args[i] == "-V" || args[i] == "--env" {
			i++ // value-taking flag in space form: skip its value too
			continue
		}
		// Any other flag is not a remote spec. Skipping all flags keeps a flag
		// value that happens to look pathy (e.g. --keyring.tcb.priv=/a/b) from
		// being misread as a recipe repo.
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		if isRemoteSpec(args[i]) {
			rest = append(append([]string{}, args[:i]...), args[i+1:]...)
			return rest, args[i], true
		}
	}
	return args, "", false
}

// parseRemoteSpec resolves a remote spec into a git clone URL, the in-repo
// subdirectory the recipe lives in, and an optional ref to check out. A full
// git URL ("…://…" or "git@…") clones as-is with the recipe at its root. A
// host/path spec takes the first three segments (host/owner/repo) as the repo,
// any remaining segments as the recipe subdir, and an optional trailing "@ref"
// to pin the recipe commit/tag/branch.
func parseRemoteSpec(spec string) (cloneURL, subdir, ref string, err error) {
	if strings.Contains(spec, "://") || strings.HasPrefix(spec, "git@") {
		return spec, "", "", nil
	}
	s := spec
	if at := strings.LastIndex(s, "@"); at >= 0 {
		ref = s[at+1:]
		s = s[:at]
		if ref == "" {
			return "", "", "", fmt.Errorf("remote %q: empty ref after '@'", spec)
		}
	}
	segs := strings.Split(strings.Trim(s, "/"), "/")
	if len(segs) < 2 || segs[0] == "" {
		return "", "", "", fmt.Errorf("remote %q: expected host/owner/repo[/subdir][@ref]", spec)
	}
	// host/owner/repo is the repo; fewer than three segments (host/repo) means
	// the whole spec is the repo with no subdir.
	n := min(3, len(segs))
	cloneURL = "https://" + strings.Join(segs[:n], "/") + ".git"
	subdir = path.Join(segs[n:]...)
	return cloneURL, subdir, ref, nil
}

// remoteCacheDir is where a remote recipe is checked out: a readable mirror of
// the repo under the system temp dir (/tmp/pekit/remote/<host>/<path>). It is
// recreated fresh each run and removed when the verb finishes.
func remoteCacheDir(cloneURL string) string {
	s := cloneURL
	for _, p := range []string{"https://", "http://", "ssh://", "git://", "git@"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimSuffix(s, ".git")
	s = strings.ReplaceAll(s, ":", "/") // scp form git@host:path → host/path
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '.' || r == '-' || r == '_':
			return r
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			return r
		default:
			return '_'
		}
	}, s)
	return filepath.Join(os.TempDir(), "pekit", "remote", filepath.FromSlash(s))
}

// cloneRecipe checks the recipe repo out at dir. With no ref it shallow-clones
// the default branch; with a ref it shallow-fetches that tag/branch/commit
// (reusing the [source] fetch path), falling back to a full clone for a ref a
// server won't serve shallowly.
func cloneRecipe(cloneURL, ref, dir string) error {
	if ref == "" {
		if err := runGit("", "clone", "--quiet", "--depth", "1", cloneURL, dir); err != nil {
			os.RemoveAll(dir)
			return fmt.Errorf("cloning %s: %w", cloneURL, err)
		}
		return nil
	}
	src := &Source{Git: cloneURL, Rev: ref}
	if err := shallowFetch(src, dir); err != nil {
		os.RemoveAll(dir)
		if err := fullClone(src, dir); err != nil {
			os.RemoveAll(dir)
			return err
		}
	}
	return nil
}

// runRemote fetches a remote recipe and runs the verb inside it. The checkout
// is always recreated fresh (any stale copy is removed first) and deleted when
// the verb finishes, so a remote build never accumulates trees under /tmp. rest
// is the original argv with the spec removed (verb and flags intact), re-run in
// the checkout so version resolution, [source], and flags behave exactly as a
// local invocation there would.
func runRemote(spec string, rest []string) error {
	cloneURL, subdir, ref, err := parseRemoteSpec(spec)
	if err != nil {
		return err
	}
	dest := remoteCacheDir(cloneURL)
	os.RemoveAll(dest) // always start from a clean tree (always re-fetch)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	refLabel := "default branch"
	if ref != "" {
		refLabel = ref
	}
	fmt.Fprintf(os.Stderr, "pekit: fetching recipe %s (%s)\n", spec, refLabel)
	if err := cloneRecipe(cloneURL, ref, dest); err != nil {
		return err
	}
	defer func() {
		os.RemoveAll(dest)
		fmt.Fprintf(os.Stderr, "pekit: removed %s\n", dest)
	}()

	recipeDir := filepath.Join(dest, subdir)
	if _, err := os.Stat(filepath.Join(recipeDir, "pekit.toml")); err != nil {
		where := ""
		if subdir != "" {
			where = fmt.Sprintf(" (subdir %q)", subdir)
		}
		return fmt.Errorf("remote %s%s: no pekit.toml in the checkout", spec, where)
	}
	fmt.Fprintf(os.Stderr, "pekit: %s in %s\n", rest[0], recipeDir)
	return inDir(recipeDir, func() error { return run(rest) })
}
