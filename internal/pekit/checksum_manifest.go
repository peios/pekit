package pekit

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

// Some upstreams sign a checksum manifest instead of the release artifact:
// a clear-signed text file listing each artifact's digest (Dash's
// dash-X.tar.gz.sha512sum is the worked example). `of = "checksums"` verifies
// such a manifest against the recipe's pinned keys and then requires exactly
// one entry naming the fetched artifact, whose digest (and size, when the
// manifest states one) must match the bytes pekit downloaded. The manifest's
// signer is recorded in the lock exactly as a detached signature's is; the
// artifact digest is the lock's sha256 as for every URL source.

// checksumManifestEntry is one line of a checksum manifest.
type checksumManifestEntry struct {
	Name      string
	Algorithm string // "sha256" or "sha512"
	Digest    string // lowercase hex
	Size      int64  // -1 when the manifest states none
}

// bsdChecksumLine matches the BSD/`--tag` form: `SHA512 (name) = digest`.
var bsdChecksumLine = regexp.MustCompile(`^(SHA256|SHA512|[A-Za-z0-9-]+) \((.+)\) = ([0-9A-Fa-f]+)$`)

// verifyChecksumManifest authenticates a clear-signed checksum manifest and
// checks the artifact against its entry for artifactURL's file name,
// returning the manifest signer's primary-key fingerprint.
func verifyChecksumManifest(keyRoot string, sigCfg URLSignatureConfig, manifest []byte, manifestURL, artifactURL, artifact, field string) (string, error) {
	what := "checksum manifest " + manifestURL
	block, rest := clearsign.Decode(manifest)
	if block == nil {
		return "", diag("signature_invalid",
			"%s is not a clear-signed OpenPGP message; %s.of = \"checksums\" accepts only clear-signed manifests", what, field)
	}
	// Only the signed text is ever read, but a manifest that carries anything
	// outside its signed block is not the file upstream signed: refuse it
	// rather than silently ignore the extra bytes.
	if start := bytes.Index(manifest, []byte("-----BEGIN PGP SIGNED MESSAGE-----")); len(bytes.TrimSpace(manifest[:start])) > 0 || len(bytes.TrimSpace(rest)) > 0 {
		return "", diag("signature_invalid", "%s carries content outside its signed block", what)
	}
	sigBytes, err := io.ReadAll(block.ArmoredSignature.Body)
	if err != nil {
		return "", diag("signature_invalid", "%s: read signature: %v", what, err)
	}
	open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(block.Bytes)), nil }
	fpr, err := verifyPinnedSignature(keyRoot, sigCfg, open, sigBytes, what, field)
	if err != nil {
		return "", err
	}

	name, err := artifactFileName(artifactURL)
	if err != nil {
		return "", err
	}
	entries, err := parseChecksumManifest(block.Plaintext)
	if err != nil {
		return "", diag("checksum_manifest_invalid", "%s: %v", what, err)
	}
	var matches []checksumManifestEntry
	for _, entry := range entries {
		if entry.Name == name {
			matches = append(matches, entry)
		}
	}
	switch len(matches) {
	case 0:
		return "", diag("checksum_manifest_invalid", "%s has no entry for %q", what, name)
	case 1:
	default:
		return "", diag("checksum_manifest_invalid", "%s lists %q %d times; exactly one entry is required", what, name, len(matches))
	}
	entry := matches[0]
	digest, size, err := digestFile(artifact, entry.Algorithm)
	if err != nil {
		return "", wrapDiag("checksum_manifest_invalid", artifact, err)
	}
	if digest != entry.Digest {
		return "", diag("checksum_mismatch",
			"%s is %s:%s but the signed %s lists %s:%s — possible tamper or a re-rolled release; do not build until resolved",
			name, entry.Algorithm, digest, what, entry.Algorithm, entry.Digest)
	}
	if entry.Size >= 0 && size != entry.Size {
		return "", diag("checksum_mismatch", "%s is %d bytes but the signed %s lists %d", name, size, what, entry.Size)
	}
	return fpr, nil
}

// artifactFileName is the name a manifest entry must carry: the last path
// segment of the artifact URL, exactly. Entries naming a directory, a
// relative path or a differently encoded name never match.
func artifactFileName(artifactURL string) (string, error) {
	parsed, err := url.Parse(artifactURL)
	if err != nil {
		return "", wrapDiag("checksum_manifest_invalid", artifactURL, err)
	}
	name := path.Base(parsed.Path)
	if name == "" || name == "." || name == "/" {
		return "", diag("checksum_manifest_invalid", "artifact URL %s names no file", artifactURL)
	}
	return name, nil
}

// parseChecksumManifest reads the signed text. Three line forms are
// accepted, each naming one file:
//
//	<digest>  <name>          GNU coreutils (`*<name>` marks binary mode)
//	<digest> <size> <name>    digest, byte count, name (Dash's form)
//	SHA512 (<name>) = <digest> BSD / `--tag` form
//
// The algorithm is fixed by the BSD tag or, in the other forms, by the
// digest's length: SHA-256 and SHA-512 only. Anything else, including MD5
// and SHA-1 digests, makes the whole manifest unusable rather than being
// skipped, so a manifest cannot be padded with lines pekit ignores.
func parseChecksumManifest(text []byte) ([]checksumManifestEntry, error) {
	var entries []checksumManifestEntry
	for i, raw := range strings.Split(string(text), "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		entry, err := parseChecksumLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", i+1, err)
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no checksum entries")
	}
	return entries, nil
}

func parseChecksumLine(line string) (checksumManifestEntry, error) {
	if m := bsdChecksumLine.FindStringSubmatch(line); m != nil {
		algorithm := strings.ToLower(m[1])
		if algorithm != "sha256" && algorithm != "sha512" {
			return checksumManifestEntry{}, fmt.Errorf("unsupported digest algorithm %s (SHA256 and SHA512 only)", m[1])
		}
		digest := strings.ToLower(m[3])
		if digestAlgorithm(digest) != algorithm {
			return checksumManifestEntry{}, fmt.Errorf("%s digest has the wrong length", m[1])
		}
		return checksumManifestEntry{Name: m[2], Algorithm: algorithm, Digest: digest, Size: -1}, nil
	}
	fields := strings.Fields(line)
	entry := checksumManifestEntry{Size: -1}
	switch len(fields) {
	case 2:
		entry.Name = strings.TrimPrefix(fields[1], "*")
	case 3:
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || size < 0 {
			return checksumManifestEntry{}, fmt.Errorf("malformed size %q", fields[1])
		}
		entry.Size = size
		entry.Name = fields[2]
	default:
		return checksumManifestEntry{}, fmt.Errorf("malformed checksum line %q", line)
	}
	entry.Digest = strings.ToLower(fields[0])
	if _, err := hex.DecodeString(entry.Digest); err != nil {
		return checksumManifestEntry{}, fmt.Errorf("malformed digest %q", fields[0])
	}
	entry.Algorithm = digestAlgorithm(entry.Digest)
	if entry.Algorithm == "" {
		return checksumManifestEntry{}, fmt.Errorf("unsupported %d-bit digest for %s (SHA-256 and SHA-512 only)", len(entry.Digest)*4, entry.Name)
	}
	if entry.Name == "" {
		return checksumManifestEntry{}, fmt.Errorf("malformed checksum line %q", line)
	}
	return entry, nil
}

func digestAlgorithm(hexDigest string) string {
	switch len(hexDigest) {
	case sha256.Size * 2:
		return "sha256"
	case sha512.Size * 2:
		return "sha512"
	}
	return ""
}

func digestFile(file, algorithm string) (string, int64, error) {
	var h hash.Hash
	switch algorithm {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return "", 0, fmt.Errorf("unsupported digest algorithm %s", algorithm)
	}
	f, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
