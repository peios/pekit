package main

import "fmt"

// packageEngine builds one package target in a specific format.
type packageEngine func(name string, pkg Package) error

// packageEngines is the format switchboard. Empty for now — no packaging
// engine has landed yet, so every format is unrecognised by design.
var packageEngines = map[string]packageEngine{}

func buildPackage(name string, pkg Package) error {
	engine, ok := packageEngines[pkg.Format]
	if !ok {
		return fmt.Errorf("package %s: unrecognised package format %q", name, pkg.Format)
	}
	return engine(name, pkg)
}
