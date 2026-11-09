---
title: On-Disk Layout
description: Every file and directory pekit reads or writes, in the
  workspace, the cache directory, and the state directory, with what each
  holds.
---

This appendix collects every path pekit uses. `CACHE` is the cache
directory, `$PEKIT_CACHE_DIR`, `$XDG_CACHE_HOME/pekit`, or
`~/.cache/pekit`. `STATE` is the state directory, `$PEKIT_STATE_DIR`,
`$XDG_STATE_HOME/pekit`, or `~/.local/state/pekit`. `CONFIG` is the
configuration directory: `$XDG_CONFIG_HOME/pekit`, else `~/.config/pekit`.
`BUDGET` is the machine's budget's directory: `$XDG_RUNTIME_DIR/pekit/budget`,
else `CACHE/budget`.

## In the workspace

| Path | Holds | Written by |
|---|---|---|
| `workspace.gcsl` | The root file, and `pekitconf`. | The user. |
| `pekit.lock` | The pinned libraries and their sources, as JSON. | `pekit lock`. |
| `.gcsl/libraries/<path>/` | Each pinned library, exported. | `pekit lock`, and restoring before a build. |
| `pekit-out/<target>/<function>[.N]/` | Symbolic links to each target's results. | Each build. |
| `pekit-out/.steps/<function>/<identity>/` | A step's outputs: copies of what targets gave, and read-only links for exposed outputs. | Each build. |
| `pekit-out/.fetch/<sha256>/<name>` | An exposed fetched file. | Each build. |
| `pekit-out/.unpack/<key>/` | An exposed unpacked archive. | Each build. |
| `pekit-out/.git/<key>/<name>/` | An exposed git commit's files. | Each build. |

## In the cache directory

| Path | Holds |
|---|---|
| `CACHE/git/` | Bare clones of the repositories libraries are pinned from. |
| `CACHE/scratch/step-<token>-<node>-<attempt>/` | A step attempt's staging directory: `w/`, `out/`, and `tmp/`. |
| `CACHE/scratch/swept-<token>/` | Staging directories renamed for the sweeper to remove. |
| `CACHE/scratch/kept-<token>-<node>/` | An empty scratch directory for one run of a step marked `#[scratch]`. |
| `CACHE/scratch/unpack-<token>-<n>/` | An archive being unpacked. |
| `CACHE/scratch/git-<token>-<n>/` | A git commit being checked out. |
| `CACHE/store/` | The store, below. |

## In the store

| Path | Holds |
|---|---|
| `lock` | The store's lock, held shared by every process with the store open. |
| `cas/<xx>/<digest>` | A blob, read-only. |
| `cas/<xx>/<digest>.x` | The executable copy of a blob. |
| `cas/<xx>/<digest>.rN`, `<digest>.x.rN` | Numbered copies of a blob. |
| `records/<xx>/<key>` | A record, as JSON: a step's, a program's, a manifest, a fetch's, an unpacking's, or a git commit's. |
| `trees/<digest>`, `trees/<digest>-<bases>` | A materialized tree's instance. |
| `complete/<digest>` | A mark that a tree has everything it names. |
| `seen/<digest>.json` | The stamps of a workspace's files, by a digest of its root's path. |
| `git/` | Bare clones of the repositories `fetch_git` fetched from. |
| `scratch/<digest>` | A step's scratch directory, with `<digest>.lock` and `<digest>.size`. |
| `claims/<base>` | The lock file of the claim on a step, by its base receipt. |
| `usage/` | A journal of what each build used. |
| `eviction.json` | Each record's eviction priority, and what was evicted. |
| `size`, `size.lock` | The ledger of the blobs' size, and its lock. |
| `tmp/` | Files and directories being written, each named with a process's token. |

## In the state directory

| Path | Holds |
|---|---|
| `STATE/keychain-key` | The machine's 32-byte key that secrets' identities are hashed with, mode `0600`. |
| `STATE/peaks/<digest>`, `STATE/peaks/kind-<digest>` | The last peaks of a step's program, and of its kind's. |

## The machine's budget

| Path | Holds |
|---|---|
| `CONFIG/machine.conf` | The budget's limits, `jobs`, `memory`, and `unseen`. Written by the user. |
| `BUDGET/job-<n>`, `BUDGET/memory-<n>` | The lock files of the budget's jobs and units of memory. |
| `BUDGET/waiting`, `BUDGET/gate` | The lock files a waiting program holds. |
| `BUDGET/fifo-<pid>-<n>` | A running program's jobserver. |

[The Machine's Budget](~pekit/the-machines-budget) says what each is.

## In a step's sandbox

[Paths in a Step](~pekit/paths-in-a-step) lists the paths a step sees.
