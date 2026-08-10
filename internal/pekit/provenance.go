package pekit

import (
	"crypto/ed25519"
	"runtime/debug"
	"strings"
)

// packRun carries the per-run packaging context every written artifact
// shares: the signing key and the provenance identity of the producing
// run (manifest §3.3.4 recipe_ref / builder).
type packRun struct {
	SignKey   ed25519.PrivateKey
	RecipeRef string
	Builder   string
}

// recipeRef pins the recipe tree's VCS identity for the manifest build
// block: "git:<commit>", with "+dirty" appended when the enclosing work
// tree has uncommitted changes anywhere — a dirty tree means the commit
// alone does not describe the build's inputs, so over-marking beats
// lying. Empty when the recipe is not inside a git work tree. Unlike
// source resolution, this deliberately resolves the enclosing
// repository: a workspace member's identity is the workspace repo's
// commit.
func recipeRef(root string) string {
	commit, err := commandOutput(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	ref := "git:" + strings.TrimSpace(commit)
	status, err := commandOutput(root, "git", "status", "--porcelain")
	if err != nil || strings.TrimSpace(status) != "" {
		ref += "+dirty"
	}
	return ref
}

// pekitBuilder identifies this pekit binary for the manifest build
// block: "pekit/<vcs-revision>" (12 hex chars, "+dirty" when built from
// a modified tree), falling back to the module version when the binary
// carries no VCS stamp.
func pekitBuilder() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "pekit/unknown"
	}
	var revision string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if dirty {
			revision += "+dirty"
		}
		return "pekit/" + revision
	}
	version := strings.Trim(info.Main.Version, "()")
	if version == "" {
		version = "unknown"
	}
	return "pekit/" + version
}
