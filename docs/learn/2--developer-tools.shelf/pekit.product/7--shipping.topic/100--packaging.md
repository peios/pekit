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

## Give a package its metadata

A package can carry more than its files. pekit keeps any other field of a
package's body and never reads it, so each packager defines the fields it
takes. The ppk packager reads a field named `ppk`, and records what it holds
in the package's manifest:

```gcsl
def package dev.example.greet-dev() {
    let g = build greet()
    version = version
    files = {"include" = g.headers; "lib/libgreet.a" = g.archive}
    ppk = {
        description = "The greet library, for developers"
        license = "MIT"
        license_class = "free"
        homepage = "https://example.com/greet"
        dependencies = {"dev.example.greet" = "= {version}"}
    }
}
```

`ppk` has two kinds of field:

- **Single values,** each a string: `description`, `homepage`, `license`,
  `license_class`, and `source_license`.
- **Tables,** each a record of strings:
  - `dependencies`: the packages this one needs, each with a version
    constraint.
  - `requires`: the capabilities it needs, such as `sh`, each with a
    constraint.
  - `provides`: the capabilities it offers, each with a version.
  - `claims`: links it owns outside its files, each an absolute path and
    the link's target, such as `"/usr/bin/sh" = "dash"`.
  - `descriptors`: files and directories of the package that get a
    security descriptor of their own, each a path as `files` gives it and
    the descriptor in hexadecimal.

An empty string means any version, or no version. ppk checks every value as
`ppk pack` does, and finds an ELF program's libraries and their floors
itself, so don't write those. A field ppk has no form for is an error
naming it, so a misspelt field never goes missing quietly. A package
without `ppk` is packed with no metadata, as before.

A signed file, such as firmware, takes its signature from a file beside it
named with `.peios.sig` added. Place the signature in `files` like any other
file, or in the directory you place. `ppk pack` records it in the signed
file's entry and doesn't pack it as a file:

```gcsl
def package dev.example.firmware() {
    version = "1.0-1"
    files = {
        "lib/firmware/acme/fw.bin" = file("fw.bin")
        "lib/firmware/acme/fw.bin.peios.sig" = file("fw.bin.peios.sig")
    }
    ppk = {descriptors = {"lib/firmware/acme" = "0100048014000000..."}}
}
```

The descriptor is shortened here. What ppk requires of a descriptor or a
signature is under SECURITY METADATA in `ppk(1)`.

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

## Publish strictly

A strict build refuses a target's own effects, and publishing is one. To
publish what a strict build packs, pass `--strict` to `pekit publish`:

```sh
pekit publish 'pkgs/**' --to /srv/artifacts --strict
```

pekit packs every package strictly, then publishes them, taking every pack
from the store. If any package fails to pack strictly, pekit publishes
none, and says how many failed:

```text
error: nothing is published, since 1 of 12 packages did not pack strictly
```

## Replace a published version

ppk refuses to publish a version whose contents changed, because something
might already have fetched the old contents. A repository that nothing has
fetched from yet, such as one being bootstrapped, can need a version
rebuilt and published again. For that, give the ppk packager
`replace = true` in `workspace.gcsl`:

```gcsl
pekitconf = {
    packagers = [packager(replace = true)]
}
```

The packager then publishes with `ppk fetch --replace`, and each line pekit
prints names it:

```text
published example/dev.example.greet with ppk --replace
```

> [!CAUTION]
> A replaced version breaks anything that fetched the old one. Use
> `replace = true` only on a repository nothing has fetched from, and don't
> commit it.

## Write a packager

Each packager is a `packaging.Packager`, from GCSL's standard `packaging`
module. It has a `name`, a `pack` function of a package to the directory it
packed it into, and a `publish` function of a package, that directory, and a
store. A library for another format gives a packager of its own, and a
workspace lists it in `pekitconf.packagers` beside ppk's.