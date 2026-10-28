---
title: What This Manual Covers
description: The scope of this manual, the source it describes, and the
  documents that hold what it leaves out.
---

This manual describes the `pekit` crate in pekit's repository, and the
behavior of the `pekit` command built from it. It covers the engine's
state, its keys and on-disk formats, its concurrency, its limits, and its
failure behavior, with the reasoning behind its design where the reasoning
is the implementation's own.

A manual's authority comes from the software. Where this manual and the
source disagree, the source is right and the manual is stale. Nothing here
is a promise about a later version: the encodings of keys, records, and
trees change whenever the implementation needs them to, and old records
are then not found.

## What is covered elsewhere

These documents hold what this manual leaves out:

| Subject | Where |
|---|---|
| Using pekit: commands, workflows, and concepts | The other topics of this product, starting at [Pekit](~pekit/pekit) |
| Every command, option, exit status, environment variable, and file | `pekit(1)` |
| The lock file's format | `pekit.lock(5)` |
| The GCSL language, its standard library, and graph mode | The [GCSL documentation](~gcsl/gcsl), `gcsl(5)`, and `gcsl-stdlib(3)` |
| What a library such as gcc or rust does | The library's own README and source |
| Design history, and decisions made and changed | The task records of the work that produced them |

Where this manual names a function or type of the source, such as
`Store::put_tree`, it does so to help a reader find the implementation. The
names are not an interface.
