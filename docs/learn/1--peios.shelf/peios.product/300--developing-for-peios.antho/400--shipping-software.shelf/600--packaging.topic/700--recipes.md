---
title: Recipes
type: concept
description: What a pekit recipe is, covering recipe workspaces and the
  include file, sources, builds and packages, signed kernel modules, how a
  package's version is made, why a package builds the same way every time,
  and what a release records.
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

`workspace.gcsl` can also give pekit's settings for the workspace, as a
field named `pekitconf` of the type `PekitConfig`. Its one setting,
`artifact_dir`, is the directory pekit packs packages into, relative to
the workspace's root:

```gcsl
pekitconf: PekitConfig = { artifact_dir = "_artifacts" }
```

A field of `workspace.gcsl` belongs to the whole workspace, so no recipe
file can define `pekitconf` too.

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

A source can instead be a source tree on your machine, such as a checkout
of a project you are working on. Give its path, relative to the
workspace's root unless absolute:

```gcsl
def source ppk-checkout() {
    local_path = "../ppk"
}
```

A build works on a copy of the tree, so it never changes the tree itself.
In a Git work tree, the copy holds the files Git would commit, as they are
on disk: uncommitted changes are built, and files that Git ignores, such
as earlier build output, are not. Anywhere else it holds everything. Nothing
pins what a local tree holds, so packages built from one cannot be
released.

A **build** turns the unpacked source into an install tree laid out as
`/usr`. A `defmeta` gives its metadata: the source it builds and the
packages it needs. pekit calls it with three arguments:

- `source_path`: the build's copy of the source.
- `out_path`: an empty directory to install into.
- `arch`: the architecture it builds for.

It runs shell commands with `sh` and returns the install tree:

```gcsl
defmeta build busybox {
    source = source busybox-net-upstream-tarball
    dependencies = []
}

def build busybox(source_path, out_path, arch) {
    sh("make -C {source_path} defconfig")
    sh("make -C {source_path} -j$(nproc)")
    sh("make -C {source_path} CONFIG_PREFIX={out_path} install")
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

## Signed kernel modules

The Peios kernel loads a module only if it carries a signature by the
module-signing key the kernel was built to trust. A package that holds
kernel modules gives `sign_modules = true`, and pekit signs every module
in it before packing:

```gcsl
def package dev.peios.kernel-modules() {
    build = build kernel
    files = {
        "lib/*" = "modules/lib/*"
    }
    dependencies = []
    sign_modules = true
}
```

pekit signs after the build, so the private key never reaches a build's
commands, and the install tree a build returns, which pekit caches, stays
unsigned. Each module must be compressed with zstd and named `.ko.zst`,
because the signature has to be inside the compression. pekit decompresses
each module, appends its signature, and compresses it again. pekit refuses
a package that holds an uncompressed `.ko` file.

A signature depends only on the module and the key. So signing the same
module with the same key always gives the same bytes, and a signed package
can be built again identically, as a release requires.

pekit reads the key from `peios/keys/modsig.pem` in your configuration
directory, `$XDG_CONFIG_HOME` or `~/.config`, or from the path in
`PEKIT_MODSIG_KEY`. The file holds the ML-DSA-65 private key,
including its seed, followed by its certificate, both in PEM.

## Builds that repeat

A packed version never changes, so building a version again has to give
the same package. pekit sets `SOURCE_DATE_EPOCH` for every build to the
time of the newest file in the unpacked source. Build tools that stamp a
time into what they make use it instead of the clock, so the same recipe
and source give the same package however often they are built. If a
package's version is already packed, pekit does not build it again.

pekit 0.1.0 runs builds on the host, with the host's tools, and refuses
recipes that give build or runtime `dependencies`.

## Cached builds

`pekit package` keeps what builds make, so that a build runs only when
something it depends on has changed, and then redoes as little as it can.

pekit keeps the install tree each build returns, under a key made of
everything pekit gives the build: the recipe file the build is in and the
files it imports, the source's content and `SOURCE_DATE_EPOCH`, the
architecture, and pekit's version. When a package is built and its build's
key has a tree kept, pekit packs the package from that tree without running
the build. The key can't see what a build uses from the host, such as its
compiler. If that changes, `pekit package --rebuild` runs the build again.

Each build also has a work directory that pekit keeps between runs. It
holds the build's copy of its source, which pekit syncs before each run.
A file is rewritten only if its content or execute bit changed, or the
build itself changed it, so every other file keeps its modification time.
A file is deleted only if it was deleted from the source. Whatever a build
makes in its copy of the source, or beside it, stays for the next run, so
a build tool that redoes only what changed, such as `make`, does little
work after a small change. `--rebuild` empties the work directory first.

A local source's `SOURCE_DATE_EPOCH` is the time of its newest file, so
touching a file, as switching Git branches does, changes the key even if
the content is the same. The build then runs again, but in its kept work
directory, where the content hasn't changed, so it has little to redo.

## Releases

A recipe workspace is kept in Git, and Git holds only the recipes. The
packages themselves live in artifact directories and repositories, which
are what systems install from.

A release records which commit made each package. `pekit release` builds
packages from a workspace with no uncommitted changes, packs them, and
tags the commit with each package's name and version, such as
`net.busybox.busybox@1.37.0-1`. One tag for each package means that the
commit that made any package's version can be found from the package
alone, whichever recipe defines it.

A release builds every package, even one already packed, and packing
accepts it only if it is identical to what is packed. A release doesn't
use cached builds: each build runs from nothing in a new work directory.
Releasing a version again therefore proves that the current commit still
makes it from its source alone. If the
recipe has changed in a way that changes a package, the release fails
until its `revision` is raised. Tags are only added: when a later commit
makes the same package, the tag stays at the first commit that made it.
