package pekit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// The source lockfile pins fetched inputs trust-on-first-use: pekit writes an
// entry the first time it resolves a (source, version) pair and verifies
// against it on every later fetch, so unattended version sweeps stay
// unattended while re-fetches of known versions become tamper-evident. The
// file is machine-written, lives beside pekit.toml, and is meant to be
// committed. Local sources and dry runs are never locked; git sources are
// locked only when a version is selected (a bare branch ref is a deliberately
// moving target, like --local).

const lockFileName = "pekit.lock"
const lockSchema = 1

const lockFileHeader = `# pekit.lock — machine-written by pekit; records pinned source inputs.
# Do not edit by hand. To accept changed upstream bytes for a version, run:
#   pekit lock --repin --version <version>
`

type LockFile struct {
	Schema  int          `toml:"schema"`
	Sources []LockSource `toml:"source,omitempty"`
}

type LockSource struct {
	Version string `toml:"version"`
	// URL-source assertion: the artifact's sha256. URL is provenance only —
	// the hash, not the address, is what the entry asserts.
	URL    string `toml:"url,omitempty"`
	SHA256 string `toml:"sha256,omitempty"`
	// Git-source assertion: the commit the rendered ref resolved to.
	Ref    string `toml:"ref,omitempty"`
	Commit string `toml:"commit,omitempty"`
	// SignatureKey is the hex fingerprint of the pinned upstream key that
	// verified this entry at lock time; empty when no [source.url.signature]
	// block is configured.
	SignatureKey string `toml:"signature_key,omitempty"`
	LockedAt     string `toml:"locked_at,omitempty"`
}

func (e LockSource) kind() string {
	if e.Commit != "" {
		return "git"
	}
	return "url"
}

func lockFilePath(recipeRoot string) string {
	return filepath.Join(recipeRoot, lockFileName)
}

// LoadLockFile reads a recipe's lockfile; a missing file is an empty lock.
func LoadLockFile(recipeRoot string) (LockFile, error) {
	path := lockFilePath(recipeRoot)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return LockFile{Schema: lockSchema}, nil
	}
	if err != nil {
		return LockFile{}, wrapDiag("read_file", path, err)
	}
	var lock LockFile
	md, err := toml.Decode(string(data), &lock)
	if err != nil {
		return LockFile{}, wrapDiag("parse_toml", path, err)
	}
	if len(md.Undecoded()) > 0 {
		return LockFile{}, diagAt("unknown_key", path, "unknown lockfile key %q", md.Undecoded()[0].String())
	}
	if lock.Schema != lockSchema {
		return LockFile{}, diagAt("lock_schema", path, "unsupported lockfile schema %d (this pekit supports %d)", lock.Schema, lockSchema)
	}
	return lock, nil
}

func (l *LockFile) Find(version string) *LockSource {
	for i := range l.Sources {
		if l.Sources[i].Version == version {
			return &l.Sources[i]
		}
	}
	return nil
}

// Put inserts or replaces the entry for its version.
func (l *LockFile) Put(entry LockSource) {
	if existing := l.Find(entry.Version); existing != nil {
		*existing = entry
		return
	}
	l.Sources = append(l.Sources, entry)
}

// SaveLockFile writes the lock atomically with entries in stable version
// order, so an unattended update produces a one-entry diff.
func SaveLockFile(recipeRoot string, lock LockFile) error {
	lock.Schema = lockSchema
	sort.SliceStable(lock.Sources, func(i, j int) bool {
		return compareVersionText(lock.Sources[i].Version, lock.Sources[j].Version) < 0
	})
	var buf bytes.Buffer
	buf.WriteString(lockFileHeader)
	buf.WriteString("\n")
	if err := toml.NewEncoder(&buf).Encode(lock); err != nil {
		return wrapDiag("lock_write", lockFilePath(recipeRoot), err)
	}
	path := lockFilePath(recipeRoot)
	tmp, err := os.CreateTemp(recipeRoot, "."+lockFileName+".tmp-*")
	if err != nil {
		return wrapDiag("lock_write", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	_, writeErr := tmp.Write(buf.Bytes())
	closeErr := tmp.Close()
	if writeErr != nil {
		return wrapDiag("lock_write", path, writeErr)
	}
	if closeErr != nil {
		return wrapDiag("lock_write", path, closeErr)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return wrapDiag("lock_write", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return wrapDiag("lock_write", path, err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type urlLockState struct {
	Hash   string
	Locked bool
}

// applyURLLock enforces the lockfile against a fetched URL artifact: a locked
// version must hash to its pinned sha256; an unlocked version is verified
// (signature first, when configured) and pinned. Returns the artifact hash so
// provenance can carry the anchor.
func applyURLLock(ctx *Context, recipe RecipeConfig, cfg URLSourceConfig, renderedURL, artifact string, version Version) (urlLockState, error) {
	lock, err := LoadLockFile(recipe.Root)
	if err != nil {
		return urlLockState{}, err
	}
	hash, err := fileSHA256(artifact)
	if err != nil {
		return urlLockState{}, wrapDiag("lock_hash", artifact, err)
	}
	entry := lock.Find(version.Raw)
	repin := ctx.Inv.Repin
	if entry != nil && !repin {
		if entry.kind() != "url" {
			return urlLockState{}, diag("lock_kind_mismatch",
				"version %q is locked as a %s source but the recipe now fetches a URL; run `pekit lock --repin --version %s` to accept the change",
				version.Raw, entry.kind(), version.Raw)
		}
		if entry.SHA256 != hash {
			return urlLockState{}, diag("lock_mismatch",
				"artifact for version %q hashes to sha256:%s but is locked to sha256:%s — upstream's published bytes changed; if that change is legitimate, run `pekit lock --repin --version %s`",
				version.Raw, hash, entry.SHA256, version.Raw)
		}
		// Upgrade path: a signature block added after the version was locked
		// verifies on the next resolve and is recorded.
		if cfg.Signature.Configured() && entry.SignatureKey == "" {
			fpr, err := verifySourceSignature(ctx, recipe, cfg, renderedURL, artifact, version)
			if err != nil {
				return urlLockState{}, err
			}
			entry.SignatureKey = fpr
			entry.LockedAt = lockTimestamp(ctx)
			if err := SaveLockFile(recipe.Root, lock); err != nil {
				return urlLockState{}, err
			}
			ctx.Renderer.Event(Event{Type: "lock", Version: version.Raw, Message: "recorded signature by " + shortFingerprint(fpr)})
		}
		return urlLockState{Hash: hash, Locked: true}, nil
	}
	signatureKey := ""
	if cfg.Signature.Configured() {
		fpr, err := verifySourceSignature(ctx, recipe, cfg, renderedURL, artifact, version)
		if err != nil {
			return urlLockState{}, err
		}
		signatureKey = fpr
	}
	newEntry := LockSource{
		Version:      version.Raw,
		URL:          renderedURL,
		SHA256:       hash,
		SignatureKey: signatureKey,
		LockedAt:     lockTimestamp(ctx),
	}
	message := "pinned sha256:" + hash
	if repin && entry != nil {
		if entry.SHA256 == hash && entry.kind() == "url" {
			message = "repinned; sha256 unchanged (" + hash + ")"
		} else if entry.kind() == "url" {
			message = "REPINNED: sha256 " + entry.SHA256 + " -> " + hash
		} else {
			message = "REPINNED: git commit " + entry.Commit + " -> url sha256:" + hash
		}
	}
	if signatureKey != "" {
		message += ", signed by " + shortFingerprint(signatureKey)
	}
	lock.Put(newEntry)
	if err := SaveLockFile(recipe.Root, lock); err != nil {
		return urlLockState{}, err
	}
	ctx.Renderer.Event(Event{Type: "lock", Version: version.Raw, Path: lockFilePath(recipe.Root), Message: message})
	return urlLockState{Hash: hash, Locked: true}, nil
}

// applyGitLock enforces the lockfile against a resolved git commit. A moved
// tag is a hard stop even though the locked commit is still fetchable —
// surfacing that upstream's published state changed is the point.
func applyGitLock(ctx *Context, recipe RecipeConfig, renderedRef, commit string, version Version) error {
	lock, err := LoadLockFile(recipe.Root)
	if err != nil {
		return err
	}
	entry := lock.Find(version.Raw)
	repin := ctx.Inv.Repin
	if entry != nil && !repin {
		if entry.kind() != "git" {
			return diag("lock_kind_mismatch",
				"version %q is locked as a %s source but the recipe now fetches git; run `pekit lock --repin --version %s` to accept the change",
				version.Raw, entry.kind(), version.Raw)
		}
		if entry.Commit != commit {
			return diag("lock_mismatch",
				"git ref %q resolves to %s but version %q is locked to %s — the upstream tag moved; if that change is legitimate, run `pekit lock --repin --version %s`",
				renderedRef, commit, version.Raw, entry.Commit, version.Raw)
		}
		return nil
	}
	message := "pinned commit " + commit
	if repin && entry != nil {
		if entry.Commit == commit && entry.kind() == "git" {
			message = "repinned; commit unchanged (" + commit + ")"
		} else if entry.kind() == "git" {
			message = "REPINNED: commit " + entry.Commit + " -> " + commit
		} else {
			message = "REPINNED: url sha256:" + entry.SHA256 + " -> git commit " + commit
		}
	}
	lock.Put(LockSource{
		Version:  version.Raw,
		Ref:      renderedRef,
		Commit:   commit,
		LockedAt: lockTimestamp(ctx),
	})
	if err := SaveLockFile(recipe.Root, lock); err != nil {
		return err
	}
	ctx.Renderer.Event(Event{Type: "lock", Version: version.Raw, Path: lockFilePath(recipe.Root), Message: message})
	return nil
}

// lockedMirroredCommit reports the commit a failed source refresh may
// fall back to: the selected version's lock entry, provided it is a git
// entry for the same rendered ref and its commit object is already
// present in the mirror clone. Anything less returns "" and the caller
// treats the refresh failure as fatal. Lockfile read errors are
// swallowed here — the caller's applyGitLock surfaces them.
func lockedMirroredCommit(recipe RecipeConfig, ref string, version Version, rawRepo string) string {
	if version.Raw == "" {
		return ""
	}
	lock, err := LoadLockFile(recipe.Root)
	if err != nil {
		return ""
	}
	entry := lock.Find(version.Raw)
	if entry == nil || entry.kind() != "git" || entry.Ref != ref || entry.Commit == "" {
		return ""
	}
	if err := runSimple(rawRepo, "git", "cat-file", "-e", entry.Commit+"^{commit}"); err != nil {
		return ""
	}
	return entry.Commit
}

func firstErrorLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

func lockTimestamp(ctx *Context) string {
	return ctx.Start.UTC().Format(time.RFC3339)
}

func shortFingerprint(fpr string) string {
	if len(fpr) > 16 {
		return fpr[:16]
	}
	return fpr
}

// runLockCmd implements the lock command: with no version selection it
// reports the current lock; with one it fetches and pins the selected
// versions. --repin demands exactly one exact version — accepting changed
// upstream bytes is a deliberate, single-version ceremony.
func runLockCmd(ctx *Context, recipe RecipeConfig, member string) error {
	statusOnly := ctx.Inv.Version == "" && !ctx.Inv.Latest && !ctx.Inv.AllVersions
	if ctx.Inv.Repin {
		if statusOnly {
			return diag("invalid_flags", "--repin requires an exact --version")
		}
		if ctx.Inv.Latest || ctx.Inv.AllVersions || looksLikeConstraint(ctx.Inv.Version) || len(ctx.Inv.Version) == 0 {
			return diag("invalid_flags", "--repin requires exactly one exact --version")
		}
	}
	if statusOnly {
		lock, err := LoadLockFile(recipe.Root)
		if err != nil {
			return err
		}
		if len(lock.Sources) == 0 {
			ctx.Renderer.Event(Event{Type: "lock_status", Member: member, Path: lockFilePath(recipe.Root), Message: "no locked sources"})
			return nil
		}
		for _, entry := range lock.Sources {
			msg := ""
			if entry.kind() == "git" {
				msg = "git " + entry.Ref + " commit " + entry.Commit
			} else {
				msg = "url sha256:" + entry.SHA256
				if entry.SignatureKey != "" {
					msg += " signed by " + shortFingerprint(entry.SignatureKey)
				}
			}
			ctx.Renderer.Event(Event{Type: "lock_status", Member: member, Version: entry.Version, Message: msg})
		}
		return nil
	}
	if !recipe.Source.HasReproducible() {
		return diag("lock_unsupported", "recipe has no lockable source ([source.git] or [source.url])")
	}
	versions, err := ResolveVersions(ctx.Inv, recipe.Source)
	if err != nil {
		return err
	}
	if ctx.Inv.Repin && len(versions) != 1 {
		return diag("invalid_flags", "--repin requires exactly one exact --version")
	}
	for _, version := range versions {
		if _, err := ResolveSource(ctx, recipe, version); err != nil {
			return err
		}
	}
	return nil
}
