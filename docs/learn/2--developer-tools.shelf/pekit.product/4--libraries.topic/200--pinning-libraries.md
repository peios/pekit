---
title: Pinning Libraries
type: how-to
description: Pin every library a workspace imports at a git commit with
  pekit lock, update the pins, take libraries from other repositories, and
  build offline and strictly from the pins.
related:
  - pekit/libraries/libraries
  - pekit/concepts/hermetic-and-strict-builds
---

`pekit lock` pins each library a workspace imports, and each library those
import, at a commit of the git repository it comes from. It records the
pins in `pekit.lock`, at the workspace's root. Commit that file, and every
checkout builds with exactly the same libraries.

## Before you start

- `git`, which pekit runs to fetch libraries.
- A workspace whose build files import at least one library.

## Pin the workspace's libraries

1. From anywhere in the workspace, run:

   ```sh
   pekit lock
   ```

   pekit finds every library the workspace imports, fetches each from its
   repository, and pins it at the commit the repository's `HEAD` names:

   ```text
   pinned github.com/gcslib/gcc at 72dae8c75eabc34c63615380a9197e1021a39952
   ```

   It exports each library into `.gcsl/libraries/<path>/`.

2. Commit `pekit.lock`. Don't commit `.gcsl/`: any command that builds puts
   back a pinned library that's missing, from the commit it's pinned at.

## Check the result

`pekit.lock` lists each library under `libraries`, with its repository and
commit. Run `pekit build --strict` on a target: a strict build fails unless
every library it imports is pinned, so a successful one confirms the pins
are complete.

## Update the pins

`pekit lock` pins only libraries that aren't pinned yet. To move every
library to what its repository holds now, run:

```sh
pekit lock --update
```

pekit prints a line for each library it pins again. A library that nothing
imports any more is dropped from the lock, and its directory with it:

```text
dropped example.org/greeting, no longer imported
```

## When a library differs from its pin

If a library under `.gcsl/libraries/` differs from the commit it's pinned
at, because you edited it there, every build warns:

```text
warning: `.gcsl/libraries/github.com/gcslib/gcc` differs from the commit `pekit.lock` pins, 72dae8c75eabc34c63615380a9197e1021a39952: remove it to have the pinned one put back, or commit the change to its repository and run `pekit lock --update`
```

To work on a library, use `$GCSL_LIBRARY_PATH` instead of editing the
exported copy. See [Writing a Library](~pekit/writing-a-library).

## Take libraries from elsewhere

By default, a library's repository follows the convention of Go's modules:
the library `github.com/gcslib/gcc` is the repository
`https://github.com/gcslib/gcc`, at its `HEAD`, with the library at the
repository's root. `pekit lock` records this as a source in `pekit.lock`.

To take libraries from another repository, a mirror, a branch, or a
directory inside a repository, edit the sources. Each source has a prefix of
import paths, a git repository, the directory in it that holds the
prefix's libraries, and the branch, tag, or commit to follow:

```json
{
  "sources": [
    {"prefix": "github.com/gcslib/ppk", "git": "<pekit-repository>", "dir": "gcslib/github.com/gcslib/ppk", "rev": "main"}
  ]
}
```

Replace *pekit-repository* with the URL or path of pekit's repository,
which holds the ppk library under `gcslib/`.

A library uses the first source whose prefix its path is under. Run
`pekit lock --update` after changing a source, so that the libraries it
covers are pinned from it. `git` can be any repository git can fetch,
including a path on your machine.

## Build offline

pekit keeps a bare clone of each repository in its cache directory, and
fetches only when it needs a commit it doesn't have. A workspace whose
libraries are pinned and already fetched builds without the network.

## Build strictly from the pins

A strict build, `--strict`, fails unless its libraries are exactly those
`pekit.lock` pins. That means:

- Every library it imports is pinned.
- No library differs from its pin.
- `$GCSL_LIBRARY_PATH` is unset.

```text
error: a strict build uses the libraries `pekit.lock` pins, and only them:
  the workspace has no `pekit.lock`: run `pekit lock`
  `$GCSL_LIBRARY_PATH` is set, which would find libraries elsewhere: unset it
```

`pekit.lock(5)` describes every field of `pekit.lock`.
