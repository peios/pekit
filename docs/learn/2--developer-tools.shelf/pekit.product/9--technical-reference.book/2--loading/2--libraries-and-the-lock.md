---
title: Libraries and the Lock
description: Where pekit finds a library, where it places one in the
  logical layout, how pekit lock pins and exports libraries, how a build
  puts back and checks them, and what a strict build requires.
---

A library is a directory of GCSL files named by an import path, such as
`github.com/gcslib/gcc`. pekit finds, places, and pins libraries itself;
GCSL only reads the sources it is given.

## Where a library is found

A library's directory is looked for in each directory of
`$GCSL_LIBRARY_PATH`, in order, then in `.gcsl/libraries/` at the
workspace's root, and the first found is used. Its sources are the `.gcsl`
files directly in that directory.

Wherever a library was found, its sources are given to GCSL at
`/w/.gcsl/libraries/<path>/` in the logical layout
(`gcsl::SourceFile::with_dir`). A relative path in a library's code
therefore names the same logical path in every workspace. To the rest of
the engine, a library's files are workspace files under that path: they
are hashed, put into the store, and staged as any are. Only where one is
on the host differs, for a library found through `$GCSL_LIBRARY_PATH`
(`Libraries::host`).

So a step a library makes has the same identity, the same receipt for the
same library contents, and the same outputs in every workspace, whether
the library came from a checkout or from the lock.

## The lock

`pekit.lock` is JSON at the workspace's root, described in
`pekit.lock(5)`: `sources`, each a prefix of import paths with a git
repository, a directory in it, and a revision; and `libraries`, each pinned
by its path, repository, directory, commit, and the digest of its exported
tree.

A library's source is the first source whose prefix is its path or a part
of its path ending before a `/`. A library no source covers is given one by
the convention of Go's modules: the first three parts of its path,
`HOST/OWNER/REPO`, are the prefix, `https://HOST/OWNER/REPO` the
repository, its root the directory, and `HEAD` the revision. The source is
added to the lock.

## Pinning

`pekit lock` reads the import paths of the workspace's sources, without
checking them, and works through them and every library they import,
transitively:

1. A library the lock pins, without `--update`, is exported again at its
   pinned commit.
2. Any other is fetched from its source: its repository's bare clone is
   made or fetched, the source's revision resolved to a commit with
   `git rev-parse`, and the library exported at that commit.
3. Each exported library's tree is put into the store, and its digest
   recorded as the pin's `tree`.

Each pin that changed is reported. Each library the lock pinned that
nothing imports any more is dropped, and its directory under
`.gcsl/libraries/` removed. The lock is written with its libraries sorted
by path.

A library is exported with `git archive` of the commit's directory, into
`.gcsl/libraries/<path>/`, replacing what was there. Repositories are bare
clones under the cache directory's `git/`, fetched only to resolve a
revision or to get a pinned commit the clone lacks. Git runs as the `git`
program found on `PATH`.

## Putting back and checking

Before loading, `build`, `test`, `run`, `query`, `package`, and `publish`
restore the lock's libraries (`lock::restore`):

- A pinned library whose directory is missing is exported from its commit.
- A pinned library whose directory is there is put into the store as a
  tree, and its digest compared with the pin's. A library that differs is
  reported.

Outside a strict build, each difference is a warning, and the build goes on
with the library as it is.

A strict build fails, listing every problem, if any of these holds
(`lock::check_strict`):

- The workspace imports a library and has no `pekit.lock`.
- A library it loaded isn't pinned.
- A pinned library differs from its pin.
- `$GCSL_LIBRARY_PATH` is set and not empty.

## What the lock tells a build

pekit sets the `libraries` module's `pinned_libraries` context, where the
workspace declares it, to the lock's libraries, each with its path,
repository, directory, and commit. A library can compare these with what it
takes another way, as the gcslib rust library compares a git crate's commit
in `Cargo.lock` with the commit a library of the same repository is pinned
at.
