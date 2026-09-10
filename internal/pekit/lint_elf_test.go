package pekit

import (
	"debug/elf"
	"encoding/binary"
	"testing"
)

func TestELF64GNUPropertyNoteUsesFourByteNoteFraming(t *testing.T) {
	bo := binary.LittleEndian
	desc := make([]byte, 16)
	bo.PutUint32(desc[0:4], gnuPropertyX86Feature1And)
	bo.PutUint32(desc[4:8], 4)
	bo.PutUint32(desc[8:12], x86Feature1IBT|x86Feature1SHSTK)

	note := make([]byte, 12+4+len(desc))
	bo.PutUint32(note[0:4], 4)
	bo.PutUint32(note[4:8], uint32(len(desc)))
	bo.PutUint32(note[8:12], ntGnuPropertyType)
	copy(note[12:16], "GNU\x00")
	copy(note[16:], desc)

	seen := false
	forEachNote(note, 4, bo, func(name string, typ uint32, got []byte) {
		if name == "GNU" && typ == ntGnuPropertyType {
			seen = propertyHasCET(got, 8, bo)
		}
	})
	if !seen {
		t.Fatal("ELF64 GNU property note did not report IBT+SHSTK")
	}

	// The old parser incorrectly used the eight-byte property-entry alignment
	// for the outer note too, skipped four descriptor bytes, and missed CET.
	oldSeen := false
	forEachNote(note, 8, bo, func(name string, typ uint32, got []byte) {
		if name == "GNU" && typ == ntGnuPropertyType {
			oldSeen = propertyHasCET(got, 8, bo)
		}
	})
	if oldSeen {
		t.Fatal("regression fixture does not distinguish the old outer-note alignment")
	}
}

func TestEmbeddedDebugSectionExcludesGDBAutoLoadMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{name: ".debug_info", want: true},
		{name: ".debug_line", want: true},
		{name: ".debug_gdb_scripts", want: false},
		{name: ".gnu_debuglink", want: false},
	} {
		if got := isEmbeddedDebugSection(tc.name); got != tc.want {
			t.Errorf("isEmbeddedDebugSection(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDynamicRelaSectionAcceptsGoLinkerName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		type_ elf.SectionType
		flags elf.SectionFlag
		class elf.Class
		want  bool
	}{
		{name: ".rela.dyn", type_: elf.SHT_RELA, flags: elf.SHF_ALLOC, class: elf.ELFCLASS64, want: true},
		{name: ".rela", type_: elf.SHT_RELA, flags: elf.SHF_ALLOC, class: elf.ELFCLASS64, want: true},
		{name: ".rela.debug_info", type_: elf.SHT_RELA, class: elf.ELFCLASS64, want: false},
		{name: ".rel.dyn", type_: elf.SHT_REL, flags: elf.SHF_ALLOC, class: elf.ELFCLASS64, want: false},
		{name: ".rela.dyn", type_: elf.SHT_RELA, flags: elf.SHF_ALLOC, class: elf.ELFCLASS32, want: false},
	} {
		s := &elf.Section{SectionHeader: elf.SectionHeader{Name: tc.name, Type: tc.type_, Flags: tc.flags}}
		if got := isDynamicRelaSection(s, tc.class); got != tc.want {
			t.Errorf("isDynamicRelaSection(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
