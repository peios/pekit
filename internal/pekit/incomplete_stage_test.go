package pekit

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func incompleteStageFixture(t *testing.T, isolated bool) (*Context, RecipeConfig, *WorkspaceConfig, SourceState, TargetConfig) {
	t.Helper()
	root := t.TempDir()
	source := SourceState{Kind: "recipe", SourceRoot: root, LiteralRoot: root, OutBase: filepath.Join(root, "out"), WorkBase: filepath.Join(root, "out")}
	recipe := RecipeConfig{Root: root, OutDir: "out"}
	target := TargetConfig{Kind: CommandBuild, Name: "main", Command: ShellCommand{Shell: `test ! -e "$PEKIT_OUT/stale"; printf rebuilt > "$PEKIT_OUT/fresh"`}}
	inv := Invocation{Cwd: root, Command: CommandBuild}
	ctx := &Context{Inv: inv, App: &App{Stdout: io.Discard, Stderr: io.Discard}, Start: time.Now(), Renderer: newRenderer(inv, io.Discard, io.Discard, time.Now)}
	var workspace *WorkspaceConfig
	if isolated {
		workspace = &WorkspaceConfig{Root: root, Isolation: IsolationConfig{Enabled: true}}
		// Pre-created job metadata isolates this test from source acquisition.
		// Root composition deliberately fails before any namespace is launched.
		ctx.Jobs = map[string]*buildJob{source.WorkBase: {Stages: map[string]bool{}, Directory: filepath.Join(root, "job")}}
		writeFile(t, filepath.Join(root, "env.pekit.toml"), "[sandbox]\ncommand='exit 42'\n")
	}
	writeFile(t, filepath.Join(targetStage(source, target.Kind, target.Name), "stale"), "partial output")
	return ctx, recipe, workspace, source, target
}

func TestRunTargetRebuildsRecordedIncompleteStage(t *testing.T) {
	for _, isolated := range []bool{false, true} {
		name := "ordinary"
		if isolated {
			name = "isolated root composition"
		}
		t.Run(name, func(t *testing.T) {
			ctx, recipe, workspace, source, target := incompleteStageFixture(t, isolated)
			if err := markStageRunning(source, target.Kind, target.Name); err != nil {
				t.Fatal(err)
			}
			err := runTarget(ctx, recipe, workspace, source, Version{}, target, "")
			if isolated {
				if diagCode(err) != "target_failed" {
					t.Fatalf("must reach root composition instead of refusing stale stage: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			stage := targetStage(source, target.Kind, target.Name)
			if fileExists(filepath.Join(stage, "stale")) {
				t.Fatal("incomplete contents survived rebuilding")
			}
			status, recorded := readStageStatus(source, target.Kind, target.Name)
			want := stageStatusOK
			if isolated {
				want = stageStatusRunning
			}
			if !recorded || status != want {
				t.Fatalf("status = %q, recorded %v, want %q", status, recorded, want)
			}
			if !isolated && !fileExists(filepath.Join(stage, "fresh")) {
				t.Fatal("replacement target did not run")
			}
		})
	}
}

func TestIncompleteStageRecoveryPreservesRefusalsAndDryRun(t *testing.T) {
	for _, tc := range []struct {
		name, marker                  string
		isolated, reuse, dry, trusted bool
		code                          string
	}{
		{name: "unknown isolated stage", isolated: true, code: "isolation_legacy_stage"},
		{name: "complete legacy stage", marker: stageStatusOK, isolated: true, code: "isolation_legacy_stage"},
		{name: "unrecognized marker", marker: "unexpected", isolated: true, code: "isolation_legacy_stage"},
		{name: "incomplete explicit reuse", marker: stageStatusRunning, reuse: true, code: "stage_incomplete"},
		{name: "incomplete isolated explicit reuse", marker: stageStatusRunning, isolated: true, reuse: true, code: "isolation_legacy_stage"},
		{name: "trusted incomplete explicit reuse", marker: stageStatusRunning, isolated: true, reuse: true, trusted: true, code: "stage_incomplete"},
		{name: "incomplete dry run", marker: stageStatusRunning, isolated: true, dry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, recipe, workspace, source, target := incompleteStageFixture(t, tc.isolated)
			stage := targetStage(source, target.Kind, target.Name)
			if tc.marker != "" {
				writeFile(t, stageStatusPath(source, target.Kind, target.Name), tc.marker+"\n")
			}
			if tc.reuse {
				reuse := ""
				ctx.Inv.NoBuild = &reuse
			}
			ctx.Inv.DryRun = tc.dry
			if tc.trusted {
				ctx.Jobs[source.WorkBase].Stages[stage] = true
			}
			err := runTarget(ctx, recipe, workspace, source, Version{}, target, "")
			if diagCode(err) != tc.code {
				t.Fatalf("error = %v, want %q", err, tc.code)
			}
			data, err := os.ReadFile(filepath.Join(stage, "stale"))
			if err != nil || string(data) != "partial output" {
				t.Fatalf("refusal/dry run changed stage: %q, %v", data, err)
			}
			status, recorded := readStageStatus(source, target.Kind, target.Name)
			if recorded != (tc.marker != "") || status != tc.marker {
				t.Fatalf("marker changed: %q %v", status, recorded)
			}
		})
	}
}
