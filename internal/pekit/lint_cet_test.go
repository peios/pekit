package pekit

import (
	"bytes"
	"encoding/binary"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func cetRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s unavailable", name)
	}
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func TestCETCodeForcedNotesAndSplitDebug(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("x86 fixture")
	}
	for _, packed := range []bool{false, true} {
		for _, protection := range []string{"full", "none"} {
			t.Run(protection+map[bool]string{true: "-relr", false: "-rela"}[packed], func(t *testing.T) {
				dir := t.TempDir()
				src, bin := filepath.Join(dir, "test.c"), filepath.Join(dir, "test.so")
				writeFile(t, src, `
static __attribute__((noinline,nocf_check)) int direct_only(void) { return 3; }
static int callback_a(void) { return 1; }
static int callback_b(void) { return 2; }
static int (*volatile callbacks[])(void) = {callback_a, callback_b};
int exported(void) { return callbacks[0]() + callbacks[1]() + direct_only(); }
`)
				args := []string{"-g", "-O2", "-shared", "-fPIC", "-fcf-protection=" + protection, "-Wl,--build-id,-z,ibt,-z,shstk", src, "-o", bin}
				if packed {
					args = append(args, "-Wl,-z,pack-relative-relocs")
				}
				cetRun(t, "cc", args...)
				info, err := analyzeELF(bin)
				if err != nil || info == nil || !info.CET {
					t.Fatalf("fixture must advertise CET: %v, %v", info, err)
				}
				if packed && !info.Relr {
					t.Fatal("fixture did not produce RELR")
				}
				bad, checked, err := checkCETCode(bin, "", info)
				if err != nil || checked < 3 {
					t.Fatalf("checked=%d, bad=%v, err=%v", checked, bad, err)
				}
				if protection == "full" && len(bad) != 0 {
					t.Fatalf("protected payload (with an unprotected direct-only function) failed: %v", bad)
				}
				if protection == "none" {
					names := map[string]bool{}
					for _, target := range bad {
						names[target.Name] = true
					}
					for _, want := range []string{"exported", "callback_a", "callback_b"} {
						if !names[want] {
							t.Errorf("forced notes hid unprotected %s: %v", want, bad)
						}
					}
					if names["direct_only"] {
						t.Error("direct-only function was classified as an indirect target")
					}
				}
				debug := bin + ".debug"
				cetRun(t, "objcopy", "--only-keep-debug", bin, debug)
				cetRun(t, "strip", "--strip-all", bin)
				got, count, err := checkCETCode(bin, debug, info)
				if err != nil || count != checked || !reflect.DeepEqual(got, bad) {
					t.Fatalf("stripping changed result: %v / %d / %v vs %v / %d", got, count, err, bad, checked)
				}
				if _, _, err := checkCETCode(bin, "", info); err == nil {
					t.Fatal("missing local symbols silently reduced validation coverage")
				}
				// Exercise the public lint path, including cross-package debug
				// lookup. A note-only implementation would accept the bad case.
				writeFile(t, filepath.Join(dir, "pekit.toml"), "out_dir = 'out'\n")
				writeFile(t, filepath.Join(dir, "lint.pekit.toml"), "[elf]\ncet = true\n")
				writeFile(t, filepath.Join(dir, "main.package.pekit.toml"), `format = "tar"
[package]
name = "test"
version = "1.0"
[files]
"@recipe:test.so" = "usr/lib/test.so"
`)
				writeFile(t, filepath.Join(dir, "debug.package.pekit.toml"), `format = "tar"
[package]
name = "test-debug"
version = "1.0"
[files]
"@recipe:test.so.debug" = "usr/lib/debug/.build-id/`+info.BuildID[:2]+`/`+info.BuildID[2:]+`.debug"
`)
				events, lintErr := lintEvents(t, dir, "lint", "--version", "1.0")
				if protection == "full" && lintErr != nil || protection == "none" && (diagCode(lintErr) != "lint_failed" || lintRules(events["lint"])["elf.cet"] == 0) {
					t.Fatalf("payload lint did not enforce code evidence: %v, %v", lintErr, events)
				}
				cetRun(t, "objcopy", "--remove-section=.note.gnu.build-id", debug)
				if _, _, err := checkCETCode(bin, debug, info); err == nil || !strings.Contains(err.Error(), "does not match") {
					t.Fatalf("mismatched debug file accepted: %v", err)
				}
			})
		}
	}
}

func TestCETCodeX8632REL(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "386" {
		t.Skip("x86 fixture")
	}
	dir := t.TempDir()
	src, bin := filepath.Join(dir, "test.s"), filepath.Join(dir, "test.so")
	writeFile(t, src, `
.text
.globl exported
.type exported,@function
exported:
endbr32
ret
.type callback,@function
callback:
ret
.space 3
.data
.long callback
`)
	// No 32-bit libc or multilib runtime is needed for this fixture.
	cetRun(t, "cc", "-m32", "-nostdlib", "-shared", "-Wl,--build-id,-z,ibt,-z,shstk", src, "-o", bin)
	info, _ := analyzeELF(bin)
	bad, checked, err := checkCETCode(bin, "", info)
	if err != nil || checked != 2 || len(bad) != 1 || bad[0].Name != "callback" {
		t.Fatalf("ELF32 REL: checked=%d, bad=%v, err=%v", checked, bad, err)
	}
}

func TestCETWalkRELR(t *testing.T) {
	for _, width := range []int{4, 8} {
		for _, tc := range []struct {
			name  string
			words []uint64
			want  []uint64
			fail  bool
		}{
			{"bitmap", []uint64{0x1000, 1 | 2 | 8, 0x2000}, []uint64{0x1000, 0x1000 + uint64(width), 0x1000 + 3*uint64(width), 0x2000}, false},
			{"unanchored", []uint64{3}, nil, true},
			{"overflow", []uint64{^uint64(0) - 1}, nil, true},
		} {
			t.Run(tc.name+string(rune('0'+width)), func(t *testing.T) {
				var data bytes.Buffer
				for _, w := range tc.words {
					if width == 4 {
						binary.Write(&data, binary.LittleEndian, uint32(w))
					} else {
						binary.Write(&data, binary.LittleEndian, w)
					}
				}
				word := func(b []byte) uint64 {
					if width == 4 {
						return uint64(binary.LittleEndian.Uint32(b))
					}
					return binary.LittleEndian.Uint64(b)
				}
				var got []uint64
				err := cetWalkRELR(&data, uint64(data.Len()), width, word, func(a uint64) error { got = append(got, a); return nil })
				if (err != nil) != tc.fail || !tc.fail && !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("got %x, err=%v, want %x (fail=%v)", got, err, tc.want, tc.fail)
				}
			})
		}
	}
}

func TestCETExportedProcessEntryIsNotACallback(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("x86 fixture")
	}
	dir := t.TempDir()
	src, bin := filepath.Join(dir, "entry.c"), filepath.Join(dir, "entry")
	writeFile(t, src, `int main(void) { return 0; }`)
	// Host CRT entry code need not have ENDBR even with -rdynamic. main is
	// nevertheless a callback passed to libc, and must still be checked.
	cetRun(t, "cc", "-g", "-rdynamic", "-fcf-protection=full", "-Wl,-z,ibt,-z,shstk", src, "-o", bin)
	info, _ := analyzeELF(bin)
	bad, checked, err := checkCETCode(bin, "", info)
	if err != nil || checked == 0 || len(bad) != 0 {
		t.Fatalf("protected callback: %v, %d, %v", bad, checked, err)
	}
	cetRun(t, "cc", "-g", "-rdynamic", "-fcf-protection=none", "-Wl,-z,ibt,-z,shstk", src, "-o", bin)
	info, _ = analyzeELF(bin)
	bad, _, err = checkCETCode(bin, "", info)
	found := false
	for _, b := range bad {
		if b.Name == "main" {
			found = true
		}
		if b.Name == "_start" {
			t.Error("process entry classified as callback")
		}
	}
	if err != nil || !found {
		t.Fatalf("unprotected main callback hidden: %v, %v", bad, err)
	}
}
