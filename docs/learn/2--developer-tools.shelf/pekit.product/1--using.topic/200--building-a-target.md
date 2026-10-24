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
the workspace. Each step writes to a directory named after the target
and the step, `pekit-out/app/hello/cc/` for the step `cc` of `app/hello`.
A target that calls a function twice has steps `cc` and `cc.2`.

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
