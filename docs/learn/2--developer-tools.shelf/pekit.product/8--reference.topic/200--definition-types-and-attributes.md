---
title: Definition Types and Attributes
type: reference
description: The definition types pekit builds, tests, packs, and runs, and
  the attributes that change how pekit runs a function, with what each one
  means to pekit.
related:
  - pekit/concepts/targets-and-steps
  - pekit/building/writing-steps
---

GCSL's prelude declares the definition types and attributes that build
files use. Plain GCSL runs them as ordinary calls; pekit gives them their
meaning as a build system. This page lists what each means to pekit.
GCSL's [Attributes](~gcsl/attributes) and [Definition Types and
Metadata](~gcsl/definition-types) pages describe them as language features.

## Definition types

Each definition type declares a kind of target, which one command acts on:

| Definition type | Declares | Its body gives | Command |
|---|---|---|---|
| `build` | Something to build. | Any value: a file, a directory, or a record or list of them. | `pekit build` |
| `test` | A check. Its body is a step, unless marked `#[hermetic(false)]`. | Nothing. The test passes if the body succeeds. | `pekit test` |
| `package` | A package of any format. | A `version` and the `files` it installs under `/usr`. | `pekit package`, `pekit publish`, `pekit release` |
| `script` | A task you run by hand. Only a script can call a script. | Any value. | `pekit run` |
| `Toolchain::<kind>` | A toolchain of a kind, to register in `pekitconf`. | Its platforms and the files it `provides`. | None; libraries ask for it. |

A definition can declare parameters with defaults, as
`def test runs(prog: File = build app())`. The defaults are worked out
where the definition is called, outside any step, so a test is given what
it tests.

## Attributes

These attributes change how pekit runs a function. Put them on the lines
before a `fn` or `def`:

| Attribute | Means to pekit |
|---|---|
| `#[hermetic]` | A call of the function from a target's code is a step: run in a sandbox, and its result kept in the store. `#[hermetic(false)]` turns this off, as for a test that needs the network. |
| `#[retry(n)]` | A step that fails runs again from the start, up to `n` more times. One that passes only on a retry is reported as flaky. |
| `#[timeout(limit)]` | A step that runs longer than `limit`, such as `5m`, fails. The limit covers all its attempts together. |
| `#[device("kvm")]` | The step's sandbox has `/dev/kvm`, where the machine has it. It doesn't key the step. |
| `#[scratch]` | The step has a directory at `/pekit-scratch` that pekit keeps between its runs. It doesn't key the step. |
| `#[track_caller]` | An error in the function's body is reported where it was called. |

`#[retry]`, `#[timeout]`, and `#[track_caller]` mean the same in plain
GCSL. `#[hermetic]`, `#[device]`, and `#[scratch]` mean nothing to plain
GCSL, which runs such a call at once, as any call.
