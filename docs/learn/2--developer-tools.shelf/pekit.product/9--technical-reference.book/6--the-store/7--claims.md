---
title: Claims
description: How a build claims a step it missed in the store, so that
  another build that needs the step at the same time waits for its record
  rather than running it again.
---

A build that misses a step in the store claims it before it looks in the
remote cache or runs it (`try_claim`). The claim is the lock file
`claims/<base>` in the store, by the step's base receipt, the receipt
without its contexts (see [Receipts](~pekit/receipts)), held exclusively
with `flock`. The build holds it until the step is recorded, or has failed
every attempt, or the process ends.

A build that finds the step claimed waits for the claim on a thread of its
own (`Pool::claim`), not one of the pool's, holding nothing of the
machine's budget, and goes on with every other step meanwhile. Once it
holds the claim, it looks in the store again, as it looked first:

- the step's record is there, with everything it names: the step is found
  in the store, and its claim let go of;
- otherwise the build that held it failed the step or ended first: this
  build looks further, and runs the step, holding the claim.

A step's base receipt is the same whatever contexts it reads, so a build
can wait for a step that read a context bound otherwise, miss, and run its
own. That costs a wait, never a wrong result.

Claims are taken only where steps are taken from the store, not by
`--rebuild` or `--check-reproducible`. A claim that can't be opened or
locked isn't taken, and the step runs as it would have.

An eviction, which runs only while no other process has the store, removes
`claims/`. Nothing else removes a claim's file.
