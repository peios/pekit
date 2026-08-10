package pekit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

const signTestRecipe = `
out_dir = "out"

[source.url]
url = "https://example.test/app-{{version}}.tar.gz"
extract = true
root = "app-{{version}}"

[build]
command = "true"
`

const signTestMemberDef = `
format = "peipkg"

[package]
name = "app"
version = "{{version}}-1"
architecture = "x86_64"
description = "app"
license = "MIT"

[files]
"@source:payload.txt" = "usr/share/app/payload.txt"
`

// writeSeedKey writes a fresh Ed25519 raw-seed key file and returns its
// path and public key.
func writeSeedKey(t *testing.T, dir string) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "signing.key")
	if err := os.WriteFile(path, priv.Seed(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, pub
}

// signedPrefixAndEnvelope decompresses a .peipkg and walks the raw tar
// blocks to split it at the signature entry: it returns the bytes the
// signature covers (everything before the entry, including any pax
// records the entry carries) and the decoded envelope document. Walking
// blocks by hand, instead of archive/tar, is the point — the test
// checks the signed-byte boundary the spec defines (§5.1.2), not what a
// reader reconstructs.
func signedPrefixAndEnvelope(t *testing.T, artifact string) ([]byte, map[string]any) {
	t.Helper()
	compressed, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}

	zero := make([]byte, 512)
	off, paxStart, paxName := 0, -1, ""
	for off+512 <= len(data) {
		hdr := data[off : off+512]
		if bytes.Equal(hdr, zero) {
			break
		}
		sizeField := strings.TrimRight(string(hdr[124:136]), "\x00 ")
		size64, err := strconv.ParseInt(sizeField, 8, 64)
		if err != nil {
			t.Fatalf("tar size field %q at offset %d: %v", sizeField, off, err)
		}
		size := int(size64)
		padded := (size + 511) &^ 511
		typeflag := hdr[156]
		if typeflag == 'x' || typeflag == 'g' {
			if paxStart < 0 {
				paxStart = off
			}
			paxName = paxPathRecord(data[off+512 : off+512+size])
			off += 512 + padded
			continue
		}
		name := paxName
		if name == "" {
			name = cString(hdr[:100])
		}
		entryStart := off
		if paxStart >= 0 {
			entryStart = paxStart
		}
		if name == ".peipkg/signature" {
			var envelope map[string]any
			if err := json.Unmarshal(data[off+512:off+512+size], &envelope); err != nil {
				t.Fatalf("signature envelope JSON: %v", err)
			}
			return data[:entryStart], envelope
		}
		off += 512 + padded
		paxStart, paxName = -1, ""
	}
	t.Fatalf("%s has no .peipkg/signature entry", artifact)
	return nil, nil
}

// paxPathRecord extracts the "path" record from a pax extended header
// body ("LEN key=value\n" records), or "".
func paxPathRecord(body []byte) string {
	for len(body) > 0 {
		sp := bytes.IndexByte(body, ' ')
		if sp < 0 {
			return ""
		}
		n, err := strconv.Atoi(string(body[:sp]))
		if err != nil || n <= sp || n > len(body) {
			return ""
		}
		record := string(body[sp+1 : n-1]) // strip length prefix and trailing \n
		if key, value, ok := strings.Cut(record, "="); ok && key == "path" {
			return value
		}
		body = body[n:]
	}
	return ""
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// verifySignedArtifact asserts an artifact carries a valid envelope
// signed by pub over exactly the spec-defined byte range.
func verifySignedArtifact(t *testing.T, artifact string, pub ed25519.PublicKey) {
	t.Helper()
	prefix, envelope := signedPrefixAndEnvelope(t, artifact)
	if v, ok := envelope["schema_version"].(float64); !ok || v != 1 {
		t.Errorf("%s: envelope schema_version = %v", artifact, envelope["schema_version"])
	}
	if envelope["algorithm"] != "ed25519" {
		t.Errorf("%s: envelope algorithm = %v", artifact, envelope["algorithm"])
	}
	pubSum := sha256.Sum256(pub)
	if got, want := envelope["key_fingerprint"], hex.EncodeToString(pubSum[:]); got != want {
		t.Errorf("%s: envelope key_fingerprint = %v, want %v", artifact, got, want)
	}
	sig, err := base64.RawStdEncoding.DecodeString(envelope["signature"].(string))
	if err != nil || len(sig) != ed25519.SignatureSize {
		t.Fatalf("%s: envelope signature: len=%d err=%v", artifact, len(sig), err)
	}
	digest := sha256.Sum256(prefix)
	if !ed25519.Verify(pub, digest[:], sig) {
		t.Errorf("%s: signature does not verify over the bytes preceding the signature entry", artifact)
	}
}

func TestPackageSignsPeipkgArtifacts(t *testing.T) {
	dir := t.TempDir()
	recipe := filepath.Join(dir, "app")
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload\n")})
	writeFile(t, filepath.Join(recipe, "pekit.toml"), signTestRecipe)
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), signTestMemberDef)
	keyPath, pub := writeSeedKey(t, recipe)
	chdir(t, recipe)

	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{"package", "--version", "1.0", "--json",
		"--keyring.signing.package_key=" + keyPath})
	if err != nil {
		t.Fatalf("package failed: %v\nstderr=%s", err, stderr.String())
	}

	// Both the member artifact and the corresponding-source package are
	// signed by the configured key.
	for _, pattern := range []string{
		"out/*/package/*/app_1.0-1_x86_64.peipkg",
		"out/*/package/*/app-source_1.0-1_noarch.peipkg",
	} {
		verifySignedArtifact(t, globOne(t, filepath.Join(recipe, pattern)), pub)
	}
	if got := strings.Count(stdout.String(), `"type":"sign"`); got != 2 {
		t.Errorf("expected 2 sign events, got %d:\n%s", got, stdout.String())
	}

	// The private key sits in the recipe directory, but the source
	// package's recipe allowlist must not ship it.
	srcArtifact := globOne(t, filepath.Join(recipe, "out/*/package/*/app-source_1.0-1_noarch.peipkg"))
	for name := range readPeipkgEntries(t, srcArtifact) {
		if strings.Contains(name, "signing.key") {
			t.Fatalf("signing key leaked into the source package: %s", name)
		}
	}
}

func TestPublishUnsignedPeipkgRequiresFlag(t *testing.T) {
	dir := t.TempDir()
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload\n")})
	writeFile(t, filepath.Join(dir, "pekit.toml"), signTestRecipe)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), signTestMemberDef+`
[[publish.localdir]]
path = "repo"
`)
	keyPath, pub := writeSeedKey(t, dir)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}

	// The source lockfile pins on first resolve, so provenance is
	// anchored and the unsigned gate is what refuses — even on dry-run.
	for _, args := range [][]string{
		{"publish", "--version", "1.0"},
		{"publish", "--version", "1.0", "--dry-run"},
	} {
		if err := app.Run(args); diagCode(err) != "unsigned_publish" {
			t.Fatalf("%v: expected unsigned_publish, got %v", args, err)
		}
	}

	// --allow-unsigned publishes without a key.
	if err := app.Run([]string{"publish", "--version", "1.0", "--allow-unsigned"}); err != nil {
		t.Fatalf("publish --allow-unsigned failed: %v\nstderr=%s", err, stderr.String())
	}

	// With a key configured, publish needs no flag and the published
	// artifact verifies.
	if err := os.RemoveAll(filepath.Join(dir, "repo")); err != nil {
		t.Fatal(err)
	}
	err := app.Run([]string{"publish", "--version", "1.0",
		"--keyring.signing.package_key=" + keyPath})
	if err != nil {
		t.Fatalf("signed publish failed: %v\nstderr=%s", err, stderr.String())
	}
	verifySignedArtifact(t, filepath.Join(dir, "repo", "app_1.0-1_x86_64.peipkg"), pub)
}

func TestSigningKeyErrors(t *testing.T) {
	dir := t.TempDir()
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload\n")})
	writeFile(t, filepath.Join(dir, "pekit.toml"), signTestRecipe)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), signTestMemberDef)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}

	// A configured key that does not exist is a hard error, not a
	// silent fall-back to unsigned artifacts.
	err := app.Run([]string{"package", "--version", "1.0",
		"--keyring.signing.package_key=" + filepath.Join(dir, "absent.key")})
	if diagCode(err) != "signing_key" {
		t.Fatalf("missing key file: expected signing_key, got %v", err)
	}

	garbage := filepath.Join(dir, "garbage.key")
	writeFile(t, garbage, "not a key, and not 32 bytes either")
	err = app.Run([]string{"package", "--version", "1.0",
		"--keyring.signing.package_key=" + garbage})
	if diagCode(err) != "signing_key" {
		t.Fatalf("garbage key file: expected signing_key, got %v", err)
	}
}
