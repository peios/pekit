package pekit

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/peios/peipkg/pack"
	"github.com/peios/peipkg/repopub"
)

const peipkgKeyringPrefix = "keyring:"

type resolvedPeipkgSigningKey struct {
	Key  ed25519.PrivateKey
	Path string
}

// publishPeipkgRepository publishes a batch, rather than invoking the
// repository publisher once per artifact. Both indexes are signed whole-file
// snapshots, so batching is part of keeping publication work proportional to
// releases rather than to their member count.
func publishPeipkgRepository(ctx *Context, op plannedPeipkgPublish, repositoryKey resolvedPeipkgSigningKey,
	packageKey ed25519.PrivateKey, member string) error {

	for _, inst := range op.Instances {
		if ctx.Inv.DryRun {
			ctx.Renderer.Event(Event{
				Type: "publish_plan", Member: member, Package: instanceID(inst), Path: op.Dir,
				Message: "would publish package to peipkg repository",
			})
		}
	}
	if ctx.Inv.DryRun {
		return nil
	}

	paths := make([]string, 0, len(op.Instances))
	for _, inst := range op.Instances {
		paths = append(paths, inst.Artifact)
	}

	return ctx.PublishRegistry.WithPeipkg(op.Dir, func() error {
		at := ctx.App.Now().UTC()
		exists, err := peipkgRepositoryExists(op.Dir)
		if err != nil {
			return wrapDiag("peipkg_repository", op.Dir, err)
		}
		if !exists {
			var trusted []ed25519.PublicKey
			if packageKey != nil {
				trusted = append(trusted, packageKey.Public().(ed25519.PublicKey))
			}
			if err := repopub.Init(op.Dir, repopub.InitOptions{
				Name: op.Name, Key: repositoryKey.Key, GeneratedAt: at, TrustedKeys: trusted,
			}); err != nil {
				return diagAt("peipkg_repository_init", op.Dir,
					"initialize peipkg repository with signing key %s: %v", repositoryKey.Path, err)
			}
			ctx.Renderer.Event(Event{
				Type: "publish", Member: member, Path: op.Dir,
				Message: fmt.Sprintf("initialized peipkg repository %q", op.Name),
			})
		}

		result, err := repopub.Publish(op.Dir, repopub.PublishOptions{
			Key: repositoryKey.Key, Paths: paths, GeneratedAt: at,
			AllowUnsigned: ctx.Inv.AllowUnsigned,
		})
		if err != nil {
			return diagAt("peipkg_repository_publish", op.Dir,
				"publish to peipkg repository with signing key %s: %v", repositoryKey.Path, err)
		}
		for _, inst := range op.Instances {
			ctx.Renderer.Event(Event{
				Type: "publish", Member: member, Package: instanceID(inst), Path: op.Dir,
				Message: fmt.Sprintf("published package to peipkg repository (index_version %d)", result.IndexVersion),
			})
		}
		return nil
	})
}

func resolvePeipkgSigningKeys(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig,
	ops []plannedPeipkgPublish) (map[string]resolvedPeipkgSigningKey, error) {

	keys := make(map[string]resolvedPeipkgSigningKey, len(ops))
	for _, op := range ops {
		key, path, err := resolvePeipkgSigningKey(ctx, recipe, workspace, op.SigningKey)
		if err != nil {
			return nil, err
		}
		keys[op.Dir] = resolvedPeipkgSigningKey{Key: key, Path: path}
	}
	return keys, nil
}

func resolvePeipkgSigningKey(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig,
	ref string) (ed25519.PrivateKey, string, error) {

	base := recipe.Root
	keyPath := ref
	if strings.HasPrefix(ref, peipkgKeyringPrefix) {
		entry := strings.TrimPrefix(ref, peipkgKeyringPrefix)
		if entry == "" {
			return nil, "", diag("repository_signing_key",
				"publish.peipkg signing_key has an empty keyring entry")
		}
		values, err := resolveKeyrings(ctx.Inv, recipe.Root, workspace)
		if err != nil {
			return nil, "", err
		}
		keyPath = values[envNameFromKeyring(entry)]
		if keyPath == "" {
			return nil, "", diag("repository_signing_key",
				"publish.peipkg signing_key references missing keyring entry %q", entry)
		}
		base = ctx.Inv.Cwd
	} else if workspace != nil {
		base = workspace.Root
	}

	path, err := absPath(base, keyPath)
	if err != nil {
		return nil, "", wrapDiag("repository_signing_key", keyPath, err)
	}
	key, err := pack.LoadSigningKey(path)
	if err != nil {
		return nil, "", diagAt("repository_signing_key", path,
			"load peipkg repository signing key: %v", err)
	}
	return key, path, nil
}

func peipkgRepositoryExists(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, ".peipkg-repo.json"))
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}
