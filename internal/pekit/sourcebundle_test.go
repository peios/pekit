package pekit

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func unpackSourceBundle(t *testing.T, archive, destination string) string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	root := ""
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(h.Name, "usr/src/dist/") {
			continue
		}
		name := filepath.Join(destination, h.Name)
		if !withinDirectory(destination, name) {
			t.Fatal("escaped archive")
		}
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(name, os.FileMode(h.Mode)); err != nil {
				t.Fatal(err)
			}
		case tar.TypeSymlink:
			if err := os.Symlink(h.Linkname, name); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, data, os.FileMode(h.Mode)); err != nil {
				t.Fatal(err)
			}
		}
		if filepath.Base(name) == "build-inputs.json" {
			root = filepath.Dir(name)
		}
	}
	if root == "" {
		t.Fatal("missing source manifest")
	}
	return root
}

func TestSourceBundleReconstruction(t *testing.T) {
	if os.Getenv("PEKIT_TEST_SANDBOX") != "1" {
		t.Skip("requires real namespaces")
	}
	dependencyRoot := sandboxFixtureRoot(t)
	dir := t.TempDir()
	ws := filepath.Join(dir, "original")
	recipe := filepath.Join(ws, "app")
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload")})
	writeFile(t, filepath.Join(ws, "workspace.pekit.toml"), `include=["app"]
[env]
DISTRO_FLAG="captured-policy"
[isolation]
enabled=true
inputs=[]
`)
	writeFile(t, filepath.Join(ws, "helpers/install"), "#!/bin/sh\nprintf '%s' \"$DISTRO_FLAG\" > \"$PEKIT_OUT/result\"\n")
	if err := os.Chmod(filepath.Join(ws, "helpers/install"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "test.env.pekit.toml"), "dependency_provider='apt'\n[sandbox]\ncommand="+shellTOML("cp -a "+shellQuote(dependencyRoot+"/.")+" \"$PEKIT_SANDBOX_ROOT\"")+"\n")
	writeFile(t, filepath.Join(ws, "lint.pekit.toml"), "[policy]\n")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `out_dir="out"
[source_package]
workspace_inputs=["helpers/install"]
[source.url]
url="`+rawURL+`"
extract=true
root="app-{{version}}"
[build.vendor]
command='printf acquired > "$PEKIT_OUT/dependency.c"'
[build.main]
needs=["vendor"]
command='"$PEKIT_WORKSPACE_ROOT/helpers/install"; printf rewritten > "$PEKIT_VENDOR_OUT/dependency.c"'
[test]
needs=["main"]
gate=true
command='sh "$PEKIT_RECIPE_ROOT/tests/check.sh"'
`)
	writeFile(t, filepath.Join(recipe, "tests/check.sh"), "test \"$(cat \"$PEKIT_MAIN_OUT/result\")\" = captured-policy\n")
	writeFile(t, filepath.Join(recipe, "tools/extra.c"), "/* retained source fixture */\n")
	if err := os.MkdirAll(filepath.Join(recipe, "tests/empty-fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), `format="peipkg"
[package]
name="app"
version="{{version}}-1"
architecture="noarch"
description="fixture"
license="MIT"
[files]
"main:result"="usr/share/app/result"
`)
	if err := os.Symlink("../test.env.pekit.toml", filepath.Join(recipe, "test.env.pekit.toml")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(recipe, "private.keyring.pekit.toml"), "secret='do not export'")
	writeFile(t, filepath.Join(ws, "sibling/private"), "not a build input")
	runTestCmd(t, ws, "git", "init")
	writeFile(t, filepath.Join(ws, ".gitignore"), "operator-private.pem\n")
	writeFile(t, filepath.Join(recipe, "operator-private.pem"), "ignored secret sentinel")
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--version", "1.0", "--env", "test"}); err != nil {
		t.Fatalf("%v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	archive := globOne(t, filepath.Join(recipe, "out", "*", "package", "*", "app-source_1.0-1_noarch.peipkg"))
	root := unpackSourceBundle(t, archive, filepath.Join(dir, "reconstructed"))
	for _, name := range []string{"workspace/app/tests/check.sh", "workspace/app/tools/extra.c", "workspace/helpers/install", "workspace/workspace.pekit.toml", "workspace/test.env.pekit.toml", "workspace/lint.pekit.toml", "source/payload.txt", "rebuild.py"} {
		if !fileExists(filepath.Join(root, name)) {
			t.Fatal("missing", name)
		}
	}
	acquired, err := os.ReadFile(filepath.Join(root, "acquisition/vendor/dependency.c"))
	if err != nil || string(acquired) != "acquired" {
		t.Fatalf("original acquired source not captured: %q %v", acquired, err)
	}
	if !dirExists(filepath.Join(root, "workspace/app/tests/empty-fixture")) {
		t.Fatal("empty fixture directory missing")
	}
	if fileExists(filepath.Join(root, "workspace/app/operator-private.pem")) || fileExists(filepath.Join(root, "workspace/app/private.keyring.pekit.toml")) || fileExists(filepath.Join(root, "workspace/sibling/private")) {
		t.Fatal("private/unrelated source exported")
	}
	// Remove access to the checkout and disable the only upstream URL entirely.
	if err := os.Rename(ws, ws+"-unavailable"); err != nil {
		t.Fatal(err)
	}
	serveURLs(t, map[string][]byte{})
	data, err := os.ReadFile(filepath.Join(root, "build-inputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest sourceBundleManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	reconstructed := filepath.Join(root, filepath.FromSlash(manifest.Recipe))
	chdir(t, reconstructed)
	if err := app.Run([]string{"package", "--version", "1.0", "--env", "test", "--local=" + filepath.Join(root, "source")}); err != nil {
		t.Fatalf("reconstruction: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	binary := globOne(t, filepath.Join(reconstructed, "out", "*", "package", "*", "app_1.0-1_noarch.peipkg"))
	if got := string(readPeipkgEntries(t, binary)["usr/share/app/result"]); got != "captured-policy" {
		t.Fatalf("rebuilt %q", got)
	}
}

func shellTOML(value string) string { data, _ := json.Marshal(value); return string(data) }

func TestCapturedHelpersSurviveWorkspaceMutation(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	recipeRoot := filepath.Join(ws, "app")
	writeFile(t, filepath.Join(recipeRoot, "pekit.toml"), "out_dir='out'\n[build]\ncommand='true'\n")
	writeFile(t, filepath.Join(ws, "workspace.pekit.toml"), "include=['app']\n[isolation]\nenabled=true\ninputs=['helpers']\n")
	writeFile(t, filepath.Join(ws, "helpers/tool"), "original")
	recipe, err := LoadRecipe(filepath.Join(recipeRoot, "pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := LoadWorkspace(filepath.Join(ws, "workspace.pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	source := cleanSourceState(recipe)
	ctx := &Context{Inv: Invocation{Cwd: ws}}
	job, err := (&sandboxCommand{Recipe: recipe, Workspace: workspace, Source: source}).job(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "helpers/tool"), "changed")
	for _, path := range []string{filepath.Join(job.Inputs[filepath.Join(ws, "helpers")], "tool"), filepath.Join(job.SourceInputs.Directory, "workspace/helpers/tool")} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "original" {
			t.Fatalf("captured bytes changed: %s %q %v", path, data, err)
		}
	}
	writeFile(t, filepath.Join(recipeRoot, "pekit.toml"), "out_dir='changed'\n")
	if err := job.SourceInputs.unchanged(); diagCode(err) != "source_input_changed" {
		t.Fatalf("expected changed-input rejection: %v", err)
	}
}

func TestSourceInputsRejectEscapingAndMissingHelpers(t *testing.T) {
	for _, input := range []string{"../outside", "missing", "helpers"} {
		t.Run(input, func(t *testing.T) {
			dir := t.TempDir()
			ws := filepath.Join(dir, "ws")
			recipeRoot := filepath.Join(ws, "app")
			writeFile(t, filepath.Join(recipeRoot, "pekit.toml"), "out_dir='out'\n[build]\ncommand='true'\n")
			writeFile(t, filepath.Join(dir, "outside/secret"), "private")
			if err := os.Symlink(filepath.Join(dir, "outside"), filepath.Join(ws, "helpers")); err != nil {
				t.Fatal(err)
			}
			recipe, _ := LoadRecipe(filepath.Join(recipeRoot, "pekit.toml"))
			workspace := WorkspaceConfig{Root: ws, Isolation: IsolationConfig{Enabled: true, Inputs: []string{input}}}
			_, err := (&sandboxCommand{Recipe: recipe, Workspace: workspace, Source: cleanSourceState(recipe)}).job(&Context{Inv: Invocation{Cwd: ws}})
			if err == nil {
				t.Fatal("unsafe input accepted")
			}
		})
	}
}

func TestRebuildScriptVerifiesBeforeExecuting(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "source/file"), "original")
	writeFile(t, filepath.Join(dir, "workspace/app/pekit.toml"), "# recipe")
	if err := os.Chmod(filepath.Join(dir, "source/file"), 0777); err != nil {
		t.Fatal(err)
	}
	identity, err := inputIdentity(filepath.Join(dir, "source/file"))
	if err != nil {
		t.Fatal(err)
	}
	identity = "0644:" + strings.SplitN(identity, ":", 2)[1]
	manifest := sourceBundleManifest{Schema: 2, Recipe: "workspace/app", Files: []sourceBundleFile{{Path: "source/file", Identity: identity}}}
	data, _ := json.Marshal(manifest)
	writeFile(t, filepath.Join(dir, "build-inputs.json"), string(data))
	writeFile(t, filepath.Join(dir, "rebuild.py"), sourceRebuildScript)
	bin := filepath.Join(dir, "bin")
	writeFile(t, filepath.Join(bin, "pekit"), "#!/bin/sh\nprintf invoked > "+shellQuote(filepath.Join(dir, "invoked"))+"\n")
	if err := os.Chmod(filepath.Join(bin, "pekit"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	runTestCmd(t, dir, "python3", "rebuild.py", "package")
	info, _ := os.Stat(filepath.Join(dir, "source/file"))
	if info.Mode().Perm() != 0644 {
		t.Fatal("source mode not restored")
	}
	if err := os.Remove(filepath.Join(dir, "invoked")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "source/file"), "tampered")
	command := exec.Command("python3", "rebuild.py", "package")
	command.Dir = dir
	if err := command.Run(); err == nil || fileExists(filepath.Join(dir, "invoked")) {
		t.Fatal("tampered input reached Pekit")
	}
}

func TestCatalogueSourceInputCapture(t *testing.T) {
	path := os.Getenv("PEKIT_TEST_CATALOGUE")
	if path == "" {
		t.Skip("set PEKIT_TEST_CATALOGUE to check real recipes")
	}
	ws, err := LoadWorkspace(filepath.Join(path, "workspace.pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	members, err := discoverWorkspaceMembers(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		t.Run(member.ID, func(t *testing.T) {
			recipe, err := LoadRecipe(member.RecipePath)
			if err != nil {
				t.Fatal(err)
			}
			_, err = captureSourceInputs(&Context{Inv: Invocation{Cwd: path}}, recipe, &ws, cleanSourceState(recipe), filepath.Join(t.TempDir(), "inputs"), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
