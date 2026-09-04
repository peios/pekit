package pekit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stageSource(t *testing.T) SourceState {
	t.Helper()
	return SourceState{WorkBase: t.TempDir()}
}

// A stage that no target has ever written has no recorded status, and that is
// "unknown" rather than "failed": refusing it would turn an explicit
// --no-build into a rebuild.
func TestStageStatusUnrecordedIsNotFailed(t *testing.T) {
	source := stageSource(t)
	if _, recorded := readStageStatus(source, CommandBuild, "source"); recorded {
		t.Fatal("a stage never written reported a recorded status")
	}
}

// The running marker is written before the command and is what distinguishes a
// stage left over from a failed run from one that completed (PEI-551).
func TestStageStatusRunningThenComplete(t *testing.T) {
	source := stageSource(t)

	if err := markStageRunning(source, CommandBuild, "kernel"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	status, recorded := readStageStatus(source, CommandBuild, "kernel")
	if !recorded || status != stageStatusRunning {
		t.Fatalf("status = %q recorded = %v, want %q true", status, recorded, stageStatusRunning)
	}

	if err := markStageComplete(source, CommandBuild, "kernel"); err != nil {
		t.Fatalf("mark complete: %v", err)
	}
	status, recorded = readStageStatus(source, CommandBuild, "kernel")
	if !recorded || status != stageStatusOK {
		t.Fatalf("status = %q recorded = %v, want %q true", status, recorded, stageStatusOK)
	}
}

// The marker must not land inside the stage: a stage's contents become a
// package payload.
func TestStageStatusLivesOutsideTheStage(t *testing.T) {
	source := stageSource(t)
	stage := targetStage(source, CommandBuild, "tools")
	marker := stageStatusPath(source, CommandBuild, "tools")
	if strings.HasPrefix(marker, stage+string(os.PathSeparator)) {
		t.Errorf("marker %q is inside the stage %q", marker, stage)
	}
}

// Both failures on PEI-551 left a stage that the next run could neither use nor
// rebuild from. Whatever the cause, the recorded status has to survive it —
// a stage emptied by a failed clone and a stage half-cleaned by a failed
// removal are both "running", never "ok".
func TestStageLeftByAFailedRunStaysRunning(t *testing.T) {
	source := stageSource(t)
	stage := targetStage(source, CommandBuild, "upstream")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := markStageRunning(source, CommandBuild, "upstream"); err != nil {
		t.Fatal(err)
	}

	// A clone that failed after emptying the stage: the directory exists and
	// is empty, which is exactly the state that used to be silently reused.
	if !dirExists(stage) {
		t.Fatal("stage should exist")
	}
	entries, err := os.ReadDir(stage)
	if err != nil || len(entries) != 0 {
		t.Fatalf("stage entries = %v (err %v), want empty", entries, err)
	}
	if status, _ := readStageStatus(source, CommandBuild, "upstream"); status != stageStatusRunning {
		t.Errorf("status = %q, want %q", status, stageStatusRunning)
	}
}

// removeStage clears a tree whose directories were left unwritable, which
// os.RemoveAll alone cannot.
func TestRemoveStageClearsAnUnwritableTree(t *testing.T) {
	stage := filepath.Join(t.TempDir(), "source")
	deep := filepath.Join(stage, "drivers", "net")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(stage, "drivers", "net"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(stage, "drivers"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(stage, "drivers"), 0o700)
		_ = os.Chmod(filepath.Join(stage, "drivers", "net"), 0o700)
	})

	if err := os.RemoveAll(stage); err == nil {
		t.Skip("this platform removes unwritable directories; nothing to prove")
	}
	if err := removeStage(stage); err != nil {
		t.Fatalf("removeStage: %v", err)
	}
	if dirExists(stage) {
		t.Error("stage still present after removeStage")
	}
}
