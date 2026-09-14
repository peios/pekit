package pekit

import (
	"bytes"
	"encoding/binary"
	"github.com/klauspost/compress/zstd"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCoordinatorModuleSigning(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl unavailable")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("C compiler unavailable")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "key.pem")
	cert := filepath.Join(dir, "cert.pem")
	cmd := exec.Command("openssl", "req", "-new", "-x509", "-newkey", "ML-DSA-65", "-nodes", "-keyout", key, "-out", cert, "-subj", "/CN=Pekit module signing test", "-days", "1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate test key: %v: %s", err, output)
	}
	private, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, append(private, public...), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "module.c")
	module := filepath.Join(dir, "module.ko")
	writeFile(t, source, "int example(void) { return 42; }\n")
	if output, err := exec.Command("cc", "-c", source, "-o", module).CombinedOutput(); err != nil {
		t.Fatalf("compile: %v %s", err, output)
	}
	original, err := os.ReadFile(module)
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := filepath.Join(dir, "module.ko.zst")
	if err := os.WriteFile(compressed, encoder.EncodeAll(original, nil), 0644); err != nil {
		t.Fatal(err)
	}
	encoder.Close()
	for _, path := range []string{module, compressed} {
		for attempt := 0; attempt < 2; attempt++ {
			if err := signModuleFile(path, key); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if path == compressed {
				decoder, err := zstd.NewReader(nil)
				if err != nil {
					t.Fatal(err)
				}
				data, err = decoder.DecodeAll(data, nil)
				decoder.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.HasSuffix(data, []byte(moduleSignatureMagic)) {
				t.Fatal("missing kernel signature marker")
			}
			offset := len(data) - len(moduleSignatureMagic) - 12
			size := int(binary.BigEndian.Uint32(data[offset+8 : offset+12]))
			if data[offset+2] != 2 || !bytes.Equal(data[:offset-size], original) {
				t.Fatal("module data changed or signatures stacked")
			}
			sig := filepath.Join(dir, "verify.der")
			input := filepath.Join(dir, "verify.ko")
			if err := os.WriteFile(sig, data[offset-size:offset], 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(input, original, 0600); err != nil {
				t.Fatal(err)
			}
			if err := coordinatorOpenSSL("cms", "-verify", "-binary", "-noverify", "-nointern", "-inform", "DER", "-in", sig, "-content", input, "-certfile", cert, "-out", os.DevNull); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestSigningPathRejectsLiteralSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	outside := filepath.Join(dir, "outside")
	writeFile(t, filepath.Join(outside, "firmware"), "outside")
	if err := os.Mkdir(stage, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stage, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := containedSigningPath(stage, "link/firmware"); err == nil {
		t.Fatal("literal symlink component escaped signing stage")
	}
}
