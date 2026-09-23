package pekit

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// signedGitUpstream is a one-commit repository whose release tags and
// commits the tests sign by writing the raw objects themselves, so no host
// gpg or signing configuration is involved.
type signedGitUpstream struct {
	t      *testing.T
	repo   string
	commit string
}

func newSignedGitUpstream(t *testing.T, dir string) *signedGitUpstream {
	t.Helper()
	repo := filepath.Join(dir, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")
	commit := strings.TrimSpace(runTestCmdOutput(t, repo, "git", "rev-parse", "HEAD"))
	return &signedGitUpstream{t: t, repo: repo, commit: commit}
}

func armoredDetachSign(t *testing.T, entity *openpgp.Entity, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, entity, bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (u *signedGitUpstream) writeObject(kind string, raw []byte) string {
	u.t.Helper()
	path := filepath.Join(u.t.TempDir(), "object")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		u.t.Fatal(err)
	}
	return strings.TrimSpace(runTestCmdOutput(u.t, u.repo, "git", "hash-object", "-t", kind, "-w", path))
}

// signedTag publishes refs/tags/<ref> as an annotated tag whose object names
// itself name and is signed by signer (unsigned when signer is nil).
func (u *signedGitUpstream) signedTag(ref, name string, signer *openpgp.Entity) {
	u.t.Helper()
	payload := []byte("object " + u.commit + "\ntype commit\ntag " + name +
		"\ntagger Upstream <upstream@example.test> 1700000000 +0000\n\nrelease " + name + "\n")
	raw := payload
	if signer != nil {
		raw = append(append([]byte{}, payload...), armoredDetachSign(u.t, signer, payload)...)
	}
	object := u.writeObject("tag", raw)
	runTestCmd(u.t, u.repo, "git", "update-ref", "refs/tags/"+ref, object)
}

// signedCommit replaces the release commit with a copy carrying a gpgsig
// header from signer, points refs/tags/<ref> at it lightweight, and returns
// its id.
func (u *signedGitUpstream) signedCommit(ref string, signer *openpgp.Entity) string {
	u.t.Helper()
	tree := strings.TrimSpace(runTestCmdOutput(u.t, u.repo, "git", "rev-parse", u.commit+"^{tree}"))
	headers := "tree " + tree + "\nauthor Upstream <upstream@example.test> 1700000000 +0000\ncommitter Upstream <upstream@example.test> 1700000000 +0000\n"
	body := "\nrelease\n"
	sig := strings.TrimSuffix(string(armoredDetachSign(u.t, signer, []byte(headers+body))), "\n")
	gpgsig := "gpgsig " + strings.ReplaceAll(sig, "\n", "\n ") + "\n"
	commit := u.writeObject("commit", []byte(headers+gpgsig+body))
	runTestCmd(u.t, u.repo, "git", "update-ref", "refs/tags/"+ref, commit)
	return commit
}

func writeSignedGitRecipe(t *testing.T, dir, repo, object string, keyFiles ...*openpgp.Entity) string {
	t.Helper()
	recipe := filepath.Join(dir, "recipe")
	signature := ""
	if len(keyFiles) > 0 {
		var bundle []byte
		for _, entity := range keyFiles {
			bundle = append(bundle, armoredPublicKeyBytes(t, entity)...)
		}
		writeFile(t, filepath.Join(recipe, "keys", "upstream.asc"), string(bundle))
		signature = "\n[source.git.signature]\nkey_files = [\"keys/upstream.asc\"]\n"
		if object != "" {
			signature += "object = \"" + object + "\"\n"
		}
	}
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"
`+signature+`
[build]
command = "true"
`)
	return recipe
}

func buildGitVersion(t *testing.T, recipe, version string) error {
	t.Helper()
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	return (&App{Stdout: &stdout, Stderr: &stderr}).Run([]string{"build", "--version", version})
}

func fingerprintOf(entity *openpgp.Entity) string {
	return hex.EncodeToString(entity.PrimaryKey.Fingerprint)
}

func TestGitSignedTagVerifiedAndPinned(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	upstream := newSignedGitUpstream(t, dir)
	upstream.signedTag("v1.0.0", "v1.0.0", signer)
	recipe := writeSignedGitRecipe(t, dir, upstream.repo, "", newTestSigner(t), signer)

	if err := buildGitVersion(t, recipe, "1.0.0"); err != nil {
		t.Fatalf("signed build failed: %v", err)
	}
	lock, err := LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.Find("1.0.0")
	if entry == nil || entry.Commit != upstream.commit || entry.SignatureKey != fingerprintOf(signer) {
		t.Fatalf("lock entry = %+v; want commit %s signed by %s", entry, upstream.commit, fingerprintOf(signer))
	}
	// Later resolves re-verify against the recorded signer.
	if err := buildGitVersion(t, recipe, "1.0.0"); err != nil {
		t.Fatalf("locked signed rebuild failed: %v", err)
	}
}

func TestGitSignatureFailuresStopBeforeLocking(t *testing.T) {
	cases := []struct {
		name  string
		setup func(u *signedGitUpstream, trusted *openpgp.Entity)
		code  string
	}{
		{"lightweight tag", func(u *signedGitUpstream, _ *openpgp.Entity) {
			runTestCmd(u.t, u.repo, "git", "tag", "v1.0.0")
		}, "signature_missing"},
		{"annotated but unsigned", func(u *signedGitUpstream, _ *openpgp.Entity) {
			u.signedTag("v1.0.0", "v1.0.0", nil)
		}, "signature_missing"},
		{"unpinned signer", func(u *signedGitUpstream, _ *openpgp.Entity) {
			u.signedTag("v1.0.0", "v1.0.0", newTestSigner(u.t))
		}, "signature_invalid"},
		{"signed tag of another release", func(u *signedGitUpstream, trusted *openpgp.Entity) {
			u.signedTag("v1.0.0", "v0.9.0", trusted)
		}, "signature_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			trusted := newTestSigner(t)
			upstream := newSignedGitUpstream(t, dir)
			tc.setup(upstream, trusted)
			recipe := writeSignedGitRecipe(t, dir, upstream.repo, "", trusted)
			err := buildGitVersion(t, recipe, "1.0.0")
			if diagCode(err) != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
			if fileExists(lockFilePath(recipe)) {
				t.Fatal("a failed verification wrote the lock")
			}
			if dirs, _ := filepath.Glob(filepath.Join(recipe, "out", "*", "git-*", "source")); len(dirs) != 0 {
				t.Fatalf("a failed verification checked out the source: %v", dirs)
			}
		})
	}
}

func TestGitSignatureFingerprintAllowlist(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	other := newTestSigner(t)
	upstream := newSignedGitUpstream(t, dir)
	upstream.signedTag("v1.0.0", "v1.0.0", signer)
	recipe := writeSignedGitRecipe(t, dir, upstream.repo, "", signer)
	path := filepath.Join(recipe, "pekit.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pinned := strings.Replace(string(data), "key_files = [\"keys/upstream.asc\"]\n",
		"key_files = [\"keys/upstream.asc\"]\nfingerprints = [\""+fingerprintOf(other)+"\"]\n", 1)
	writeFile(t, path, pinned)
	if err := buildGitVersion(t, recipe, "1.0.0"); diagCode(err) != "signature_untrusted_key" {
		t.Fatalf("want signature_untrusted_key, got %v", err)
	}
}

func TestGitSignedCommitVerified(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	upstream := newSignedGitUpstream(t, dir)
	commit := upstream.signedCommit("v1.0.0", signer)
	recipe := writeSignedGitRecipe(t, dir, upstream.repo, "commit", signer)
	if err := buildGitVersion(t, recipe, "1.0.0"); err != nil {
		t.Fatalf("signed-commit build failed: %v", err)
	}
	lock, err := LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if entry := lock.Find("1.0.0"); entry == nil || entry.Commit != commit || entry.SignatureKey != fingerprintOf(signer) {
		t.Fatalf("lock entry = %+v; want commit %s signed by %s", entry, commit, fingerprintOf(signer))
	}

	// The original commit carries no signature.
	runTestCmd(t, upstream.repo, "git", "update-ref", "refs/tags/v2.0.0", upstream.commit)
	if err := buildGitVersion(t, recipe, "2.0.0"); diagCode(err) != "signature_missing" {
		t.Fatalf("want signature_missing for an unsigned commit, got %v", err)
	}
}

func TestGitSignatureRecordedForVersionLockedBeforeTheBlock(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	upstream := newSignedGitUpstream(t, dir)
	upstream.signedTag("v1.0.0", "v1.0.0", signer)
	recipe := writeSignedGitRecipe(t, dir, upstream.repo, "")
	if err := buildGitVersion(t, recipe, "1.0.0"); err != nil {
		t.Fatalf("unsigned-policy build failed: %v", err)
	}
	if lock, _ := LoadLockFile(recipe); lock.Find("1.0.0").SignatureKey != "" {
		t.Fatal("a recipe without a signature block recorded a signer")
	}
	writeSignedGitRecipe(t, dir, upstream.repo, "", signer)
	if err := buildGitVersion(t, recipe, "1.0.0"); err != nil {
		t.Fatalf("build after adding the signature block failed: %v", err)
	}
	if lock, _ := LoadLockFile(recipe); lock.Find("1.0.0").SignatureKey != fingerprintOf(signer) {
		t.Fatalf("signer not recorded on upgrade: %+v", lock.Find("1.0.0"))
	}
}

func TestGitSignatureRejectsSSHSignedTag(t *testing.T) {
	dir := t.TempDir()
	upstream := newSignedGitUpstream(t, dir)
	payload := "object " + upstream.commit + "\ntype commit\ntag v1.0.0\ntagger U <u@example.test> 1700000000 +0000\n\nrelease\n"
	object := upstream.writeObject("tag", []byte(payload+"-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"))
	runTestCmd(t, upstream.repo, "git", "update-ref", "refs/tags/v1.0.0", object)
	recipe := writeSignedGitRecipe(t, dir, upstream.repo, "", newTestSigner(t))
	if err := buildGitVersion(t, recipe, "1.0.0"); diagCode(err) != "signature_unsupported" {
		t.Fatalf("want signature_unsupported, got %v", err)
	}
}

func TestGitSignatureConfigValidation(t *testing.T) {
	for name, block := range map[string]string{
		"missing key_files": "[source.git.signature]\nobject = \"tag\"\n",
		"bad object":        "[source.git.signature]\nkey_files = [\"k.asc\"]\nobject = \"branch\"\n",
		"unknown key":       "[source.git.signature]\nkey_files = [\"k.asc\"]\nurl = \"x\"\n",
		"tracked snapshot":  "[source.git.signature]\nkey_files = [\"k.asc\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			source := "[source.git]\nurl = \"https://example.test/r.git\"\nref = \"v{{version}}\"\n"
			if name == "tracked snapshot" {
				source = "[source.git]\nurl = \"https://example.test/r.git\"\nref = \"refs/heads/main\"\ntracked_path = \"data.txt\"\n"
			}
			writeFile(t, filepath.Join(dir, "pekit.toml"), "out_dir = \"out\"\n\n"+source+"\n"+block+"\n[build]\ncommand = \"true\"\n")
			if _, err := LoadRecipe(filepath.Join(dir, "pekit.toml")); err == nil {
				t.Fatal("expected a configuration error")
			}
		})
	}
}

func TestLintRequiresGitSignatureOrAllowance(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	upstream := newSignedGitUpstream(t, dir)
	upstream.signedTag("v1.0.0", "v1.0.0", signer)
	recipe := writeSignedGitRecipe(t, dir, upstream.repo, "")
	writeFile(t, filepath.Join(recipe, "lint.pekit.toml"), "[source]\nsignature.required = true\nsignature.fingerprint = \"full\"\n")
	events, _ := lintEvents(t, recipe, "lint")
	if lintRules(events["lint"])["source.signature.required"] != 1 {
		t.Fatalf("an unsigned git source was not reported: %v", lintRules(events["lint"]))
	}
	writeSignedGitRecipe(t, dir, upstream.repo, "", signer)
	events, _ = lintEvents(t, recipe, "lint")
	if got := lintRules(events["lint"]); got["source.signature.required"] != 0 || got["source.signature.fingerprint"] != 1 {
		t.Fatalf("signed git source: want only the missing-fingerprint finding, got %v", got)
	}
}
