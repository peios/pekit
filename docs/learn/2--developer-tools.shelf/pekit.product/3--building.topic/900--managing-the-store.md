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

To set it for every build on the machine, whatever their environment, put
`cache_limit = 20GiB` in `machine.conf` in pekit's configuration directory
instead (see [Sharing the Machine](~pekit/sharing-the-machine)). The
variable overrides the file.

A limit is a number of bytes, with a unit or none: `KiB`, `MiB`, `GiB`,
and `TiB` count in powers of 1024, and `KB`, `MB`, `GB`, and `TB` in powers
of 1000. `0` means no limit.

When a build ends with the store over its limit, pekit removes what is
cheapest to make again until the store is under nine tenths of the limit.
[Reuse and the Store](~pekit/reuse-and-the-store) explains the order. An
eviction waits until no other pekit process is using the store, so a build
that runs while another is running leaves it to a later build. If the store
is over its limit then, pekit warns that nothing was evicted.

## Keep room free on the disk

The store's limit counts the store, not what shares its disk: the
directories builds write outputs and scratch files to, and other programs.
To keep room free whatever fills the disk, set `keep_free` in
`machine.conf`, or `PEKIT_KEEP_FREE`:

```text
keep_free = 30GiB
```

Once less is free during a build, pekit starts no new step. Once none of
its steps is running, it evicts what the build hasn't used, if no other
pekit is using the store, and then goes on. It warns of what it evicted, or
why it couldn't. As the build ends, it evicts again if less is free.

Whether or not `keep_free` is set, a step that fails because the disk is
full runs once more, after pekit makes room for it. If it fails again, its
error says how much is free.

## See and evict the store

`pekit store stats` shows where the store is, how large it was when it was
last measured, its limit, when it was last evicted, and whether another
pekit process is using it:

```text
store: /home/me/.cache/pekit/store
size: 402.3 GiB, by its ledger, last walked 3 hours ago
limit: 400.0 GiB
last evicted: 2 days ago
in use: by another pekit
```

A build evicts only when it ends with no other pekit process using the
store, so on a machine where builds overlap, the store can grow past its
limit, and each build warns of it. `pekit store evict` evicts it now
instead. It waits for the builds that are using the store to end, and a
build that starts meanwhile waits for the eviction rather than holding
it off:

```sh
pekit store evict
```

`--limit` evicts to another size than the store's limit, such as
`--limit 300GiB` to make room ahead of a large build.

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

Keep each workspace's `pekit-out/` on the same file system as the store.
pekit links outputs to the store's files, and it can't link across file
systems, so it copies every output instead, and copies it again on every
build. A large build then writes far more to disk than it needs to. pekit
warns when this happens. To fix it, make `pekit-out` a symbolic link to a
directory on the store's file system.

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

It removes nothing while another pekit process has the store open, since
that process would lose the files it is using. Run it once no other build
is running. The store's lock file stays, so a pekit that was waiting for the
store finds it empty.

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

## Verify the store

If a store file might have been changed, for example by a `chmod -R` over
`pekit-out/`, or after a disk fault, verify the store:

```sh
pekit store verify
```

It reads every file the store keeps and hashes it again, and lists each
whose contents aren't those its name says, and each that isn't read-only.
Builds can run meanwhile. On a large store this takes minutes, reading on
up to eight threads, and it reports how far it is every few seconds.

To repair what it finds:

```sh
pekit store verify --repair
```

It makes each file that isn't read-only so again, and removes each whose
contents are wrong, so that the next build fetches or builds again what
needed it. Like `pekit store evict`, it waits for the builds that are
using the store to end, and builds that start meanwhile wait for it.
