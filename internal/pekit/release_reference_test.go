package pekit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseReferenceAllowanceCannotReachPublication(t *testing.T) {
	ws, args := releaseFixture(t, "[release.reference_allow]\n'payload.junk'='Reference fixture only'\n")
	p := filepath.Join(ws, "a/package.pekit.toml")
	data, _ := os.ReadFile(p)
	writeFile(t, p, strings.ReplaceAll(string(data), "usr/share/a/payload.txt", "usr/share/a/unwanted.la"))
	releaseCommit(t, ws)
	if err := runRelease(t, ws, append([]string{"--recipe", "a", "release", "--all"}, args...)); err == nil || !strings.Contains(err.Error(), "lint_failed") {
		t.Fatalf("publication escaped lint: %v", err)
	}
	refs, _ := filepath.Glob(filepath.Join(ws, ".pekit/releases/candidate-*/*/reference/qualification.json"))
	if len(refs) != 1 {
		t.Fatalf("reference did not qualify: %v", refs)
	}
	var r releaseReceipt
	data, _ = os.ReadFile(refs[0])
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Allowed) != 1 {
		t.Fatalf("reference allowance not recorded: %+v", r)
	}
	native, _ := filepath.Glob(filepath.Join(ws, ".pekit/releases/candidate-*/*/native/qualification.json"))
	if len(native) != 0 {
		t.Fatal("native falsely qualified", native)
	}
	if _, err := os.Stat(filepath.Join(ws, "public")); !os.IsNotExist(err) {
		t.Fatal("failed native check touched publication", err)
	}
}

func TestReleaseReferenceAllowanceRejectsUnknownOrEmpty(t *testing.T) {
	for _, allow := range []string{"'elf.typo'='reason'", "'elf.cet'=' '", "'payload.dirs.lib'='reason'"} {
		root := t.TempDir()
		path := filepath.Join(root, "workspace.pekit.toml")
		writeFile(t, path, "include=['*']\n[release]\npath='public'\nname='test'\nsigning_key='keyring:signing.repository_key'\nenvironments=['reference','native']\n[release.reference_allow]\n"+allow+"\n")
		if _, err := LoadWorkspace(path); err == nil {
			t.Fatalf("accepted %s", allow)
		}
	}
}
