---
title: The Remote Cache
description: How pekit keeps records and their contents in a server of
  Bazel's Remote Execution API, how a step missing from the store is looked
  for there, and how a recorded step is written to it.
---

A remote cache holds records and their contents for many machines. pekit
uses a server's content-addressable store and action cache, never its
execution service (`remote`). The server is any of the Remote Execution
API, version 2, spoken over gRPC.

## Records as action results

- **Contents.** A pekit blob is named by the SHA-256 digest of its
  contents, as a REAPI blob is, so a file is the same blob in both.
- **Keys.** A record is kept as an action result. Its action cache key is
  the digest and size of an *action blob*: `pekit-action-1`, a zero byte,
  and the record's key in hex. The action blob is uploaded too, so a
  server that checks an action exists finds it.
- **Outputs.** The result's output files are the record's own encoding, at
  `record`, and each file the record names, at `blobs/<digest>`. Its output
  directories are each tree the record names, at `trees/<digest>`, each as
  a REAPI `Tree` message.
- **What a record names.** Every 64-digit hex string in the record's
  encoding that names a blob in the store, a tree if the blob is a tree's
  encoding, as eviction finds them.

A `Tree` message holds a root `Directory` and every directory under it,
each once. A `Directory` lists its files, with digest, size, and whether
each is executable, its directories by digest, and its symbolic links by
target. pekit's trees name the same entries, so a tree is made again from
its `Tree` message and has the same digest.

## Finding a step

A step whose receipt has no usable record in the store is looked for in
the cache, as work on the pool, never on the thread that evaluates
(`engine::looked_remotely`):

1. Its manifest is asked for by its key and merged into the store's: each
   set of contexts either holds is kept, the store's first.
2. A receipt is made for each set of the merged manifest, from the values
   the step's contexts give now. Each is asked for in turn, until one is
   found.
3. The step is looked up in the store again. Found, it is counted as found
   in the store and as found remotely. Missed, it runs.

A record found is put into the store only once everything it names is
there:

- Every file blob the result and its trees name that the store lacks is
  downloaded and checked against its digest.
- Each tree is made again from its `Tree` message and checked against the
  digest its path names.
- The record is written last.

A blob that is missing, or not its digest's contents, fails the lookup.
The record is not written, so the step runs. Blobs already downloaded stay
in the store, each its digest's.

Steps are looked for only where records are reused. A build with
`--rebuild`, the second build of `--check-reproducible`, and a build on a
host whose packages cannot be identified ask the cache nothing.

## Writing a step

A build that uploads writes each step it records, after the record and the
manifest are in the store, as work on the pool:

1. The cache's manifest for the step's base is merged into the store's.
2. For the step's record and then the manifest, the cache is asked which
   of their blobs it lacks.
3. Those blobs are sent, batches of small ones by `BatchUpdateBlobs` and
   each large one by `ByteStream`.
4. The action result is written.

The build waits for its uploads before it ends, and before the store is
evicted. A failed upload fails no build.

## Transfers

- **Batches.** A batch is kept within the cache's
  `max_batch_total_size_bytes`, and within 4 MiB, gRPC's default message
  limit, counting 256 bytes for each blob besides its contents. A blob too
  large for a batch is read or written by `ByteStream`, in parts of 1 MiB,
  and a read blob is hashed as it is written to the store.
- **Empty blobs.** The empty blob is never transferred, since every cache
  has it.
- **Capabilities.** The cache's capabilities are asked for once, before
  its first transfer. A cache that names its digest functions without
  SHA-256 is not used.

## Configuration and failure

The cache's URL, instance name, headers, and whether to upload come from
`pekit`'s options and environment, never from the workspace's files.
`pekit(1)` documents them. One runtime of two threads per process runs
every call, and the pool's threads wait on it.

Connecting has 10 seconds. Once a call fails, because the cache cannot be
reached or refuses it, the process asks the cache nothing more and builds
as if it had none. A lookup that fails for a missing or wrong blob affects
that step only. The build's first failure of either kind is reported as a
warning when it ends.
