package pekit

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
)

// This file implements the signer role of PSPK chapter 3, "Binary Signing
// and PIP": it produces the fixed 3310-byte signature blob the Peios
// kernel verifies at exec and library load, and stores it in the ELF
// `.peios.sig` section.
//
// Both placements PSPK defines are produced, chosen by what the file
// is. An ELF file carries the blob in its `.peios.sig` section. Any
// other file cannot, and the alternative placement — the
// security.peios.sig extended attribute — cannot travel inside a
// package either, because PSPU §5.11 forbids extended attributes on
// package entries. So a non-ELF target gets a detached sidecar,
// `<file>.peios.sig`, holding the bare blob: peipkg's pack-time checks
// tie it to its target, and the installer (peipkg install and
// peipkg-compose alike) derives the xattr from it and never writes the
// sidecar itself to the root. The hash is over the whole file as it
// sits on disk — for a compressed firmware blob, the compressed bytes —
// which is what the kernel verifies before it decompresses anything.
//
// The spec is explicit that a signer which gets any detail wrong loses
// the property silently: an unverifiable binary executes with no tier
// and no diagnostic. Every step here that can fail does so loudly, and
// the finished file is re-verified through an independent lookup before
// the signer returns.

// pipSigVersion is the only signature blob version PSPK defines.
const pipSigVersion = 0x01

// pipSigSize is the fixed size of a signature blob: one version byte
// followed by a raw ML-DSA-65 signature.
const pipSigSize = 1 + mldsa65.SignatureSize

// pipSectionName is the ELF section that carries the signature. The
// kernel compares all eleven bytes including the terminating NUL.
const pipSectionName = ".peios.sig"

// pipSidecarSuffix is appended to a non-ELF file's path to name its
// detached signature. It is the section name on purpose: pekit emits
// one thing, and only where it lands differs.
const pipSidecarSuffix = pipSectionName

// signKindPIP is the `sign.<kind>` key that selects this signer.
const signKindPIP = "pip"

// oidMLDSA65 is id-ml-dsa-65 from NIST's CSOR arc.
var oidMLDSA65 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 18}

// pipSigningKey is a loaded ML-DSA-65 key pair plus the fingerprint
// pekit reports for it. The blob itself carries no key identifier, so
// the fingerprint exists purely for humans and logs: it is the
// lowercase hex SHA-256 of the raw 1952-byte public key — the same bytes
// the kernel's key table holds.
type pipSigningKey struct {
	priv        *mldsa65.PrivateKey
	pub         *mldsa65.PublicKey
	fingerprint string
}

// loadPIPSigningKey reads an ML-DSA-65 private key from path. Accepted
// encodings:
//
//   - a PKCS#8 PEM ("PRIVATE KEY") or bare DER PrivateKeyInfo whose
//     privateKey field is the ML-DSA-65-PrivateKey CHOICE of
//     draft-ietf-lamps-dilithium-certificates — the [0] seed, the
//     expanded key OCTET STRING, or the SEQUENCE carrying both — which
//     is what `openssl genpkey -algorithm ML-DSA-65` produces;
//   - a raw 32-byte seed.
func loadPIPSigningKey(path string) (*pipSigningKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) == mldsa65.SeedSize {
		var seed [mldsa65.SeedSize]byte
		copy(seed[:], raw)
		pub, priv := mldsa65.NewKeyFromSeed(&seed)
		return newPIPSigningKey(pub, priv), nil
	}
	der := raw
	if block, _ := pem.Decode(raw); block != nil {
		if block.Type != "PRIVATE KEY" {
			return nil, fmt.Errorf("PEM block is %q, want PRIVATE KEY", block.Type)
		}
		der = block.Bytes
	}
	var info struct {
		Version    int
		Algorithm  pkix.AlgorithmIdentifier
		PrivateKey []byte
	}
	rest, err := asn1.Unmarshal(der, &info)
	if err != nil {
		return nil, fmt.Errorf("not a raw seed, PEM, or DER PKCS#8 key: %v", err)
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("trailing data after PKCS#8 structure")
	}
	if !info.Algorithm.Algorithm.Equal(oidMLDSA65) {
		return nil, fmt.Errorf("key algorithm %v is not ML-DSA-65 (%v)", info.Algorithm.Algorithm, oidMLDSA65)
	}
	seed, expanded, err := parseMLDSAPrivateKeyChoice(info.PrivateKey)
	if err != nil {
		return nil, err
	}
	var pub *mldsa65.PublicKey
	var priv *mldsa65.PrivateKey
	if seed != nil {
		var s [mldsa65.SeedSize]byte
		copy(s[:], seed)
		pub, priv = mldsa65.NewKeyFromSeed(&s)
		// When both forms are present they must agree; a mismatch
		// means a corrupted or hand-edited key file.
		if expanded != nil && !bytes.Equal(priv.Bytes(), expanded) {
			return nil, fmt.Errorf("PKCS#8 seed and expanded key disagree")
		}
	} else {
		priv = new(mldsa65.PrivateKey)
		if err := priv.UnmarshalBinary(expanded); err != nil {
			return nil, fmt.Errorf("expanded ML-DSA-65 key: %v", err)
		}
		pub = priv.Public().(*mldsa65.PublicKey)
	}
	return newPIPSigningKey(pub, priv), nil
}

// parseMLDSAPrivateKeyChoice decodes the ML-DSA-65-PrivateKey CHOICE
// carried inside the PKCS#8 privateKey OCTET STRING:
//
//	seed        [0] OCTET STRING (SIZE 32)
//	expandedKey     OCTET STRING (SIZE 4032)
//	both            SEQUENCE { seed OCTET STRING, expandedKey OCTET STRING }
func parseMLDSAPrivateKeyChoice(data []byte) (seed, expanded []byte, err error) {
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("empty PKCS#8 privateKey")
	}
	var rest []byte
	switch data[0] {
	case 0x80: // [0] IMPLICIT OCTET STRING
		var rv asn1.RawValue
		rest, err = asn1.Unmarshal(data, &rv)
		if err == nil {
			seed = rv.Bytes
		}
	case 0x04: // OCTET STRING
		rest, err = asn1.Unmarshal(data, &expanded)
	case 0x30: // SEQUENCE { seed, expandedKey }
		var both struct {
			Seed     []byte
			Expanded []byte
		}
		rest, err = asn1.Unmarshal(data, &both)
		seed, expanded = both.Seed, both.Expanded
	default:
		return nil, nil, fmt.Errorf("unrecognised ML-DSA-65-PrivateKey encoding (tag 0x%02x)", data[0])
	}
	if err != nil {
		return nil, nil, fmt.Errorf("ML-DSA-65-PrivateKey: %v", err)
	}
	if len(rest) != 0 {
		return nil, nil, fmt.Errorf("trailing data after ML-DSA-65-PrivateKey")
	}
	if seed != nil && len(seed) != mldsa65.SeedSize {
		return nil, nil, fmt.Errorf("ML-DSA-65 seed is %d bytes, want %d", len(seed), mldsa65.SeedSize)
	}
	if expanded != nil && len(expanded) != mldsa65.PrivateKeySize {
		return nil, nil, fmt.Errorf("expanded ML-DSA-65 key is %d bytes, want %d", len(expanded), mldsa65.PrivateKeySize)
	}
	return seed, expanded, nil
}

func newPIPSigningKey(pub *mldsa65.PublicKey, priv *mldsa65.PrivateKey) *pipSigningKey {
	sum := sha256.Sum256(pub.Bytes())
	return &pipSigningKey{priv: priv, pub: pub, fingerprint: hex.EncodeToString(sum[:])}
}

// signBlob produces the 3310-byte blob for a content hash: the version
// byte followed by a pure (not pre-hashed), deterministic ML-DSA-65
// signature with an empty context. Deterministic signing keeps a signed
// build reproducible; the verifier cannot tell the two variants apart.
func (k *pipSigningKey) signBlob(hash [sha256.Size]byte) ([]byte, error) {
	blob := make([]byte, pipSigSize)
	blob[0] = pipSigVersion
	if err := mldsa65.SignTo(k.priv, hash[:], nil, false, blob[1:]); err != nil {
		return nil, err
	}
	return blob, nil
}

// signPIPFile signs one ELF file in place: it locates or appends the
// `.peios.sig` section, hashes the file with the section contents
// zeroed, signs the hash, writes the blob into the reserved bytes, and
// then re-verifies the result through an independent lookup.
func signPIPFile(path string, key *pipSigningKey) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	data, off, err := reservePIPSection(data)
	if err != nil {
		return err
	}
	// The layout is now final. Hash exactly what the kernel will hash:
	// the file with the section contents (already zero) excluded.
	hash := sha256.Sum256(data)
	blob, err := key.signBlob(hash)
	if err != nil {
		return err
	}
	copy(data[off:off+pipSigSize], blob)
	if err := verifyPIPFile(data, key.pub); err != nil {
		return fmt.Errorf("post-sign verification failed: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	// Write through a temporary file and rename: the signed file must be a
	// new inode rather than an in-place rewrite, and a crash must not
	// leave a half-written binary behind.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".pipsign-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(st.Mode().Perm()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// ELF64 layout constants (System V ABI).
const (
	elfHeaderSize  = 64
	elfShdrSize    = 64
	elfClass64     = 2
	elfData2LSB    = 1
	elfEVCurrent   = 1
	shtProgbits    = 1
	shnUndef       = 0
	shnLoReserve   = 0xff00
	ehdrShoff      = 40
	ehdrShentsize  = 58
	ehdrShnum      = 60
	ehdrShstrndx   = 62
	shdrName       = 0
	shdrType       = 4
	shdrOffset     = 24
	shdrSize       = 32
	shdrAddralign  = 48
	pipSectionCStr = pipSectionName + "\x00"
)

// elfSections is the parsed section-header view of an ELF64 LSB file,
// validated to the structural requirements PSPK places on a file that
// can carry a signature section.
type elfSections struct {
	shoff    uint64
	shnum    int
	shstrndx int
	shstrtab []byte // the section-name string table's contents
}

// parseELF64 validates the structure PSPK requires and returns the
// section view. A file failing any check cannot carry a section
// signature at all.
func parseELF64(data []byte) (*elfSections, error) {
	if len(data) < 4 || string(data[:4]) != "\x7fELF" {
		return nil, fmt.Errorf("not an ELF file")
	}
	if len(data) < elfHeaderSize {
		return nil, fmt.Errorf("truncated ELF header")
	}
	if data[4] != elfClass64 {
		return nil, fmt.Errorf("not ELFCLASS64 (32-bit ELF cannot carry a signature)")
	}
	if data[5] != elfData2LSB {
		return nil, fmt.Errorf("not ELFDATA2LSB (big-endian ELF cannot carry a signature)")
	}
	if data[6] != elfEVCurrent {
		return nil, fmt.Errorf("e_ident[EI_VERSION] is %d, want EV_CURRENT", data[6])
	}
	le := binary.LittleEndian
	shoff := le.Uint64(data[ehdrShoff:])
	shentsize := int(le.Uint16(data[ehdrShentsize:]))
	shnum := int(le.Uint16(data[ehdrShnum:]))
	shstrndx := int(le.Uint16(data[ehdrShstrndx:]))
	if shentsize != elfShdrSize {
		return nil, fmt.Errorf("e_shentsize is %d, want %d", shentsize, elfShdrSize)
	}
	if shnum == 0 {
		return nil, fmt.Errorf("file has no section header table")
	}
	if shnum >= shnLoReserve {
		return nil, fmt.Errorf("extended section numbering (e_shnum=%d) is not supported", shnum)
	}
	if shstrndx == shnUndef || shstrndx >= shnum {
		return nil, fmt.Errorf("e_shstrndx %d is not a valid section index", shstrndx)
	}
	end := shoff + uint64(shnum)*elfShdrSize
	if shoff < elfHeaderSize || end < shoff || end > uint64(len(data)) {
		return nil, fmt.Errorf("section header table [%d, %d) lies outside the file", shoff, end)
	}
	strOff, strSize := sectionRange(data, shoff, shstrndx)
	if strOff+strSize < strOff || strOff+strSize > uint64(len(data)) {
		return nil, fmt.Errorf("section-name string table lies outside the file")
	}
	return &elfSections{shoff: shoff, shnum: shnum, shstrndx: shstrndx, shstrtab: data[strOff : strOff+strSize]}, nil
}

func (e *elfSections) shdr(data []byte, index int) []byte {
	start := e.shoff + uint64(index)*elfShdrSize
	return data[start : start+elfShdrSize]
}

func sectionRange(data []byte, shoff uint64, index int) (off, size uint64) {
	start := shoff + uint64(index)*elfShdrSize
	le := binary.LittleEndian
	return le.Uint64(data[start+shdrOffset:]), le.Uint64(data[start+shdrSize:])
}

// findSection returns the index of the lowest-numbered section whose
// name matches exactly (including the terminating NUL), or -1.
func (e *elfSections) findSection(data []byte, name string) int {
	for i := 0; i < e.shnum; i++ {
		nameOff := binary.LittleEndian.Uint32(e.shdr(data, i)[shdrName:])
		if uint64(nameOff) >= uint64(len(e.shstrtab)) {
			continue
		}
		rest := e.shstrtab[nameOff:]
		if len(rest) > len(name) && string(rest[:len(name)]) == name && rest[len(name)] == 0 {
			return i
		}
	}
	return -1
}

// reservePIPSection returns the file with a zero-filled `.peios.sig`
// section in place and the offset of its contents. If the build already
// reserved one (via a linker script, say) it must be SHT_PROGBITS of
// exactly 3310 bytes and is zeroed; otherwise the section is appended.
//
// Appending never moves existing bytes: the section contents, a new
// section-name string table, and a new section header table are added
// at the end of the file, and only e_shoff and e_shnum in the ELF header
// change. The old string table and header table remain as dead bytes.
// Nothing loadable is touched, so program headers are unaffected.
func reservePIPSection(data []byte) ([]byte, uint64, error) {
	elf, err := parseELF64(data)
	if err != nil {
		return nil, 0, err
	}
	le := binary.LittleEndian
	if idx := elf.findSection(data, pipSectionName); idx >= 0 {
		hdr := elf.shdr(data, idx)
		if typ := le.Uint32(hdr[shdrType:]); typ != shtProgbits {
			return nil, 0, fmt.Errorf("existing %s section has type %d, want SHT_PROGBITS", pipSectionName, typ)
		}
		off, size := sectionRange(data, elf.shoff, idx)
		if size != pipSigSize {
			return nil, 0, fmt.Errorf("existing %s section is %d bytes, want %d", pipSectionName, size, pipSigSize)
		}
		if off+size < off || off+size > uint64(len(data)) {
			return nil, 0, fmt.Errorf("existing %s section lies outside the file", pipSectionName)
		}
		out := append([]byte(nil), data...)
		clear(out[off : off+size])
		return out, off, nil
	}

	out := append([]byte(nil), data...)
	pad := func(align int) {
		for len(out)%align != 0 {
			out = append(out, 0)
		}
	}
	// 1. Section contents.
	pad(8)
	sigOff := uint64(len(out))
	out = append(out, make([]byte, pipSigSize)...)
	// 2. New string table: the old one plus our name.
	strtab := append(append([]byte(nil), elf.shstrtab...), pipSectionCStr...)
	nameOff := uint32(len(elf.shstrtab))
	strOff := uint64(len(out))
	out = append(out, strtab...)
	// 3. New section header table: old entries, retargeted string table,
	//    and the new section last.
	pad(8)
	newShoff := uint64(len(out))
	oldTable := data[elf.shoff : elf.shoff+uint64(elf.shnum)*elfShdrSize]
	table := append([]byte(nil), oldTable...)
	strHdr := table[elf.shstrndx*elfShdrSize:]
	le.PutUint64(strHdr[shdrOffset:], strOff)
	le.PutUint64(strHdr[shdrSize:], uint64(len(strtab)))
	sigHdr := make([]byte, elfShdrSize)
	le.PutUint32(sigHdr[shdrName:], nameOff)
	le.PutUint32(sigHdr[shdrType:], shtProgbits)
	le.PutUint64(sigHdr[shdrOffset:], sigOff)
	le.PutUint64(sigHdr[shdrSize:], pipSigSize)
	le.PutUint64(sigHdr[shdrAddralign:], 1)
	table = append(table, sigHdr...)
	out = append(out, table...)
	// 4. Point the header at the new table.
	le.PutUint64(out[ehdrShoff:], newShoff)
	le.PutUint16(out[ehdrShnum:], uint16(elf.shnum+1))
	return out, sigOff, nil
}

// verifyPIPFile checks a finished file the way the kernel will: it
// locates the `.peios.sig` section following the spec's lookup rules,
// re-derives the content hash with the section contents zeroed, and
// verifies the blob with pub. It is the signer's self-check and shares
// no state with reservePIPSection beyond the bytes on disk.
func verifyPIPFile(data []byte, pub *mldsa65.PublicKey) error {
	elf, err := parseELF64(data)
	if err != nil {
		return err
	}
	idx := elf.findSection(data, pipSectionName)
	if idx < 0 {
		return fmt.Errorf("no %s section", pipSectionName)
	}
	le := binary.LittleEndian
	if typ := le.Uint32(elf.shdr(data, idx)[shdrType:]); typ != shtProgbits {
		return fmt.Errorf("%s section has type %d, want SHT_PROGBITS", pipSectionName, typ)
	}
	off, size := sectionRange(data, elf.shoff, idx)
	if size != pipSigSize {
		return fmt.Errorf("%s section is %d bytes, want %d", pipSectionName, size, pipSigSize)
	}
	if off+size < off || off+size > uint64(len(data)) {
		return fmt.Errorf("%s section lies outside the file", pipSectionName)
	}
	blob := data[off : off+size]
	if blob[0] != pipSigVersion {
		return fmt.Errorf("signature version byte is 0x%02x, want 0x%02x", blob[0], pipSigVersion)
	}
	h := sha256.New()
	h.Write(data[:off])
	h.Write(make([]byte, size))
	h.Write(data[off+size:])
	if !mldsa65.Verify(pub, h.Sum(nil), nil, blob[1:]) {
		return fmt.Errorf("ML-DSA-65 signature does not verify")
	}
	return nil
}

// isELF reports whether data begins with the ELF magic — the whole
// test that decides between the section and sidecar placements.
func isELF(data []byte) bool {
	return len(data) >= 4 && string(data[:4]) == "\x7fELF"
}

// signPIPDetached signs one non-ELF file: the blob over SHA-256 of the
// entire file is written to `<path>.peios.sig`, through a temporary
// file and rename so a crash leaves no truncated sidecar. The target is
// not touched. Like signPIPFile it re-verifies through the independent
// verifier before returning.
func signPIPDetached(path string, key *pipSigningKey) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if isELF(data) {
		return fmt.Errorf("%s is an ELF file; it carries its signature in a section, not a sidecar", path)
	}
	blob, err := key.signBlob(sha256.Sum256(data))
	if err != nil {
		return err
	}
	if err := verifyPIPDetached(data, blob, key.pub); err != nil {
		return fmt.Errorf("post-sign verification failed: %v", err)
	}
	sidecar := path + pipSidecarSuffix
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(sidecar)+".pipsign-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, sidecar); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// verifyPIPDetached checks a sidecar blob against the file bytes it
// claims to sign, exactly as the kernel does for the xattr placement.
func verifyPIPDetached(data, blob []byte, pub *mldsa65.PublicKey) error {
	if len(blob) != pipSigSize {
		return fmt.Errorf("signature blob is %d bytes, want %d", len(blob), pipSigSize)
	}
	if blob[0] != pipSigVersion {
		return fmt.Errorf("signature version byte is 0x%02x, want 0x%02x", blob[0], pipSigVersion)
	}
	hash := sha256.Sum256(data)
	if !mldsa65.Verify(pub, hash[:], nil, blob[1:]) {
		return fmt.Errorf("ML-DSA-65 signature does not verify")
	}
	return nil
}

// pipSignTarget runs a build target's `sign.pip` table over its staged
// output: every pattern is expanded relative to stage, each match is
// signed with the key its keyring entry names, and one `sign` event is
// emitted per file. Every failure is fatal — a signing setup that
// degrades quietly is exactly what the spec warns produces a silent,
// total loss of protection.
func pipSignTarget(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, target TargetConfig, stage, member, version string) error {
	rules := target.Sign[signKindPIP]
	if len(rules) == 0 {
		return nil
	}
	keyrings, err := resolveKeyrings(ctx.Inv, recipe.Root, workspace)
	if err != nil {
		return err
	}
	label := string(target.Kind) + "." + target.Name + ".sign." + signKindPIP
	keys := map[string]*pipSigningKey{}
	// A file matched by several patterns is signed once, by the first
	// pattern in sorted order that names it; a second pattern naming a
	// different key for the same file is an error rather than a race.
	signedBy := map[string]string{}
	for _, pattern := range sortedKeys(rules) {
		entry := rules[pattern]
		key, ok := keys[entry]
		if !ok {
			value := keyrings[envNameFromKeyring(entry)]
			if value == "" {
				return diag("signing_key", "%s: keyring entry %s is not configured (pass --keyring=<file> or --keyring.%s=<path>)", label, entry, entry)
			}
			path, err := absPath(ctx.Inv.Cwd, value)
			if err != nil {
				return wrapDiag("signing_key", value, err)
			}
			key, err = loadPIPSigningKey(path)
			if err != nil {
				return diagAt("signing_key", path, "%s: load ML-DSA-65 key %s: %v", label, entry, err)
			}
			keys[entry] = key
		}
		matches, err := doublestar.Glob(os.DirFS(stage), filepath.ToSlash(pattern), doublestar.WithNoFollow(), doublestar.WithFilesOnly())
		if err != nil {
			return diag("invalid_sign_pattern", "%s: pattern %q: %v", label, pattern, err)
		}
		if len(matches) == 0 {
			return diag("sign_target_missing", "%s: pattern %q matched nothing in %s", label, pattern, stage)
		}
		sort.Strings(matches)
		for _, rel := range matches {
			// A broad pattern (`usr/lib/firmware/**`) sweeps up the
			// sidecars this very loop writes; a signature is never a
			// signing target.
			if strings.HasSuffix(rel, pipSidecarSuffix) {
				continue
			}
			// A symlink is signed through its target, never itself: the
			// kernel reads the target's attribute, and peipkg rejects a
			// sidecar whose target is a link. Firmware trees are full of
			// version-alias links, so silently skipping is the useful
			// behaviour, not an error.
			if st, err := os.Lstat(filepath.Join(stage, filepath.FromSlash(rel))); err != nil {
				return diagAt("sign_failed", rel, "%s: %v", label, err)
			} else if st.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if prev, done := signedBy[rel]; done {
				if prev != entry {
					return diag("sign_conflict", "%s: %s is matched by patterns naming different keys (%s and %s)", label, rel, prev, entry)
				}
				continue
			}
			path := filepath.Join(stage, filepath.FromSlash(rel))
			placement, err := pipSignPath(path, key)
			if err != nil {
				return diagAt("sign_failed", path, "%s: %v", label, err)
			}
			signedBy[rel] = entry
			ctx.Renderer.Event(Event{Type: "sign", Member: member, Target: target.Name, Version: version, Path: path,
				Message: fmt.Sprintf("pip-signed (%s) with key %s (%s)", placement, entry, key.fingerprint)})
		}
	}
	return nil
}

// pipSignPath signs one file in whichever placement its contents
// allow, returning a short name for the placement used in the event.
func pipSignPath(path string, key *pipSigningKey) (string, error) {
	head := make([]byte, 4)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	n, err := io.ReadFull(f, head)
	f.Close()
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	if isELF(head[:n]) {
		return "section", signPIPFile(path, key)
	}
	return "sidecar", signPIPDetached(path, key)
}
