---
title: Retries and Timeouts
description: "How pekit runs a step's attempts under #[retry], what makes a
  step flaky, how #[timeout] spans the attempts, and what a step's failure
  does to the steps and targets after it."
---

GCSL applies `#[retry]` and `#[timeout]` to an ordinary call itself. For a
step's own call, pekit applies them instead, since the step's body runs as
a unit of its own on pekit's schedule.

## Attempts

A step's first attempt is attempt 0. When an attempt fails, by its code's
own failure, and the step's `#[retry(n)]` allows another, the step runs
again from its start, in a new sandbox staged afresh as
`step-<token>-<node>-<attempt>` under the cache's scratch directory. Nothing
of the failed attempt carries over: not its files, not its `/tmp`, and
not its programs' writes.

A step that passes on an attempt after the first is *flaky*. It is
recorded as any step that succeeds, and the build warns:

```text
warning: step `NAME` passed only when it was run again: it is flaky
```

`pekit test` reports a test whose step was flaky as `flaky`.

## Timeouts

A step's `#[timeout(limit)]` is a deadline for all its attempts together,
from the start of its first attempt. The time the pool spends staging an
attempt's sandbox doesn't count: the deadline starts once the first
attempt is staged. When the deadline passes, the running program is
stopped, and the step fails with "the step timed out", at the program it
was running. A timed-out step isn't retried.

## What a failure reaches

A step that fails after its attempts fails every step that needs its
result, without running them, with the same failure. A target whose value
needs the failed step fails with that failure, reported where the target's
code, or the library code it called, made the step. Other targets of the
same build carry on, and the command exits with status 1 once every target
has finished or failed.
