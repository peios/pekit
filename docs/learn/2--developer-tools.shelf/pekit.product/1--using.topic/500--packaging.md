---
title: Packaging
type: guide
description: Describe packages with the package definition type, pack them with a workspace's packagers, and publish them.
---

A package says what it contains, not how a format stores it. Write one
with GCSL's `package` definition type, beside the build it ships, such as
in a `package.gcsl` next to `build.gcsl`:

```text
let version = "1.0-1"

def package dev.example.greet() {
    version = version
    files = {"bin/greet" = build app()}
}

def package dev.example.greet-dev() {
    let g = build greet()
    version = version
    files = {"include" = g.headers; "lib/libgreet.a" = g.archive}
}
```

The definition's path is the package's name. Each of its `files` is at
its path under `/usr`: a file at its path, a list of files under it by
their names, and a directory's tree under it. Files in one directory see
each other's definitions, so `build app()` needs no import.

## Packing with ppk

The ppk library packs a package as `ppk pack` does, as a step of its
own:

```text
import "github.com/gcslib/ppk"

def build packages() {
    greet = ppk.pack(package dev.example.greet())
    greet_dev = ppk.pack(package dev.example.greet-dev())
}
```

`ppk.pack` gives the artifact directory of one package, for the
architecture of `^platform`, packed into a new directory, so the same
files and version always give the same package. A package whose files
didn't change isn't packed again: changing `main.c` repacks
`dev.example.greet` and leaves `dev.example.greet-dev` alone, and
changing the version alone repacks both and recompiles nothing.

The ppk library asks the `toolchain` module for a `Toolchain::Ppk`, so a
workspace registers the `ppk` tool it uses. A definition's files are
found from the directory of the code that runs it, so name a tool kept
beside `workspace.gcsl` with `fileroot.this`:

```text
import * from "github.com/gcslib/ppk"
import * from toolchain
import fileroot
import path

def Toolchain::Ppk workspace_ppk() {
    let tools = dir(path.join(fileroot.this, "tools"))
    host_platform = ^host_platform
    platform = {}
    provides = {root = tools; runtime = dir("/"); ppk = tools.file("ppk")}
}

pekitconf = {
    toolchains = [Toolchain::C system_c, Toolchain::Ppk workspace_ppk]
    packagers = [packager()]
}
```

## Packing with the workspace's packagers

A workspace lists the packagers it uses in `pekitconf.packagers`, as it
does its toolchains. `ppk.packager()`, above, is ppk's. `pekit package`
then packs each package it's given with each of them, every package below
the current directory if it's given none:

```text
$ pekit package
packed example/dev.example.greet with ppk
  pekit-out/example/dev.example.greet/pack/artifacts
packed example/dev.example.greet-dev with ppk
  pekit-out/example/dev.example.greet-dev/pack/artifacts
6 steps run, 0 found in the store
```

It names packages as `pekit build` names targets: `pekit package
dev.example.greet`, or `pekit package pkgs/*`.

## Publishing

Copying a package into a store is publishing. It changes the world
outside the build, so it's never a step: it's done each time, in order,
after the packing it needs. `pekit publish` packs each package it's given,
as `pekit package` does, and publishes it with each packager into the
store `--to` names:

```text
$ pekit publish dev.example.greet --to /srv/artifacts
published example/dev.example.greet with ppk
  pekit-out/example/dev.example.greet/pack/artifacts
0 steps run, 5 found in the store
```

Packing doesn't depend on the store, so publishing what you've packed,
or the same package into a second store, packs nothing again. ppk
publishes into an artifact directory with `ppk fetch`, which refuses a
version whose contents changed: publish a new version instead.

Each packager is a GCSL `packaging.Packager` of the standard `packaging`
module: a `name`, a `pack` function of a package to the directory it
packed it into, and a `publish` function of a package, that directory
and a store. A library for another format gives a packager of its own.
