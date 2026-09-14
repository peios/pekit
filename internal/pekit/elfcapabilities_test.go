package pekit

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageELFVersionCapabilities(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("requires ELF C compiler")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	writeFile(t, filepath.Join(source, "lib.c"), "int cpp(void){return 42;}\n")
	writeFile(t, filepath.Join(source, "lib.map"), "GLIBCXX_3.4.30 {global:cpp; local:*;};\n")
	writeFile(t, filepath.Join(source, "app.c"), "extern int cpp(void); int main(void){return cpp()==42?0:1;}\n")
	runTestCmd(t, source, "cc", "-shared", "-fPIC", "-nostdlib", "-Wl,-soname,libstdc++.so.6", "-Wl,--version-script=lib.map", "-o", "libcpp.so", "lib.c")
	runTestCmd(t, source, "cc", "-o", "app", "app.c", "./libcpp.so")
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), "include=['recipe']\n[policy]\nsymbol_capabilities=['libstdc++.so.6']\n")
	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), "[source.local]\npath='../source'\n[build.main]\ncommand='true'\n")
	base := `format='peipkg'
[package]
version='1.0-1'
architecture='x86_64'
description='ELF capability fixture'
license='MIT'
`
	writeFile(t, filepath.Join(recipe, "runtime.package.pekit.toml"), base+"name='cpp-runtime'\n[files]\n'@source:libcpp.so'='usr/lib/x86_64-linux-peios/libstdc++.so.6'\n")
	writeFile(t, filepath.Join(recipe, "consumer.package.pekit.toml"), base+"name='cpp-consumer'\n[dependencies]\n'libstdc++.so.6'='*'\n[files]\n'@source:app'='usr/bin/cpp-consumer'\n")
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--local", "--version", "1.0", "--all"}); err != nil {
		t.Fatalf("package: %v\n%s", err, stderr.String())
	}
	provider := readPeipkgManifest(t, globOne(t, filepath.Join(recipe, "out/*/package/*/cpp-runtime_1.0-1_x86_64.peipkg")))
	consumer := readPeipkgManifest(t, globOne(t, filepath.Join(recipe, "out/*/package/*/cpp-consumer_1.0-1_x86_64.peipkg")))
	capability := "elfver(libstdc++.so.6:GLIBCXX_3.4.30)"
	found := false
	for _, p := range provider.Provides {
		if p.Name == capability {
			found = true
			if p.Version != "" {
				t.Fatal("capability must be exact identity, not a package version")
			}
		}
	}
	if !found {
		t.Fatalf("runtime archive lost capability: %+v", provider.Provides)
	}
	found = false
	for _, d := range consumer.Dependencies {
		if d.Name == capability {
			found = true
			if d.Constraint != "" {
				t.Fatal("exact requirement unexpectedly versioned")
			}
		}
	}
	if !found {
		t.Fatalf("consumer archive lost requirement: %+v", consumer.Dependencies)
	}
	// Existing explicit SONAME metadata must not replace a distinct exact need.
	for _, d := range consumer.Dependencies {
		if strings.HasPrefix(d.Name, "elfver(") {
			t.Logf("archive requires %s", d.Name)
		}
	}
}
