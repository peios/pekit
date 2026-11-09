---
title: Checking Pekit
type: how-to
description: Run pekit's own check, give its tests what they need from the
  machine and the Peios workspace, and tell a skipped test from a passing
  one.
---

`tools/check` is the check a change to pekit must pass. It checks style,
runs Clippy with every warning denied, and runs every test of the
workspace and of the modsig library's tool:

```sh
tools/check
```

Some tests take several minutes, such as the one that boots an ISO, and are
ignored unless asked for. `tools/check --all` runs them too.

## What the tests need

Many tests build something real, and need what that build needs on the
machine. They read every file they would fetch from disk, so that no test
downloads anything.

- **Host tools.** A static `busybox` at `/usr/bin/busybox`, `git`, `cc`,
  `ppk` on `PATH`, and `/dev/kvm`. The ISO test also needs QEMU, OVMF,
  `xorriso`, `mkfs.vfat` and mtools.
- **Room for temporary files.** The examples unpack toolchains into
  temporary directories, several gigabytes at once. If `/tmp` is a small
  or shared memory-backed filesystem, set `TMPDIR` to a directory on disk.
- **A user who can't read every file.** One test checks what happens when
  a file can't be read, which it can't arrange as root.
- **The Peios workspace.** The examples take the gcslib libraries from
  their clones in `gcslib/`, the files their builds fetch from
  `local/pekit2-data/`, and the sources of ppk, peiso, peiosutils and
  prelude from the repositories of those names. Each is found in the
  directory pekit's repository is in.

## Skipped is not passed

Cargo can't mark a test as skipped once it has started, so a test that
finds something missing returns early, and Cargo reports it as passed. To
keep that from passing unnoticed, such a test writes a line that Cargo
shows even for a passing test:

```text
SKIPPED, NOT PASSED: example_4_builds_zlib_with_its_own_build_system_and_the_host_s_tools: zlib's source is not on this machine; set $PEKIT_TEST_NO_SKIP to fail instead
```

With `PEKIT_TEST_NO_SKIP` set to any value, the same test fails instead,
naming what is missing. `tools/check` sets it, so a passing check means
that every test it runs ran. A plain `cargo test` doesn't, so it can still
skip, and says so.

## Check from a worktree

A worktree outside the Peios workspace has no `gcslib`, `ppk` or `local`
beside it. Point the tests at the workspace with `PEKIT_TEST_WORKSPACE`:

```sh
PEKIT_TEST_WORKSPACE=~/projects/peios tools/check
```

Two variables override a single location within it:

| Variable | Default |
| --- | --- |
| `PEKIT_TEST_GCSLIB` | `gcslib` in the workspace |
| `PEKIT_TEST_DOWNLOADS` | `local/pekit2-data` in the workspace |

## Check the documentation

`tools/check-docs` lints this documentation, builds it with Trail, and
checks the man pages:

```sh
tools/check-docs
```

Links to another product resolve only in the combined Learn site, so build
that as well when a change adds one.
