---
title: The Step Sandbox
description: The bubblewrap command pekit runs a step's programs under,
  every mount, namespace, and variable it sets, and what a program sees.
---

Each program a step runs is wrapped in its own bubblewrap command
(`StepSandbox::command`). Every program of a step sees the same sandbox,
since each binds the same staging directory.

## Isolation

The command unshares every namespace bubblewrap can: user, IPC, PID,
network, UTS, and cgroup, with a new session, and dies with its parent. So
a program has no network, its own processes and users, and stops if pekit
stops. Its root is an empty `tmpfs`.

## Mounts

The sandbox binds, in this order:

1. The host snapshot's directories, read-only at their own paths, unless
   the build is strict. Each directory of the snapshot that is hidden, such
   as `/usr/local`, is an empty `tmpfs`.
2. The staging directory's `w/` at `/w`, and `out/` at `/pekit-out`,
   read-only.
3. The step's own output directory at its path, writable.
4. Each mounted tree and each sealed composition, read-only at its path.
5. Each secret file given, read-only at `/pekit-secrets/<profile>/<name>`.
6. The staging directory's `tmp/` at `/tmp`, writable.
7. The step's scratch directory at `/pekit-scratch`, writable, for a step
   marked `#[scratch]`.
8. `/proc`, and a minimal `/dev`: `null`, `zero`, `full`, `random`,
   `urandom`, `tty`, `ptmx`, `pts`, `shm`, and the standard streams.
9. `/dev/kvm`, for a step marked `#[device("kvm")]` on a machine that has
   it.

A path outside these isn't there. A GCSL file function given such a path
fails with "it is outside the step".

## Working directory and environment

A program starts in the step's own output directory, unless its `exec`
gives `cwd`. Its environment is cleared, then set to the variables its
`exec` gives. If it gives no `PATH`, and the build isn't strict, `PATH` is
`/usr/bin:/bin:/usr/sbin:/sbin`. In a strict build a program given no
`PATH` has none.

## GCSL's own file work

GCSL's file functions inside a step, such as `fs.write`, run in pekit's
process, not in the sandbox, against the staging directory. pekit maps each
logical path to the staging directory itself, and refuses two things:

- **A path outside what the step has.** It fails with "it is outside the
  step".
- **A write to an input or a sealed composition.** `fs.write`, `fs.mkdir`,
  `fs.copy`'s destination, and `fs.set_executable` fail on a path in an
  input of the step or in a sealed composition. The error says that the
  path is an input of the step, or a finished composition.

Temporary directories GCSL makes for a step, a `Dir.sandbox`'s copy among
them, are `.pekit-tmp-<n>` in the step's own output directory, so that a
file given from one has a path no other step's file has.

## Sandboxes a step makes

A `Dir.sandbox`'s commands run in a bubblewrap of their own beside the
step's, never inside it, since a host such as Ubuntu forbids one
bubblewrap inside another. That bubblewrap has the sandbox's directory as
its root, and the same isolation, devices, and scratch directory as the
step's own. [Compositions and Overlays](~pekit/compositions-and-overlays)
describes how its root is made.
