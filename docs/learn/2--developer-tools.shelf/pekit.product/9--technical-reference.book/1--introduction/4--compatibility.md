---
title: Compatibility
description: The platforms pekit runs on, the programs it needs, and what
  is shared or not between versions and machines.
---

pekit runs on Linux. It needs:

- `bwrap`, bubblewrap, on `PATH`, for every step and every program a
  target's code runs. A host that forbids unprivileged user namespaces, or
  one bubblewrap inside another, runs bubblewraps side by side: pekit never
  nests one in another.
- `git`, on `PATH`, for `pekit lock`, for putting back pinned libraries,
  for `fetch_git`, and for `pekit release`.
- A file system for the cache directory that supports hard links, and on
  which the user can make overlay mounts in a user namespace, for
  compositions mounted into sandboxes.

The execution platform is the machine pekit runs on, named as Rust names
architectures and operating systems: `x86_64-linux`, `aarch64-linux`.
Steps never run on another machine.

## What is shared

The store is shared by every workspace and every pekit process of the same
user on a machine. Its records are found only by the receipts of the same
encoding: a version of pekit that changes how receipts, identities, trees,
or records are encoded finds none of the old ones, and runs the steps
again. Nothing is migrated.

A step's receipt names the execution platform, and outside a strict build
the host snapshot's fingerprint. So a store copied between machines reuses
only what the machines agree on: a strict build's steps on any machine of
the platform, and others only on a machine with the same installed
packages.

A step given a secret is keyed by an identity hashed with the machine's own
key, so it is reused only on the machine that recorded it.
