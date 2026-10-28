---
title: Profiling a Build
type: how-to
description: Find where a build spends its time, with a trace of its steps
  and programs from --trace, and a profile of its GCSL code from
  --gcsl-profile.
related:
  - pekit/concepts/targets-and-steps
  - pekit/concepts/reuse-and-the-store
---

A slow build spends its time in one of two places: running steps, or
evaluating GCSL code. pekit can record either. A trace shows each step and
program on a timeline, and a GCSL profile shows which functions took the
time.

## See the steps on a timeline

1. Build with `--trace` and a file to write:

   ```sh
   pekit build --trace build-trace.json app
   ```

2. Open `build-trace.json` in [Perfetto](https://ui.perfetto.dev), or in
   `chrome://tracing` in a Chromium-based browser.

The trace has these rows:

- **steps.** One span for each attempt of each step, named by its function
  and identity, such as `compile/9927fb8abbee4a05`. A step found in the
  store is a mark rather than a span.
- **worker 1, worker 2, and so on.** The work each of pekit's workers did.
  Each program a step ran is a span, such as `exec cc`. So is pekit's own
  work for a step: `stage` places the step's inputs in its sandbox before it
  runs, and `keep` stores the files of its result.
- **evaluation.** One span for each time a target's or a step's code ran.

The trace names programs, but never shows their arguments, so it holds no
secrets.

Look for long spans on the steps row, and for gaps where few workers are
busy. A gap usually means that one step holds up the rest, and that
splitting it would let more run at once.

## See where the code spends its time

When even a build where nothing changed is slow, the time is in evaluating
code, not in running steps.

1. Build with `--gcsl-profile` and a file to write:

   ```sh
   pekit build --gcsl-profile profile.txt app
   ```

2. Read `profile.txt`. It starts by counting how many times the targets'
   code and the steps' code ran, and how long that took. A table follows,
   with a row for each function and built-in that ran, the most expensive
   first:

   ```text
       own (ms)   total (ms)      calls      found  kind     function
          0.052        0.055          1          0  fn       link  build.gcsl:13
          0.037        0.028          3          0  fn       exec  <stdlib>/prelude.gcsl:57
          0.024        0.027          1          0  fn       adder  build.gcsl:28
   ```

Each row gives the time spent in the function's own body, its total time
including the calls it made, and how many times it was called. For a
memoized function, `found` is how many of those calls found an earlier
result.

A target's code stops each time it waits for a step or an effect, and
carries on when the result is ready. So a target that waits a hundred
times runs about a hundred times, but never runs the same code twice. A
function called thousands of times, once on each run of a target, is
usually worth memoizing with `->>`.

Timing every call slows the build, so a profiled build takes longer than
an ordinary one. Compare the rows with each other, not with a build you
timed without the profile.

## Rebuild everything to measure it

A build that finds its steps in the store measures nothing of them. To
time every step, add `--rebuild`, which runs every step and every program
again:

```sh
pekit build --rebuild --trace build-trace.json app
```
