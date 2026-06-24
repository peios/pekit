package pekit

import "testing"

func TestValidateSelectorAllowsPlus(t *testing.T) {
	// Package selectors mirror package names, which may contain '+'.
	for _, ok := range []string{"gcc-c++", "libstdc++", "libstdc++-devel", "gcc", "g++"} {
		if err := validateSelector("package", ok); err != nil {
			t.Errorf("validateSelector(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-leading", "a/b", "a:b"} {
		if err := validateSelector("package", bad); err == nil {
			t.Errorf("validateSelector(%q) = nil, want error", bad)
		}
	}
}
