---
title: Releasing
type: how-to
description: Release a package with pekit release, which builds it from a
  committed tree strictly and reproducibly with release secrets, publishes
  it, and tags the commit it was built from.
related:
  - pekit/shipping/packaging
  - pekit/concepts/hermetic-and-strict-builds
  - pekit/building/using-secrets
---

`pekit release` makes a release of one package. It builds the package as a
release should be built, publishes it, and tags the commit it was built
from. Everything a release depends on is pinned, so anyone with the commit
can build the same bytes.

## Before you start

- A workspace in a git repository, with every change committed.
- Every library pinned in `pekit.lock`. See [Pinning
  Libraries](~pekit/pinning-libraries).
- Steps that use only pinned toolchains, since a release is a strict
  build.
- The release profile's secrets in the keychain, if the package needs any.
  See [Using Secrets](~pekit/using-secrets).
- A packager registered in `pekitconf.packagers`. See
  [Packaging](~pekit/packaging).

## Release a package

1. Commit everything, and check that `git status` is clean.

2. Release the package into its store:

   ```sh
   pekit release dev.example.greet --to /srv/artifacts
   ```

## Check the result

pekit reports what it published and the tag it added:

```text
published dev.example.greet@1.0-1 with ppk into /srv/artifacts
tagged dev.example.greet@1.0-1 at 4834d15186dbf67463ca1410d67bc578faca1f4e
```

`git tag` lists the new tag. Push it to share the release.

## What a release is built from

A release is built from a commit, never from your working tree. The
workspace must match the repository's `HEAD`: every file committed, and
nothing changed or added, except the build's own `pekit-out/` and `.gcsl/`.
If any file differs, pekit names it and stops.

pekit then exports the workspace's tree at `HEAD` into a directory of its
own, and builds there. So a file that git ignores never reaches a release.

## How a release is built

A release builds as `pekit build --strict --check-reproducible` would, with
the keychain's `release` profile:

- **No host tools.** Its steps run their programs from pinned toolchains,
  and a target's own code only fetches, unpacks, asks the keychain, and
  reads the workspace and the build's outputs. See [Hermetic and Strict
  Builds](~pekit/hermetic-and-strict-builds).
- **Pinned libraries.** Its libraries are exactly those `pekit.lock` pins.
- **Reproducible steps.** Every step runs a second time, and a step that
  gives other outputs from the same inputs fails the release.
- **Release secrets.** Secrets come from the `release` profile, so a release
  signs with its own keys.

## Publishing and tagging

Once the package is built and checked, pekit publishes it with each of the
workspace's packagers into the store `--to` names. Then it tags `HEAD` as
`NAME@VERSION`, the package's name and version: `dev.example.greet@1.0-1`.
The tag is a lightweight tag, so it needs no git identity.

A version is released from one commit. If its tag is on another commit,
pekit refuses before publishing anything: give the new release a version of
its own. Releasing again from the commit that has the tag publishes the
same package again, and adds no tag. pekit only ever adds tags. It never
moves or deletes one.
