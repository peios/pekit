---
title: Recording Corresponding Source
type: how-to
description: Record what each recipe's packages are built from in
  pekit.sources with pekit sources, so that a package of the source can be
  built from it, and keep it current, which a strict publish checks.
related:
  - pekit/shipping/packaging
  - pekit/shipping/releasing
  - pekit/building/fetching-files
---

A package that ships a binary often has to come with its corresponding
source: everything the binary was built from, as the GPL asks of anyone
who distributes a GPL program, say. pekit works that out for you. It
records what each recipe's packages are built from in `pekit.sources`, a
file at the workspace's root, beside `pekit.lock`. A package of a recipe's
source is then built from that record alone, so it's the same wherever
it's built, and a change to a recipe's source shows in review.

## What a recipe's source is

A recipe is a directory of package definitions, such as `org.gnu.bash`.
Its source is everything a build of all its packages used, except tools:

- each file its build fetched, by URL and SHA-256;
- each git commit its build fetched, by repository and commit;
- each file and directory of the workspace that its code read, that its
  steps were given, or that its packages hold, by its path in the
  workspace. The GCSL code itself is among them, with `workspace.gcsl`
  and `pekit.lock`, which loading the workspace takes;
- each library whose code it ran. A library that `pekit.lock` pins is
  recorded whole, by its import path and its directory, since a build
  restores a pinned library whole. One the lock doesn't pin is recorded
  by the files of its code alone.

A tool isn't source:

- What a registered toolchain is made from: its definition's fetches, and
  everything fetched to make it, such as the source of a tool it compiles.
- A fetch or a `fetch_git` marked `tool = true` (see [Fetching
  Files](~pekit/fetching-files#mark-a-tool)).
- A path that leads out of the workspace, such as a link to a repository
  of packages that build roots are taken from.

Packing isn't part of a recipe's source either: pekit builds a recipe's
packages without packing them to record it, so a packager's own program
isn't counted.

Nor is what the package of the source holds. A package whose own code reads
`pekit.sources` is built from the record, so pekit counts its code but not
what it reads, fetches or holds. Otherwise a file the record once named
would stay recorded after the recipe stopped using it, or even after it was
deleted, since the package of the source holds it.

## Before you start

- A workspace whose packages build strictly, if the workspace requires
  `--strict`. See [Hermetic and Strict
  Builds](~pekit/hermetic-and-strict-builds).
- Every tool your builds fetch outside a toolchain marked `tool = true`.

## Record the sources

1. Run `pekit sources` with the packages whose recipes to record, or none
   to record every recipe below this directory:

   ```sh
   pekit sources --strict 'org.gnu.bash/**'
   ```

   pekit builds each recipe's packages on their own, every package of the
   recipe whichever you named, and records what each build used, with the
   names of the recipe's packages:

   ```text
   recorded org.gnu.bash: 21 fetched, 0 git, 15 files, 9 directories
   ```

2. Review `pekit.sources`, and commit it with the change that made it.

A recipe's record replaces what was recorded of it. Recipes you didn't name
keep theirs.

## Keep it current

Run `pekit sources` again whenever a recipe's source changes: a new
version, a new patch, a build that reads another file, or a new package. A
strict publish or a release checks the record of each recipe it
publishes, and publishes nothing if one isn't what the recipe's build uses
and the packages it has:

```text
error: nothing is published, since `pekit.sources` does not record what org.gnu.bash is built from: run `pekit sources --strict` for it, and review the change
```

A workspace with no `pekit.sources` publishes without the check.

## Check the result

Each recipe in `pekit.sources` lists its `packages`, its `fetched` files,
its `git` commits, the `libraries` whose code it ran, and the workspace's
`files` and `dirs` it read, each sorted:

```json
{
  "recipe": "org.gnu.bash",
  "packages": ["org.gnu.bash", "org.gnu.bash-doc"],
  "fetched": [
    {
      "url": "https://ftpmirror.gnu.org/bash/bash-5.3.tar.gz",
      "sha256": "0d5cd86965f869a26cf64f4b71be7b96f90a3ba8b3d74e27e8e9d9d5550f31ba"
    }
  ],
  "git": [],
  "libraries": ["github.com/gcslib/ppk"],
  "files": [
    "org.gnu.bash/build.gcsl",
    "pekit.lock",
    "recipe.gcsl",
    "workspace.gcsl"
  ],
  "dirs": [".gcsl/libraries/github.com/gcslib/ppk", "org.gnu.bash/patches"]
}
```

A directory stands for everything in it. A package of the
source fetches each file again by its digest, which the store answers, and
takes the commits and the workspace's files as they are.

A build from a package of the source restores every library that its
`pekit.lock` pins, and fetches any that it lacks. So give the package a
lock that pins only the recipe's `libraries`, beside their directories.
It then builds with no network, from the store's tools and what the
package holds.
