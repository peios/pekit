---
title: Packaging
type: how-to
description: Describe packages with the package definition type, pack them
  with the workspace's packagers such as ppk, and publish them into a
  package store with pekit publish.
related:
  - pekit/shipping/releasing
  - pekit/shipping/building-an-image
---

A package says what it contains, not how a format stores it. You write
packages with the `package` definition type, beside the builds they ship.
The workspace's packagers turn each into a file of their format, such as a
Peios package, and publish it into a store.

## Before you start

- The ppk library, and a `ppk` program to register as its tool. See
  [Libraries](~pekit/libraries).
- A target that builds what you want to ship.

## Describe a package

In a build file beside the build it ships, such as `package.gcsl` next to
`build.gcsl`, declare the package:

```gcsl
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

The definition's path is the package's name. Each of its `files` is placed
at its path under `/usr`: a file at its path, a list of files under it by
their names, and a directory's tree under it. Files in one directory see
each other's declarations, so `build app()` needs no import.

## Register the packager

1. In `workspace.gcsl`, register the `ppk` tool as a toolchain, and the ppk
   packager in `pekitconf.packagers`:

   ```gcsl
   import * from "github.com/gcslib/ppk"
   import * from toolchain

   def Toolchain::Ppk workspace_ppk() {
       let tools = dir("tools")
       host_platform = ^host_platform
       platform = {}
       provides = {root = tools; runtime = dir("/"); ppk = tools.file("ppk")}
   }

   pekitconf = {
       toolchains = [Toolchain::C system_c, Toolchain::Ppk workspace_ppk]
       packagers = [packager()]
   }
   ```

   This keeps the `ppk` program at `tools/ppk` in the workspace. A relative
   path is relative to the file it's written in, so `dir("tools")` in
   `workspace.gcsl` is `tools` at the workspace's root.

2. Pack every package below the current directory:

   ```sh
   pekit package
   ```

## Check the result

pekit packs each package with each packager, and prints where each went:

```text
packed example/dev.example.greet with ppk
  pekit-out/example/dev.example.greet/pack/artifacts
packed example/dev.example.greet-dev with ppk
  pekit-out/example/dev.example.greet-dev/pack/artifacts
6 steps run, 0 found in the store
```

Name packages to pack only those, as `pekit build` names targets:
`pekit package dev.example.greet`, or `pekit package 'pkgs/**'`.

## What reruns

Each packing is a step of its own, so a package whose files didn't change
isn't packed again. Changing `main.c` repacks `dev.example.greet` and
leaves `dev.example.greet-dev` alone. Changing the version alone repacks
both and compiles nothing.

The ppk packager packs a package as `ppk pack` does, for the architecture
of `^platform`, into a new directory, so the same files and version always
give the same package.

## Pack in a build

A build can pack packages itself, with the ppk library's `pack`:

```gcsl
import "github.com/gcslib/ppk"

def build packages() {
    greet = ppk.pack(package dev.example.greet())
    greet_dev = ppk.pack(package dev.example.greet-dev())
}
```

`ppk.pack` gives the artifact directory of one package.

## Publish packages

Copying a package into a store is publishing. It changes things outside the
build, so it's never a step: it happens on each run, in order, after the
packing it needs. `pekit publish` packs each package it's given, as `pekit
package` does, then publishes it with each packager into the store `--to`
names:

```sh
pekit publish dev.example.greet --to /srv/artifacts
```

```text
published example/dev.example.greet with ppk
  pekit-out/example/dev.example.greet/pack/artifacts
0 steps run, 5 found in the store
```

Packing doesn't depend on the store, so publishing what you've packed, or
the same package into a second store, packs nothing again. ppk publishes
into an artifact directory with `ppk fetch`, which refuses a version whose
contents changed: publish a new version instead. If one package fails to
publish, the packages published before it stay published.

## Write a packager

Each packager is a `packaging.Packager`, from GCSL's standard `packaging`
module. It has a `name`, a `pack` function of a package to the directory it
packed it into, and a `publish` function of a package, that directory, and a
store. A library for another format gives a packager of its own, and a
workspace lists it in `pekitconf.packagers` beside ppk's.