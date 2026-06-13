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
	Pkg      *PackageFile
	Name     string       // [package] name, or the project directory name
	Root     string       // absolute project root (provenance, etc.)
	Files    []StagedFile // sorted by Dest, sources verified to exist
	OutStage string       // absolute path to outDir/package/<name>
}

// packageEngine builds one package in a specific format.
type packageEngine func(job PackageJob) error

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
