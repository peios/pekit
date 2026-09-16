package pekit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceLinkContained(t *testing.T) {
	for _, tt := range []struct {
		target string
		safe   bool
	}{
		{"missing", true}, {"missing/child", true}, {"missing/../file", true},
		{"file", true}, {"dir/../missing", true}, {"chain", true},
		{"../outside", false}, {"missing/../../outside", false},
		{"/etc/passwd", false}, {"link", false}, {"file/child", false},
		{"escape/../file", false},
	} {
		t.Run(tt.target, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "dir"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "file"), []byte("hello"), 0644); err != nil {
				t.Fatal(err)
			}
			for name, target := range map[string]string{"link": tt.target, "chain": "missing", "escape": "../outside"} {
				if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			}
			if got := sourceLinkContained(root, filepath.Join(root, "link")); got != tt.safe {
				t.Fatalf("got %v want %v", got, tt.safe)
			}
		})
	}
}
