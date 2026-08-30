package pekit

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509/pkix"
	"debug/elf"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
)

// syntheticELF builds a minimal but structurally complete ELF64 LSB
// file: header, null section, a .text section with payload, a section
// name string table, and optionally a pre-reserved .peios.sig section
// of reserveSize bytes.
func syntheticELF(payload []byte, reserveSize int) []byte {
	le := binary.LittleEndian
	names := "\x00.text\x00.shstrtab\x00"
	if reserveSize > 0 {
		names += pipSectionName + "\x00"
	}
	var body bytes.Buffer
	body.Write(make([]byte, elfHeaderSize))
	textOff := body.Len()
	body.Write(payload)
	sigOff := body.Len()
	if reserveSize > 0 {
		body.Write(make([]byte, reserveSize))
	}
	strOff := body.Len()
	body.WriteString(names)
	for body.Len()%8 != 0 {
		body.WriteByte(0)
	}
	shoff := body.Len()
	shdr := func(name uint32, typ uint32, off, size int) {
		h := make([]byte, elfShdrSize)
		le.PutUint32(h[shdrName:], name)
		le.PutUint32(h[shdrType:], typ)
		le.PutUint64(h[shdrOffset:], uint64(off))
		le.PutUint64(h[shdrSize:], uint64(size))
		le.PutUint64(h[shdrAddralign:], 1)
		body.Write(h)
	}
	body.Write(make([]byte, elfShdrSize)) // SHN_UNDEF
	shdr(1, shtProgbits, textOff, len(payload))
	shdr(7, 3 /* SHT_STRTAB */, strOff, len(names))
	shnum := 3
	if reserveSize > 0 {
		shdr(17, shtProgbits, sigOff, reserveSize)
		shnum = 4
	}
	out := body.Bytes()
	copy(out, "\x7fELF")
	out[4] = elfClass64
	out[5] = elfData2LSB
	out[6] = elfEVCurrent
	le.PutUint16(out[16:], 2)  // ET_EXEC
	le.PutUint16(out[18:], 62) // EM_X86_64
	le.PutUint32(out[20:], 1)  // EV_CURRENT
	le.PutUint64(out[ehdrShoff:], uint64(shoff))
	le.PutUint16(out[52:], elfHeaderSize)
	le.PutUint16(out[ehdrShentsize:], elfShdrSize)
	le.PutUint16(out[ehdrShnum:], uint16(shnum))
	le.PutUint16(out[ehdrShstrndx:], 2)
	return out
}

func testPIPKey(t *testing.T) *pipSigningKey {
	t.Helper()
	pub, priv, err := mldsa65.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return newPIPSigningKey(pub, priv)
}

// independentVerify checks a signed file using the standard library's
// ELF reader rather than the signer's own parser, then re-derives the
// hash and verifies the blob. It is the test's model of the kernel.
func independentVerify(t *testing.T, data []byte, pub *mldsa65.PublicKey) {
	t.Helper()
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("debug/elf rejects signed file: %v", err)
	}
	sec := f.Section(pipSectionName)
	if sec == nil {
		t.Fatalf("no %s section", pipSectionName)
	}
	if sec.Type != elf.SHT_PROGBITS || sec.Size != pipSigSize {
		t.Fatalf("section type=%v size=%d", sec.Type, sec.Size)
	}
	blob := data[sec.Offset : sec.Offset+sec.Size]
	if blob[0] != pipSigVersion {
		t.Fatalf("version byte %#x", blob[0])
	}
	h := sha256.New()
	h.Write(data[:sec.Offset])
	h.Write(make([]byte, sec.Size))
	h.Write(data[sec.Offset+sec.Size:])
	if !mldsa65.Verify(pub, h.Sum(nil), nil, blob[1:]) {
		t.Fatal("signature does not verify")
	}
}

func signTemp(t *testing.T, data []byte, key *pipSigningKey) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := signPIPFile(path, key); err != nil {
		t.Fatalf("signPIPFile: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o755 {
		t.Errorf("mode changed to %v", st.Mode().Perm())
	}
	return out
}

func TestPIPSignAppendsSection(t *testing.T) {
	key := testPIPKey(t)
	orig := syntheticELF([]byte("hello, kernel"), 0)
	signed := signTemp(t, orig, key)
	independentVerify(t, signed, key.pub)
	// Existing bytes are never moved: the original file is a prefix of
	// the signed one, save for the two header fields that were retargeted.
	if !bytes.Equal(signed[elfHeaderSize:len(orig)], orig[elfHeaderSize:]) {
		t.Error("signing moved existing file bytes")
	}
	// Deterministic: signing the same input twice yields identical bytes.
	if again := signTemp(t, orig, key); !bytes.Equal(again, signed) {
		t.Error("signing is not deterministic")
	}
	// Tampering with any covered byte breaks verification.
	tampered := append([]byte(nil), signed...)
	tampered[elfHeaderSize] ^= 0xff
	if err := verifyPIPFile(tampered, key.pub); err == nil {
		t.Error("tampered file still verifies")
	}
	// A different key does not verify.
	if err := verifyPIPFile(signed, testPIPKey(t).pub); err == nil {
		t.Error("foreign key verifies")
	}
}

func TestPIPSignFillsReservedSection(t *testing.T) {
	key := testPIPKey(t)
	orig := syntheticELF([]byte("payload"), pipSigSize)
	signed := signTemp(t, orig, key)
	if len(signed) != len(orig) {
		t.Fatalf("reserved section should be filled in place; size %d -> %d", len(orig), len(signed))
	}
	independentVerify(t, signed, key.pub)
	// Signing a signed file again re-zeroes and re-signs identically.
	if again := signTemp(t, signed, key); !bytes.Equal(again, signed) {
		t.Error("re-signing changed the file")
	}
}

func TestPIPSignRejectsUnsuitableFiles(t *testing.T) {
	key := testPIPKey(t)
	cases := map[string][]byte{
		"not elf":    []byte("#!/bin/sh\necho hi\n"),
		"wrong size": syntheticELF([]byte("x"), 100),
		"32-bit":     func() []byte { d := syntheticELF([]byte("x"), 0); d[4] = 1; return d }(),
		"big-endian": func() []byte { d := syntheticELF([]byte("x"), 0); d[5] = 2; return d }(),
		"no sections": func() []byte {
			d := syntheticELF([]byte("x"), 0)
			binary.LittleEndian.PutUint16(d[ehdrShnum:], 0)
			return d
		}(),
		"bad shoff": func() []byte {
			d := syntheticELF([]byte("x"), 0)
			binary.LittleEndian.PutUint64(d[ehdrShoff:], 1<<40)
			return d
		}(),
		"bad shstrndx": func() []byte {
			d := syntheticELF([]byte("x"), 0)
			binary.LittleEndian.PutUint16(d[ehdrShstrndx:], 9)
			return d
		}(),
	}
	for name, data := range cases {
		path := filepath.Join(t.TempDir(), "bin")
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := signPIPFile(path, key); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, data) {
			t.Errorf("%s: file modified despite error", name)
		}
	}
}

func TestPIPSignRealBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Skip(err)
	}
	if _, err := parseELF64(data); err != nil {
		t.Skipf("test binary is not a signable ELF: %v", err)
	}
	key := testPIPKey(t)
	signed := signTemp(t, data, key)
	independentVerify(t, signed, key.pub)
	f, err := elf.NewFile(bytes.NewReader(signed))
	if err != nil {
		t.Fatal(err)
	}
	// Every original section survives with the same name and contents.
	orig, _ := elf.NewFile(bytes.NewReader(data))
	if len(f.Sections) != len(orig.Sections)+1 {
		t.Fatalf("section count %d -> %d", len(orig.Sections), len(f.Sections))
	}
	for i, s := range orig.Sections {
		if s.Type == elf.SHT_STRTAB && s.Name == ".shstrtab" {
			continue // relocated by design: it gains the new section's name
		}
		if f.Sections[i].Name != s.Name || f.Sections[i].Offset != s.Offset || f.Sections[i].Size != s.Size {
			t.Errorf("section %d changed: %+v -> %+v", i, s.SectionHeader, f.Sections[i].SectionHeader)
		}
	}
}

// pkcs8 wraps an ML-DSA-65-PrivateKey CHOICE encoding in PKCS#8 PEM.
func pkcs8(t *testing.T, choice []byte) []byte {
	t.Helper()
	der, err := asn1.Marshal(struct {
		Version    int
		Algorithm  pkix.AlgorithmIdentifier
		PrivateKey []byte
	}{0, pkix.AlgorithmIdentifier{Algorithm: oidMLDSA65}, choice})
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestLoadPIPSigningKeyEncodings(t *testing.T) {
	var seed [mldsa65.SeedSize]byte
	rand.Read(seed[:])
	pub, priv := mldsa65.NewKeyFromSeed(&seed)
	want := newPIPSigningKey(pub, priv).fingerprint

	seedChoice, _ := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, Bytes: seed[:]})
	expChoice, _ := asn1.Marshal(priv.Bytes())
	bothChoice, _ := asn1.Marshal(struct{ Seed, Expanded []byte }{seed[:], priv.Bytes()})
	cases := map[string][]byte{
		"raw seed":     seed[:],
		"pkcs8 seed":   pkcs8(t, seedChoice),
		"pkcs8 expand": pkcs8(t, expChoice),
		"pkcs8 both":   pkcs8(t, bothChoice),
		"der pkcs8":    func() []byte { b, _ := pem.Decode(pkcs8(t, bothChoice)); return b.Bytes }(),
	}
	for name, data := range cases {
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		key, err := loadPIPSigningKey(path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if key.fingerprint != want {
			t.Errorf("%s: fingerprint %s, want %s", name, key.fingerprint, want)
		}
	}

	// Bad inputs.
	wrongSeed := append([]byte(nil), seed[:]...)
	wrongSeed[0] ^= 1
	mismatch, _ := asn1.Marshal(struct{ Seed, Expanded []byte }{wrongSeed, priv.Bytes()})
	ed25519OID, _ := asn1.Marshal(struct {
		Version    int
		Algorithm  pkix.AlgorithmIdentifier
		PrivateKey []byte
	}{0, pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 3, 101, 112}}, expChoice})
	bad := map[string][]byte{
		"seed/expanded mismatch": pkcs8(t, mismatch),
		"wrong algorithm":        pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ed25519OID}),
		"garbage":                []byte("not a key"),
		"wrong pem type":         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}}),
	}
	for name, data := range bad {
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadPIPSigningKey(path); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestPIPSignInteropOpenSSL cross-checks against OpenSSL 3.5+: a key
// OpenSSL generated loads with the fingerprint OpenSSL derives, and a
// signature pekit produced verifies under OpenSSL's ML-DSA-65.
func TestPIPSignInteropOpenSSL(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not on PATH")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if out, err := exec.Command("openssl", "genpkey", "-algorithm", "ML-DSA-65", "-out", keyPath).CombinedOutput(); err != nil {
		t.Skipf("openssl cannot generate ML-DSA-65 keys: %v\n%s", err, out)
	}
	pubDER, err := exec.Command("openssl", "pkey", "-in", keyPath, "-pubout", "-outform", "DER").Output()
	if err != nil {
		t.Fatal(err)
	}
	raw := pubDER[len(pubDER)-mldsa65.PublicKeySize:]
	sum := sha256.Sum256(raw)
	key, err := loadPIPSigningKey(keyPath)
	if err != nil {
		t.Fatalf("load openssl key: %v", err)
	}
	if key.fingerprint != hex.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint %s, openssl says %x", key.fingerprint, sum)
	}

	signed := signTemp(t, syntheticELF([]byte("interop"), 0), key)
	independentVerify(t, signed, key.pub)
	f, _ := elf.NewFile(bytes.NewReader(signed))
	sec := f.Section(pipSectionName)
	h := sha256.New()
	h.Write(signed[:sec.Offset])
	h.Write(make([]byte, sec.Size))
	h.Write(signed[sec.Offset+sec.Size:])
	msgPath := filepath.Join(dir, "msg")
	sigPath := filepath.Join(dir, "sig")
	pubPath := filepath.Join(dir, "pub.der")
	os.WriteFile(msgPath, h.Sum(nil), 0o600)
	os.WriteFile(sigPath, signed[sec.Offset+1:sec.Offset+sec.Size], 0o600)
	os.WriteFile(pubPath, pubDER, 0o600)
	out, err := exec.Command("openssl", "pkeyutl", "-verify", "-pubin", "-inkey", pubPath, "-keyform", "DER",
		"-in", msgPath, "-sigfile", sigPath).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Signature Verified Successfully") {
		t.Fatalf("openssl rejects pekit's signature: %v\n%s", err, out)
	}
}

const pipSignRecipe = `
out_dir = "out"

[source.url]
url = "https://example.test/app-{{version}}.tar.gz"
extract = true
root = "app-{{version}}"

[build]
command = """
mkdir -p "$PEKIT_OUT/bin" "$PEKIT_OUT/lib"
cp "$PEKIT_RECIPE_ROOT/app.elf" "$PEKIT_OUT/bin/app"
cp "$PEKIT_RECIPE_ROOT/app.elf" "$PEKIT_OUT/lib/libapp.so.1"
printf '#!/bin/sh\n' > "$PEKIT_OUT/bin/script"
ln -s script "$PEKIT_OUT/bin/script-alias"
"""

[build.sign.pip]
"bin/app" = "tcb.priv"
"lib/*.so.*" = "tcb.priv"
`

func setupPIPSignRecipe(t *testing.T, recipeText string) (string, *pipSigningKey, string) {
	t.Helper()
	dir := t.TempDir()
	recipe := filepath.Join(dir, "app")
	serveURLs(t, map[string][]byte{"https://example.test/app-1.0.tar.gz": makeTarGz(t, "app-1.0", "payload\n")})
	writeFile(t, filepath.Join(recipe, "pekit.toml"), recipeText)
	if err := os.WriteFile(filepath.Join(recipe, "app.elf"), syntheticELF([]byte("app"), 0), 0o755); err != nil {
		t.Fatal(err)
	}
	var seed [mldsa65.SeedSize]byte
	rand.Read(seed[:])
	pub, priv := mldsa65.NewKeyFromSeed(&seed)
	keyPath := filepath.Join(dir, "tcb.key")
	if err := os.WriteFile(keyPath, seed[:], 0o600); err != nil {
		t.Fatal(err)
	}
	chdir(t, recipe)
	return recipe, newPIPSigningKey(pub, priv), keyPath
}

func TestBuildSignsTargetOutput(t *testing.T) {
	recipe, key, keyPath := setupPIPSignRecipe(t, pipSignRecipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--version", "1.0", "--json", "--keyring.tcb.priv=" + keyPath}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s", err, stderr.String())
	}
	for _, rel := range []string{"bin/app", "lib/libapp.so.1"} {
		data, err := os.ReadFile(globOne(t, filepath.Join(recipe, "out/*/build/main", rel)))
		if err != nil {
			t.Fatal(err)
		}
		independentVerify(t, data, key.pub)
	}
	if got := strings.Count(stdout.String(), `"type":"sign"`); got != 2 {
		t.Errorf("expected 2 sign events, got %d:\n%s", got, stdout.String())
	}
	if !strings.Contains(stdout.String(), key.fingerprint) {
		t.Errorf("sign events should name the key fingerprint:\n%s", stdout.String())
	}
	// The unsigned script is left alone.
	script, _ := os.ReadFile(globOne(t, filepath.Join(recipe, "out/*/build/main/bin/script")))
	if !bytes.HasPrefix(script, []byte("#!/bin/sh")) {
		t.Error("script was modified")
	}
}

// TestBuildSignsNonELFDetached: a non-ELF match gets a `<file>.peios.sig`
// sidecar holding the bare blob over the whole file, the file itself is
// untouched, and a `**` pattern that sweeps the sidecar back up does not
// sign the signature.
func TestBuildSignsNonELFDetached(t *testing.T) {
	recipeText := strings.Replace(pipSignRecipe, `"bin/app" = "tcb.priv"`, `"bin/**" = "tcb.priv"`, 1)
	recipe, key, keyPath := setupPIPSignRecipe(t, recipeText)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--version", "1.0", "--json", "--keyring.tcb.priv=" + keyPath}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s", err, stderr.String())
	}
	script, err := os.ReadFile(globOne(t, filepath.Join(recipe, "out/*/build/main/bin/script")))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(script, []byte("#!/bin/sh\n")) {
		t.Errorf("script was modified: %q", script)
	}
	blob, err := os.ReadFile(globOne(t, filepath.Join(recipe, "out/*/build/main/bin/script"+pipSidecarSuffix)))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if err := verifyPIPDetached(script, blob, key.pub); err != nil {
		t.Errorf("sidecar does not verify: %v", err)
	}
	if err := verifyPIPDetached([]byte("#!/bin/sh\n#"), blob, key.pub); err == nil {
		t.Error("sidecar verified against different bytes")
	}
	if _, err := os.Stat(globOne(t, filepath.Join(recipe, "out/*/build/main/bin/script"))+pipSidecarSuffix+pipSidecarSuffix); err == nil {
		t.Error("the sidecar was itself signed")
	}
	// The symlink matched by the same pattern is skipped: no sidecar, no event.
	if _, err := os.Lstat(globOne(t, filepath.Join(recipe, "out/*/build/main/bin/script-alias")) + pipSidecarSuffix); err == nil {
		t.Error("symlink got a sidecar")
	}
	// The ELF file matched by the same pattern still takes the section.
	data, err := os.ReadFile(globOne(t, filepath.Join(recipe, "out/*/build/main/bin/app")))
	if err != nil {
		t.Fatal(err)
	}
	independentVerify(t, data, key.pub)
	if _, err := os.Stat(globOne(t, filepath.Join(recipe, "out/*/build/main/bin/app")) + pipSidecarSuffix); err == nil {
		t.Error("ELF target also got a sidecar")
	}
	if got := strings.Count(stdout.String(), `"type":"sign"`); got != 3 {
		t.Errorf("expected 3 sign events, got %d:\n%s", got, stdout.String())
	}
	if !strings.Contains(stdout.String(), "pip-signed (sidecar)") || !strings.Contains(stdout.String(), "pip-signed (section)") {
		t.Errorf("events should name the placement:\n%s", stdout.String())
	}
}

func TestBuildSignFailures(t *testing.T) {
	cases := []struct {
		name   string
		recipe string
		args   []string
		code   string
	}{
		{"missing keyring entry", pipSignRecipe, nil, "signing_key"},
		{"unloadable key", pipSignRecipe, []string{"--keyring.tcb.priv=/nonexistent/key"}, "signing_key"},
		{"pattern matches nothing", strings.Replace(pipSignRecipe, `"bin/app" = "tcb.priv"`, `"bin/missing" = "tcb.priv"`, 1), nil, "sign_target_missing"},
		{"unknown kind", strings.Replace(pipSignRecipe, "[build.sign.pip]", "[build.sign.gpg]", 1), nil, "unknown_key"},
		{"sign on test target", strings.Replace(pipSignRecipe, "[build.sign.pip]", "[test]\ncommand = \"true\"\n[test.sign.pip]", 1), nil, "mixed_targets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, keyPath := setupPIPSignRecipe(t, tc.recipe)
			args := []string{"build", "--version", "1.0", "--json"}
			if tc.args != nil {
				args = append(args, tc.args...)
			} else if tc.code != "signing_key" {
				args = append(args, "--keyring.tcb.priv="+keyPath)
			}
			var stdout, stderr bytes.Buffer
			app := &App{Stdout: &stdout, Stderr: &stderr}
			err := app.Run(args)
			if err == nil {
				t.Fatalf("expected failure %s", tc.code)
			}
			if !strings.Contains(err.Error(), tc.code) && !strings.Contains(stderr.String(), tc.code) && !strings.Contains(stdout.String(), tc.code) {
				t.Fatalf("expected diagnostic %s, got %v\nstdout=%s\nstderr=%s", tc.code, err, stdout.String(), stderr.String())
			}
		})
	}
}

// TestPIPSignedBinaryStillRuns signs a real system binary and executes
// it: the appended section must not disturb the loader.
func TestPIPSignedBinaryStillRuns(t *testing.T) {
	src, err := exec.LookPath("true")
	if err != nil {
		t.Skip(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skip(err)
	}
	if _, err := parseELF64(data); err != nil {
		t.Skipf("%s is not a signable ELF: %v", src, err)
	}
	key := testPIPKey(t)
	signed := signTemp(t, data, key)
	independentVerify(t, signed, key.pub)
	path := filepath.Join(t.TempDir(), "true")
	if err := os.WriteFile(path, signed, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(path).CombinedOutput(); err != nil {
		t.Fatalf("signed binary failed to run: %v\n%s", err, out)
	}
}
