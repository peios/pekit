package pekit

import (
	"path/filepath"
	"testing"
)

// A dependency's constraint and its root come from one [dependencies]
// entry, and merging them as two independent maps let a base's root
// survive an override that meant to drop it. The member's attempt to
// make it an ordinary same-root dependency silently failed, and the
// shipped manifest placed the package in the base's root — with no way
// to un-set it (PEI-434).
func TestOverridingADependencyDropsTheBasesRoot(t *testing.T) {
	base := PackageMeta{
		Dependencies:    map[string]string{"libfoo": "*"},
		DependencyRoots: map[string]string{"libfoo": "initramfs"},
	}
	// The plain form: a constraint and no root.
	over := PackageMeta{Dependencies: map[string]string{"libfoo": "1.2"}}

	got := mergePackageMeta(base, over)
	if got.Dependencies["libfoo"] != "1.2" {
		t.Errorf("constraint = %q, want the override's", got.Dependencies["libfoo"])
	}
	if root, ok := got.DependencyRoots["libfoo"]; ok {
		t.Errorf("the base's root %q survived the override", root)
	}
}

// The same for optional dependencies, which are the same shape.
func TestOverridingAnOptionalDependencyDropsTheBasesRoot(t *testing.T) {
	base := PackageMeta{
		OptionalDependencies:    map[string]string{"libbar": "*"},
		OptionalDependencyRoots: map[string]string{"libbar": "initramfs"},
	}
	over := PackageMeta{OptionalDependencies: map[string]string{"libbar": "2.0"}}

	got := mergePackageMeta(base, over)
	if root, ok := got.OptionalDependencyRoots["libbar"]; ok {
		t.Errorf("the base's optional root %q survived the override", root)
	}
}

// An override that says nothing about dependencies still inherits the
// base's, roots included — "maps replace wholesale" is the rule, and
// only a layer that speaks replaces.
func TestASilentOverrideInheritsTheBasesDependencyRoots(t *testing.T) {
	base := PackageMeta{
		Dependencies:    map[string]string{"libfoo": "*"},
		DependencyRoots: map[string]string{"libfoo": "initramfs"},
	}
	got := mergePackageMeta(base, PackageMeta{})
	if got.Dependencies["libfoo"] != "*" {
		t.Errorf("constraint = %q, want the base's", got.Dependencies["libfoo"])
	}
	if got.DependencyRoots["libfoo"] != "initramfs" {
		t.Errorf("root = %q, want the base's", got.DependencyRoots["libfoo"])
	}
}

// And an override that re-declares the root keeps it.
func TestAnOverrideMayRestateTheRoot(t *testing.T) {
	base := PackageMeta{
		Dependencies:    map[string]string{"libfoo": "*"},
		DependencyRoots: map[string]string{"libfoo": "initramfs"},
	}
	over := PackageMeta{
		Dependencies:    map[string]string{"libfoo": "1.2"},
		DependencyRoots: map[string]string{"libfoo": "initramfs"},
	}
	got := mergePackageMeta(base, over)
	if got.DependencyRoots["libfoo"] != "initramfs" {
		t.Errorf("root = %q, want the override's", got.DependencyRoots["libfoo"])
	}
}

func TestExplicitlyEmptyPackageSectionsClearInheritedEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.pekit.toml")
	writeFile(t, path, `
side_effects = []

[dependencies]

[optional_dependencies]

[conflicts]

[provides]

[replaces]

[sd_overrides]
`)
	over, err := LoadPackageFile(path)
	if err != nil {
		t.Fatal(err)
	}
	base := PackageMeta{
		Dependencies:         map[string]string{"old-dependency": "*"},
		OptionalDependencies: map[string]string{"old-optional": "*"},
		Conflicts:            map[string]string{"old-conflict": "*"},
		Provides:             map[string]string{"old-provide": "1"},
		Replaces:             map[string]string{"old-replacement": "<= 1"},
		SideEffects:          []string{"ldconfig"},
		SDOverrides:          map[string]string{"usr/bin/old": "system"},
	}

	got := mergePackageMeta(base, over.Package)
	if len(got.Dependencies) != 0 || len(got.OptionalDependencies) != 0 ||
		len(got.Conflicts) != 0 || len(got.Provides) != 0 ||
		len(got.Replaces) != 0 || len(got.SideEffects) != 0 ||
		len(got.SDOverrides) != 0 {
		t.Fatalf("empty overlay sections did not clear inherited entries: %#v", got)
	}
}
