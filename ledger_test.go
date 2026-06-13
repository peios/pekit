package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLedgerRecordAndSkip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pekit.built")
	l, err := loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.has("0.34.0") {
		t.Error("empty ledger should not contain anything")
	}
	if err := l.add("0.34.0"); err != nil {
		t.Fatal(err)
	}
	if !l.has("0.34.0") {
		t.Error("0.34.0 should be recorded")
	}

	// Persisted and reloadable.
	l2, err := loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if !l2.has("0.34.0") {
		t.Error("0.34.0 should survive reload")
	}
}

func TestLedgerMissingFileIsEmpty(t *testing.T) {
	l, err := loadLedger(filepath.Join(t.TempDir(), "nope.built"))
	if err != nil {
		t.Fatalf("missing file should be empty, not error: %v", err)
	}
	if l.has("anything") {
		t.Error("missing ledger should be empty")
	}
}

func TestLedgerSemverSorted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pekit.built")
	l, _ := loadLedger(path)
	for _, v := range []string{"0.10.0", "0.9.0", "0.34.0", "0.2.0"} {
		if err := l.add(v); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "0.2.0\n0.9.0\n0.10.0\n0.34.0\n"
	if string(data) != want {
		t.Errorf("ledger = %q, want semver-sorted %q", string(data), want)
	}
}

func TestLedgerAddIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pekit.built")
	l, _ := loadLedger(path)
	_ = l.add("1.0.0")
	_ = l.add("1.0.0")
	data, _ := os.ReadFile(path)
	if string(data) != "1.0.0\n" {
		t.Errorf("duplicate add should not duplicate line: %q", string(data))
	}
}
