---
title: Releasing
type: guide
description: Build a package from a commit, strictly and reproducibly, publish it, and tag the release with pekit release.
---

`pekit release` makes a release of one package: it builds it as a
release should be built, publishes it, and tags the commit it was built
from.

```sh
pekit release dev.example.greet --to /srv/artifacts
```

```text
published dev.example.greet@1.0-1 with ppk into /srv/artifacts
tagged dev.example.greet@1.0-1 at 4834d15186dbf67463ca1410d67bc578faca1f4e
```

## What a release is built from

A release is built from a commit, never from your working tree. The
workspace must be in a git repository and match its `HEAD`: every file
committed, nothing changed or added, but the build's own `pekit-out/`
and `.gcsl/`. Pekit says which files differ if any do. It then exports
the workspace's tree at `HEAD` into a directory of its own and builds
there, so a file that git ignores never reaches a release.

## How a release is built

A release builds as `pekit build --strict --check-reproducible` would,
with the keychain's `release` profile:

- Its steps borrow nothing of the host, so they run their programs from
  pinned toolchains, and a target's own code only fetches, unpacks, asks
  the keychain, and reads the workspace and the build's outputs (see
  ~pekit/building-a-target).
- Its libraries are exactly those `pekit.lock` pins.
- Every step runs a second time, and a step that gives other outputs from
  the same inputs fails the release.
- Secrets come from the `release` profile, so a release signs with its
  own keys.

## Publishing and tagging

Once the package is built and checked, pekit publishes it with each of
the workspace's packagers into the store `--to` names (see
~pekit/packaging), and tags `HEAD` `NAME@VERSION`, the package's name and
version: `dev.example.greet@1.0-1`.

A version is released from one commit. If its tag is on another commit,
pekit refuses before publishing anything: give the new release a version
of its own. Releasing again from the commit that has the tag publishes
the same package again, and adds no tag. Pekit only ever adds tags, and
never moves or deletes one.
