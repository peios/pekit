---
title: Receipts
description: What a step's receipt covers, how contexts key a step in two
  levels through a manifest, how a step is looked up, and when a context's
  files are staged.
---

A step's *receipt* is the SHA-256 that its record is kept under: a digest
of everything that can change what the step gives (`receipt::receipt`). A
step whose receipt the store has a record for is not run.

## What it covers

The receipt is a canonical encoding, after the tag `pekit-receipt-3`, of:

- **Its code.** The path and contents' digest of every source file that
  makes the step's code: the file of its function and of every function it
  can reach, library and standard files included, in order. Any change to
  one of these files, a comment included, changes the receipt.
- **Its arguments and its function's captures,** as plain data. A record's
  fields are sorted by name, a float is encoded by its bits, every NaN as
  one, and `-0.0` as `0.0`.
- **Each file it is given,** by its path and the digest of its contents, and
  each directory by its path and its tree's digest. The path stays in the
  key because it reaches command lines and names outputs. An input is
  keyed by its contents, never by the step that made it.
- **Each secret,** by its identity and whether it is revealed, never by its
  value.
- **Each function value** among its captures or contexts, such as a
  registered toolchain's definition, by the name and contents of its code's
  file, its number in that code, and its captures.
- **The contexts it read,** each by its key, with its value or a tag
  saying that it was unbound.
- **Its own output directory,** which names the files it makes.
- **The platform it runs on.** Outside a strict build, this is the execution
  platform with the host snapshot's fingerprint. In a strict build, it is
  the execution platform marked as having no host tools.

`#[retry]`, `#[timeout]`, `#[device]`, and `#[scratch]` are not in it: they
change how a step runs, never what it gives.

## Two levels: base and manifest

A step can't know which contexts it reads before it runs. So a step is
keyed in two levels:

1. Its *base* is the receipt without any context.
2. The store keeps, under a key made from the base (`pekit-manifest-1`), a
   *manifest*: every set of context keys a run of a step with that base has
   read.

To look a step up, pekit takes each set in its manifest in turn, and makes
the full receipt of the base and, for each key of the set, the value the
step's context gives it, or that it is unbound. A record under that receipt
is the step's result.

A step that runs is recorded under the receipt of the set its run read,
and that set is merged into the manifest. Merging never removes a set.

This is sound because a run is a function of what it reads. Two runs with
one base, given the same values for the contexts the first read, read the
same contexts in the same order, and give the same result.

## When a context's files are staged

A step's sandbox holds the files of its arguments and captures from its
start. A context's value can name files too, such as the registered
toolchains' trees. Those are staged into the sandbox only once the step
reads the context, at the read itself
(`StepSandbox::context_read`). So a program a step runs can't reach a file
of a context its receipt doesn't hold.

The files of every context a step can read are hashed when the step
starts, so that they can be staged on a read.
