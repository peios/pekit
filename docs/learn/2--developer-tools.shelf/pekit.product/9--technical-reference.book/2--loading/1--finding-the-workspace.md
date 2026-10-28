---
title: Finding and Checking the Workspace
description: How pekit finds a workspace's root, which files it reads,
  how it checks them through GCSL in the logical layout, and how it tells
  which workspace files changed since the last build.
---

Every command but `pekit clean` starts by loading the workspace (`load`).
Loading finds the root, reads the GCSL sources, and checks them. Nothing
is evaluated until every source checks.

## The root

The workspace's root is the nearest directory, from the current directory
upward, that holds a `workspace.gcsl` (`gcsl_disk::find_root`). With none,
the command fails with an error that names the directory it searched from.
The root is canonicalized, so a workspace reached through a symbolic link
is the same workspace as one reached directly.

## The sources

The sources are read as the `gcsl` command reads them
(`gcsl_disk::read_workspace`): every `.gcsl` file under the root, except
under a hidden directory, a hidden file, and any directory below the root
that holds its own `workspace.gcsl`. A nested workspace is never part of
the outer one.

Each library the sources import is read too, with every library those
import, from where [Libraries and the Lock](~pekit/libraries-and-the-lock)
says. GCSL's standard modules are compiled into pekit and come first in
the order of inputs, so that a diagnostic names its file by its input
number correctly.

## Checking

The sources are checked with GCSL's standard library
(`gcsl::workspace::check_with_standard`), rooted at `/w`, the workspace's
place in the logical layout. So `fileroot.workspace` is `/w`, and
`fileroot.this` in a directory `app` is `/w/app`. A diagnostic is shown
against its file, named relative to the root, and the command fails with
status 1 if any is an error.

Two rules are pekit's own, checked once GCSL's checking has passed:

- A `build` and a `script` of the same name in the same directory are an
  error, since `pekit run NAME` could mean either.
- A build, test, or package that can reach a call of a script fails to
  check. The prelude's `script` definition type reads the context
  `^script_session`, which has no default and which only `pekit run`
  binds, so GCSL reports "context `script_session` has no value in this
  definition".

## A workspace checked afresh

A build leaves state in the checked workspace: each file's top level and
each memoized call's result, which name what it fetched, unpacked, and
built. A second build in the same process, `--check-reproducible`'s second
pass or a release's publishing pass, has to make those itself. So every
build after the first checks the workspace again from the same sources
(`Loaded::ready`), rather than reusing the evaluated one.

## Which files changed

pekit tells a workspace file by its contents. To avoid reading every file
on every build, the store keeps, for each workspace root, the *stamp* of
each file it read: its device and inode, size, mode, and modification and
change times, with the digest of its contents and whether it is
executable. They are in the store's
`seen/` directory, in a JSON file named by the SHA-256 of the root's path.

A file whose stamp matches is taken to have the recorded digest, unread. A
file whose stamp differs, or which is new, is read and hashed. A stamp taken
in the same tick of the file system's clock as the build began, or later,
isn't trusted: a file changed again within that tick would keep the same
times. The next build reads such a file again. Stamps of files that are
gone are dropped as the build ends.
