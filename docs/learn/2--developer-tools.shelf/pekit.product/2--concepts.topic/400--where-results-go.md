---
title: Where Results Go
type: concept
description: How pekit lays out pekit-out, covering each step's own
  directory, the paths under each target's name, fetched and unpacked
  files, and when intermediate files appear on disk.
related:
  - pekit/concepts/step-sandboxes
  - pekit/concepts/reuse-and-the-store
  - pekit/getting-started/workspaces
---

pekit puts what a build made in `pekit-out/`, at the root of the workspace. It
has two views of the same files. One is by what each step is, which never
changes from build to build. The other is by the name of each target you
built, and it's the one `pekit build` prints.

## Each target's files

After a build, each file a target gave is under `pekit-out/`, by the
target's name and the step that made it:

```text
built app/hello: 1 step run, 0 found in the store
  ../pekit-out/app/hello/link/hello
```

Here the target `app/hello` gave a file its `link` step made. If a target
gives files from two steps of the same function, the second is under `link.2`,
the third under `link.3`, and so on. They're numbered in the order the
target's value names them. The path pekit prints is relative to the directory
you ran it from.

These paths are symbolic links into each step's own directory. The file
they lead to is a copy of the step's output, and it's yours: you can change
it or delete it, and the next build puts it back.

## Each step's own directory

Every step has a directory of its own under `pekit-out/.steps/`. It's named by
the step's function and a digest of what the step is: its function, its
arguments, and the platform it runs on. For example,
`pekit-out/.steps/link/3fa9c2e17b04d6a8/` is the directory of one call of
`link`.

A file argument counts by its path, not its contents, so the directory
stays the same when you edit a source file. Changing the file runs the step
again, in the same directory. Two targets that make the same call share
the one step and its directory, whichever of them asked for it.

## Fetched and unpacked files

Files that the build fetched, unpacked, or took from a git repository are
named by their contents rather than by a step:

| Path | Holds |
|---|---|
| `pekit-out/.fetch/<sha256>/<name>` | A file `fetch` downloaded, by its digest and the last part of its URL. |
| `pekit-out/.unpack/<key>` | What `archive.unpack` unpacked, by a digest of the archive and how much of each path it strips. |
| `pekit-out/.git/<key>/<name>` | The files of one commit `fetch_git` took, by a digest of the repository and the commit. |

A target that gives one of these directly, rather than a step's output,
shows it at this path. It isn't linked under the target's name.

## When intermediate files appear

pekit keeps every output in its store, and gives steps their inputs from
there. Most intermediate files, such as object files, are never needed on
disk, so pekit doesn't write them to `pekit-out/` unless something outside
a step might read them there. That happens when:

- A target's own code runs a program, or reads a file of `pekit-out/` by a
  path it names as text.
- `pekit run` runs a program, which might read any of them.

What a target gives always appears. Files that appear this way are
read-only, and they're the store's own files: don't change them.

## Paths in GCSL values

In GCSL code, every output's path starts with `/pekit-out`, and every
workspace file's with `/w`. A target that gives `gcc.link(...)` gives a
`File` whose path is `/pekit-out/.steps/link/<identity>/hello`, the same on
every machine. `pekit build` shows it as a path on your machine instead.

`pekit-out/` isn't part of the workspace. A listing such as
`dir(".").files()` never includes it, and a step can't be given a file in it
by its path in the workspace. Name an output by its path under
`/pekit-out` instead, or better, pass the value the step gave.
