package pekit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- config parsing -------------------------------------------------------

func TestParseGenTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pekit.toml")
	writeFile(t, path, `
[gen.uapi]
command = "true"
verify_command = "true"
verify_on_build = ["headers"]
verify_on_test = []

[gen.uapi.dependencies.apt]
clang = "*"

[gen.uapi.verify_dependencies.apt]
python3 = "*"
`)
	recipe, err := LoadRecipe(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	g, ok := recipe.Targets[CommandGen]["uapi"]
	if !ok {
		t.Fatalf("gen.uapi not parsed; targets=%v", recipe.Targets[CommandGen])
	}
	if g.VerifyCommand.Empty() {
		t.Fatal("verify_command not parsed")
	}
	if g.VerifyOnBuild == nil || len(*g.VerifyOnBuild) != 1 || (*g.VerifyOnBuild)[0] != "headers" {
		t.Fatalf("verify_on_build = %v, want [headers]", g.VerifyOnBuild)
	}
	if g.VerifyOnTest == nil {
		t.Fatal("verify_on_test empty array must parse as non-nil (gate none), got nil")
	}
	if len(*g.VerifyOnTest) != 0 {
		t.Fatalf("verify_on_test = %v, want empty", *g.VerifyOnTest)
	}
	if g.VerifyDependencies["apt"]["python3"] != "*" {
		t.Fatalf("verify_dependencies not parsed: %v", g.VerifyDependencies)
	}
}

func TestParseGenAbsentScopeIsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pekit.toml")
	writeFile(t, path, `
[gen.uapi]
command = "true"
verify_command = "true"
`)
	recipe, err := LoadRecipe(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	g := recipe.Targets[CommandGen]["uapi"]
	if g.VerifyOnBuild != nil || g.VerifyOnTest != nil {
		t.Fatalf("absent scopes must be nil, got build=%v test=%v", g.VerifyOnBuild, g.VerifyOnTest)
	}
}

func TestParseGenScopeWithoutVerifyCommandRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pekit.toml")
	writeFile(t, path, `
[gen.uapi]
command = "true"
verify_on_build = ["x"]
`)
	if _, err := LoadRecipe(path); err == nil || diagCode(err) != "invalid_gen" {
		t.Fatalf("want invalid_gen error, got %v", err)
	}
}

func TestParseGenUnknownKeyRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pekit.toml")
	writeFile(t, path, `
[gen.uapi]
command = "true"
needs = ["x"]
`)
	if _, err := LoadRecipe(path); err == nil || diagCode(err) != "unknown_key" {
		t.Fatalf("gen must reject needs; got %v", err)
	}
}

// --- scope gating ---------------------------------------------------------

func TestGates(t *testing.T) {
	empty := []string{}
	list := []string{"headers"}
	cases := []struct {
		name    string
		scope   *[]string
		running []string
		want    bool
	}{
		{"absent gates all", nil, []string{"kernel"}, true},
		{"empty gates none", &empty, []string{"kernel"}, false},
		{"list matches", &list, []string{"kernel", "headers"}, true},
		{"list misses", &list, []string{"kernel"}, false},
	}
	for _, tc := range cases {
		if got := gates(tc.scope, tc.running); got != tc.want {
			t.Errorf("%s: gates=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestGenFiresNamespaces(t *testing.T) {
	empty := []string{}
	// verify_on_build = [] (gate none), verify_on_test absent (gate all tests)
	g := TargetConfig{VerifyOnBuild: &empty}
	if genFires(g, []string{"kernel"}, nil) {
		t.Error("build-gated-none must not fire on a build")
	}
	if !genFires(g, nil, []string{"smoke"}) {
		t.Error("test scope absent must fire when a test runs")
	}
}

// --- --no-verify parsing --------------------------------------------------

func TestParseNoVerify(t *testing.T) {
	gens := map[string]TargetConfig{"uapi": {}, "docs": {}}
	all := ""
	if skipAll, _, err := parseNoVerify(Invocation{NoVerify: &all}, gens); err != nil || !skipAll {
		t.Fatalf("bare --no-verify: skipAll=%v err=%v", skipAll, err)
	}
	named := "uapi"
	skipAll, skip, err := parseNoVerify(Invocation{NoVerify: &named}, gens)
	if err != nil || skipAll || !skip["uapi"] || skip["docs"] {
		t.Fatalf("--no-verify=uapi: skipAll=%v skip=%v err=%v", skipAll, skip, err)
	}
	bad := "nope"
	if _, _, err := parseNoVerify(Invocation{NoVerify: &bad}, gens); err == nil {
		t.Fatal("--no-verify naming unknown gen must error")
	}
	if _, _, err := parseNoVerify(Invocation{}, gens); err != nil {
		t.Fatalf("nil --no-verify must be a no-op, got %v", err)
	}
}

// --- end to end -----------------------------------------------------------

// genRecipe writes a recipe whose gen target regenerates generated.txt to
// "fresh" and whose verify_command diffs the committed file against a fresh
// regen in the scratch $PEKIT_OUT. scope is spliced into [gen.thing].
func genRecipe(t *testing.T, dir, scope string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[gen.thing]
command = "printf fresh > generated.txt"
verify_command = """
printf fresh > "$PEKIT_OUT/expected.txt"
diff "$PEKIT_OUT/expected.txt" generated.txt
"""
`+scope+`

[build.main]
command = "printf ok > \"$PEKIT_OUT/marker\""
`)
}

func runIn(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	err := app.Run(args)
	return stdout.String(), stderr.String(), err
}

func TestGenAndVerifyE2E(t *testing.T) {
	dir := t.TempDir()
	genRecipe(t, dir, "")

	// Fresh generation writes the committed file.
	if _, stderr, err := runIn(t, dir, "gen", "thing"); err != nil {
		t.Fatalf("gen failed: %v\n%s", err, stderr)
	}
	if got := readFile(t, filepath.Join(dir, "generated.txt")); got != "fresh" {
		t.Fatalf("generated.txt = %q, want fresh", got)
	}
	// verify passes on an in-sync tree.
	if _, stderr, err := runIn(t, dir, "verify", "thing"); err != nil {
		t.Fatalf("verify should pass in sync: %v\n%s", err, stderr)
	}
	// Scratch dir is cleaned on success.
	if _, err := os.Stat(filepath.Join(dir, "out", ".scratch", "gen", "thing")); !os.IsNotExist(err) {
		t.Fatalf("scratch dir should be removed on success, stat err=%v", err)
	}
}

func TestPreflightBlocksStaleThenBypass(t *testing.T) {
	dir := t.TempDir()
	genRecipe(t, dir, "")
	writeFile(t, filepath.Join(dir, "generated.txt"), "fresh")

	// In sync: build proceeds.
	if _, stderr, err := runIn(t, dir, "build"); err != nil {
		t.Fatalf("in-sync build failed: %v\n%s", err, stderr)
	}

	// Drift the committed file.
	writeFile(t, filepath.Join(dir, "generated.txt"), "stale")

	// Pre-flight blocks the build.
	_, _, err := runIn(t, dir, "build")
	if err == nil || diagCode(err) != "gen_out_of_date" {
		t.Fatalf("stale build should fail gen_out_of_date, got %v", err)
	}
	if !strings.Contains(err.Error(), "pekit gen thing") {
		t.Fatalf("error should recommend regeneration, got: %v", err)
	}

	// --no-verify bypasses.
	if _, stderr, err := runIn(t, dir, "build", "--no-verify"); err != nil {
		t.Fatalf("--no-verify should bypass stale gate: %v\n%s", err, stderr)
	}
	// Named skip bypasses too.
	if _, _, err := runIn(t, dir, "build", "--no-verify=thing"); err != nil {
		t.Fatalf("--no-verify=thing should bypass: %v", err)
	}

	// Regenerating fixes it and the gate passes again.
	if _, _, err := runIn(t, dir, "gen", "thing"); err != nil {
		t.Fatalf("gen failed: %v", err)
	}
	if _, _, err := runIn(t, dir, "build"); err != nil {
		t.Fatalf("post-regen build should pass: %v", err)
	}
}

func TestPreflightScopeGateNone(t *testing.T) {
	dir := t.TempDir()
	// verify_on_build = [] → no build is gated; a stale tree still builds.
	genRecipe(t, dir, "verify_on_build = []\nverify_on_test = []")
	writeFile(t, filepath.Join(dir, "generated.txt"), "stale")
	if _, stderr, err := runIn(t, dir, "build"); err != nil {
		t.Fatalf("gate-none build should ignore drift: %v\n%s", err, stderr)
	}
	// But explicit verify still catches it.
	if _, _, err := runIn(t, dir, "verify", "thing"); err == nil {
		t.Fatal("explicit verify should still catch drift under gate-none")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
