package main

import "fmt"

// StagedFile is a fully resolved [files] entry: absolute source path on
// disk, image-relative dest inside the package.
type StagedFile struct {
	Source string
	Dest   string
}

// PackageJob is everything an engine needs to build one package.
type PackageJob struct {
	Pkg  *PackageFile
	Name string // [package] name, or the project directory name
	Root string // absolute project root (recipe dir)
	// ProvenanceDir is the git tree whose commit identifies what built
	// this package: the fetched source checkout for a [source] recipe,
	// else the recipe dir.
	ProvenanceDir string
	Files         []StagedFile // sorted by Dest, sources verified to exist
	OutStage      string       // absolute path to outDir/package/<name>
	// Local marks a --local build (working copy, no pinned commit), so the
	// engine stamps localdev provenance instead of reading git HEAD.
	Local bool
}

// packageEngine builds one package in a specific format, returning the
// path of the artifact it wrote.
type packageEngine func(job PackageJob) (string, error)

// packageEngines is the format switchboard.
var packageEngines = map[string]packageEngine{
	"tar":    tarEngine,
	"peipkg": peipkgEngine,
}

func engineFor(format string) (packageEngine, error) {
	engine, ok := packageEngines[format]
	if !ok {
		return nil, fmt.Errorf("unrecognised package format %q", format)
	}
	return engine, nil
}
