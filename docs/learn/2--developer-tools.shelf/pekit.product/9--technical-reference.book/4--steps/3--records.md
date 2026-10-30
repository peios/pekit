---
title: Records
description: What pekit keeps of a step that succeeds, what a record holds,
  which results are never recorded, and how a found record gives a step's
  result without running it.
---

A step that succeeds is recorded: its receipt, its result, and how long it
ran. A step found in the store gives its record's result without running.

## Keeping a result's files

When a step's unit ends with a value, every file and directory the value
names is put into the store (`record::keep_files`), whether or not the
result can be recorded, since later steps are given them. A file becomes a
blob, and a directory a tree. A directory that holds a mount point is put
into the store with the mounted tree's digest, unread.

A step fails unless every file and directory its result names is one of
these:

- A file or directory in the step's own output directory
- A file or directory it was given, or a context's file that a read staged
- A file of `/pekit-out/.fetch/` or `/pekit-out/.unpack/`, which are named
  by their contents

Any other path fails the step: another step's file at that path could
otherwise stand for it.

## What a record holds

A record (`StepRecord`) is JSON in the store's `records/`, under its
receipt's digest. It holds:

- **`receipt`,** the digest it is kept under.
- **`result`,** the step's value as plain data (`Stored`): integers,
  floats, booleans, strings, durations, sizes, records with their fields in
  order, lists, enum variants, sandboxes, and every file and directory by
  its path and what the store keeps of it: a file's digest, size, and
  whether it is executable, and a directory's tree. A string longer than
  64 KiB (`INLINE_TEXT`), such as a program's whole output, is kept as a
  blob, and the record names it by digest and length (`Stored::Text`).
  This matters because a record is read whole, by every eviction too, and
  JSON writes some bytes as six. A record whose string blob was evicted
  isn't live.
- **`duration_ms`,** how long the step ran, from the end of its staging to
  the end of its last attempt. Eviction weighs a record's contents by it.

Records are never evicted. A record whose contents were evicted is not
*live*: a step whose record isn't live runs again, as if it weren't found,
and records again.

## What is never recorded

These results are not recorded, and the step runs on every build:

- A result that holds a secret (`StoreError::Secret`). A secret is never
  stored. Its files are still kept, so later steps are given them.
- A result that holds a function value, which is not data.

A step that fails isn't recorded either.

## Giving a found record's result

A found record's result is turned back into a GCSL value, its files and
directories at their recorded paths. The steps after it are staged with
those files from the store. The record's files are exposed under
`pekit-out/` only once something that is not hermetic might read them, as
[Exposure](~pekit/exposure) describes.

Before a record is used, pekit checks that the store has everything it
names: every blob, and every tree with everything it names at any depth.
A tree found complete is marked in the store's `complete/`, so that it is
walked once rather than in every build. An eviction removes every mark
before it removes a blob.
