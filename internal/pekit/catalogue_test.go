package pekit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogueConfiguration(t *testing.T) {
	root := os.Getenv("PEKIT_TEST_CATALOGUE")
	if root == "" {
		t.Skip("set PEKIT_TEST_CATALOGUE to validate a package workspace")
	}
	ws, err := LoadWorkspace(filepath.Join(root, "workspace.pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	members, err := discoverWorkspaceMembers(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if _, err := LoadRecipe(member.RecipePath); err != nil {
			t.Errorf("%s: %v", member.ID, err)
		}
	}
	profiles, err := filepath.Glob(filepath.Join(root, "*.env.pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range profiles {
		if _, err := LoadEnvFile(path, false); err != nil {
			t.Error(err)
		}
	}
	t.Logf("validated %d recipe configurations and %d workspace profiles", len(members), len(profiles))
}
