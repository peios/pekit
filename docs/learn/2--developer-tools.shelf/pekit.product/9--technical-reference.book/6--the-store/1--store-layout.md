---
title: Store Layout
description: The directories of pekit's store, what each holds, and how
  pekit writes into them so that no reader sees anything in part.
---

The store is the directory `store/` under the cache directory, shared by
every workspace and every pekit process on the machine (`Store`). It
holds:

| Path | Holds |
|---|---|
| `cas/<xx>/` | Blobs by their SHA-256 digests, in directories named by a digest's first two hexadecimal digits, with their executable and numbered copies. |
| `records/<xx>/` | Step records, program records, manifests, and the records of fetches, unpacks, and git commits, by their keys. |
| `trees/` | Materialized trees, made once as directories of links to blobs. |
| `complete/` | A mark for each tree found to have everything it names, at any depth. |
| `seen/` | For each workspace root, the stamps of the files a build read. |
| `git/` | Bare clones of the repositories `fetch_git` fetched from. |
| `scratch/` | The scratch directories of steps marked `#[scratch]`, with their lock and size files. |
| `usage/` | One journal for each build of what it used, for eviction. |
| `eviction.json` | Each record's eviction priority, and what eviction removed. |
| `size`, `size.lock` | The blobs' total size as last measured and added to, and its lock. |
| `tmp/` | What is being written. |
| `lock` | The store's lock. |

## Writing

A blob, a tree, or a record is written in full into `tmp/`, under a name
holding the writing process's token, then renamed into place. A rename is
atomic, so a reader, or another process, sees a file whole or not at all.
A process that loses a race to write the same blob finds it there, and
drops its own.

A file put into the store is hashed as it is copied into `tmp/`, flushed to
the disk unless `PEKIT_NO_SYNC` is set, and renamed to its digest's name
(`Store::put_file`). A blob is read-only. A blob under its digest's name is
therefore whole even after a power loss, and is trusted as it's found.

A tree is put at once (`Store::put_tree_mounted`,
`Store::put_tree_seen_at`). Each new blob it holds, and each of its trees'
encodings, is written whole into `tmp/` as the tree is walked. Then 16
threads flush them all together, so that one commit of the file system's
journal serves many files. Only then is each renamed into place, in the
order it was written, so a tree's entries come before the tree. Flushing
files one by one cost a journal commit each: storing 20,000 small files
took 43 seconds, and takes 7 at once. A put that fails removes everything
it wrote.

## The lock

Every process holds `lock` shared, with `flock`, while it has the store
open, and an eviction or a sweep of stale files takes it exclusively,
without waiting. So an eviction or a sweep runs only while no other process
has the store, and a process that opens the store waits for one that is
running.
