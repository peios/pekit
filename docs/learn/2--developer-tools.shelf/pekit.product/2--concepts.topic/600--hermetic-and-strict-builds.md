---
title: Hermetic and Strict Builds
type: concept
description: What makes a pekit build hermetic, how pekit keys steps that
  use the host's tools by a fingerprint of its packages, what a strict
  build refuses, and how pekit checks that a build is reproducible.
related:
  - pekit/concepts/platforms-and-toolchains
  - pekit/building/checking-reproducibility
  - pekit/shipping/releasing
---

A build is hermetic when its results depend only on its inputs: the
workspace, the libraries it pins, and the files it fetches by their
digests. A hermetic build gives the same results on any machine of the same
platform, which is what lets you trust a release, or share results between
machines.

Every step runs in a sandbox, but a sandbox alone doesn't make a build
hermetic. By default a step can still run the host's own programs, and
those differ from machine to machine.

## Builds that use the host's tools

Outside a strict build, every step can see the host's `/usr`, `/bin`,
`/lib`, `/lib64`, and `/sbin`. A step that runs `cc` from there depends on
which compiler is installed. pekit can't tell which steps used the host's
tools, so after any build that ran steps and wasn't strict, it says the
build isn't hermetic:

```text
note: steps ran with the host's own tools, so this build is not hermetic
```

So that upgrading a package doesn't leave stale results in the store,
pekit keys every such step by a fingerprint of the host. The fingerprint is
a digest of `/etc/os-release`, of the package manager's database, and of
the parts of `/etc` that choose programs, such as `/etc/alternatives`.
Installing, upgrading, or removing any package changes it, and the steps
run again.

pekit knows the package databases of dpkg, rpm, apk, and pacman. On a host
with none of them, it can't tell when the tools change, so it warns, and
doesn't reuse any step from the store:

```text
warning: the host's packages cannot be identified, so no step is reused from the store
```

Programs in `/usr/local` aren't managed by a package manager, so a change
to them would change no fingerprint. Steps don't see `/usr/local` at all.

## Strict builds

`--strict` builds as a release should: from what the workspace pins, and
nothing of the machine it runs on.

- **Steps see none of the host's tools.** A step has to run its programs
  from a pinned toolchain or build environment. The host's own toolchains,
  such as `system_c`, fail a strict build.
- **A target's own code can only fetch, unpack, read, and ask for
  secrets.** It can read the workspace and the build's outputs. It can't
  run a program, write a file, or read anything else of the host. Each of
  these fails where the target does it.
- **Every library has to be pinned.** The build fails unless its libraries
  are exactly those `pekit.lock` pins, and `$GCSL_LIBRARY_PATH` is unset.
  See [Pinning Libraries](~pekit/pinning-libraries).
- **Scratch directories start empty.** A step marked `#[scratch]` gets an
  empty directory every time, so nothing kept from an earlier run can reach
  what the build gives.

A strict build's steps aren't keyed by the host's fingerprint. Their keys
are the same on any machine of the platform, so a strict build's results
can be shared between machines.

## Reproducible builds

A step is reproducible if it gives the same outputs, byte for byte, every
time it runs with the same inputs. A hermetic build can still have a step
that isn't, such as one that writes the current time into its output.

`--check-reproducible` finds them. It builds, then runs every step again,
reusing nothing from the store, and compares what each gave the second
time with the first. A step that gave other outputs fails the command. See
[Checking That a Build Is
Reproducible](~pekit/checking-reproducibility).

`pekit release` builds strictly and checks that the build is reproducible
before it publishes anything. See [Releasing](~pekit/releasing).
