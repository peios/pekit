package pekit

import "testing"

func TestParseRejectsUnsupportedFlagUnlessAllowUnused(t *testing.T) {
	_, err := ParseInvocation([]string{"clean", "--version", "1.2.3"}, "/tmp")
	if err == nil {
		t.Fatal("expected clean --version to fail")
	}
	inv, err := ParseInvocation([]string{"clean", "--version", "1.2.3", "--allow-unused"}, "/tmp")
	if err != nil {
		t.Fatalf("allow-unused should suppress recognized unused flag: %v", err)
	}
	if inv.Command != CommandClean || inv.Version != "1.2.3" || !inv.AllowUnused {
		t.Fatalf("unexpected invocation: %#v", inv)
	}
}

func TestParseWorkspaceJobsPlacement(t *testing.T) {
	inv, err := ParseInvocation([]string{"workspace", "--jobs", "2", "package", "--all"}, "/tmp")
	if err != nil {
		t.Fatalf("expected workspace parse: %v", err)
	}
	if !inv.WorkspaceMode || inv.DelegateCommand != CommandPackage || inv.Jobs != 2 || !inv.All {
		t.Fatalf("unexpected workspace invocation: %#v", inv)
	}
	_, err = ParseInvocation([]string{"workspace", "package", "--all", "--jobs", "2"}, "/tmp")
	if err == nil {
		t.Fatal("expected delegated --jobs to fail")
	}
}

func TestParseWorkspaceGlobalFlagBeforeDelegatedCommand(t *testing.T) {
	inv, err := ParseInvocation([]string{"workspace", "--dry-run", "--jobs", "2", "package", "--all"}, "/tmp")
	if err != nil {
		t.Fatalf("expected workspace global flag parse: %v", err)
	}
	if !inv.DryRun || inv.Jobs != 2 || inv.DelegateCommand != CommandPackage || !inv.All {
		t.Fatalf("unexpected invocation: %#v", inv)
	}
	if _, err := ParseInvocation([]string{"workspace", "--env", "ci", "build"}, "/tmp"); err == nil {
		t.Fatal("expected delegated command flag before command to fail")
	}
}

func TestParseKeyringLiteral(t *testing.T) {
	inv, err := ParseInvocation([]string{"build", "--keyring=prod", "--keyring.tcb.priv=/secret"}, "/tmp")
	if err != nil {
		t.Fatalf("parse keyring: %v", err)
	}
	if len(inv.Keyrings) != 1 || inv.Keyrings[0] != "prod" {
		t.Fatalf("unexpected keyrings: %#v", inv.Keyrings)
	}
	if got := inv.KeyringValues["tcb.priv"]; got != "/secret" {
		t.Fatalf("unexpected literal keyring value %q", got)
	}
}

func TestCleanOutputOnlyRejectsTargetEnvFlagsWithoutAllowUnused(t *testing.T) {
	_, err := ParseInvocation([]string{"clean", "--output-only", "--env", "ci"}, "/tmp")
	if err == nil {
		t.Fatal("expected clean --output-only --env to fail")
	}
	if _, err := ParseInvocation([]string{"clean", "--output-only", "--env", "ci", "--allow-unused"}, "/tmp"); err != nil {
		t.Fatalf("allow-unused should suppress output-only env flag: %v", err)
	}
}

func TestCleanModesAreMutuallyExclusive(t *testing.T) {
	if _, err := ParseInvocation([]string{"clean", "--output-only", "--target-only"}, "/tmp"); err == nil {
		t.Fatal("expected mutually exclusive clean modes to fail")
	}
}

func TestPackageRejectsAllowUnanchoredWithoutAllowUnused(t *testing.T) {
	if _, err := ParseInvocation([]string{"package", "--allow-unanchored"}, "/tmp"); err == nil {
		t.Fatal("expected package --allow-unanchored to be unused")
	}
	if _, err := ParseInvocation([]string{"package", "--allow-unanchored", "--allow-unused"}, "/tmp"); err != nil {
		t.Fatalf("allow-unused should suppress package --allow-unanchored: %v", err)
	}
}

func TestWorkspaceFlagRequiresWorkspaceCommand(t *testing.T) {
	if _, err := ParseInvocation([]string{"--workspace", "/tmp/ws", "build"}, "/tmp"); err == nil {
		t.Fatal("expected --workspace without workspace command to fail")
	}
}

func TestParseNoVerifyForms(t *testing.T) {
	inv, err := ParseInvocation([]string{"build", "--no-verify"}, "/tmp")
	if err != nil || inv.NoVerify == nil || *inv.NoVerify != "" {
		t.Fatalf("bare --no-verify: inv.NoVerify=%v err=%v", inv.NoVerify, err)
	}
	inv, err = ParseInvocation([]string{"build", "--no-verify=uapi,docs"}, "/tmp")
	if err != nil || inv.NoVerify == nil || *inv.NoVerify != "uapi,docs" {
		t.Fatalf("--no-verify=list: inv.NoVerify=%v err=%v", inv.NoVerify, err)
	}
}

func TestParseGenVerifyCommands(t *testing.T) {
	inv, err := ParseInvocation([]string{"gen", "uapi"}, "/tmp")
	if err != nil || inv.Command != CommandGen || len(inv.Positionals) != 1 || inv.Positionals[0] != "uapi" {
		t.Fatalf("gen uapi: %#v err=%v", inv, err)
	}
	if _, err := ParseInvocation([]string{"gen", "--all"}, "/tmp"); err != nil {
		t.Fatalf("gen --all should parse: %v", err)
	}
	if _, err := ParseInvocation([]string{"gen", "--all", "uapi"}, "/tmp"); err == nil {
		t.Fatal("gen --all with a selector should be rejected")
	}
	// gen resolves no versions/source, so version selection is unsupported.
	if _, err := ParseInvocation([]string{"gen", "--version", "1.0"}, "/tmp"); err == nil {
		t.Fatal("gen --version should be rejected")
	}
	// verify does not run builds, so --no-verify is meaningless on it.
	if _, err := ParseInvocation([]string{"verify", "--no-verify"}, "/tmp"); err == nil {
		t.Fatal("verify --no-verify should be rejected")
	}
}
