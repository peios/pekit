---
title: Failures of a Build
description: What goes wrong in a pekit build, what each failure looks
  like, the signal it gives, and how to get back to a known state.
---

pekit reports every failure on standard error, prefixed `error:`, and
exits with status 1. A failure in GCSL code is shown at the line that made
it, with a library's line shown beneath as "failed here, in library code"
where a library raised it. This chapter lists failures by where they
happen.

## Before anything is evaluated

| Failure | What it looks like | Cause and recovery |
|---|---|---|
| No workspace | An error naming the directory searched from. | No `workspace.gcsl` at or above the current directory. Run pekit inside a workspace. |
| A check error | GCSL diagnostics, with the file and line. | A source doesn't check. Nothing runs until every source checks. |
| A build and a script of one name | An error naming both. | One directory declares `build NAME` and `script NAME`. Rename one. |
| An ambiguous name | "`build hello` is defined in several places", with each candidate. | Several directories define the name. Give its qualified name. |
| An unknown name | "no `build` is named `hello`", with the closest names. | A typo, or a definition of another kind. |
| A bad cache limit | "`PEKIT_CACHE_LIMIT` is `X`, which is no size". | Set it to a number with a unit, or `0`. |
| A strict build's libraries | "a strict build uses the libraries `pekit.lock` pins, and only them", with each problem. | Run `pekit lock`, remove a changed library's directory, or unset `$GCSL_LIBRARY_PATH`. |
| A library can't be put back | A git error naming what was run. | The repository or commit is unreachable. Check the network and the lock's `git` field. |

## While evaluating

| Failure | What it looks like | Cause and recovery |
|---|---|---|
| An unpinned fetch | "`fetch` of `URL` is not pinned: its sha256 is `DIGEST`". | Add the digest the error gives, after checking it. |
| A fetch that doesn't match | An error naming the pin and the digest found. | The server's file changed, or the pin is wrong. Nothing is kept. |
| A fetch in a step | "`fetch` runs outside hermetic steps". | Fetch in a parameter default or the caller. The same holds for `fetch_git` and the keychain. |
| A strict refusal | "a strict build refuses a target's own `NAME`", or "reads only the workspace and the build's outputs". | Move the effect into a step, or out of the release. |
| A dynamic dependency under `--no-dynamic` | An error where the target waits for a step's result. | Remove `--no-dynamic`, or restructure the target. |
| Units waiting for each other | "the build's targets and steps wait for each other's shared results". | Two units each need a memoized result the other is computing. Restructure the shared code. |
| No toolchain | An error naming the kind and both platforms, and saying that registering replaces the default. | Register a toolchain of the kind for the platforms. |

## While running a step

| Failure | What it looks like | Cause and recovery |
|---|---|---|
| A program fails | "`PROGRAM` exited with status N", at the `exec`, with its standard output and standard error. | The step's own failure. Its retries run first, if it has any. |
| A timeout | "the step timed out", at the program it was running. | The step's attempts together ran past its `#[timeout]`. |
| A path outside the step | "it is outside the step". | The step uses a file it wasn't given. Pass it as an argument. |
| A write to an input | An error that the path is an input of the step, or a finished composition. | Work on a copy, or in a `Dir.sandbox`. |
| A mount point moved | A program fails with "Device or resource busy". | A program tried to move or remove a directory a composition mounts. Copy it first. |
| A result outside the step | An error naming the file the result names. | A step gave a path it neither made nor was given. |
| No bubblewrap | "pekit runs programs under bubblewrap (`bwrap`), which is not installed". | Install bubblewrap. |
| An unknown device | "pekit gives a step no device `NAME`: the one it knows is `kvm`". | Use `#[device("kvm")]` only. |
| A flaky step | A warning: "step `NAME` passed only when it was run again: it is flaky". | Not a failure. Find what makes it vary. |

A step that fails fails every step that needs its result, unrun, with the
same failure, and every target that needs it. Other targets carry on.

## At the end of a build

| Failure | What it looks like | Cause and recovery |
|---|---|---|
| Not reproducible | "a step is not reproducible", with the step and its files. | The step's outputs vary. See the common causes on [Checking That a Build Is Reproducible](~pekit/checking-reproducibility). |
| A staging directory not removed | An error from the sweeper. | Something under the cache's `scratch/` can't be removed. Check its permissions. |
| The store over its limit | A warning that it "could not be kept within `PEKIT_CACHE_LIMIT`". | Not a failure. Another process might hold the store; a later build evicts. |
| The host unidentified | A warning that no step is reused from the store. | The host has no package database pekit knows. Build strictly to reuse steps. |

## Getting back to a known state

These restore a known state, from least to most drastic:

1. **Remove a changed library.** Remove its directory under
   `.gcsl/libraries/`; the next build puts back the pinned one.
2. **`pekit clean`.** Removes `pekit-out/`. The next build puts every
   target's results back from the store, running nothing.
3. **`--rebuild`.** Runs every step and program again, and records each. It
   doesn't repair a changed blob: a blob already in the store isn't
   written again.
4. **`pekit clean --cache`.** Removes the store, its scratch directories,
   and the cache's staging directories. The next build runs everything.
   The state directory, and with it the machine's keychain key, stays.

## A store whose blobs were changed

A blob is shared by hard links with every staging directory, materialized
tree, and exposed output that holds it. Changing one of those files, its
contents or its permissions, changes the blob, and pekit doesn't detect
it: later steps are given the changed file, and their records are wrong.
A blob's name is its contents' SHA-256, so a blob can be checked by hashing
it and comparing the digest with its name, ignoring a `.x` or `.rN`
suffix. A plain blob's mode is `0444`, and an executable one's `0555`.
After such a change, `pekit clean --cache` is the safe recovery.

## A killed build

A build killed part way leaves files in the store's `tmp/` and staging
directories in the cache's `scratch/`. Nothing it left is seen as complete,
since everything is renamed into place only once whole. The next build that
finds no other process using the store sweeps them, as [Concurrency and
Sweeping](~pekit/concurrency-and-sweeping) describes.
