package pekit

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Package stage freshness.
//
// A package stage is a cache. The run packs the finished artifact into
// <work base>/package/<stage id>/ and leaves it there, and the stage id is
// keyed on the package identity alone — name, version, architecture and
// format (makePackageInstance). Nothing in that key, and nothing in the
// stage, records the provenance the artifact was packed with, so a stage
// the current run does not rewrite keeps handing back an artifact whose
// manifest build block describes an earlier state of the recipe: package
// one member after committing the recipe and the other members' stages
// still carry `git:<old commit>+dirty`, ready for the next collection of
// out/*/package/*/*.peipkg to pick up (PEI-773).
//
// So every stage carries a stamp naming the provenance its artifact was
// packed with, and a packaging run drops every stage whose stamp does not
// match its own before it packs anything. Only package stages are
// affected: build stages are keyed on their own inputs and keep their
// completion markers, so invalidating a package cache never costs a
// recompilation — a `--no-build` run repacks from the same completed
// build stage.
//
// A stage with no stamp is stale by definition: it was packed by a pekit
// that recorded nothing, so its provenance cannot be shown to match. The
// artifact is a cached output, not a build input, and repacking it is
// cheap.

// packageStamp records the manifest inputs a staged artifact was packed
// with: the run-wide provenance every artifact of a run shares, and the
// package identity the stage holds.
type packageStamp struct {
	RecipeRef     string `json:"recipe_ref"`
	Builder       string `json:"builder"`
	SourceRef     string `json:"source_ref"`
	Name          string `json:"name"`
	Version       string `json:"version,omitempty"`
	Architecture  string `json:"architecture,omitempty"`
	Format        string `json:"format"`
	SourcePackage string `json:"source_package,omitempty"`
}

// sameRun reports whether a stamp was written by a run with the same
// effective provenance as this one. The package identity is not compared:
// it is already the stage id, and a stage holding a different identity is
// a different stage.
func (s packageStamp) sameRun(other packageStamp) bool {
	return s.RecipeRef == other.RecipeRef &&
		s.Builder == other.Builder &&
		s.SourceRef == other.SourceRef
}

func runStamp(source SourceState, run packRun) packageStamp {
	return packageStamp{RecipeRef: run.RecipeRef, Builder: run.Builder, SourceRef: source.ProvenanceRef}
}

func instanceStamp(source SourceState, inst PackageInstance, run packRun) packageStamp {
	stamp := runStamp(source, run)
	stamp.Name = inst.Name
	stamp.Version = inst.Version
	stamp.Architecture = inst.Architecture
	stamp.Format = inst.Format
	stamp.SourcePackage = inst.SourcePackageName
	return stamp
}

func packageStageBase(source SourceState) string {
	base := source.WorkBase
	if base == "" {
		base = source.OutBase
	}
	return filepath.Join(base, "package")
}

// packageStampPath keeps the stamp beside the stage rather than inside
// it, under the work base's .pekit/ metadata directory, for the same
// reason stage completion markers live there: a stage's contents become a
// package payload.
func packageStampPath(source SourceState, stageID string) string {
	base := source.WorkBase
	if base == "" {
		base = source.OutBase
	}
	return filepath.Join(base, ".pekit", "packages", stageID+".json")
}

func readPackageStamp(source SourceState, stageID string) (packageStamp, bool) {
	data, err := os.ReadFile(packageStampPath(source, stageID))
	if err != nil {
		return packageStamp{}, false
	}
	var stamp packageStamp
	if err := json.Unmarshal(data, &stamp); err != nil {
		return packageStamp{}, false
	}
	return stamp, true
}

// writePackageStamp records what a freshly packed stage holds. It is
// written after the artifact, so an interrupted pack leaves the stage
// without a current stamp and the next run drops it.
func writePackageStamp(source SourceState, inst PackageInstance, run packRun) error {
	path := packageStampPath(source, filepath.Base(inst.Stage))
	data, err := json.MarshalIndent(instanceStamp(source, inst, run), "", "  ")
	if err != nil {
		return wrapDiag("package_stamp", "encode package stamp", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return wrapDiag("mkdir", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return wrapDiag("package_stamp", path, err)
	}
	return nil
}

// dropStalePackageStages removes every package stage whose recorded
// provenance is not this run's, together with its stamp. It runs before
// anything is packed, and it covers stages this run does not select:
// those are exactly the ones no later step would refresh.
func dropStalePackageStages(ctx *Context, source SourceState, run packRun, member string) error {
	base := packageStageBase(source)
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return wrapDiag("read_dir", base, err)
	}
	current := runStamp(source, run)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		stageID := entry.Name()
		if stamp, ok := readPackageStamp(source, stageID); ok && stamp.sameRun(current) {
			continue
		}
		stage := filepath.Join(base, stageID)
		if err := removeStage(stage); err != nil {
			return wrapDiag("clean_stage", stage, err)
		}
		if err := os.Remove(packageStampPath(source, stageID)); err != nil && !os.IsNotExist(err) {
			return wrapDiag("package_stamp", packageStampPath(source, stageID), err)
		}
		if ctx.Inv.Verbose {
			ctx.Renderer.Event(Event{Type: "package_stage_dropped", Member: member, Path: stage,
				Message: "dropped package stage packed with different recipe provenance"})
		}
	}
	return nil
}
