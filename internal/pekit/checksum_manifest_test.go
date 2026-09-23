package pekit

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

const checksumRecipe = `
out_dir = "out"

[source.url]
url = "https://example.test/app-{{version}}.tar.gz"
extract = true
root = "app-{{version}}"

[source.url.signature]
of = "checksums"
url = "https://example.test/app-{{version}}.tar.gz.sha512sum"
key_files = ["keys/upstream.key"]
fingerprints = ["%s"]

[build]
command = "cat payload.txt > \"$PEKIT_OUT/value\""
`

func clearSign(t *testing.T, entity *openpgp.Entity, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, entity.PrivateKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha512Hex(data []byte) string {
	sum := sha512.Sum512(data)
	return hex.EncodeToString(sum[:])
}

// checksumFixture serves an artifact and a manifest built by manifest (given
// the artifact bytes), pins signer, and runs `pekit build`. It returns the
// recipe directory and the build's error.
func checksumFixture(t *testing.T, signer *openpgp.Entity, pinned *openpgp.Entity, manifest func(artifact []byte) []byte) (string, error) {
	t.Helper()
	dir := t.TempDir()
	artifact := makeTarGz(t, "app-1.0", "payload")
	responses := map[string][]byte{"https://example.test/app-1.0.tar.gz": artifact}
	if m := manifest(artifact); m != nil {
		responses["https://example.test/app-1.0.tar.gz.sha512sum"] = m
	}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"),
		fmt.Sprintf(checksumRecipe, strings.ToUpper(hex.EncodeToString(pinned.PrimaryKey.Fingerprint))))
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "upstream.key"), armoredPublicKeyBytes(t, signer), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	return dir, app.Run([]string{"build", "--version", "1.0"})
}

// Every accepted line form authenticates the artifact and records the
// manifest's signer as the lock's signature_key (PEI-752).
func TestChecksumManifestVerifiedAndPinned(t *testing.T) {
	forms := map[string]func(artifact []byte) string{
		"dash": func(a []byte) string {
			return fmt.Sprintf("%s %d app-1.0.tar.gz\n", sha512Hex(a), len(a))
		},
		"gnu": func(a []byte) string {
			return fmt.Sprintf("%s  other-1.0.tar.gz\n%s *app-1.0.tar.gz\n", sha512Hex([]byte("x")), sha512Hex(a))
		},
		"gnu-sha256": func(a []byte) string { return fmt.Sprintf("%s  app-1.0.tar.gz\n", sha256Hex(a)) },
		"bsd": func(a []byte) string {
			return fmt.Sprintf("SHA512 (app-1.0.tar.gz) = %s\n", sha512Hex(a))
		},
	}
	for name, form := range forms {
		t.Run(name, func(t *testing.T) {
			signer := newTestSigner(t)
			dir, err := checksumFixture(t, signer, signer, func(a []byte) []byte { return clearSign(t, signer, form(a)) })
			if err != nil {
				t.Fatalf("build failed: %v", err)
			}
			lock, err := LoadLockFile(dir)
			if err != nil {
				t.Fatal(err)
			}
			entry := lock.Find("1.0")
			if want := hex.EncodeToString(signer.PrimaryKey.Fingerprint); entry == nil || entry.SignatureKey != want {
				t.Fatalf("signature_key = %+v, want %s", entry, want)
			}
		})
	}
}

func TestChecksumManifestRejections(t *testing.T) {
	cases := []struct {
		name     string
		code     string
		manifest func(t *testing.T, signer *openpgp.Entity, a []byte) []byte
	}{
		{"wrong filename", "checksum_manifest_invalid", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			return clearSign(t, s, fmt.Sprintf("%s  app-1.0.tar.xz\n", sha512Hex(a)))
		}},
		{"path instead of name", "checksum_manifest_invalid", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			return clearSign(t, s, fmt.Sprintf("%s  ./app-1.0.tar.gz\n", sha512Hex(a)))
		}},
		{"duplicate entry", "checksum_manifest_invalid", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			return clearSign(t, s, fmt.Sprintf("%s  app-1.0.tar.gz\n%s  app-1.0.tar.gz\n", sha512Hex(a), sha512Hex(a)))
		}},
		{"md5 digest", "checksum_manifest_invalid", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			sum := md5.Sum(a)
			return clearSign(t, s, fmt.Sprintf("%s  app-1.0.tar.gz\n", hex.EncodeToString(sum[:])))
		}},
		{"sha1 digest", "checksum_manifest_invalid", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			sum := sha1.Sum(a)
			return clearSign(t, s, fmt.Sprintf("%s  app-1.0.tar.gz\n", hex.EncodeToString(sum[:])))
		}},
		{"unsupported bsd tag", "checksum_manifest_invalid", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			sum := sha1.Sum(a)
			return clearSign(t, s, fmt.Sprintf("SHA1 (app-1.0.tar.gz) = %s\n", hex.EncodeToString(sum[:])))
		}},
		{"changed artifact", "checksum_mismatch", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			return clearSign(t, s, fmt.Sprintf("%s  app-1.0.tar.gz\n", sha512Hex(append([]byte("x"), a...))))
		}},
		{"wrong size", "checksum_mismatch", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			return clearSign(t, s, fmt.Sprintf("%s %d app-1.0.tar.gz\n", sha512Hex(a), len(a)+1))
		}},
		{"malformed line", "checksum_manifest_invalid", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			return clearSign(t, s, fmt.Sprintf("%s  app-1.0.tar.gz\nnot a checksum line at all\n", sha512Hex(a)))
		}},
		{"empty manifest", "checksum_manifest_invalid", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			return clearSign(t, s, "\n")
		}},
		{"not clear-signed", "signature_invalid", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			return []byte(fmt.Sprintf("%s  app-1.0.tar.gz\n", sha512Hex(a)))
		}},
		{"unsigned text around the block", "signature_invalid", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			return append([]byte(fmt.Sprintf("%s  app-1.0.tar.gz\n", sha512Hex(a))),
				clearSign(t, s, fmt.Sprintf("%s  other.tar.gz\n", sha512Hex(a)))...)
		}},
		{"tampered signed text", "signature_invalid", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			signed := clearSign(t, s, fmt.Sprintf("%s  app-1.0.tar.gz\n", sha512Hex([]byte("original"))))
			return bytes.Replace(signed, []byte(sha512Hex([]byte("original"))), []byte(sha512Hex(a)), 1)
		}},
		{"missing manifest", "signature_missing", func(t *testing.T, s *openpgp.Entity, a []byte) []byte {
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signer := newTestSigner(t)
			dir, err := checksumFixture(t, signer, signer, func(a []byte) []byte { return tc.manifest(t, signer, a) })
			if err == nil {
				t.Fatal("build succeeded")
			}
			if diagCode(err) != tc.code {
				t.Fatalf("error = %v, want %s", err, tc.code)
			}
			if fileExists(filepath.Join(dir, lockFileName)) {
				t.Fatal("nothing may be locked after a failed manifest check")
			}
		})
	}
}

// The signer must be one of the pinned keys and, among them, the pinned
// fingerprint: a manifest signed by a key outside key_files fails to verify,
// and one signed by a key that is in key_files but not in fingerprints is
// refused as untrusted.
func TestChecksumManifestUnpinnedSigner(t *testing.T) {
	signer := newTestSigner(t)
	imposter := newTestSigner(t)
	manifest := func(s *openpgp.Entity) func([]byte) []byte {
		return func(a []byte) []byte { return clearSign(t, s, fmt.Sprintf("%s  app-1.0.tar.gz\n", sha512Hex(a))) }
	}
	if _, err := checksumFixture(t, signer, signer, manifest(imposter)); diagCode(err) != "signature_invalid" {
		t.Errorf("manifest signed outside key_files: %v, want signature_invalid", err)
	}
	if _, err := checksumFixture(t, signer, imposter, manifest(signer)); diagCode(err) != "signature_untrusted_key" {
		t.Errorf("signer in key_files but not fingerprints: %v, want signature_untrusted_key", err)
	}
}

func TestChecksumManifestConfigRequiresURLAndFullFingerprints(t *testing.T) {
	base := `
[source.url]
url = "https://example.test/app-{{version}}.tar.gz"

[source.url.signature]
of = "checksums"
key_files = ["keys/upstream.key"]
`
	for name, extra := range map[string]string{
		"no url":              `fingerprints = ["9F9D45FE50AE361530983792C7271D0A49B18BA7"]`,
		"no fingerprints":     `url = "{{source_url}}.sha512sum"`,
		"short fingerprint":   "url = \"{{source_url}}.sha512sum\"\nfingerprints = [\"C7271D0A49B18BA7\"]",
		"garbage fingerprint": "url = \"{{source_url}}.sha512sum\"\nfingerprints = [\"not-a-fingerprint-not-a-fingerprint-1234\"]",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "pekit.toml"), base+extra+"\n")
			if _, err := LoadRecipe(filepath.Join(dir, "pekit.toml")); diagCode(err) != "invalid_signature" {
				t.Fatalf("LoadRecipe = %v, want invalid_signature", err)
			}
		})
	}
}

