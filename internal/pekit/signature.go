package pekit

import (
	"bytes"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// Upstream signature verification closes the trust-on-first-use gap for the
// first fetch of a new version: the upstream maintainer's key is pinned once
// per recipe (committed key files under the recipe dir, Debian
// debian/upstream/signing-key.asc-style), so new releases verify with no
// human in the loop. Verification is in-process OpenPGP — no host gpg, no
// keyring state: a pure function of (artifact, signature, pinned keys). It
// runs before a lock entry is written; a missing or invalid signature is a
// hard failure and nothing is pinned.

const armoredKeyMarker = "-----BEGIN PGP PUBLIC KEY BLOCK-----"
const armoredSigMarker = "-----BEGIN PGP SIGNATURE-----"

// verifySourceSignature fetches (or reuses the cached) detached signature for
// a URL artifact and verifies it against the recipe's pinned keys, returning
// the hex fingerprint of the signing key's primary key.
func verifySourceSignature(ctx *Context, recipe RecipeConfig, cfg URLSourceConfig, renderedURL, artifact string, version Version) (string, error) {
	sigCfg := cfg.Signature
	sigURL, err := renderSignatureURL(sigCfg.URL, renderedURL, version)
	if err != nil {
		return "", err
	}
	sigPath := artifact + ".pekitsig"
	if !fileExists(sigPath) {
		if err := downloadFile(sigURL, sigPath); err != nil {
			return "", diag("signature_missing",
				"fetch signature %s: %v — upstream may have stopped publishing signatures; verify why before removing [source.url.signature]", sigURL, err)
		}
	}
	keyring, err := loadPinnedKeys(recipe.Root, sigCfg.KeyFiles)
	if err != nil {
		return "", err
	}
	sigBytes, err := os.ReadFile(sigPath)
	if err != nil {
		return "", wrapDiag("signature_invalid", sigPath, err)
	}
	signed, err := openSignedData(artifact, sigCfg.Of)
	if err != nil {
		return "", err
	}
	defer signed.Close()
	var signer *openpgp.Entity
	if bytes.Contains(sigBytes, []byte(armoredSigMarker)) {
		signer, err = openpgp.CheckArmoredDetachedSignature(keyring, signed, bytes.NewReader(sigBytes), nil)
	} else {
		signer, err = openpgp.CheckDetachedSignature(keyring, signed, bytes.NewReader(sigBytes), nil)
	}
	if err != nil {
		return "", diag("signature_invalid",
			"signature %s does not verify against the pinned keys: %v — possible tamper or upstream key rotation; do not build until resolved", sigURL, err)
	}
	fpr := hex.EncodeToString(signer.PrimaryKey.Fingerprint)
	if len(sigCfg.Fingerprints) > 0 && !fingerprintAllowed(fpr, sigCfg.Fingerprints) {
		return "", diag("signature_untrusted_key",
			"signature verifies but signer %s is not in source.url.signature.fingerprints", fpr)
	}
	return fpr, nil
}

// renderSignatureURL resolves the signature URL template. {{source_url}} is
// substituted before version templating so the default "{{source_url}}.sig"
// composes with any version variables in an explicit template.
func renderSignatureURL(template, renderedURL string, version Version) (string, error) {
	if template == "" {
		template = "{{source_url}}.sig"
	}
	template = strings.ReplaceAll(template, "{{source_url}}", renderedURL)
	out, err := RenderTemplate(template, TemplateContext{Version: version})
	if err != nil {
		return "", wrapDiag("template", "render source.url.signature.url", err)
	}
	return out, nil
}

// openSignedData returns the byte stream the detached signature covers:
// the artifact itself, or its decompressed content for upstreams that sign
// the uncompressed tar (kernel.org's .tar.sign convention).
func openSignedData(artifact, of string) (io.ReadCloser, error) {
	if of == "decompressed" {
		stream, err := openTarStream(artifact)
		if err != nil {
			return nil, wrapDiag("signature_invalid", "decompress artifact for signature verification", err)
		}
		return stream, nil
	}
	f, err := os.Open(artifact)
	if err != nil {
		return nil, wrapDiag("signature_invalid", artifact, err)
	}
	return f, nil
}

// loadPinnedKeys reads the recipe's committed public key files (armored or
// binary) into one keyring. Paths resolve relative to the recipe root.
func loadPinnedKeys(recipeRoot string, keyFiles []string) (openpgp.EntityList, error) {
	var keyring openpgp.EntityList
	for _, kf := range keyFiles {
		path := kf
		if !filepath.IsAbs(path) {
			path = filepath.Join(recipeRoot, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, wrapDiag("signature_key_file", path, err)
		}
		var entities openpgp.EntityList
		if bytes.Contains(data, []byte(armoredKeyMarker)) {
			entities, err = openpgp.ReadArmoredKeyRing(bytes.NewReader(data))
		} else {
			entities, err = openpgp.ReadKeyRing(bytes.NewReader(data))
		}
		if err != nil {
			return nil, wrapDiag("signature_key_file", path, err)
		}
		keyring = append(keyring, entities...)
	}
	if len(keyring) == 0 {
		return nil, diag("signature_key_file", "no usable keys loaded from source.url.signature.key_files")
	}
	return keyring, nil
}

func fingerprintAllowed(fpr string, allowlist []string) bool {
	norm := normalizeFingerprint(fpr)
	for _, allowed := range allowlist {
		if normalizeFingerprint(allowed) == norm {
			return true
		}
	}
	return false
}

func normalizeFingerprint(fpr string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(fpr, " ", ""), "0x", ""))
}
