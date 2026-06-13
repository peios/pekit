package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasGlobMeta(t *testing.T) {
	cases := map[string]bool{
		"usr/bin/figlet":      false,
		"usr/share/figlet/**": true,
		"usr/share/*.flf":     true,
		"file?":               true,
		"a[bc]":               true,
		"a{b,c}":              true,
		"plain/path/here":     false,
	}
	for in, want := range cases {
		if got := hasGlobMeta(in); got != want {
			t.Errorf("hasGlobMeta(%q) = %v, want %v", in, got, want)
		}
	}
}

// writeTree creates empty files at the given slash-separated paths under root,
// making parent directories as needed.
func writeTree(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func dests(staged []StagedFile) []string {
	out := make([]string, len(staged))
	for i, s := range staged {
		out[i] = s.Dest
	}
	return out
}

func TestExpandSourceSingleFile(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "usr/bin/figlet")

	got, err := expandSource(root, SourceRef{Target: "main", Path: "usr/bin/figlet"}, "usr/bin/figlet", "figlet")
	if err != nil {
		t.Fatalf("expandSource = %v", err)
	}
	if len(got) != 1 || got[0].Dest != "usr/bin/figlet" {
		t.Fatalf("got %+v, want one usr/bin/figlet", got)
	}
	if _, err := os.Stat(got[0].Source); err != nil {
		t.Errorf("source path does not exist: %v", err)
	}

	// A missing single file is an error, with a build hint for stage refs.
	_, err = expandSource(root, SourceRef{Target: "main", Path: "usr/bin/missing"}, "usr/bin/missing", "figlet")
	if err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "pekit build main") {
		t.Errorf("want not-found-with-hint error, got: %v", err)
	}
}

func TestExpandSourceGlobRecursive(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"usr/share/figlet/standard.flf",
		"usr/share/figlet/big.flf",
		"usr/share/figlet/term.flc",
		"usr/share/figlet/sub/extra.flf",
	)
	// An empty directory under the glob must not become a payload entry.
	if err := os.MkdirAll(filepath.Join(root, "usr/share/figlet/emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := expandSource(root, SourceRef{Target: "main", Path: "usr/share/figlet/**"}, "usr/share/figlet", "figlet")
	if err != nil {
		t.Fatalf("expandSource = %v", err)
	}
	want := []string{
		"usr/share/figlet/big.flf",
		"usr/share/figlet/standard.flf",
		"usr/share/figlet/sub/extra.flf", // subdirectory preserved
		"usr/share/figlet/term.flc",
	}
	if got := dests(got); !equalStrings(got, want) {
		t.Errorf("dests = %v, want %v (sorted, identity, dirs skipped)", got, want)
	}
}

func TestExpandSourceRebase(t *testing.T) {
	// The build dropped fonts under fonts/; the glob rebases them onto
	// usr/share/figlet (base != dest).
	root := t.TempDir()
	writeTree(t, root, "fonts/standard.flf", "fonts/big.flf")

	got, err := expandSource(root, SourceRef{Target: "main", Path: "fonts/**"}, "usr/share/figlet", "figlet")
	if err != nil {
		t.Fatalf("expandSource = %v", err)
	}
	want := []string{"usr/share/figlet/big.flf", "usr/share/figlet/standard.flf"}
	if got := dests(got); !equalStrings(got, want) {
		t.Errorf("dests = %v, want %v (rebased onto dest)", got, want)
	}
}

func TestExpandSourceSingleLevelGlob(t *testing.T) {
	// `*.flf` matches one level and one extension: not the .flc, not the subdir.
	root := t.TempDir()
	writeTree(t, root, "f/standard.flf", "f/term.flc", "f/sub/deep.flf")

	got, err := expandSource(root, SourceRef{Path: "f/*.flf"}, "usr/share/figlet", "figlet")
	if err != nil {
		t.Fatalf("expandSource = %v", err)
	}
	want := []string{"usr/share/figlet/standard.flf"}
	if got := dests(got); !equalStrings(got, want) {
		t.Errorf("dests = %v, want %v (single level, .flf only)", got, want)
	}
}

func TestExpandSourceWholeStage(t *testing.T) {
	// `**` with dest "." packs the whole stage, identity-mapped.
	root := t.TempDir()
	writeTree(t, root, "usr/bin/a", "usr/share/x/y")

	got, err := expandSource(root, SourceRef{Target: "main", Path: "**"}, ".", "p")
	if err != nil {
		t.Fatalf("expandSource = %v", err)
	}
	want := []string{"usr/bin/a", "usr/share/x/y"}
	if got := dests(got); !equalStrings(got, want) {
		t.Errorf("dests = %v, want %v (whole stage, identity)", got, want)
	}
}

func TestExpandSourceEmptyMatchErrors(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "usr/bin/figlet")

	_, err := expandSource(root, SourceRef{Target: "main", Path: "usr/share/figlet/**"}, "usr/share/figlet", "figlet")
	if err == nil || !strings.Contains(err.Error(), "matched no files") {
		t.Errorf("want empty-match error, got: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
