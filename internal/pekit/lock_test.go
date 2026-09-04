package pekit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func makeTarGz(t *testing.T, root, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	data := []byte(payload)
	if err := tw.WriteHeader(&tar.Header{Name: root + "/payload.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serveURLs swaps http.DefaultClient for one serving from the given map; the
// map may be mutated between runs to simulate upstream changes.
func serveURLs(t *testing.T, responses map[string][]byte) {
	t.Helper()
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, ok := responses[req.URL.String()]
		if !ok {
			return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
}

const lockTestRecipe = `
out_dir = "out"

[source.url]
url = "https://example.test/app-{{version}}.tar.gz"
extract = true
root = "app-{{version}}"

[build]
command = "cat payload.txt > \"$PEKIT_OUT/value\""
`

func TestURLSourceLocksOnFirstFetchAndDetectsChange(t *testing.T) {
	dir := t.TempDir()
	rawURL := "https://example.test/app-1.0.tar.gz"
	responses := map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload")}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"), lockTestRecipe)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("first build failed: %v\nstderr=%s", err, stderr.String())
	}
	lock, err := LoadLockFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.Find("1.0")
	if entry == nil {
		t.Fatal("expected lock entry for version 1.0")
	}
	wantHash := sha256Hex(responses[rawURL])
	if entry.SHA256 != wantHash {
		t.Fatalf("locked sha256 = %s, want %s", entry.SHA256, wantHash)
	}
	if entry.URL != rawURL {
		t.Fatalf("locked url = %s, want %s", entry.URL, rawURL)
	}

	// Upstream changes its published bytes. The cache still matches the lock,
	// so a plain rebuild passes; a re-download must hard-stop.
	responses[rawURL] = makeTarGz(t, "app-1.0", "tampered")
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("cached rebuild failed: %v\nstderr=%s", err, stderr.String())
	}
	err = app.Run([]string{"build", "--version", "1.0", "--refresh-source"})
	if err == nil {
		t.Fatal("expected lock mismatch after upstream change")
	}
	if diagCode(err) != "lock_mismatch" {
		t.Fatalf("expected lock_mismatch, got %v", err)
	}

	// Repin is the explicit acceptance ceremony; afterwards builds pass and
	// the lock carries the new hash.
	if err := app.Run([]string{"lock", "--repin", "--version", "1.0"}); err != nil {
		t.Fatalf("repin failed: %v\nstderr=%s", err, stderr.String())
	}
	lock, err = LoadLockFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry = lock.Find("1.0")
	if entry == nil || entry.SHA256 != sha256Hex(responses[rawURL]) {
		t.Fatalf("expected repinned hash, got %+v", entry)
	}
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("post-repin build failed: %v\nstderr=%s", err, stderr.String())
	}
}

func TestLockRepinRequiresExactVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), lockTestRecipe)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"lock", "--repin"}); err == nil {
		t.Fatal("expected repin without version to fail")
	}
	if err := app.Run([]string{"lock", "--repin", "--latest"}); err == nil {
		t.Fatal("expected repin with --latest to fail")
	}
}

func TestGitSourceLocksCommitAndDetectsTagMove(t *testing.T) {
	dir := t.TempDir()
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
	runTestCmd(t, repo, "git", "tag", "v1.0.0")

	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"

[build]
command = "true"
`)
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--version", "1.0.0"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s", err, stderr.String())
	}
	lock, err := LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.Find("1.0.0")
	if entry == nil || entry.Commit == "" || entry.Ref != "v1.0.0" {
		t.Fatalf("expected git lock entry, got %+v", entry)
	}
	firstCommit := entry.Commit

	// Upstream moves the tag: hard stop, even though the locked commit is
	// still fetchable.
	writeFile(t, filepath.Join(repo, "payload.txt"), "changed")
	runTestCmd(t, repo, "git", "commit", "-am", "second")
	runTestCmd(t, repo, "git", "tag", "-f", "v1.0.0")
	err = app.Run([]string{"build", "--version", "1.0.0"})
	if err == nil {
		t.Fatal("expected lock mismatch after tag move")
	}
	if diagCode(err) != "lock_mismatch" {
		t.Fatalf("expected lock_mismatch, got %v", err)
	}

	if err := app.Run([]string{"lock", "--repin", "--version", "1.0.0"}); err != nil {
		t.Fatalf("repin failed: %v\nstderr=%s", err, stderr.String())
	}
	lock, err = LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	entry = lock.Find("1.0.0")
	if entry == nil || entry.Commit == firstCommit || entry.Commit == "" {
		t.Fatalf("expected repinned commit, got %+v", entry)
	}
	if err := app.Run([]string{"build", "--version", "1.0.0"}); err != nil {
		t.Fatalf("post-repin build failed: %v\nstderr=%s", err, stderr.String())
	}
}

// TestGitSourceBuildsFromLockWhenFetchFails covers the refresh-failure
// fallback: once a version is locked and its commit mirrored, losing the
// upstream (rate limit, offline, deleted repo) downgrades the fetch
// error to a warning and the build proceeds from the locked commit. An
// unlocked resolve still fails hard — nothing pins what the ref means.
func TestGitSourceBuildsFromLockWhenFetchFails(t *testing.T) {
	dir := t.TempDir()
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
	runTestCmd(t, repo, "git", "tag", "v1.0.0")

	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"

[build]
command = "true"
`)
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--version", "1.0.0"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s", err, stderr.String())
	}
	lock, err := LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.Find("1.0.0")
	if entry == nil || entry.Commit == "" {
		t.Fatalf("expected git lock entry, got %+v", entry)
	}

	// Upstream disappears: the locked, mirrored version keeps building.
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--version", "1.0.0"}); err != nil {
		t.Fatalf("locked build after upstream loss failed: %v\nstderr=%s", err, stderr.String())
	}

	// Without the lock the same failed refresh is fatal.
	if err := os.Remove(filepath.Join(recipe, "pekit.lock")); err != nil {
		t.Fatal(err)
	}
	err = app.Run([]string{"build", "--version", "1.0.0"})
	if err == nil {
		t.Fatal("expected unlocked build to fail when the refresh fails")
	}
	if diagCode(err) != "git_fetch" {
		t.Fatalf("expected git_fetch, got %v", err)
	}
}

func newTestSigner(t *testing.T) *openpgp.Entity {
	t.Helper()
	entity, err := openpgp.NewEntity("Upstream Test", "", "upstream@example.test", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	return entity
}

func publicKeyBytes(t *testing.T, entity *openpgp.Entity) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := entity.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func detachSign(t *testing.T, entity *openpgp.Entity, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := openpgp.DetachSign(&buf, entity, bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func detachSignAt(t *testing.T, entity *openpgp.Entity, data []byte, at time.Time, lifetime uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := openpgp.DetachSign(&buf, entity, bytes.NewReader(data), &packet.Config{
		Time:            func() time.Time { return at },
		SigLifetimeSecs: lifetime,
	}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const signedRecipe = `
out_dir = "out"

[source.url]
url = "https://example.test/app-{{version}}.tar.gz"
extract = true
root = "app-{{version}}"

[source.url.signature]
key_files = ["keys/upstream.key"]

[build]
command = "cat payload.txt > \"$PEKIT_OUT/value\""
`

func TestURLSignatureVerifiedAndPinned(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	artifact := makeTarGz(t, "app-1.0", "payload")
	responses := map[string][]byte{
		"https://example.test/app-1.0.tar.gz":     artifact,
		"https://example.test/app-1.0.tar.gz.sig": detachSign(t, signer, artifact),
	}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"), signedRecipe)
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "upstream.key"), publicKeyBytes(t, signer), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("signed build failed: %v\nstderr=%s", err, stderr.String())
	}
	lock, err := LoadLockFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.Find("1.0")
	wantFpr := hex.EncodeToString(signer.PrimaryKey.Fingerprint)
	if entry == nil || entry.SignatureKey != wantFpr {
		t.Fatalf("expected signature_key %s, got %+v", wantFpr, entry)
	}
}

func TestURLSignatureMadeBeforeKeyExpiryIsAccepted(t *testing.T) {
	dir := t.TempDir()
	created := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	signer, err := openpgp.NewEntity("Historical Upstream", "", "upstream@example.test", &packet.Config{
		Algorithm:       packet.PubKeyAlgoEdDSA,
		Time:            func() time.Time { return created },
		KeyLifetimeSecs: uint32((24 * time.Hour) / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := makeTarGz(t, "app-1.0", "payload")
	responses := map[string][]byte{
		"https://example.test/app-1.0.tar.gz":     artifact,
		"https://example.test/app-1.0.tar.gz.sig": detachSignAt(t, signer, artifact, created.Add(time.Hour), 0),
	}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"), signedRecipe)
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "upstream.key"), publicKeyBytes(t, signer), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("historical signature failed: %v\nstderr=%s", err, stderr.String())
	}
}

func TestHistoricalVerificationSelectsSelfSignatureValidAtSigningTime(t *testing.T) {
	created := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	signer, err := openpgp.NewEntity("Renewed Upstream", "", "upstream@example.test", &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
		Time:      func() time.Time { return created },
	})
	if err != nil {
		t.Fatal(err)
	}
	var identityName string
	var original *packet.Signature
	for name, identity := range signer.Identities {
		identityName = name
		original = identity.SelfSignature
		break
	}
	renewedAt := created.Add(48 * time.Hour)
	if err := signer.SignIdentity(identityName, signer, &packet.Config{
		Time: func() time.Time { return renewedAt },
	}); err != nil {
		t.Fatal(err)
	}

	parsed, err := openpgp.ReadKeyRing(bytes.NewReader(publicKeyBytes(t, signer)))
	if err != nil {
		t.Fatal(err)
	}
	identity := parsed[0].Identities[identityName]
	current := identity.SelfSignature
	if !current.CreationTime.Equal(renewedAt) {
		t.Fatalf("expected parser to select renewed self-signature at %s, got %s", renewedAt, current.CreationTime)
	}

	restore := selectIdentitySelfSignaturesAt(parsed, original.CreationTime.Add(time.Minute))
	if !identity.SelfSignature.CreationTime.Equal(original.CreationTime) {
		t.Fatalf("expected historical self-signature at %s, got %s", original.CreationTime, identity.SelfSignature.CreationTime)
	}
	restore()
	if identity.SelfSignature != current {
		t.Fatal("expected current self-signature to be restored")
	}
}

func TestExpiredSignatureIsRejectedEvenWhenKeyAlsoExpired(t *testing.T) {
	dir := t.TempDir()
	created := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	signer, err := openpgp.NewEntity("Historical Upstream", "", "upstream@example.test", &packet.Config{
		Algorithm:       packet.PubKeyAlgoEdDSA,
		Time:            func() time.Time { return created },
		KeyLifetimeSecs: uint32((24 * time.Hour) / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := makeTarGz(t, "app-1.0", "payload")
	responses := map[string][]byte{
		"https://example.test/app-1.0.tar.gz":     artifact,
		"https://example.test/app-1.0.tar.gz.sig": detachSignAt(t, signer, artifact, created.Add(time.Hour), uint32((time.Hour)/time.Second)),
	}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"), signedRecipe)
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "upstream.key"), publicKeyBytes(t, signer), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err = app.Run([]string{"build", "--version", "1.0"})
	if err == nil || diagCode(err) != "signature_invalid" || !strings.Contains(err.Error(), "signature expired") {
		t.Fatalf("expected expired signature rejection, got %v", err)
	}
}

func TestURLSignatureInvalidFailsAndNothingIsLocked(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	imposter := newTestSigner(t)
	artifact := makeTarGz(t, "app-1.0", "payload")
	responses := map[string][]byte{
		"https://example.test/app-1.0.tar.gz":     artifact,
		"https://example.test/app-1.0.tar.gz.sig": detachSign(t, imposter, artifact),
	}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"), signedRecipe)
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "upstream.key"), publicKeyBytes(t, signer), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{"build", "--version", "1.0"})
	if err == nil {
		t.Fatal("expected signature verification to fail")
	}
	if diagCode(err) != "signature_invalid" {
		t.Fatalf("expected signature_invalid, got %v", err)
	}
	if fileExists(filepath.Join(dir, lockFileName)) {
		t.Fatal("nothing should be locked after a failed signature")
	}
}

func TestURLSignatureMissingFails(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	artifact := makeTarGz(t, "app-1.0", "payload")
	responses := map[string][]byte{
		"https://example.test/app-1.0.tar.gz": artifact,
	}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"), signedRecipe)
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "upstream.key"), publicKeyBytes(t, signer), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{"build", "--version", "1.0"})
	if err == nil {
		t.Fatal("expected missing signature to fail")
	}
	if diagCode(err) != "signature_missing" {
		t.Fatalf("expected signature_missing, got %v", err)
	}
}

func TestURLSignatureUntrustedFingerprintFails(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	artifact := makeTarGz(t, "app-1.0", "payload")
	responses := map[string][]byte{
		"https://example.test/app-1.0.tar.gz":     artifact,
		"https://example.test/app-1.0.tar.gz.sig": detachSign(t, signer, artifact),
	}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"), strings.Replace(signedRecipe,
		`key_files = ["keys/upstream.key"]`,
		`key_files = ["keys/upstream.key"]
fingerprints = ["deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"]`, 1))
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "upstream.key"), publicKeyBytes(t, signer), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{"build", "--version", "1.0"})
	if err == nil {
		t.Fatal("expected untrusted fingerprint to fail")
	}
	if diagCode(err) != "signature_untrusted_key" {
		t.Fatalf("expected signature_untrusted_key, got %v", err)
	}
}

// A locked URL source is anchored: publish no longer needs --allow-unanchored
// once the version is pinned, because the lock is the anchor.
func TestPublishLockedURLSourceIsAnchored(t *testing.T) {
	dir := t.TempDir()
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload")})
	writeFile(t, filepath.Join(dir, "pekit.toml"), lockTestRecipe)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
":value" = "usr/share/value"

[[publish.localdir]]
path = "repo"
`)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"publish", "--version", "1.0"}); err != nil {
		t.Fatalf("publish of locked source failed: %v\nstderr=%s", err, stderr.String())
	}
	if !fileExists(filepath.Join(dir, "repo", "main.tar")) {
		t.Fatal("expected published artifact")
	}
}

func TestLockStatusListsEntries(t *testing.T) {
	dir := t.TempDir()
	rawURL := "https://example.test/app-1.0.tar.gz"
	body := makeTarGz(t, "app-1.0", "payload")
	serveURLs(t, map[string][]byte{rawURL: body})
	writeFile(t, filepath.Join(dir, "pekit.toml"), lockTestRecipe)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"lock", "--version", "1.0"}); err != nil {
		t.Fatalf("lock fetch failed: %v\nstderr=%s", err, stderr.String())
	}
	stdout.Reset()
	if err := app.Run([]string{"lock"}); err != nil {
		t.Fatalf("lock status failed: %v\nstderr=%s", err, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte(sha256Hex(body))) {
		t.Fatalf("expected lock status to include pinned hash, got %s", stdout.String())
	}
}

func TestLockFileRoundTripSortsEntries(t *testing.T) {
	dir := t.TempDir()
	lock := LockFile{}
	lock.Put(LockSource{Version: "2.10.0", URL: "u", SHA256: "b"})
	lock.Put(LockSource{Version: "2.2.0", URL: "u", SHA256: "a"})
	lock.Put(LockSource{Version: "2.2.0", URL: "u", SHA256: "c"}) // replace
	if err := SaveLockFile(dir, lock); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadLockFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Sources) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(loaded.Sources))
	}
	if loaded.Sources[0].Version != "2.2.0" || loaded.Sources[0].SHA256 != "c" {
		t.Fatalf("expected replaced 2.2.0 entry first, got %+v", loaded.Sources)
	}
	if loaded.Sources[1].Version != "2.10.0" {
		t.Fatalf("expected version-ordered entries, got %+v", loaded.Sources)
	}
}

func sha256Hex(data []byte) string {
	tmp, err := os.CreateTemp("", "hash-*")
	if err != nil {
		panic(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		panic(err)
	}
	if err := tmp.Close(); err != nil {
		panic(err)
	}
	sum, err := fileSHA256(tmp.Name())
	if err != nil {
		panic(err)
	}
	return sum
}
