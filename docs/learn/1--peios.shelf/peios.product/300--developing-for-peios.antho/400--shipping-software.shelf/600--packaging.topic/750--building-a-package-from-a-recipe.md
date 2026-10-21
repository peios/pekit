---
title: Building a Package from a Recipe
type: how-to
description: Set up a recipe workspace with pekit init, build and pack a
  package with pekit package, release packages with pekit release, and fix
  a source whose hash does not match.
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
  `/system/artifacts`; to use a directory of your own, set one for the
  workspace or pass `--artifact-dir`.

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
3. To pack into a directory of the workspace rather than
   `/system/artifacts`, set it in `workspace.gcsl`:

   ```gcsl
   pekitconf: PekitConfig = { artifact_dir = "_artifacts" }
   ```

   The path is relative to the workspace's root. If the workspace is
   under version control, have it ignore the directory.

## Build a package

Run `pekit package` with the package's name, from anywhere in the
workspace:

```sh
pekit package net.busybox.busybox
```

To pack into another directory for one run, pass `--artifact-dir`,
which overrides the workspace's setting.

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
architecture is the host's. From the workspace's root, read its ref:

```sh
cat _artifacts/refs/net.busybox.busybox/1.37.0-1/x86_64
```

It holds the identity that `pekit package` printed. Running
`pekit package` again for the same version prints the same identity
without building anything, and says so:

```text
pekit: net.busybox.busybox@1.37.0-1?arch=x86_64 is already packed
```

## Release packages

When a recipe change is ready to ship, release its packages from a
workspace kept in Git.

1. Commit every change in the workspace. pekit refuses to release from a
   workspace with uncommitted changes, untracked files included, because
   the tags it makes must name exactly what was built. Files that Git
   ignores, such as the artifact directory, do not count.
2. Release every package the recipe defines, by the recipe's directory:

   ```sh
   pekit release --recipe net.busybox.busybox
   ```

   To release only some packages, name them instead. pekit builds and
   packs each package, then tags the commit, and prints each package's
   tag and identity:

   ```text
   net.busybox.busybox@1.37.0-1 sha256:5181c2230d80666129f50cb472da3f38be40b3566a3d4638fea915f9dea25d83
   ```

3. Push the tags with Git. pekit makes them only in your repository.

If the release fails because a packed version cannot be replaced, the
recipe now makes something different from what that version already
holds. Raise the recipe's `revision`, commit, and release again. A
failed release makes no tags, but packages it packed before failing stay
packed; releasing them again matches them.

## Fixing a source hash

If the archive at a source's URL does not have the SHA-256 the recipe
gives, pekit stops before building, keeps nothing it fetched, and exits
with status 1. Its message names the URL, the SHA-256 of what it fetched,
and the one the recipe gives.

Check the archive against the hash its upstream publishes. If upstream
has replaced the archive, update the recipe's `sha256`, and raise its
`revision` if that version is already packed.
