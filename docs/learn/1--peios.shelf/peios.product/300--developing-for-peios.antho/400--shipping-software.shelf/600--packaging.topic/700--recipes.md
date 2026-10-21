---
title: Recipes
type: concept
description: What a pekit recipe is, covering recipe workspaces and the
  include file, sources, builds and packages, how a package's version is
  made, and why a package builds the same way every time.
related:
  - peios/packaging/building-a-package-from-a-recipe
  - peios/packaging/packages
---

A recipe says how to make Peios packages from an upstream project: where
its source comes from, how to build it, and which of the built files each
package ships. `pekit` reads recipes, builds them, and packs the results
with `ppk`. For what a package is, see
[Packages](~peios/packaging/packages).

Recipes are written in GCSL, so an editor with the GCSL language server
checks them as you write them.

## Recipe workspaces

Recipes live in a recipe workspace: a GCSL workspace, which is a directory
tree with a `workspace.gcsl` file at its root. By convention, each upstream
project has a directory named like its main package, holding a
`pekit.gcsl`:

```text
pkgs/
  workspace.gcsl
  pekit-include.gcsl
  net.busybox.busybox/
    pekit.gcsl
```

`pekit-include.gcsl` holds pekit's declarations of the kinds of definition
a recipe makes. `pekit init` writes it, and every directory of the
workspace sees its declarations without importing them. pekit refuses to
work in a workspace whose copy differs from its own, so change it only by
running `pekit init`. `workspace.gcsl` is an ordinary GCSL file that
belongs to you.

## What a recipe defines

A recipe file starts with three fields:

- `pekit = 1`: the recipe format the file is written for.
- `version`: the upstream version, such as `"1.37.0"`.
- `revision`: the recipe's own revision of that version, such as `"1"`.
  pekit appends it to the version, so the package is version `1.37.0-1`.
  Raise it to ship a changed recipe for the same upstream version.

It then makes three kinds of definition.

A **source** gives the URL of an upstream archive and the SHA-256 of what
the URL serves. pekit fetches the archive, checks it, and unpacks it, so a
build needs no network and the recipe pins exactly what it builds:

```gcsl
def source busybox-net-upstream-tarball() {
    url = "https://busybox.net/downloads/busybox-{version}.tar.bz2"
    sha256 = "3311dff32e746499f4df0d5df04d7eb396382d7e108bb9250e7b519b837043a4"
}
```

A **build** turns the unpacked source into an install tree laid out as
`/usr`. A `defmeta` gives its metadata: the source it builds and the
packages it needs. pekit calls it with three arguments:

- `source_path`: the unpacked source.
- `out_path`: an empty directory to install into.
- `arch`: the architecture it builds for.

It runs shell commands with `sh` and returns the install tree:

```gcsl
defmeta build busybox {
    source = source busybox-net-upstream-tarball
    dependencies = []
}

def build busybox(source_path, out_path, arch) {
    let _ = sh("make -C {source_path} defconfig")
    let _ = sh("make -C {source_path} -j$(nproc)")
    let _ = sh("make -C {source_path} CONFIG_PREFIX={out_path} install")
    = out_path
}
```

A **package** names its build and maps each path it ships under `/usr` to
the path in the install tree it comes from. A last segment of `*` on both
sides takes every entry of a directory:

```gcsl
def package net.busybox.busybox() {
    build = build busybox
    files = {
        "bin/*" = "bin/*"
        "sbin/*" = "sbin/*"
    }
    dependencies = []
}
```

Several packages can take different files from one build.

Recipes and packages have separate names. A package's name is the reverse
domain name of its upstream project, such as `net.busybox.busybox`. A
source's or a build's name only has to be unique among the workspace's
sources or builds.

## Builds that repeat

A packed version never changes, so building a version again has to give
the same package. pekit sets `SOURCE_DATE_EPOCH` for every build to the
time of the newest file in the unpacked source. Build tools that stamp a
time into what they make use it instead of the clock, so the same recipe
and source give the same package however often they are built. If a
package's version is already packed, pekit does not build it again.

pekit 0.1.0 runs builds on the host, with the host's tools, and refuses
recipes that give build or runtime `dependencies`.
