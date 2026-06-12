package main

import "fmt"

// PackageJob is everything an engine needs to build one package target:
// where the build artifacts are and where the package must land.
type PackageJob struct {
	Name       string
	BuildStage string // absolute path to outDir/build/<name>
	OutStage   string // absolute path to outDir/package/<name>
}

// packageEngine builds one package target in a specific format.
type packageEngine func(job PackageJob) error

// packageEngines is the format switchboard.
var packageEngines = map[string]packageEngine{
	"tar": tarEngine,
}

func engineFor(name string, pkg Package) (packageEngine, error) {
	engine, ok := packageEngines[pkg.Format]
	if !ok {
		return nil, fmt.Errorf("package %s: unrecognised package format %q", name, pkg.Format)
	}
	return engine, nil
}
