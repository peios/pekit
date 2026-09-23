package pekit

import (
	"bytes"
	"path/filepath"
	"testing"
)

// `[replaces] name = "*"` is the dependency-style wildcard. Peipkg's manifest
// grammar has no `*` constraint (a bare `*` fails validation); an entry with
// no constraint is how it spells "every version". Pekit writes the one as the
// other, so a recipe using the documented spelling packs a valid archive
// (PEI-722).
func TestReplacesWildcardPacksAnUnconstrainedEntry(t *testing.T) {
	dir := t.TempDir()
	recipe := filepath.Join(dir, "app")
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload\n")})
	writeFile(t, filepath.Join(recipe, "pekit.toml"), signTestRecipe)
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), signTestMemberDef+`
[replaces]
older-app = "*"
oldest-app = "< 2.0"
`)
	chdir(t, recipe)

	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--version", "1.0"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s", err, stderr.String())
	}
	m := readPeipkgManifest(t, globOne(t, filepath.Join(recipe, "out/*/package/*/app_1.0-1_x86_64.peipkg")))
	if len(m.Replaces) != 2 {
		t.Fatalf("replaces = %+v, want two entries", m.Replaces)
	}
	for _, r := range m.Replaces {
		switch r.Name {
		case "older-app":
			if r.Constraint != nil && *r.Constraint != "" {
				t.Errorf("older-app constraint = %q, want none", *r.Constraint)
			}
		case "oldest-app":
			if r.Constraint == nil || *r.Constraint != "< 2.0" {
				t.Errorf("oldest-app constraint = %v, want \"< 2.0\"", r.Constraint)
			}
		default:
			t.Errorf("unexpected replaces entry %q", r.Name)
		}
	}
}
