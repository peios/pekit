---
title: Packing, Publishing, and Releasing
description: How pekit package and pekit publish call the workspace's
  packagers, and the stages of pekit release, from checking the working
  tree to tagging the commit.
---

## Packing

The workspace's packagers are the `packaging` module's `^packagers`, set
from `pekitconf.packagers`. Each is a `packaging.Packager`: a `name`, a
`pack` function of a package to the directory it packed it into, and a
`publish` function of a package, that directory, and a store.

`pekit package` evaluates `packaging.pack(package NAME())` for each package
selected. The module's `pack` calls each packager's `pack`, which is
normally a hermetic step. A workspace with no packager fails with an error
saying to list one in `pekitconf.packagers`.

## Publishing

`pekit publish` evaluates `packaging.publish(package NAME(), "STORE")` for
each package. The module packs the package with each packager, then calls
each packager's `publish` with the packed directory and the store, as the
target's own effects, in program order, outside any step. The store is an
argument rather than a context, so packing doesn't depend on it, and
publishing into a second store packs nothing again.

A package that fails to publish fails alone, as any target does, and the
packages published before it stay published.

## Releasing

`pekit release PACKAGE --to STORE` works in these stages (`release`):

1. **Check the tree.** The workspace has to be in a git repository and match
   its `HEAD`: every file committed, nothing changed or untracked, except
   `pekit-out/` and `.gcsl/`. Otherwise the command fails, naming each file
   that differs.
2. **Export.** The workspace's tree at `HEAD` is exported with `git archive`
   into a directory of its own under the cache, so no file the repository
   ignores, or doesn't hold, reaches the build.
3. **Build strictly.** The package is packed there with `--strict`,
   `--check-reproducible`, and the `release` profile. Its value gives the
   package's name and version.
4. **Check the tag.** The tag is `NAME@VERSION`. If a tag of that name is on
   another commit, the release fails, before anything is published.
5. **Publish.** The package is published with each packager into the store.
   Publishing is a target's own effect, which a strict build refuses, so
   this pass keeps the strict build's step settings, no host tools and the
   same platform key, so that its steps are found in the store, and allows
   the target's effects, which the strict pass checked.
6. **Tag.** `HEAD` is tagged `NAME@VERSION` with a lightweight tag, which
   needs no git identity. If the tag is already on `HEAD`, the package is
   published again and no tag is added.

A tag is only ever added, never moved or deleted.
