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

## The step's user

A program runs as user 1000 and group 1000, named `pekit`, whoever runs
pekit (`sandbox::STEP_UID`). Without this, it would run as the number of
the user who ran pekit, which differs between machines. Files the step is
given, and files it makes, belong to that user. The host's other files,
such as those under `/usr`, belong to `nobody`.

Every step has the same user database (`sandbox::PASSWD` and
`sandbox::GROUP`), as Nix gives its builds. `/etc/passwd` holds:

```text
root:x:0:0:root:/root:/bin/sh
pekit:x:1000:1000:pekit:/nonexistent:/bin/sh
nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin
```

`/etc/group` holds:

```text
root:x:0:
pekit:x:1000:
nogroup:x:65534:
```

Programs that look up users by name or number find them there, and tests
that need a file every system has, such as `/etc/passwd`, find one. The
step's user has no home directory, so a program that writes to one fails
rather than leaving files nothing keys.

A program can be in more groups than `pekit`. A user namespace that
pekit makes without privileges keeps the groups of the user who ran pekit,
and shows each of them as 65534, `nogroup`. So the number of groups `id -G`
lists depends on the machine.

## Mounts

The sandbox binds, in this order:

1. The host snapshot's directories, read-only at their own paths, unless
   the build is strict. Each directory of the snapshot that is hidden, such
   as `/usr/local`, is an empty `tmpfs`.
2. The user database, read-only at `/etc/passwd` and `/etc/group`, in
   every build, strict or not.
3. The staging directory's `w/` at `/w`, and `out/` at `/pekit-out`,
   read-only.
4. The step's own output directory at its path, writable.
5. Each mounted tree and each sealed composition, read-only at its path.
6. Each secret file given, read-only at `/pekit-secrets/<profile>/<name>`.
7. The staging directory's `tmp/` at `/tmp`, writable.
8. The step's scratch directory at `/pekit-scratch`, writable, for a step
   marked `#[scratch]`.
9. `/proc`, and a minimal `/dev`: `null`, `zero`, `full`, `random`,
   `urandom`, `tty`, `ptmx`, `pts`, `shm`, and the standard streams.
10. `/dev/kvm`, for a step marked `#[device("kvm")]` on a machine that has
    it.

A path outside these isn't there. A GCSL file function given such a path
fails with "it is outside the step".

## Working directory and environment

A program starts in the step's own output directory, unless its `exec`
gives `cwd`. Its environment is cleared, then set to the variables its
`exec` gives. `LOGNAME` and `USER` are `pekit` unless the `exec` gives
them, since some programs, such as DejaGnu's `runtest`, need them. If it
gives no `PATH`, and the build isn't strict, `PATH` is
`/usr/bin:/bin:/usr/sbin:/sbin`. In a strict build a program given no
`PATH` has none.

## What a program doesn't inherit from pekit

How pekit was started isn't part of a step's key, so pekit keeps it from
reaching a step's programs:

- **Standard input.** A program's standard input is `/dev/null`, even when
  its `exec` is interactive. It can't read the terminal pekit runs in, or
  whatever else pekit's standard input is.
- **Signal dispositions.** A program starts with every signal's default
  disposition. A shell's `&` starts pekit with `SIGINT` and `SIGQUIT`
  ignored, and a signal ignored stays ignored across `exec`, so without
  this every program of the step would ignore them, which test suites of
  signals, such as bash's, notice. pekit catches each signal it inherited
  as ignored with a handler that does nothing (`default_dispositions`):
  `exec` resets a caught signal to its default, and pekit itself still
  ignores the signal. `SIGILL`, `SIGFPE` and `SIGSEGV` can't be caught
  this way, so a pekit started with one of them ignored passes it on.
- **The file-creation mask.** A program's `umask` is `022`
  (`sandbox::UMASK`), whatever pekit was started with. The modes a step
  gives its files are part of what it made, as an archive it makes
  records them. pekit can't set a child's mask alone, so it sets its own
  before a build runs anything (`fixed_umask`). GCSL's file work for a
  step, which runs in pekit's process, gets the same mask, and so does
  every other program pekit starts and every file it makes, a step's or
  not.

A program does inherit pekit's resource limits, such as `ulimit -n`, the
number of files it can open. These can differ between a terminal and
another launcher: an agent's shell can raise the open-file limit from
1,024 to 524,288. They aren't part of a step's key.

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
its root, and the same isolation, user, `LOGNAME` and `USER`, devices,
and scratch directory as the step's own. It has the step's user database
unless its root has its own `/etc/passwd` or `/etc/group`.
[Compositions and Overlays](~pekit/compositions-and-overlays) describes
how its root is made.
