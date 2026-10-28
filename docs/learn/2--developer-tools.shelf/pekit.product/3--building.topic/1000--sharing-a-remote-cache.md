---
title: Sharing a Remote Cache
type: how-to
description: Share the steps one machine ran with every other through a
  remote cache that speaks Bazel's Remote Execution API, with CI writing
  and developers reading.
related:
  - pekit/concepts/reuse-and-the-store
  - pekit/building/managing-the-store
  - pekit/concepts/hermetic-and-strict-builds
---

pekit's store holds the steps one machine ran. A remote cache shares them
between machines, so a developer's build finds what CI already built. pekit
speaks Bazel's Remote Execution API (REAPI), so any server of it works, such
as bazel-remote, Buildbarn, BuildBuddy, or NativeLink.

## Read from a cache

Name the cache with `--remote-cache`, or set `PEKIT_REMOTE_CACHE` in your
shell's startup file:

```sh
export PEKIT_REMOTE_CACHE=grpcs://cache.example.org:443
```

A URL is `grpc://HOST:PORT`, or `grpcs://HOST:PORT` for TLS with the
system's certificate authorities. A Buildbarn server also needs its
instance name, given with `--remote-instance` or `PEKIT_REMOTE_INSTANCE`.

A step that isn't in the store is then looked for in the cache before it
runs. One found there is downloaded with every file it gave, each checked
against its digest, and put into the store. The build counts it as found in
the store, and says how many came from the cache:

```text
built app/hello: 0 steps run, 1 found in the store (1 from the remote cache)
```

Steps are looked for only where the store's results are reused. So
`--rebuild` and the second build of `--check-reproducible` never use the
cache.

## Write to a cache

A machine writes to the cache with `--remote-upload`, or with
`PEKIT_REMOTE_UPLOAD=1`. Each step it records is then sent with every file
it gave, but only the files the cache doesn't already have. The build waits
for its uploads before it ends.

Let CI write and developers only read. A step's result is keyed by what the
step is and everything it was given. A cache trusts the machines that write
to it to give the results those inputs give. CI's machines are the ones to
trust.

## Give credentials

Send credentials as headers, with `--remote-header`, which can be given
more than once, or with `PEKIT_REMOTE_HEADERS`, one header to a line:

```sh
export PEKIT_REMOTE_HEADERS='Authorization: Bearer TOKEN'
```

Keep credentials in the environment or on the command line, never in the
workspace's files, which everyone who clones the workspace reads.

## Which machines share steps

A step's receipt names the platform it ran on. A build that isn't strict
also names the host's packages, since its steps use the host's tools. So
those steps are shared only between machines with the same packages.

A strict build's steps use pinned toolchains and none of the host's tools.
They're shared between every machine of the platform, which makes strict
builds the ones a team gains most from sharing. See [Hermetic and Strict
Builds](~pekit/hermetic-and-strict-builds).

## When the cache is down

If a call to the cache fails, the build stops asking it and runs its steps
itself. It warns once, at the end:

```text
warning: the remote cache failed a call: Unavailable: ...; the build went on without the remote cache
```

A cache that gives a file that doesn't match its digest, or has lost one,
isn't trusted for that step. The step runs, and the build warns in the same
way.

`pekit(1)` documents each option and environment variable.
