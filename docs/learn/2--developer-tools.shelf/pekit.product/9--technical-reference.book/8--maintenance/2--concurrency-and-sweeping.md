---
title: Concurrency and Sweeping
description: How several pekit processes share one store and cache
  directory, how each names what it makes, and how what a killed process
  left is removed.
---

Any number of pekit processes can use one store at once, from one workspace
or several. Each writes only through names of its own and atomic renames,
and nothing they share is changed in place.

## The store's lock

Every process holds the store's `lock` file shared while it has the store
open. An eviction, and a sweep of what killed processes left, take it
exclusively without waiting, and do nothing if they can't. So neither runs
while another process has the store open, and a process opening the store
waits for one that is running.

## Names of its own

Every process names what it makes in a directory others share, the store's
`tmp/` and the cache's `scratch/`, with its *token*: 16 hexadecimal digits
read once from `/dev/urandom` (`sweep::token`). A process's number isn't
used, since another process sharing the store, in another PID namespace,
can have the same one.

## Races

Two processes that make the same blob, tree, record, or materialized tree
each write their own in `tmp/` and rename it into place. The rename that
loses finds the other's there, and drops its own, which is as good, since
both are of the same contents. Within a process, a lock for each
materialized tree, and for each repository `fetch_git` fetches into, keeps
two threads from doing the same work.

Two runs of one step marked `#[scratch]` don't share its scratch directory:
the first holds it by a lock, and the other gets an empty one of its own.

## Sweeping as a build goes

A step's staging directory often holds tens of thousands of links, and
removing it takes as long as staging it. So a build renames a finished
step's staging directory, which takes no time, and a thread of its own, the
*sweeper*, removes it while the build goes on. The build waits for the
sweeper before it ends, and fails if a directory couldn't be removed.

## Sweeping what a killed process left

A process killed before it removed what it made leaves it behind. Once the
sweeper is done, a build tries the store's lock exclusively. If it gets it,
no other process has the store open, so everything in the shared
directories is some dead process's or its own, and it removes
(`sweep::sweep_stale`):

- Every entry of the store's `tmp/` that isn't its own
- Every staging directory, swept directory, empty scratch directory,
  unpacking, and checkout in the cache's `scratch/`: the entries whose
  names start with `step-`, `swept-`, `kept-`, `unpack-`, or `git-`

If another process has the store, nothing is swept, and a later build does
it.
