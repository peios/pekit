---
title: Fetches, Unpacks, and Git Commits
description: How pekit answers fetch, archive.unpack, and fetch_git from
  the store, where each result's path comes from, and how each is kept and
  recorded.
---

pekit answers GCSL's `fetch`, `archive.unpack`, and `fetch_git` itself,
never running the programs plain GCSL runs. Each result is named by its
contents, so it has the same path in every build, and is kept in the store
so that it is fetched or unpacked once on a machine.

## Fetches

`fetch(url, sha256)` is the only part of a build that touches the network
(`fetch`). With a pin:

1. A blob of the pinned digest in the store is the file, and the network is
   not used.
2. Otherwise the file is downloaded on the host, hashed as it is streamed
   into the store, and kept only if its digest is the pin. A download that
   goes 60 seconds without a byte fails.

The file's path is `/pekit-out/.fetch/<sha256>/<name>`, where `<name>` is
the URL's last part as GCSL names it. Without a pin, the file is downloaded
only to report its digest, and the call fails with that digest. A pin that
is no digest, or a file that doesn't match, fails the call too.

How long a download took is recorded, for eviction to weigh.

## Unpacks

`archive.unpack(archive, strip)` is answered by pekit's own unpacker
(`unpack`), since the build environment that would give a step `tar` is
itself an archive.

- **Formats.** An archive is read as zip by its first bytes, as a wheel or
  a jar is; otherwise as gzip, xz, bzip2, or zstd, or a plain tar.
- **Paths.** Each entry's path has its first `strip` components left off,
  and an entry with no more is left out. An entry whose path would leave the
  directory, by being absolute, holding `..`, or passing through a symbolic
  link the archive made, fails the unpacking. Nothing is ever written
  outside it.
- **Kinds of entry.** A symbolic link is kept as it is, and a sandbox
  resolves it inside itself. A hard link becomes a copy. A device or a pipe
  is left out. Of each file's mode, only whether it is executable is kept.

An unpacking is keyed by the archive's digest and `strip`, and recorded as
the tree it made. Its path is `/pekit-out/.unpack/<key>`. It runs in a
directory `unpack-<token>-<n>` under the cache's scratch directory, then is
put into the store.

## Git commits

`fetch_git(url, commit)` gives the files of the repository at the commit,
whose whole name pins its files as a digest does (`git_fetch`):

1. A record for the URL and commit names the tree of its files, which the
   store holds whole.
2. Otherwise the commit is fetched into a bare clone of the URL in the
   store's `git/`: the commit alone, shallowly, or, if the server won't give
   a commit by its name, with every branch and tag. It is checked out into
   `git-<token>-<n>` under the cache's scratch directory, without `.git` or
   submodules, put into the store, and recorded with how long it took.

Fetches of one repository take turns, since a build fetches several crates
of one repository at once into one clone. One that waited finds the record
the other made.

Git runs with no configuration but the repository's own: the user's and
the system's are left out, so that nothing of the machine, such as a
line-ending conversion or a filter, changes the files a commit gives. The
path is `/pekit-out/.git/<key>/<name>`, where the key is a digest of the URL
and the commit.

## In a step

A step has no network, so `fetch` and `fetch_git` in a step's code fail.
`archive.unpack` works in a step, on an archive the step was given, and is
answered and kept the same way.
