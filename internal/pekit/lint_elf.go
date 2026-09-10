package pekit

import (
	"bufio"
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"strings"
)

// elfInfo is what the [elf] rules need to know about one staged object,
// read once with debug/elf. Everything is a plain observation; the rules
// decide what counts as a finding.
type elfInfo struct {
	Type    elf.Type
	Machine elf.Machine
	Class   elf.Class

	Dynamic bool // has PT_DYNAMIC
	Interp  bool // has PT_INTERP (a program, not a library)

	HasGnuStack bool
	StackExec   bool
	Relro       bool
	BindNow     bool
	Relr        bool
	Textrel     bool
	PIE         bool // DF_1_PIE, set by the linker on -pie output
	// RelativeRelocs counts R_*_RELATIVE entries left in .rela.dyn — the
	// ones -z pack-relative-relocs would have moved into DT_RELR.
	RelativeRelocs int

	Rpath  string // DT_RPATH or DT_RUNPATH, whichever is present
	Soname string

	CET              bool // x86: GNU_PROPERTY_X86_FEATURE_1_AND has IBT and SHSTK
	HasDebugSections bool
	HasSymtab        bool
	BuildID          string
}

const (
	dtRelr                    = elf.DynTag(36)
	dfBindNow                 = 0x8
	dfTextrel                 = 0x4
	df1Now                    = 0x1
	df1PIE                    = 0x08000000
	ntGnuBuildID              = 3
	ntGnuPropertyType         = 5
	gnuPropertyX86Feature1And = 0xc0000002
	x86Feature1IBT            = 0x1
	x86Feature1SHSTK          = 0x2
)

// analyzeELF reads one file. It returns (nil, nil) for anything that is not
// an ELF object, so callers can hand it every staged file.
func analyzeELF(path string) (*elfInfo, error) {
	ef, err := elf.Open(path)
	if err != nil {
		return nil, nil
	}
	defer ef.Close()
	info := &elfInfo{Type: ef.Type, Machine: ef.Machine, Class: ef.Class}
	for _, p := range ef.Progs {
		switch p.Type {
		case elf.PT_DYNAMIC:
			info.Dynamic = true
		case elf.PT_INTERP:
			info.Interp = true
		case elf.PT_GNU_STACK:
			info.HasGnuStack = true
			info.StackExec = p.Flags&elf.PF_X != 0
		case elf.PT_GNU_RELRO:
			info.Relro = true
		}
	}
	if info.Dynamic {
		if vals, err := ef.DynValue(elf.DT_FLAGS); err == nil {
			for _, v := range vals {
				info.BindNow = info.BindNow || v&dfBindNow != 0
				info.Textrel = info.Textrel || v&dfTextrel != 0
			}
		}
		if vals, err := ef.DynValue(elf.DT_FLAGS_1); err == nil {
			for _, v := range vals {
				info.BindNow = info.BindNow || v&df1Now != 0
				info.PIE = info.PIE || v&df1PIE != 0
			}
		}
		if vals, err := ef.DynValue(elf.DT_BIND_NOW); err == nil && len(vals) > 0 {
			info.BindNow = true
		}
		if vals, err := ef.DynValue(elf.DT_TEXTREL); err == nil && len(vals) > 0 {
			info.Textrel = true
		}
		if vals, err := ef.DynValue(dtRelr); err == nil && len(vals) > 0 {
			info.Relr = true
		}
		for _, tag := range []elf.DynTag{elf.DT_RUNPATH, elf.DT_RPATH} {
			if vals, err := ef.DynString(tag); err == nil && len(vals) > 0 && vals[0] != "" {
				info.Rpath = vals[0]
			}
		}
		if vals, err := ef.DynString(elf.DT_SONAME); err == nil && len(vals) > 0 {
			info.Soname = vals[0]
		}
	}
	for _, s := range ef.Sections {
		switch {
		case s.Name == ".symtab":
			info.HasSymtab = true
		case strings.HasPrefix(s.Name, ".debug_"):
			info.HasDebugSections = true
		case s.Name == ".rela.dyn" && ef.Class == elf.ELFCLASS64:
			info.RelativeRelocs = countRelativeRelocs(ef, s)
		case s.Name == ".note.gnu.build-id":
			if data, err := s.Data(); err == nil {
				forEachNote(data, 4, ef.ByteOrder, func(name string, typ uint32, desc []byte) {
					if name == "GNU" && typ == ntGnuBuildID {
						info.BuildID = hex.EncodeToString(desc)
					}
				})
			}
		case s.Name == ".note.gnu.property":
			align := 4
			if ef.Class == elf.ELFCLASS64 {
				align = 8
			}
			if data, err := s.Data(); err == nil {
				forEachNote(data, align, ef.ByteOrder, func(name string, typ uint32, desc []byte) {
					if name == "GNU" && typ == ntGnuPropertyType {
						info.CET = info.CET || propertyHasCET(desc, align, ef.ByteOrder)
					}
				})
			}
		}
	}
	return info, nil
}

// relativeRelocType is R_<arch>_RELATIVE for the architectures whose value
// is known here; 0 means "do not count".
func relativeRelocType(m elf.Machine) uint32 {
	switch m {
	case elf.EM_X86_64:
		return 8
	case elf.EM_AARCH64:
		return 1027
	case elf.EM_RISCV:
		return 3
	case elf.EM_PPC64:
		return 22
	case elf.EM_S390:
		return 12
	case elf.EM_LOONGARCH:
		return 3
	}
	return 0
}

func countRelativeRelocs(ef *elf.File, s *elf.Section) int {
	want := relativeRelocType(ef.Machine)
	if want == 0 {
		return 0
	}
	r := s.Open()
	buf := make([]byte, 24)
	n := 0
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return n
		}
		info := ef.ByteOrder.Uint64(buf[8:16])
		if uint32(info) == want {
			n++
		}
	}
}

// forEachNote walks an ELF note section. align is the padding unit for the
// name and descriptor (4 for ordinary notes, 8 for .note.gnu.property in a
// 64-bit object).
func forEachNote(data []byte, align int, bo binary.ByteOrder, fn func(name string, typ uint32, desc []byte)) {
	pad := func(n int) int { return (n + align - 1) &^ (align - 1) }
	for len(data) >= 12 {
		namesz := int(bo.Uint32(data[0:4]))
		descsz := int(bo.Uint32(data[4:8]))
		typ := bo.Uint32(data[8:12])
		data = data[12:]
		if pad(namesz) > len(data) {
			return
		}
		name := strings.TrimRight(string(data[:namesz]), "\x00")
		data = data[pad(namesz):]
		if descsz > len(data) {
			return
		}
		fn(name, typ, data[:descsz])
		if pad(descsz) > len(data) {
			return
		}
		data = data[pad(descsz):]
	}
}

func propertyHasCET(desc []byte, align int, bo binary.ByteOrder) bool {
	pad := func(n int) int { return (n + align - 1) &^ (align - 1) }
	for len(desc) >= 8 {
		typ := bo.Uint32(desc[0:4])
		size := int(bo.Uint32(desc[4:8]))
		desc = desc[8:]
		if size > len(desc) {
			return false
		}
		if typ == gnuPropertyX86Feature1And && size >= 4 {
			v := bo.Uint32(desc[0:4])
			return v&x86Feature1IBT != 0 && v&x86Feature1SHSTK != 0
		}
		if pad(size) > len(desc) {
			return false
		}
		desc = desc[pad(size):]
	}
	return false
}

// fileContainsBytes streams a file looking for needle, keeping memory flat
// on multi-hundred-megabyte objects.
func fileContainsBytes(path string, needle []byte) bool {
	if len(needle) == 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	chunk := make([]byte, 1<<20)
	var carry []byte
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			window := append(carry, chunk[:n]...)
			if bytes.Contains(window, needle) {
				return true
			}
			keep := len(needle) - 1
			if keep > len(window) {
				keep = len(window)
			}
			carry = append([]byte(nil), window[len(window)-keep:]...)
		}
		if err != nil {
			return false
		}
	}
}
