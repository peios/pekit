---
title: Building Go
type: how-to
description: Build Go programs hermetically from the modules go.sum names
  with the go library, one package at a time, including tests, go vet,
  the race detector, embedded files, and the pinned Go toolchain.
related:
  - pekit/languages/building-rust
  - pekit/testing-and-running/testing
  - pekit/libraries/libraries
---

The go library builds Go programs from the modules your `go.sum` names,
with no network and nothing of the host. It compiles a module one package
at a time, as Bazel's Go rules do. Import it as `github.com/gcslib/go`.

## Before you start

- The go, buildenv, and gcc libraries. See [Libraries](~pekit/libraries).
- A Go module with a `go.sum`.
- `curl` and `sha256sum` on the host, to pin the module files.

## Register the toolchain

The library asks the `toolchain` module for a `Toolchain::Go`. Its
`go_1_26` is Go 1.26.8 from go.dev, pinned by its SHA-256. It runs on the
buildenv library's busybox environment. Register both in `workspace.gcsl`,
with the gcc library's C toolchain for the race detector:

```gcsl
import Toolchain::* from "github.com/gcslib/buildenv"
import Toolchain::* from "github.com/gcslib/gcc"
import Toolchain::* from "github.com/gcslib/go"
import Toolchain from toolchain

pekitconf = {
    toolchains = [Toolchain::C gcc_x86_64, Toolchain::Go go_1_26, Toolchain::BuildEnv busybox_x86_64]
}
```

## Pin the modules

pekit fetches a file only by its SHA-256. `go.sum` records a different
hash, of a module's contents, so a second file, `go.pins`, records the
digest of each module file as the module proxy serves it.

1. Add a script that writes it:

   ```gcsl
   import "github.com/gcslib/go"

   def script pin_modules() {
       let count = go.pin_modules(file("go.sum"), "{dir(".").path}/go.pins")
   }
   ```

2. Run it, and commit `go.pins` beside `go.sum`:

   ```sh
   pekit run pin_modules
   ```

Run it again whenever `go.sum` changes, and review the change to `go.pins`
as you would review `go.sum`. A build fails when `go.pins` lacks a file
`go.sum` names, and says which.

The build fetches each file by its pin and lays them out as a module proxy
for the go command. The go command checks each module against `go.sum` as
it reads it, so the pins name the bytes and `go.sum` names what they must
be.

## Build a program

1. Plan the module with `go.workspace`, then build what you need of it:

   ```gcsl
   import "github.com/gcslib/go"

   fn module() -> go.GoWorkspace {
       = go.workspace(dir("."), file("go.sum"), file("go.pins"))
   }

   def build server() {
       = go.bin(module(), "./cmd/server")
   }
   ```

2. Build it:

   ```sh
   pekit build server
   ```

`go.bin(ws, package)` gives the program of a main package, named after its
last path element unless you give `name`. Give `ldflags` for the linker,
such as `ldflags = ["-B", "gobuildid"]`. The program carries the build
information `go build -trimpath` embeds, which `go version -m` prints.

`go.workspace` builds for Linux on x86-64, with cgo off. Give `goos` and
`goarch` for another platform. Its build mode is `exe` unless you give
`buildmode`, such as `buildmode = "pie"`. Give `tags` for build tags.

## Check the result

pekit prints the program's path. Change one source file and build again:
pekit compiles that file's package, and the packages that import it,
again, and finds every other package in the store. A change that leaves a
package's archive the same, such as a new comment, compiles nothing that
imports it.

## How planning works

The go command plans the build: `go list` lists each package a program
needs, with its files and imports. Each package is then a step of its own,
compiled as the go command compiles it. `compile` compiles its Go files,
`asm` its assembly, and `pack` adds the objects to its archive. A step
holds only the package's own files and the archives of the packages it
imports.

The go command reads the sources' imports to plan, so an edit plans again.
Planning is quick, and a package whose files and imports didn't change is
found in the store.

The standard library is built once for each configuration, by the go
command, as one step.

## Tests

`go.tests(ws, package)` gives a package's test program, as `go test -c`
builds it, or nothing for a package with no test files. `go.run_test(test)`
runs one as a step, in the package's directory, as `go test` does:

```gcsl
def test store(tests: List<go.GoTest> = go.tests(module(), "./internal/store")) {
    let ran = for each in tests { = go.run_test(each) }
}
```

Give `run_test` what a test reads in `given`, its variables in `env`, and
its arguments in `args`, such as `-test.run=TestOpen`. A test program is a
static program, as `go test` links one, whatever the workspace's build
mode.

## Vet and the race detector

`go.vet(ws)` runs `go vet` on every package, and `go.race(ws)` runs every
package's tests with `go test -race`. Each is one step, which fails as the
go command does. The race detector links with C, so `race` uses the
registered C toolchain:

```gcsl
def test vet(checked: {} = go.vet(module())) {}

def test race(checked: {} = go.race(module())) {}
```

Give either `patterns` to check only some packages, such as
`patterns = ["./internal/..."]`.

## Embedded files

A package's `//go:embed` files are placed with it, and its compile is given
the go command's own record of what each pattern matches. An edit to an
embedded file compiles its package again.

## What it doesn't build

These are errors, which say so:

- A package with cgo files.
- A package of a module that `go.mod` replaces with a local directory.

Everything is pinned, so a strict build gives the same program, byte for
byte, on any machine. See [Hermetic and Strict
Builds](~pekit/hermetic-and-strict-builds).
