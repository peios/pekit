package pekit

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/klauspost/compress/zstd"
)

const moduleSignatureMagic = "~Module signature appended~\n"

// Module signatures use the kernel PKCS#7 trailer. OpenSSL is a trusted
// coordinator tool; scripts/sign-file from the untrusted build is never run
// with a private key. The configured PEM contains the key and certificate.
func moduleSignTarget(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, target TargetConfig, stage, member, version string) error {
	rules := target.Sign["module"]
	if len(rules) == 0 {
		return nil
	}
	values, err := resolveKeyrings(ctx.Inv, recipe.Root, workspace)
	if err != nil {
		return err
	}
	signed := map[string]string{}
	for _, pattern := range sortedKeys(rules) {
		entry := rules[pattern]
		key, err := absPath(ctx.Inv.Cwd, values[envNameFromKeyring(entry)])
		if err != nil {
			return err
		}
		if values[envNameFromKeyring(entry)] == "" {
			return diag("signing_key", "missing module signing key %s", entry)
		}
		matches, err := doublestar.Glob(os.DirFS(stage), pattern, doublestar.WithNoFollow(), doublestar.WithFilesOnly())
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			return diag("sign_target_missing", "module pattern %s matched nothing", pattern)
		}
		for _, rel := range matches {
			st, err := os.Lstat(filepath.Join(stage, rel))
			if err != nil {
				return err
			}
			if st.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if previous, ok := signed[rel]; ok {
				if previous != entry {
					return diag("sign_conflict", "module %s matches different keys", rel)
				}
				continue
			}
			path, err := containedSigningPath(stage, rel)
			if err != nil {
				return err
			}
			if err := signModuleFile(path, key); err != nil {
				return diagAt("sign_failed", path, "%v", err)
			}
			signed[rel] = entry
			ctx.Renderer.Event(Event{Type: "sign", Member: member, Target: target.Name, Version: version, Path: path, Message: "module signed with key " + entry})
		}
	}
	return nil
}

func signModuleFile(path, key string) error {
	if _, err := loadPIPSigningKey(key); err != nil {
		return fmt.Errorf("module signing requires an ML-DSA-65 private key followed by its certificate: %w", err)
	}
	compressed := strings.HasSuffix(path, ".ko.zst")
	if !compressed && !strings.HasSuffix(path, ".ko") {
		return fmt.Errorf("module signing requires .ko or .ko.zst")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if compressed {
		reader, err := zstd.NewReader(bytes.NewReader(data), zstd.WithDecoderMaxMemory(256<<20))
		if err != nil {
			return err
		}
		data, err = io.ReadAll(io.LimitReader(reader, 256<<20))
		reader.Close()
		if err != nil {
			return err
		}
	}
	if len(data) >= 256<<20 {
		return fmt.Errorf("module exceeds 256 MiB signing limit")
	}
	if bytes.HasSuffix(data, []byte(moduleSignatureMagic)) {
		offset := len(data) - len(moduleSignatureMagic) - 12
		if offset < 0 {
			return fmt.Errorf("invalid existing module signature")
		}
		size := int(binary.BigEndian.Uint32(data[offset+8 : offset+12]))
		if size > offset {
			return fmt.Errorf("invalid existing module signature size")
		}
		data = data[:offset-size]
	}
	module, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer module.Close()
	if module.Type != elf.ET_REL {
		return fmt.Errorf("module signing requires a relocatable ELF object")
	}
	work, err := os.MkdirTemp("", "pekit-modsig-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	input := filepath.Join(work, "module")
	sig := filepath.Join(work, "signature.der")
	if err := os.WriteFile(input, data, 0600); err != nil {
		return err
	}
	// OpenSSL 3.5 needs authenticated attributes for ML-DSA, matching the
	// kernel's PKCS7_WAIVE_AUTHATTRS_REJECTION_FOR_MLDSA configuration.
	if err := coordinatorOpenSSL("cms", "-sign", "-binary", "-nocerts", "-nosmimecap", "-md", "sha512", "-in", input, "-signer", key, "-inkey", key, "-outform", "DER", "-out", sig); err != nil {
		return err
	}
	if err := coordinatorOpenSSL("cms", "-verify", "-binary", "-noverify", "-nointern", "-inform", "DER", "-in", sig, "-content", input, "-certfile", key, "-out", os.DevNull); err != nil {
		return fmt.Errorf("post-sign verification: %w", err)
	}
	signature, err := os.ReadFile(sig)
	if err != nil {
		return err
	}
	trailer := make([]byte, 12)
	trailer[2] = 2
	binary.BigEndian.PutUint32(trailer[8:], uint32(len(signature)))
	result := append(append(append(data, signature...), trailer...), []byte(moduleSignatureMagic)...)
	if compressed {
		writer, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(19)))
		if err != nil {
			return err
		}
		result = writer.EncodeAll(result, nil)
		writer.Close()
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".modsig-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(result); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(st.Mode().Perm()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func coordinatorOpenSSL(args ...string) error {
	cmd := exec.Command("openssl", args...)
	// Do not let build-provided OpenSSL configuration or provider paths select
	// code in the signing process. No worker environment reaches this function.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "OPENSSL_CONF=/dev/null"}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("openssl %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return nil
}
