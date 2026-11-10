---
title: A Target's Own Effects
description: How pekit runs the effects of a target's code outside any
  step, which effects it answers itself, and what a strict build refuses.
---

A target's own code is not hermetic. Its effects run on the *host view*
(`HostView`), one at a time in program order, every time the code runs.
Fetches, unpacks and some reads are isolated and run out of order, as
[Order](~pekit/the-pool#order) describes. None of them is cached.

## The host view

The host view maps the logical layout onto the host. GCSL's own file-system
work, such as `fs.read` or `dir.files()`, maps `/w` to the workspace's root
and `/pekit-out` to its output directory, and any other absolute path to
itself. A relative path given to `file` or `dir` resolves against the
directory of the file it is written in, never the working directory. The
`fs` built-ins, such as `fs.exists` and `fs.read`, take only absolute
paths: GCSL refuses a relative one.

A program a target runs runs under bubblewrap, with the host's own
directories bound where they are and the workspace and outputs bound at
`/w` and `/pekit-out`, so that it sees the paths GCSL code gave it. It has
the network and the environment pekit was run with. Its working directory
is the target's directory under `/w`. Because it runs under bubblewrap, a
program can't gain privileges: `sudo` fails in one. Temporary directories
GCSL makes for a target are under the cache directory, never `/tmp`.

## Effects pekit answers itself

pekit answers some effects itself rather than running them as plain GCSL
would:

- **`fetch`.** From the store by the pinned digest, or downloaded on the
  host and kept only if its digest matches. See [Fetches, Unpacks, and Git
  Commits](~pekit/fetches-unpacks-and-git).
- **`fetch_git`.** From the store, or fetched into a bare clone and checked
  out.
- **`archive.unpack`.** Unpacked by pekit itself, without `tar`, and kept.
- **`keychain.string` and `keychain.file`.** From the workspace's keychain
  configuration or the environment. See [Secrets](~pekit/secrets-in-steps).
- **A read of an output.** `dir.files()` and `file.read()` of a path inside
  a build's output are answered from the store's tree of it until a target's
  own effect has run a program or written. See
  [Exposure](~pekit/exposure).

`fetch`, `fetch_git`, and the keychain are refused inside a step, each with
an error saying to resolve it in a parameter default or in the caller and
pass the result in.

## What a strict build refuses

Under `--strict`, a target's own effect is refused unless it is one of
these:

- `fetch`, `fetch_git`, `archive.unpack`, `keychain.string`, or
  `keychain.file`
- A read, `fs.exists`, `fs.read`, `file`, `file.read`, `dir`, or
  `dir.files`, of a path at or under `/w` or `/pekit-out`

pekit judges a path once each `.` is left out and each `..` has taken the
component before it, so `/w/../etc` is `/etc`. It follows no symbolic
link, so a link in the workspace to a directory outside it, such as a
package repository, can be read. A read elsewhere fails with
"a strict build reads only the workspace and the build's outputs, and this
reads `PATH`", `PATH` as judged. A relative path is left to GCSL, which
refuses it. Any other effect fails with "a
strict build refuses a target's own `NAME`: a release's targets run
programs and write files only in steps". Each failure is reported where the
target's code made the effect.

The host's toolchains, such as `system_c`, call `which` to find the host's
tools, which a strict build refuses, so they fail a strict build where
their definition runs.

## Dynamic dependencies

A target's unit that stops for a step's result has a *dynamic dependency*:
what it builds after depends on what the step made. The step runs, its
result is given, and the unit resumes.

Under `--no-dynamic`, a target whose unit stops for a step's result fails
there instead. Only a stop for a step counts. A stop for an effect of the
target's own, such as a fetch, is part of evaluating it.
