---
title: Managing the Store
type: how-to
description: Set how large pekit's store can grow, move it to another
  disk, and clear the output directory, the store, or both with pekit
  clean.
related:
  - pekit/concepts/reuse-and-the-store
  - pekit/concepts/where-results-go
---

pekit's store holds the result of every step it ran, shared by every
workspace on the machine. pekit keeps it to a size limit by itself, so most
machines need no management. This page covers the settings for when the
defaults don't suit a machine.

## Set the size limit

The store can grow to 100 GiB unless you set `PEKIT_CACHE_LIMIT`. To set a
limit of 20 GiB, set the variable in your shell's startup file:

```sh
export PEKIT_CACHE_LIMIT=20GiB
```

A limit is a number of bytes, with a unit or none: `KiB`, `MiB`, `GiB`,
and `TiB` count in powers of 1024, and `KB`, `MB`, `GB`, and `TB` in powers
of 1000. `0` means no limit.

When a build ends with the store over its limit, pekit removes what is
cheapest to make again until the store is under nine tenths of the limit.
[Reuse and the Store](~pekit/reuse-and-the-store) explains the order. An
eviction waits until no other pekit process is using the store, so a build
that runs while another is running leaves it to a later build.

## Move the store

The store is in pekit's cache directory. To keep it on another disk, set
`PEKIT_CACHE_DIR`:

```sh
export PEKIT_CACHE_DIR=/srv/cache/pekit
```

Without it, the cache directory is `pekit` under `$XDG_CACHE_HOME`, or
`~/.cache/pekit` if that isn't set either. Moving the variable doesn't
move the store: the next build starts with an empty store in the new
place. Move the directory yourself to keep what's in it.

The cache directory also holds the git repositories pekit fetched from,
and the directories steps are staged in. Keep it on a file system that
supports hard links, as every local Linux file system does.

## Clear what builds made

`pekit clean` removes the workspace's `pekit-out/`:

```sh
pekit clean
```

The store is untouched, so the next build finds its steps there and puts
`pekit-out/` back without running them.

`pekit clean --cache` also removes the store, the scratch directories of
steps marked `#[scratch]`, and the directories steps are staged in. The
next build runs every step again:

```sh
pekit clean --cache
```

Neither removes pekit's state directory, which holds the key that
identifies this machine's secrets. See [Using Secrets](~pekit/using-secrets).

To run every step again without losing the store, build with `--rebuild`
instead. It runs every step and every program, and records each as it
goes.

## Check the result

After `pekit clean --cache`, `pekit build` reports every step as run and
none found in the store.

## Don't change files in the store

pekit hard-links the store's files into the directories it stages steps
in, and into `pekit-out/.steps/` when it writes an intermediate file
there. A hard link is the same file, not a copy. Changing the permissions
or contents of one changes the store's file too, and pekit would then give
the changed file to later steps. Leave these files read-only.

The files each target gives are the exception: pekit copies them, so you
can change them freely.
