---
title: Overview
description: What pekit is as an implementation, and the decisions that
  shape it which its name and its commands don't reveal.
---

pekit is a build system that embeds GCSL. It loads a workspace of GCSL
files, evaluates the definitions it is asked for in GCSL's graph mode, and
runs each call of a function marked `#[hermetic]` as a step: a unit of
work run in a bubblewrap sandbox holding only its inputs, keyed by those
inputs, and recorded in a content-addressed store. It is one Rust crate,
`pekit`, with the `pekit` command as its only executable.

## What is unusual about it

Three decisions shape the implementation more than any other.

**The build language is a general programming language, evaluated
incrementally.** A target's code is not a declaration that pekit reads and
then plans from. It is a GCSL program that pekit evaluates as a *unit*:
the program runs until it needs a result pekit has not given it, stops,
and runs again from where it stopped once the result is given. A call of a
hermetic function does not run where it is made; GCSL records it as a node
of an action graph, and the program carries on with a *hole* where the
result will be. So a target's code learns every step it can reach before
any runs, and still decides what to build next from what an earlier step
made. One thread evaluates every unit; what runs in parallel is the
programs steps run, on a pool of threads.

**Every path is in one logical layout.** The workspace is at `/w` and
every output is under `/pekit-out`, in GCSL values, in keys, and in every
sandbox, whatever the host's paths are. A step's output directory is named
by a digest of what the step is, not by the target that reached it. So
the same call is the same step, at the same paths, in every build,
checkout, and machine of a platform, and two targets that make it share
it.

**Steps are keyed by what they read, and so are the programs inside
them.** A step's key holds the contents of every file it is given, and of
the GCSL contexts only those it read as it ran. A step that runs again,
because one input changed, runs its programs against keys of their own:
each program a step runs is recorded by what it can see, and a program that
sees what it saw before is not run, its writes put back from the store.

## The rest of this manual

[Loading a Workspace](~pekit/finding-the-workspace) describes how pekit
reads and checks a workspace and its libraries. [Evaluation](~pekit/units-and-the-graph)
describes how targets become steps, and [Steps](~pekit/identities) how
steps are named, keyed, and recorded. [Sandboxes](~pekit/staging) and
[The Store](~pekit/store-layout) describe where steps run and what is kept.
[Failure Modes](~pekit/failures-of-a-build) collects what goes wrong.
