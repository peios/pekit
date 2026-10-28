---
title: Identities
description: How pekit names a step's output directory by a digest of what
  the step is, apart from its inputs' contents, and why.
---

A step's *identity* names its output directory,
`/pekit-out/.steps/<function>/<identity>`, as 16 hexadecimal digits: the
first 64 bits of a SHA-256 (`receipt::identity`). Every file a step makes is
under that directory, so the identity is part of every path its outputs
have, and of every key those paths reach.

## What it covers

The identity is a digest of what the step is, encoded after the tag
`pekit-identity-1`:

- **Its callee.** The name of its code's file, its function's number in that
  code, and the function's captures.
- **Its arguments,** as plain data. Every file and directory counts by its
  path alone, not its contents. The result of a step not yet given counts
  by that step's own name, so that a step is named before the steps it waits
  for have run.
- **The platform it runs on.**

The contexts it was made in don't name it. Two steps of one build that only
their contexts tell apart, such as one compile for two platforms through
`with`, would have one identity. The one met later is named with a digest of
those contexts too.

## Why not its contents

An identity doesn't hold its inputs' contents, so editing a source keeps
every step at the same paths: the step runs again in the same directory,
and gives files at the same paths. A step after it that is given a rebuilt
file with the same bytes at the same path has the same receipt, and is
found in the store. This is early cutoff. Naming a step by its contents
would move every later step whenever an earlier one ran again, even to the
same bytes.

An identity doesn't hold the target that reached the step. Two targets that
make the same call make one step, and a step's paths don't change with what
else is built, or in what order. A target's view of its steps, under
`pekit-out/<target>/<function>[.N]`, is a link into the step's directory,
made after the build.

## Collisions

Two different steps with the same function and identity would share a
directory. An identity is 64 bits for each function's steps, which makes
that chance negligible for any number of steps a store holds.
