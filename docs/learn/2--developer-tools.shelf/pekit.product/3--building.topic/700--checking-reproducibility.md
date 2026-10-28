---
title: Checking That a Build Is Reproducible
type: how-to
description: Find steps that give different outputs from the same inputs
  with --check-reproducible, read its report, and fix the usual causes.
related:
  - pekit/concepts/hermetic-and-strict-builds
  - pekit/shipping/releasing
---

A reproducible step gives the same outputs, byte for byte, every time it
runs with the same inputs. pekit relies on this when it reuses results, and
a release relies on it so that anyone can rebuild what you published and
get the same bytes. `--check-reproducible` finds the steps that don't.

## Check a build

1. Build with the check:

   ```sh
   pekit build --check-reproducible stamped
   ```

   pekit builds the targets, then runs every step again, reusing nothing
   from the store, and compares what each step gave the second time with
   what it gave the first.

2. Read the report. A step that gave other outputs fails the command, named
   with each file that differs:

   ```text
   error: a step is not reproducible: run again with the same inputs, it gave other outputs
     .steps/stamp/7367551baf538abf
       /pekit-out/.steps/stamp/7367551baf538abf/stamp.txt
   ```

   The first line under the error is the step's directory under
   `pekit-out/`, and the lines under it are its files that differed.

3. Fix the step, and check again.

## Check the result

When every step is reproducible, the command reports the build as usual,
and exits with status 0.

## What the check compares

pekit compares only steps given the same inputs both times. A step whose
input differed because an earlier step wasn't reproducible isn't reported
too, so each report points at the step where the difference started.

The second run doesn't change the store: each step's first record stays.
`--check-reproducible` works with `pekit build`, `pekit test`, and
`pekit package`. It doesn't work with `pekit run` or `pekit publish`,
which change things outside the build and can't be done twice. Check what
they build with `pekit build --check-reproducible` instead.

## Common causes

Most steps that aren't reproducible record something of the moment they
ran:

- **Times.** A timestamp written into an output, or an archive that
  records each file's modification time. Many tools honor
  `SOURCE_DATE_EPOCH`; set it in the step's `exec`. The gcc library's
  `archive` uses `ar rcsD`, which records no times.
- **Order.** A list of files in the order a directory returned them.
  Sort it.
- **Randomness.** A random identifier, or a hash table iterated in an order
  that changes between runs.
- **Parallel tools.** A tool that writes its output in the order its own
  threads finish.

A step that uses the host's tools can also differ between machines, which
this check doesn't see, because it runs both times on one machine. Build
with `--strict` to keep the host out. See [Hermetic and Strict
Builds](~pekit/hermetic-and-strict-builds).
