package pekit

import (
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"sort"
)

type cetTarget struct {
	Address uint64
	Name    string
	Reason  string
}

// checkCETCode checks observable indirect-call entry points, not every function.
// Direct-only functions and intra-function jump tables need not have ENDBR.
// This is bounded static evidence, not a proof of all runtime targets or SHSTK
// compatibility. In particular, generated code and computed pointers are outside
// its scope. Bytes always come from the payload, never the separate debug file.
func checkCETCode(filename, debugFile string, info *elfInfo) ([]cetTarget, int, error) {
	f, err := elf.Open(filename)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	syms, err := f.Symbols()
	if errors.Is(err, elf.ErrNoSymbols) && debugFile != "" {
		dbgInfo, e := analyzeELF(debugFile)
		if e != nil || dbgInfo == nil || info.BuildID == "" || dbgInfo.BuildID != info.BuildID || dbgInfo.Machine != info.Machine || dbgInfo.Class != info.Class {
			return nil, 0, fmt.Errorf("separate debug file does not match payload build ID, machine and class")
		}
		dbg, e := elf.Open(debugFile)
		if e != nil {
			return nil, 0, e
		}
		syms, err = dbg.Symbols()
		dbg.Close()
	}
	if err != nil {
		return nil, 0, fmt.Errorf("cannot inspect local callback targets without a symbol table or matching build-ID debug file: %w", err)
	}
	dyn, err := f.DynamicSymbols()
	if err != nil && !errors.Is(err, elf.ErrNoSymbols) {
		return nil, 0, err
	}
	functions := make(map[uint64]string)
	targets := make(map[uint64]cetTarget)
	isFunction := func(s elf.Symbol) bool {
		t := elf.ST_TYPE(s.Info)
		return s.Section != elf.SHN_UNDEF && (t == elf.STT_FUNC || t == elf.STT_GNU_IFUNC)
	}
	for _, s := range append(syms, dyn...) {
		if isFunction(s) {
			if old, ok := functions[s.Value]; !ok || s.Name < old {
				functions[s.Value] = s.Name
			}
		}
	}
	add := func(addr uint64, why string) {
		if name, ok := functions[addr]; ok {
			if _, exists := targets[addr]; !exists {
				targets[addr] = cetTarget{addr, name, why}
			}
		}
	}
	libcStart := false
	for _, s := range dyn {
		libcStart = libcStart || (s.Name == "__libc_start_main" && s.Section == elf.SHN_UNDEF)
		vis := elf.ST_VISIBILITY(s.Other)
		bind := elf.ST_BIND(s.Info)
		if isFunction(s) && bind != elf.STB_LOCAL && (vis == elf.STV_DEFAULT || vis == elf.STV_PROTECTED) {
			// -rdynamic may export the process entry point. The loader jumps
			// there without the C call ABI; it is not a callable export. An
			// actual pointer relocation to it is still checked below.
			if s.Value != f.Entry || !(info.Interp || info.PIE || info.Type == elf.ET_EXEC) {
				add(s.Value, "exported function")
			}
		}
	}
	if libcStart {
		for _, s := range syms {
			if s.Name == "main" && isFunction(s) {
				add(s.Value, "main callback passed to libc")
			}
		}
	}
	if err := cetRelocationTargets(f, dyn, add); err != nil {
		return nil, 0, err
	}
	if len(targets) == 0 {
		return nil, 0, fmt.Errorf("no supported indirect-call targets found; CET code compatibility could not be assessed")
	}
	addresses := make([]uint64, 0, len(targets))
	for addr := range targets {
		addresses = append(addresses, addr)
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i] < addresses[j] })
	endbr := []byte{0xf3, 0x0f, 0x1e, 0xfa}
	if f.Machine == elf.EM_386 {
		endbr[3] = 0xfb
	}
	var bad []cetTarget
	for _, addr := range addresses {
		code, err := cetAddressBytes(f, addr, 4, true)
		if err != nil {
			return nil, len(targets), fmt.Errorf("target %#x: %w", addr, err)
		}
		if !bytes.Equal(code, endbr) {
			bad = append(bad, targets[addr])
		}
	}
	return bad, len(targets), nil
}

func cetAddressBytes(f *elf.File, addr uint64, size int, executable bool) ([]byte, error) {
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || (executable && p.Flags&elf.PF_X == 0) || addr < p.Vaddr {
			continue
		}
		off := addr - p.Vaddr
		if off > p.Filesz || uint64(size) > p.Filesz-off {
			continue
		}
		buf := make([]byte, size)
		_, err := p.ReadAt(buf, int64(off))
		return buf, err
	}
	return nil, fmt.Errorf("address is outside file-backed load segments")
}

// Only pointers in loaded data count. Relocations in instructions may describe
// direct calls; unwind tables describe code ranges rather than call targets.
func cetDataPointer(f *elf.File, addr uint64) bool {
	for _, s := range f.Sections {
		if addr >= s.Addr && addr-s.Addr < s.Size && s.Flags&elf.SHF_ALLOC != 0 && s.Flags&elf.SHF_EXECINSTR == 0 {
			return s.Name != ".eh_frame" && s.Name != ".eh_frame_hdr" && s.Name != ".gcc_except_table"
		}
	}
	return false
}

func cetRelocationTargets(f *elf.File, dyn []elf.Symbol, add func(uint64, string)) error {
	width := 8
	if f.Class == elf.ELFCLASS32 {
		width = 4
	}
	word := func(b []byte) uint64 {
		if width == 4 {
			return uint64(f.ByteOrder.Uint32(b))
		}
		return f.ByteOrder.Uint64(b)
	}
	readPointer := func(addr uint64) (uint64, error) {
		b, err := cetAddressBytes(f, addr, width, false)
		if err != nil {
			return 0, err
		}
		return word(b), nil
	}
	for _, s := range f.Sections {
		if s.Flags&elf.SHF_ALLOC == 0 {
			continue
		}
		if s.Type == elf.SectionType(19) { // SHT_RELR (not yet in debug/elf)
			if err := cetWalkRELR(s.Open(), s.Size, width, word, func(addr uint64) error {
				if !cetDataPointer(f, addr) {
					return nil
				}
				target, err := readPointer(addr)
				if err == nil {
					add(target, "relocated function pointer (RELR)")
				}
				return err
			}); err != nil {
				return fmt.Errorf("%s: %w", s.Name, err)
			}
			continue
		}
		if s.Type != elf.SHT_REL && s.Type != elf.SHT_RELA {
			continue
		}
		entrySize := width * 2
		if s.Type == elf.SHT_RELA {
			entrySize += width
		}
		if s.Size%uint64(entrySize) != 0 {
			return fmt.Errorf("truncated %s relocation table", s.Name)
		}
		b := make([]byte, entrySize)
		r := s.Open()
		for n := uint64(0); n < s.Size; n += uint64(entrySize) {
			if _, err := io.ReadFull(r, b); err != nil {
				return err
			}
			addr, ri := word(b), word(b[width:])
			typ, sym := uint32(ri), ri>>32
			if width == 4 {
				typ, sym = uint32(ri&255), ri>>8
			}
			// Both x86 ABIs use 8 for RELATIVE and 1 for absolute pointers.
			irelative := uint32(37)
			if f.Machine == elf.EM_386 {
				irelative = 42
			}
			if typ != 8 && typ != 1 && typ != irelative || !cetDataPointer(f, addr) {
				continue
			}
			var target uint64
			if s.Type == elf.SHT_RELA {
				target = word(b[width*2:])
			} else {
				var err error
				target, err = readPointer(addr)
				if err != nil {
					return err
				}
			}
			if typ == 1 {
				// DynamicSymbols omits ELF's leading null symbol. Do not apply
				// its indexes to a relocation section linked to another table.
				if int(s.Link) >= len(f.Sections) || f.Sections[s.Link].Type != elf.SHT_DYNSYM {
					continue
				}
				if sym == 0 || sym > uint64(len(dyn)) {
					return fmt.Errorf("invalid dynamic symbol in %s", s.Name)
				}
				if dyn[sym-1].Section == elf.SHN_UNDEF {
					continue // external provider, checked in its own payload
				}
				target += dyn[sym-1].Value
			}
			add(target, "relocated function pointer")
		}
	}
	return nil
}

func cetWalkRELR(r io.Reader, size uint64, width int, word func([]byte) uint64, visit func(uint64) error) error {
	if size%uint64(width) != 0 {
		return fmt.Errorf("truncated RELR table")
	}
	b := make([]byte, width)
	var base uint64
	anchored := false
	advance := func(n uint64) error {
		max := ^uint64(0)
		if width == 4 {
			max = 1<<32 - 1
		}
		if base > max-n {
			return fmt.Errorf("RELR address overflow")
		}
		base += n
		return nil
	}
	for n := uint64(0); n < size; n += uint64(width) {
		if _, err := io.ReadFull(r, b); err != nil {
			return err
		}
		v := word(b)
		if v&1 == 0 {
			base, anchored = v, true
			if err := visit(base); err != nil {
				return err
			}
			if err := advance(uint64(width)); err != nil {
				return err
			}
		} else {
			if !anchored {
				return fmt.Errorf("RELR bitmap without an address")
			}
			for bit := 1; bit < width*8; bit++ {
				if v&(uint64(1)<<bit) != 0 {
					if err := visit(base); err != nil {
						return err
					}
				}
				if err := advance(uint64(width)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
