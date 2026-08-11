package pekit

import "testing"

// shellExpandable renders user env values into the export prelude. It has to
// keep parameter expansion working while making it impossible for a value to
// break out of its own quoting — an unescaped quote or a trailing backslash
// would otherwise corrupt every export that follows it.
func TestShellExpandable(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"plain", "-O2", `"-O2"`},
		{"multi word", "-O2 -pipe", `"-O2 -pipe"`},
		{"expansion survives", "$CFLAGS -fPIC", `"$CFLAGS -fPIC"`},
		{"command substitution survives", "$(nproc)", `"$(nproc)"`},
		{"quote escaped", `a "b" c`, `"a \"b\" c"`},
		{"backslash escaped", `a\b`, `"a\\b"`},
		{"trailing backslash cannot escape the closing quote", `a\`, `"a\\"`},
		{"backtick escaped", "a`b`", "\"a\\`b\\`\""},
		{"empty", "", `""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellExpandable(tc.value); got != tc.want {
				t.Fatalf("shellExpandable(%q) = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
}
