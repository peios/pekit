---
title: Program Records
description: How pekit records each program a step runs by what the
  program can see, how it replays a program it finds, and when a step's
  programs are neither found nor recorded.
---

A step runs again whenever anything in its receipt changes, including any
byte of its code's files. Most of the programs it runs then see what they
saw before. So pekit also records each program a step runs, by what the
program can see, and replays a program it finds instead of running it
(`programs` and `engine::programs`).

## Which programs

Each `exec`, `exec_live`, and `Sandbox.exec` of a step is a program. The
first time a step is given a secret, at its start or when a read binds one,
its programs are neither found nor recorded from then on. With `--rebuild`,
programs are run and recorded but not found. In `--check-reproducible`'s
second build, they are neither found nor recorded.

## A program's key

A program's key covers:

- **The platform,** as the step's receipt names it, with the host snapshot's
  fingerprint.
- **The program itself:** the built-in, its words, its working directory,
  its environment, and `exec`'s options. Its words name files by path
  alone; their contents are the next part's.
- **What it can see:** every input the step has staged, by path and
  contents' digest, and the digests of the trees it can write, the step's
  own output directory and its `/tmp`, as they are before it runs.
- **The step's view:** a chain that starts from the step's platform and own
  output directory, and folds in each effect the step had run before the
  program. An effect that only reads, `fs.exists`, `fs.read`, `File.read`,
  `Dir.files`, or `which`, isn't folded in. Any other folds in its built-in,
  its arguments by path, and what it staged, by contents.

A device and a scratch directory key nothing, and a scratch directory is
neither captured nor put back.

## Recording

A program that runs is recorded with its status, its output and error, and
the digests of the two writable trees after it, whose contents go into the
store. Hashing the trees again after each program is cheap: the step's
sandbox keeps a stamp of each file, as [Blobs and
Trees](~pekit/blobs-and-trees) describes, and reads again only a file
whose stamp changed.

## Replaying

A program found is not run. Each writable tree whose digest differs from
the record's is placed again from the store, whole, so the step's world is
exactly as the recorded run left it, and the record's result is given to
the step. A record whose contents aren't all in the store is not found.

A replay puts back what the store keeps of a tree: contents, symbolic
links, and whether a file is executable. It doesn't put back modification
times, or which files were hard links of one another. So a `make` after a
replay sees every file as new. A sealed composition put back is sealed
again.

## What stops recording

An entry the store can't hold stops a step's programs being found or
recorded from then on:

- An entry that is neither a file, a directory, nor a symbolic link, such
  as an overlay's whiteout
- A directory an overlay's attributes mark, such as an opaque one

So a step whose sandbox removes a file of the directory it was made of,
which leaves a whiteout, runs its later programs every time. Overlay
attributes that only say where an entry came from, or which layer it is, are
dropped from a copy and don't stop recording.

## Where the work is done

The engine keeps each step's view, and the writable trees' digests while it
knows them. A program is sent to the pool, and the thread that takes it
hashes the trees if needed, makes the key, and finds and replays the record
or runs the program, hashes again, and records. No tree is hashed or placed
on the thread that evaluates.

A build reports how many programs its steps found:

```text
built app/hello: 1 step run (2 programs found), 0 found in the store
```
