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
`pekit-identity-2`:

- **Its callee.** The name of its code's file, its function's number in that
  code, and the function's captures.
- **Its arguments,** as plain data. Every file and directory counts by its
  path alone, not its contents. The result of a step not yet given counts
  by that step's own name, so that a step is named before the steps it waits
  for have run.
- **The platform it runs on.**

The contexts it was made in don't name it, so binding a context moves only
the steps that read it, as their receipts say.

## Steps that only their contexts tell apart

GCSL makes one call made in two contexts two steps, such as one compile
made through `with` for two platforms. Both have one identity and share one
name. Whether they are one step depends on whether the step reads a context
they were made in differently, and only running it says which contexts it
reads. So pekit settles it as they start:

1. The first of them to start holds the name, and is found in the store or
   runs, as any step is.
2. Each of the others waits until the first is done. Then it is keyed by the
   contexts the first was keyed by: those its run read, or those the record
   it was found by was keyed by.
3. If its receipt is the same as the first step's, it is the same step. It
   takes the first step's result, and is neither run nor looked up.
4. If its receipt differs, it is another step. It reads a context they were
   made in with different values, or it was given the result of a step that
   does. It is named `<identity>-<receipt>`, with the first 16 hexadecimal
   digits of that receipt, and is found or runs there. Others with that
   receipt share that name in the same way.

A step that doesn't read the contexts the steps differ in therefore has one
name and runs once, in whatever order evaluation meets the steps. Steps that
really differ never share a name or a result. Which of them keeps the plain
name still depends on which starts first, so reordering a build can rename
those steps, and the steps after them.

If the first step fails, the next of the others to start holds the name
instead, since what the failed step read is not known.

## The tag's version

The tag's version changes when what every step's sandbox holds, beyond
the step's inputs, changes in a way that can change what a step gives.
A new version gives every step a new identity, so no step recorded under
the old sandbox is found in the store, and every step runs again once.

| Version | What changed |
|---|---|
| 1 | The first. |
| 2 | Every step runs as user and group 1000, `pekit`, with a fixed `/etc/passwd` and `/etc/group`, and `LOGNAME` and `USER` set, as [The Step Sandbox](~pekit/the-step-sandbox) describes. Before, a step ran as whoever ran pekit, with no user database. |

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
