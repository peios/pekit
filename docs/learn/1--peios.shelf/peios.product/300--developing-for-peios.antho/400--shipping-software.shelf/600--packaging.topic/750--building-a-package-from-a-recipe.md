---
title: Building a Package from a Recipe
type: how-to
description: Set up a recipe workspace with pekit init, build and pack a
  package with pekit package, and fix a source whose hash does not match.
related:
  - peios/packaging/recipes
  - peios/packaging/creating-a-package
---

pekit builds a package from its recipe and packs it into an artifact
directory. Do this to make a package from an upstream project rather than
from files you have staged yourself. For what a recipe contains, see
[Recipes](~peios/packaging/recipes).

## Before you start

- Install `pekit`, and the tools the recipe's build runs, such as a C
  compiler and `make`. pekit itself runs `curl` and `tar`.
- Have write access to the artifact directory. The default is
  `/system/artifacts`; to use a directory of your own, pass
  `--artifact-dir`.

## Set up a recipe workspace

1. In the directory that holds your recipes, run:

   ```sh
   pekit init
   ```

   This writes `pekit-include.gcsl`, and an empty `workspace.gcsl` if
   there is none. An existing `workspace.gcsl` is left as it is.
2. Run `pekit init` again whenever you upgrade pekit. Other pekit commands
   refuse a workspace whose `pekit-include.gcsl` is not the installed
   pekit's.

## Build a package

Run `pekit package` with the package's name, from anywhere in the
workspace:

```sh
pekit package net.busybox.busybox --artifact-dir artifacts
```

pekit fetches the build's source, unless its cache holds it already, and
checks its SHA-256. It unpacks the source, runs the build, copies the
files the package chooses, and packs them. It prints the package's
identity:

```text
sha256:5181c2230d80666129f50cb472da3f38be40b3566a3d4638fea915f9dea25d83
```

To build from a workspace you are not in, pass `--workspace` with its
directory.

## Check the result

The package's version is the recipe's `version` and `revision`, and its
architecture is the host's. Read its ref:

```sh
cat artifacts/refs/net.busybox.busybox/1.37.0-1/x86_64
```

It holds the identity that `pekit package` printed. Running
`pekit package` again for the same version prints the same identity
without building anything, and says so:

```text
pekit: net.busybox.busybox@1.37.0-1?arch=x86_64 is already packed
```

## Fixing a source hash

If the archive at a source's URL does not have the SHA-256 the recipe
gives, pekit stops before building, keeps nothing it fetched, and exits
with status 1. Its message names the URL, the SHA-256 of what it fetched,
and the one the recipe gives.

Check the archive against the hash its upstream publishes. If upstream
has replaced the archive, update the recipe's `sha256`, and raise its
`revision` if that version is already packed.
