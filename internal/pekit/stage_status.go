package pekit

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Stage completion tracking.
//
// A stage directory on its own says nothing about whether the target that
// produced it finished. Two failures in PEI-551 came from exactly that gap: a
// clean that failed part-way left a tree too full to replace, and a clone that
// failed left one empty — and in both cases the *next* invocation saw a
// directory, reused it, and failed somewhere unrelated and much less obviously
// (`install: cannot create .../certs/peios-modsig.pem`, which reads like a
// signing problem and is not).
//
// So record the status beside the stage rather than inferring it from the
// stage. The marker lives under the work base's .pekit/ metadata directory, not
// inside the stage, because a stage's contents become a package payload.
const (
	stageStatusRunning = "running"
	stageStatusOK      = "ok"
)

func stageStatusPath(source SourceState, kind Command, name string) string {
	base := source.WorkBase
	if base == "" {
		base = source.OutBase
	}
	return filepath.Join(base, ".pekit", "stages", string(kind)+"."+name)
}

// markStageRunning records that a target is about to write its stage. It is
// deliberately written *before* the command runs and left in place if the
// command fails, so an interrupted or failed stage is distinguishable from one
// that was never started.
func markStageRunning(source SourceState, kind Command, name string) error {
	path := stageStatusPath(source, kind, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return wrapDiag("mkdir", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(stageStatusRunning+"\n"), 0o644); err != nil {
		return wrapDiag("stage_status", path, err)
	}
	return nil
}

func markStageComplete(source SourceState, kind Command, name string) error {
	path := stageStatusPath(source, kind, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return wrapDiag("mkdir", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(stageStatusOK+"\n"), 0o644); err != nil {
		return wrapDiag("stage_status", path, err)
	}
	return nil
}

// readStageStatus reports the recorded status, and whether one was recorded at
// all. A stage staged before this tracking existed has no marker; that is
// "unknown", not "failed", and must stay reusable — refusing it would turn an
// explicit --no-build into a rebuild, which is the opposite of what the flag
// asks for.
func readStageStatus(source SourceState, kind Command, name string) (string, bool) {
	data, err := os.ReadFile(stageStatusPath(source, kind, name))
	if err != nil {
		return "", false
	}
	status := string(data)
	for len(status) > 0 && (status[len(status)-1] == '\n' || status[len(status)-1] == '\r') {
		status = status[:len(status)-1]
	}
	return status, true
}

// removeStage empties a stage directory, making a second attempt with the tree
// made writable before giving up.
//
// os.RemoveAll reports the failure it hit last, which for a deep tree is the
// parent's ENOTEMPTY rather than whatever stopped the child from going — so
// `unlinkat .../source/drivers: directory not empty` reads as "the removal did
// not recurse" when the real cause is further down. Retrying with directories
// made traversable and writable clears the common case (a build that left a
// directory read-only); anything that survives that is reported with the entry
// that is actually still there.
func removeStage(stage string) error {
	err := os.RemoveAll(stage)
	if err == nil {
		return nil
	}

	_ = filepath.WalkDir(stage, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})

	if retryErr := os.RemoveAll(stage); retryErr == nil {
		return nil
	} else {
		err = retryErr
	}

	if residual, ok := firstResidualEntry(stage); ok {
		return &StageRemovalError{Stage: stage, Residual: residual, Err: err}
	}
	return err
}

// firstResidualEntry names something still present under stage, so the error
// points at a path rather than at the directory that merely could not be
// emptied.
func firstResidualEntry(stage string) (string, bool) {
	var found string
	_ = filepath.WalkDir(stage, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			found = path
			return filepath.SkipAll
		}
		if path == stage {
			return nil
		}
		if !d.IsDir() {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found, found != ""
}

// StageRemovalError reports a stage that could not be emptied, naming a file
// that is still there.
type StageRemovalError struct {
	Stage    string
	Residual string
	Err      error
}

func (e *StageRemovalError) Error() string {
	return "could not empty stage " + e.Stage + " (still present: " + e.Residual + "): " + e.Err.Error()
}

func (e *StageRemovalError) Unwrap() error { return e.Err }
