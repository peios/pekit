package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseWorkspace(t *testing.T) {
	ws, err := ParseWorkspace(`include = "./*"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ws.Include != "./*" {
		t.Errorf("Include = %q, want ./*", ws.Include)
	}
}

func TestParseWorkspaceMissingInclude(t *testing.T) {
	_, err := ParseWorkspace(``)
	if err == nil || !strings.Contains(err.Error(), `missing required key "include"`) {
		t.Errorf("want missing-include error, got: %v", err)
	}
}

func TestParseWorkspaceUnknownKey(t *testing.T) {
	_, err := ParseWorkspace("include = \"./*\"\nfoo = \"bar\"\n")
	if err == nil || !strings.Contains(err.Error(), `unknown key "foo"`) {
		t.Errorf("want unknown-key error, got: %v", err)
	}
}

func TestFindWorkspaceRootAndMembers(t *testing.T) {
	root := t.TempDir()
	write := func(p, c string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "workspace.pekit.toml"), `include = "./*"`)
	for _, m := range []string{"a", "b"} {
		md := filepath.Join(root, m)
		if err := os.MkdirAll(md, 0o755); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(md, "pekit.toml"), `outDir = "out"`)
	}
	// A directory without a pekit.toml (e.g. the publish output) is not a member.
	if err := os.MkdirAll(filepath.Join(root, "pkgsOut"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A member walks up to the marker.
	got, found, err := findWorkspaceRoot(filepath.Join(root, "a"))
	if err != nil || !found {
		t.Fatalf("findWorkspaceRoot: found=%v err=%v", found, err)
	}
	if filepath.Clean(got) != filepath.Clean(root) {
		t.Errorf("root = %q, want %q", got, root)
	}

	ws, err := LoadWorkspace(filepath.Join(root, "workspace.pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	members, err := workspaceMembers(root, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || filepath.Base(members[0]) != "a" || filepath.Base(members[1]) != "b" {
		t.Errorf("members = %v, want dirs a, b only", members)
	}
}

func TestFindWorkspaceRootNone(t *testing.T) {
	_, found, err := findWorkspaceRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("found a workspace where none was declared")
	}
}
