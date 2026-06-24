package pekit

import (
	"testing"

	"github.com/peios/peipkg/pack"
)

// The ELF-scanning derivation itself lives in and is tested by peipkg/pack
// (DeriveELFDeps). pekit only owns the merge of derived entries onto the
// recipe-declared manifest fields, where the recipe entry must win.

func names[T any](xs []T, name func(T) string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[name(x)] = true
	}
	return m
}

func TestMergeDedupKeepsRecipeEntry(t *testing.T) {
	existing := []pack.Dependency{{Name: "libfoo.so.1", Constraint: ">= 1.2"}}
	derived := []pack.Dependency{{Name: "libfoo.so.1"}, {Name: "libc.so.6"}}

	merged := mergeDeps(existing, derived)
	if len(merged) != 2 {
		t.Fatalf("merged len = %d, want 2 (recipe libfoo + derived libc)", len(merged))
	}
	byName := names(merged, func(d pack.Dependency) string { return d.Name })
	if !byName["libfoo.so.1"] || !byName["libc.so.6"] {
		t.Fatalf("merged names = %v", byName)
	}
	// The recipe's constraint must survive the merge.
	for _, d := range merged {
		if d.Name == "libfoo.so.1" && d.Constraint != ">= 1.2" {
			t.Errorf("recipe constraint lost: %+v", d)
		}
	}

	mp := mergeProvides(
		[]pack.Provides{{Name: "libfoo.so.1", Version: "1.2"}},
		[]pack.Provides{{Name: "libfoo.so.1"}, {Name: "libbaz.so.2"}},
	)
	if len(mp) != 2 {
		t.Fatalf("mergeProvides len = %d, want 2", len(mp))
	}
	for _, p := range mp {
		if p.Name == "libfoo.so.1" && p.Version != "1.2" {
			t.Errorf("recipe provides version lost: %+v", p)
		}
	}
}
