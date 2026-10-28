---
title: Pekit
type: concept
description: What pekit is, how it turns GCSL build files into sandboxed
  steps whose results it reuses, the parts of a build, and where to start
  reading.
related:
  - pekit/getting-started/building-a-first-program
  - pekit/getting-started/workspaces
  - pekit/concepts/targets-and-steps
---

pekit is a build system. You describe what to build in GCSL, a typed
scripting language, and pekit works out which steps that needs, runs each
one in a sandbox, and keeps every result. When you build again, pekit runs
only the steps whose inputs changed.

If you have used Bazel, pekit will be familiar: a build is a graph of
cached, sandboxed actions, and the same inputs always give the same
outputs. The difference is the language. A pekit build file is an ordinary
GCSL program, so a build can compute what to build, and can decide what to
do next from what an earlier step made.

## The parts of a build

These are the parts a build is made of:

- **Workspace.** A directory tree with a `workspace.gcsl` file at its root.
  Everything pekit builds comes from a workspace.
- **Build file.** A `.gcsl` file in the workspace that declares what can
  be built.
- **Target.** A definition in a build file that you can ask pekit to
  build, such as `def build hello()`.
- **Step.** One cached, sandboxed piece of work, such as compiling a file.
  A step is a call of a GCSL function marked `#[hermetic]`.
- **Store.** Where pekit keeps the result of every step, shared by every
  workspace on the machine.
- **Output directory.** `pekit-out/`, at the workspace's root, where pekit
  puts what each target built.

A target's own code runs outside any sandbox and decides which steps to
call. pekit runs the steps, as many at once as the machine allows, and
gives their results back to the code that asked for them. [Targets and
Steps](~pekit/targets-and-steps) explains how the two fit together.

## Why steps are sandboxed

A step can see only what it's given: the files passed to it as arguments,
and the results of the steps before it. pekit keys each step by exactly
those inputs. So when none of them has changed, pekit knows the step would
give the same result, and takes it from the store instead of running it.
If a step could read anything else, a change to that file would go
unnoticed, and pekit would reuse a stale result.

The same rule makes builds portable. A step sees the workspace at `/w`
and earlier results at `/pekit-out`, wherever your checkout is, so two
machines that build the same commit run the same steps.

## What pekit does

The `pekit` command builds targets, and also runs the other kinds of
definition a workspace can hold:

| Command | Does |
|---|---|
| `pekit build` | Builds targets. |
| `pekit test` | Runs tests, and doesn't rerun a passing test whose inputs didn't change. |
| `pekit run` | Runs a script, or a program a target built. |
| `pekit query` | Lists what targets reach, or which targets a file change reaches, without building. |
| `pekit lock` | Pins the libraries a workspace imports. |
| `pekit package` and `pekit publish` | Pack packages, and publish them into a package store. |
| `pekit release` | Builds a package from a commit, checks that it's reproducible, publishes it, and tags the commit. |
| `pekit clean` | Removes `pekit-out/`, and optionally the store. |

`pekit(1)` documents every command and option.

## Where to go next

Start with [Building a First Program](~pekit/building-a-first-program),
which builds a C program from an empty directory. Then read
[Workspaces](~pekit/workspaces) and the [How Pekit
Builds](~pekit/targets-and-steps) topic. pekit's build files are GCSL, and
the [GCSL documentation](~gcsl/gcsl) covers the language itself.
