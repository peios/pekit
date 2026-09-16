package pekit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peios/peipkg/pack"
	"github.com/peios/peipkg/repopub"
)

func TestCompactSourceBundle(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		writeFile(t, filepath.Join(dir, "inputs/source", name), name)
	}
	if err := os.Symlink("missing/fixture", filepath.Join(dir, "inputs/source/dangling")); err != nil {
		t.Fatal(err)
	}
	entries, err := sourceTreeEntries(filepath.Join(dir, "inputs"), "bundle")
	if err != nil {
		t.Fatal(err)
	}
	manifest := sourceBundleManifest{Schema: 2}
	outer, err := compactSourceBundle(entries, &manifest, "bundle", dir, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(outer) != 1 || manifest.Schema != 3 || manifest.PreparedArchive != "prepared-source.tar" {
		t.Fatalf("unexpected compaction: %+v %+v", outer, manifest)
	}
	first, err := os.ReadFile(outer[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = compactSourceBundle(entries, &sourceBundleManifest{Schema: 2}, "bundle", dir, 8); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(outer[0].Source)
	if !bytes.Equal(first, second) {
		t.Fatal("source tar is not deterministic")
	}
	small := sourceBundleManifest{Schema: 2}
	unchanged, err := compactSourceBundle(entries, &small, "bundle", dir, 100)
	if err != nil || len(unchanged) != len(entries) || small.Schema != 2 {
		t.Fatal("small bundle changed")
	}
	if _, err = compactSourceBundle(entries, &small, "other", dir, 8); err == nil {
		t.Fatal("oversized non-source tree accepted")
	}
}

func TestCompactSourceRebuildSafety(t *testing.T) {
	script := filepath.Join(t.TempDir(), "rebuild.py")
	writeFile(t, script, sourceRebuildScript)
	command := exec.Command("python3", "source_rebuild_test.py", script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("reconstruction tests: %v\n%s", err, output)
	}
}

// Optional real upstream fixture: this catches scale/path/permission issues
// that small adversarial archives cannot expose. No build receipt is produced.
func TestCompactRealSourceTree(t *testing.T) {
	source := os.Getenv("PEKIT_TEST_LARGE_SOURCE")
	if source == "" {
		t.Skip("set PEKIT_TEST_LARGE_SOURCE to a prepared source directory")
	}
	dir := t.TempDir()
	entries, err := sourceTreeEntries(source, "bundle/source")
	if err != nil {
		t.Fatal(err)
	}
	entries = append([]payloadEntry{{Source: source, Dest: "bundle/source"}}, entries...)
	manifest := sourceBundleManifest{Schema: 2, Recipe: "recipe"}
	for _, entry := range entries {
		identity, err := inputIdentity(entry.Source)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Files = append(manifest.Files, sourceBundleFile{Path: strings.TrimPrefix(entry.Dest, "bundle/"), Identity: identity})
	}
	outer, err := compactSourceBundle(entries, &manifest, "bundle", dir, 90_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(outer) != 1 || manifest.Schema != 3 {
		t.Fatalf("expected large source compaction; got %d entries", len(outer))
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "build-inputs.json"), string(data))
	writeFile(t, filepath.Join(dir, "rebuild.py"), sourceRebuildScript)
	bin := filepath.Join(dir, "bin")
	writeFile(t, filepath.Join(bin, "pekit"), "#!/bin/sh\nprintf invoked > "+shellQuote(filepath.Join(dir, "invoked"))+"\n")
	if err := os.Chmod(filepath.Join(bin, "pekit"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	runTestCmd(t, dir, "python3", "rebuild.py", "build")
	if !fileExists(filepath.Join(dir, "invoked")) {
		t.Fatal("reconstruction did not finish")
	}
	info, _ := os.Stat(outer[0].Source)
	t.Logf("verified %d source entries through %d-byte compact archive", len(entries), info.Size())
	// Exercise the actual consumer's entry/decompression limits and signature
	// verifier, not merely the producer and reconstruction script.
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "diagnostic-source.peipkg")
	output, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, name := range []string{"prepared-source.tar", "build-inputs.json", "rebuild.py"} {
		files["usr/src/dist/probe-1.0-1/"+name] = filepath.Join(dir, name)
	}
	err = pack.Pack(pack.PackOptions{
		Manifest: pack.Manifest{
			Name: "probe-source", Version: "1.0-1", Architecture: "noarch",
			Description: "Source transport fixture", License: "GPL-3.0-or-later", LicenseClass: "free",
			Build: pack.BuildInfo{Timestamp: "2026-01-01T00:00:00Z", FarmID: "test", SourceRef: "fixture"},
		},
		Files: files, SignKey: key, Out: output,
	})
	closeErr := output.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	inspected, err := repopub.InspectPackage(archive, []ed25519.PublicKey{public})
	if err != nil {
		t.Fatal(err)
	}
	if !inspected.Signed || len(inspected.Payload) >= 100_000 {
		t.Fatal("invalid compact package")
	}
	t.Logf("signed outer source archive accepted with %d payload entries", len(inspected.Payload))
}
