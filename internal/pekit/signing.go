package pekit

import (
	"crypto/ed25519"

	"github.com/peios/peipkg/pack"
)

// signingKeyEntry is the well-known keyring path that configures
// package signing. Its value is the path to an Ed25519 private key —
// a raw 32-byte seed or a PKCS#8 PEM, per PSD-009 §5.2 — resolved
// against the invocation's working directory when relative. Reaching
// it through the keyring mechanism keeps private key material out of
// recipes and out of git by construction: keyring files are
// per-developer and gitignored, and the value can equally come from
// --keyring.signing.package_key=<path> on the command line.
const signingKeyEntry = "signing.package_key"

// resolveSigningKey loads the package-signing key the invocation
// configures, or nil when signing is not configured. A configured but
// unloadable key is a hard error — a signing setup that silently
// produces unsigned artifacts would defeat the point.
func resolveSigningKey(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig) (ed25519.PrivateKey, error) {
	values, err := resolveKeyrings(ctx.Inv, recipe.Root, workspace)
	if err != nil {
		return nil, err
	}
	value := values[envNameFromKeyring(signingKeyEntry)]
	if value == "" {
		return nil, nil
	}
	path, err := absPath(ctx.Inv.Cwd, value)
	if err != nil {
		return nil, wrapDiag("signing_key", value, err)
	}
	key, err := pack.LoadSigningKey(path)
	if err != nil {
		return nil, diagAt("signing_key", path, "load package signing key: %v", err)
	}
	return key, nil
}
