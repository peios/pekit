---
title: Building a Target
type: guide
description: Build a target, reuse what is unchanged, and find the results.
---

`pekit build hello` builds the `build` definition called `hello` (see
~pekit/naming-targets). Run it from any directory in the workspace:

```sh
pekit build hello
```

Pekit runs the steps the target needs and shows what it built:

```text
built app/hello: 1 step run, 0 found in the store
  ../pekit-out/app/hello/cc/hello
```

## Steps

A step is a call of a function marked `#[hermetic]`. Pekit runs each step
in a sandbox of its own, which holds only what the step is given: the
files passed to it, and the results of the steps before it. A step can't
read anything else in the workspace, and has no network. It sees the
workspace at `/w` and results at `/pekit-out`, the same on every machine,
so the paths in a build never depend on where your checkout is.

Steps that don't depend on each other run at the same time. `--jobs 4`
runs at most four programs at once. By default pekit runs as many as the
machine has processors. In a terminal, a line shows the steps running and
how many have finished while the build goes.

## Seeing where the time went

`--trace` writes a timeline of the build that `chrome://tracing` and
[Perfetto](https://ui.perfetto.dev) can open:

```sh
pekit build hello --trace build-trace.json
```

Each attempt of a step is a span on the *steps* row, and each program a
step runs is a span on the row of the worker that ran it. A step found in
the store is a mark. The trace names programs but never shows their
arguments, so it holds no secrets.

## Reusing what is unchanged

Pekit records every step that succeeds, keyed by everything that can
change what it gives: its code, its arguments, the contents of the files
it is given, and where it writes. When you build again and none of that
has changed, pekit takes the step's result from its store instead of
running it:

```text
built app/hello: 0 steps run, 1 found in the store
```

Change a file a step reads, and that step runs again, along with each
step that depends on what it gives. A step whose result holds a secret
is never recorded, so it runs every time.

The store is in pekit's cache directory: `$PEKIT_CACHE_DIR` if you set
it, else `pekit` under `$XDG_CACHE_HOME`, else `~/.cache/pekit`.

`--rebuild` runs every step again, reusing nothing from the store, and
records each as it goes.

`pekit clean` removes `pekit-out/`. `pekit clean --cache` removes the
store too, so the next build runs every step. Neither removes the state
directory, which holds this machine's keychain key.

## Toolchains

A library asks GCSL's `toolchain` module for a compiler, as
`toolchain.get(Toolchain::C)`, rather than naming one. With nothing
registered, the answer is the host's own `cc`, found on `PATH`, which
builds for the host alone. To build with pinned toolchains, register them
in `pekitconf` in `workspace.gcsl`. The gcc library provides Bootlin's
compilers, and the buildenv library a build environment for upstream
build systems:

```text
import * from "github.com/gcslib/gcc"
import * from "github.com/gcslib/buildenv"
import Toolchain from toolchain

pekitconf = {toolchains = [
    Toolchain::C gcc_x86_64,
    Toolchain::C gcc_x86_64_to_aarch64,
    Toolchain::BuildEnv busybox_x86_64,
]}
```

Pekit sets the `toolchains` context from `pekitconf.toolchains` for every
target it builds, and the first registered toolchain that fits the
platforms is used. A step is given its toolchain as an argument, so a
changed compiler rebuilds exactly the steps that use it. A pinned
toolchain's programs run on its own runtime, never on the host's
libraries. See ~pekit/building-c for the gcc library itself.

## Secrets

A build asks GCSL's `keychain` module for its secrets by name, such as
`keychain.file("module-signing")`, in a parameter default, so that the
step is given the secret. Say where each one is in `pekitconf`, for each
profile:

```text
import keychain

pekitconf = {keychain = [
    {profile = "dev"; name = "module-signing"; file = "keys/test.pem"},
    {profile = "release"; name = "module-signing"; file = "/run/secrets/module-signing.pem"},
]}
```

A relative path is relative to the workspace's root. `pekit build
--profile release` uses the `release` entries; the profile is `dev`
otherwise. A secret with no entry comes from the environment, as in plain
GCSL: `$GCSL_SECRET_FILE_MODULE_SIGNING` names its file.

A step given a secret file finds it at `/pekit-secrets/<profile>/<name>`,
read-only, the same path on every machine. No other step sees it, and
pekit never copies it into the store. What the step makes with it, such
as a signed file, is kept as any output is. A step can't ask the keychain
itself: give it the secret instead.

Pekit identifies a secret by its profile, its name, and a hash of its
contents keyed by this machine's own key, kept in the state directory,
`~/.local/state/pekit` unless `$PEKIT_STATE_DIR` or `$XDG_STATE_HOME`
says otherwise. A changed key is a new identity, so nothing made with the
old one is reused, and the identity tells nothing about the key itself.
Two machines identify the same secret differently, so a step given a
secret runs on each machine that builds it.

## Libraries

A build file imports a library by its path, such as
`import "github.com/gcslib/gcc"`, as GCSL does: pekit finds it in each
directory of `$GCSL_LIBRARY_PATH`, then in `.gcsl/libraries/` at the
workspace's root. A library's steps are keyed by its path and contents,
never by where it was found, so the same library in another place gives
the same results from the store.

## Pinning libraries

`pekit lock` pins each library the workspace imports, and each library
those import, at a commit of the git repository it comes from, in
`pekit.lock` at the workspace's root:

```text
$ pekit lock
pinned github.com/gcslib/gcc at 4834d15186dbf67463ca1410d67bc578faca1f4e
```

It exports each into `.gcsl/libraries/`. Commit `pekit.lock` and every
checkout builds with the same libraries: any command that builds puts
back a pinned library that isn't there, from the commit it pins, and
warns of one there that differs from it. `pekit lock` pins only what
isn't pinned yet, and drops each library that nothing imports. `pekit
lock --update` pins every library again, at what its repository holds
now.

By default, a library's repository follows Go's convention: the library
`github.com/gcslib/gcc` is the repository `https://github.com/gcslib/gcc`,
at its `HEAD`. To take libraries from elsewhere, give their prefix a
source in `pekit.lock`: a repository, the directory in it that holds the
prefix's libraries, and the branch, tag, or commit to follow.

```json
{
  "sources": [
    {"prefix": "github.com/gcslib", "git": "https://example.org/gcslib.git", "dir": "libs", "rev": "main"}
  ]
}
```

Repositories are kept in pekit's cache directory and fetched only when
pekit needs a commit it doesn't have, so a workspace whose libraries are
pinned builds offline. A strict build, `--strict`, fails unless its
libraries are exactly those `pekit.lock` pins: every library it imports
pinned, none differing from its pin, and `$GCSL_LIBRARY_PATH` unset.

## Fetching files

`fetch(url, sha256)` is the only part of a build that uses the network.
Pekit keeps every file it has by its SHA-256 digest, so a pinned file it
already has is never downloaded again, on any machine that shares the
store. Otherwise pekit downloads it and keeps it only if its digest
matches. Leave the digest out, and the build fails with the one it
found, ready to paste in.

A step can't fetch, since it has no network. Fetch in the code that calls
the step, and pass it the file.

`archive.unpack` works in a target or in a step. Pekit unpacks archives
itself, so a step needs no `tar`, and keeps what it unpacked, so each
archive is unpacked only once.

## Where results go

Pekit copies the files the target gives to `pekit-out/` at the root of
the workspace. Each step writes to a directory named after the
definition whose code made it and the step, `pekit-out/app/hello/cc/`
for the step `cc` of `app/hello`. A target that calls a function twice
has steps `cc` and `cc.2`.

A step that `build lib()` makes is `lib`'s, whichever target, test, or
script calls it, so building `lib` itself finds it in the store. Two
targets that make the same call in their own code have a step each. To
share the work, put it in a build of its own that both call.

The copies are your own: you can change or delete them, and the next
build puts them back.

As the build goes, `pekit-out/` also holds every step's outputs, so a
target can read what a step made and decide what to do next, such as
compiling each file a generator wrote. These intermediate files are
read-only.

## Failures, retries, and timeouts

If a step fails, pekit shows its error where it happened in your build
file, with what the program printed, and exits with status 1.

A function marked `#[retry(2)]` runs up to twice more if it fails, each
time from the start in a fresh sandbox. A step that passes only on a
retry is flaky, and pekit warns about it. `#[timeout(5m)]` limits how
long all of a step's attempts can take together.

## Platforms

Steps run on the machine pekit runs on, their *execution platform*. If
your workspace declares a `host_platform` context, as the build libraries
do, pekit sets it to that machine for every target it builds, as a record
such as `{arch = "x86_64"; os = "linux"}`. Toolchains read it to choose
what runs where. Every step is keyed by its platform too, so results from
one kind of machine are never reused on another.

What a build is *for* is the `platform` context, by default the machine
steps run on. `--platform aarch64-linux` builds for another, as
`with { platform = ... }` would for every target, so the toolchains for
that platform are chosen. `--platform aarch64` keeps the machine's
operating system.

## Dynamic dependencies

A target can wait for a step's result, to decide what to build next from
what a generator wrote, say. Pekit then builds that step, and carries on.
`--no-dynamic` fails a target that waits for a step's result, at the
point it waits, so that what a build runs is known before anything runs.
A target waiting for a file it fetches or unpacks is not a dynamic
dependency.

## The host's tools

Every step can see the host's own tools: its `/usr`, `/bin`, `/lib`,
`/lib64`, and `/sbin`, read-only. A step that runs its programs in a
sandbox of their own, as the build libraries do with pinned toolchains
and environments, uses none of them. `/usr/local` is hidden from steps. A
step whose program doesn't set `PATH` runs with
`/usr/bin:/bin:/usr/sbin:/sbin`.

Such a build isn't hermetic, and pekit notes this when it runs a step.
So that an upgraded compiler doesn't leave stale results in the store,
pekit keys every step by a fingerprint of the host's packages, which
changes whenever a package is installed, upgraded, or removed. Pekit
knows the package databases of dpkg, rpm, apk, and pacman. On a host it
can't identify, pekit warns, and doesn't reuse steps from the store.

## Strict builds

`--strict` builds as a release should: from what the workspace pins, and
nothing of the machine it runs on. Its steps see none of the host's
tools, so a step must run its programs from a pinned toolchain or
environment. A target's own code, outside its steps, can fetch and
unpack files, ask the keychain for secrets, and read the workspace and
the build's outputs. It can't run a program, write a file, or read
anything else of the host: each of these fails where the target does it.
The host's own toolchains, such as `system_c`, look for the host's
compiler, so they fail a strict build.

A strict build's steps are keyed apart from a build's that borrowed the
host's tools, and not by the host's packages, so their results are the
same on any machine of the platform.

## Checking that a build is reproducible

`--check-reproducible` builds, then runs every step again, reusing
nothing from the store, and compares what each gave with what it gave
first. A step that gives other outputs from the same inputs fails the
command, named with each file that differs:

```text
error: a step is not reproducible: run again with the same inputs, it gave other outputs
  app/drift/stamp
    /pekit-out/app/drift/stamp/out/sub/t
```

Only steps given the same inputs both times are compared, so a step
downstream of one that differed isn't reported as well. A step's first
record stays in the store. `pekit run` and `pekit publish` change the
world outside the build, so they can't be checked by doing them twice:
check what they build with `pekit build --check-reproducible`.
