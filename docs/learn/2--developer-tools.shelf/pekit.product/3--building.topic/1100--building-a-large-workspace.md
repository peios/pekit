---
title: Building a Large Workspace
type: how-to
description: Run builds of hundreds of targets, such as a package
  catalog, within a machine's memory and disk, watch them from a log,
  and keep the store in shape while builds overlap.
related:
  - pekit/concepts/sharing-the-machine
  - pekit/building/managing-the-store
  - pekit/concepts/where-results-go
---

A workspace of hundreds of targets, such as a catalog of packages, can
run for hours. It might run unattended under a service, often beside other
builds. Four things keep such builds within the machine's memory and disk,
and make them easy to follow.

## Keep pekit's own memory in bounds

pekit itself uses memory to evaluate a target's code, separately from the
programs that steps run. The code of a large target can need a few hundred
megabytes. pekit evaluates only a few targets at once, so a build of
hundreds needs the memory of a few. By default it evaluates as many as
`--jobs`, and at least 8.

On a machine that also runs other builds, set a lower number for every
build in `machine.conf`, in pekit's configuration directory
(`~/.config/pekit`):

```text
evaluating = 4
```

A lower number costs little time, because pekit evaluates code on one
thread whatever the number. `--evaluating` sets it for one build, and
`PEKIT_EVALUATING` for a shell. A run with output going to a log shows how
much memory pekit uses in each status line (see below).

## Keep outputs on the store's file system

pekit links each output in `pekit-out/` to the file the store keeps. It
can't link across file systems, so if `pekit-out/` is on another file
system than the store, pekit copies every output, on every build, and
warns:

```text
warning: `/srv/pkgs/pekit-out` is on another file system than the store, ...
```

With a store on another disk, make `pekit-out` a symbolic link to a
directory on that disk:

```sh
mkdir -p /srv/cache/pekit-outputs/pkgs
ln -s /srv/cache/pekit-outputs/pkgs pekit-out
```

## Set the store's limit for every build

The store's limit holds for builds started from a service as much as from
a shell only if it's set in `machine.conf`, not in one shell's environment:

```text
cache_limit = 400GiB
```

A build evicts only as it ends with no other pekit using the store, so
while builds overlap, the store can grow past its limit, and each build
warns that it did. Between runs, or before a large one, check it and evict
it:

```sh
pekit store stats
pekit store evict
```

`pekit store evict` waits for the builds using the store to end. Builds
that start while it waits wait for it in turn, so it is never kept from
the store for long. [Managing the Store](~pekit/managing-the-store) covers
the rest.

## Follow a build from its log

With standard error going to a file or a service's journal, pekit writes
nothing for the first minute. After that it writes a status line each
minute, and a line as each target ends:

```text
[00:41:00] 212 of 1212 targets ended, 2 failed; 3104 steps run, 40211 found; running 6: compile/9927fb8abbee4a05, link/3f1c0a9e2b7d4c68, gate/71ab0c4e9d2f6a13 and 3 more; pekit uses 3.1 GiB
[00:41:12] ended dev.peios.authd/dev.peios.authd
[00:41:30] FAILED org.gnu.bash/org.gnu.bash-doc
```

`PEKIT_PROGRESS_SECS` changes the minute, or turns the lines off with `0`.
For a full timeline of a run, give it `--trace`: the file is written again
as each of the command's builds ends, so a run that is stopped still leaves
the builds before it.

A target that fails doesn't stop the others. `pekit publish --strict`
publishes every recipe whose packages all packed, holds back the recipes
that had a failure, and exits with an error. So after a long run, look for
the `FAILED` lines, fix those recipes, and run again: the steps that
already succeeded are found in the store.
