package pekit

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
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
	return verifyURLSignature(ctx, recipe, cfg.Signature, renderedURL, artifact, version, "source.url.signature")
}

// verifyURLSignature is shared by the base URL artifact and every artifact in
// a remote patch series. The field name keeps diagnostics pointed at the
// configuration block that supplied the trust policy.
func verifyURLSignature(ctx *Context, recipe RecipeConfig, sigCfg URLSignatureConfig, renderedURL, artifact string, version Version, field string) (string, error) {
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
	verify := func(config *packet.Config) (*packet.Signature, *openpgp.Entity, error) {
		signed, openErr := openSignedData(artifact, sigCfg.Of)
		if openErr != nil {
			return nil, nil, openErr
		}
		defer signed.Close()
		return verifyDetachedSignature(keyring, signed, sigBytes, config)
	}
	sig, signer, err := verify(nil)
	if errors.Is(err, pgperrors.ErrKeyExpired) && sig != nil {
		// Key expiry retires a key for new signatures; it does not invalidate
		// releases signed while that key was valid. First-time publication still
		// requires the pinned fingerprint and the lock records the exact bytes.
		// Present-time revocation and explicit signature expiry are deliberately
		// checked above and are never relaxed by this historical retry.
		now := time.Now().Truncate(time.Second)
		if sig.CreationTime.After(now.Add(5 * time.Minute)) {
			err = fmt.Errorf("signature creation time %s is in the future", sig.CreationTime.UTC().Format(time.RFC3339))
		} else if sig.SigExpired(now) {
			err = pgperrors.ErrSignatureExpired
		} else {
			signedAt := sig.CreationTime
			restore := selectIdentitySelfSignaturesAt(keyring, signedAt)
			sig, signer, err = verify(&packet.Config{Time: func() time.Time { return signedAt }})
			restore()
		}
	}
	if err != nil {
		return "", diag("signature_invalid",
			"signature %s does not verify against the pinned keys: %v — possible tamper or upstream key rotation; do not build until resolved", sigURL, err)
	}
	fpr := hex.EncodeToString(signer.PrimaryKey.Fingerprint)
	if len(sigCfg.Fingerprints) > 0 && !fingerprintAllowed(fpr, sigCfg.Fingerprints) {
		return "", diag("signature_untrusted_key",
			"signature verifies but signer %s is not in %s.fingerprints", fpr, field)
	}
	return fpr, nil
}

// selectIdentitySelfSignaturesAt makes the v4 keyring describe the identity
// state that existed when a historical release was signed. The OpenPGP parser
// retains every certification in Identity.Signatures but ordinarily selects
// only the newest self-signature, which may itself postdate the release.
// The returned closure restores the current selections after the one retry.
func selectIdentitySelfSignaturesAt(keyring openpgp.EntityList, at time.Time) func() {
	type selection struct {
		identity *openpgp.Identity
		current  *packet.Signature
	}
	var changed []selection
	for _, entity := range keyring {
		if entity.PrimaryKey.Version != 4 {
			continue
		}
		for _, identity := range entity.Identities {
			var historical *packet.Signature
			for _, candidate := range identity.Signatures {
				if candidate.SigType == packet.SigTypeCertificationRevocation ||
					!candidate.CheckKeyIdOrFingerprint(entity.PrimaryKey) ||
					candidate.CreationTime.After(at) || candidate.SigExpired(at) ||
					entity.PrimaryKey.KeyExpired(candidate, at) {
					continue
				}
				if historical == nil || candidate.CreationTime.After(historical.CreationTime) {
					historical = candidate
				}
			}
			changed = append(changed, selection{identity: identity, current: identity.SelfSignature})
			identity.SelfSignature = historical
		}
	}
	return func() {
		for _, item := range changed {
			item.identity.SelfSignature = item.current
		}
	}
}

// verifyDetachedSignature exposes the parsed signature packet as well as the
// signer. CheckDetachedSignature intentionally hides it, but the signed
// creation time is needed to distinguish a historical signature from a new
// signature attempted after a key expired.
func verifyDetachedSignature(keyring openpgp.KeyRing, signed io.Reader, sigBytes []byte, config *packet.Config) (*packet.Signature, *openpgp.Entity, error) {
	signature := io.Reader(bytes.NewReader(sigBytes))
	if bytes.Contains(sigBytes, []byte(armoredSigMarker)) {
		block, err := armor.Decode(signature)
		if err != nil {
			return nil, nil, err
		}
		if block.Type != openpgp.SignatureType {
			return nil, nil, fmt.Errorf("invalid armored signature type %q", block.Type)
		}
		signature = block.Body
	}
	return openpgp.VerifyDetachedSignature(keyring, signed, signature, config)
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
