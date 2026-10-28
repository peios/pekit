---
title: Platforms and Toolchains
type: concept
description: How pekit tells the machine steps run on from the platform a
  build is for, how a library asks for a compiler, how a workspace
  registers its toolchains, and how pinned toolchains keep the host out of
  a build.
related:
  - pekit/building/registering-toolchains
  - pekit/building/building-for-another-platform
  - pekit/concepts/hermetic-and-strict-builds
---

A build involves two platforms. Steps run on one machine, and what they
build can be for another, as when you compile a program for a phone on a
laptop. pekit keeps the two apart, and libraries use them to choose the
right compiler for each step.

## Two platforms

GCSL has two contexts for platforms, each a record such as
`{arch = "x86_64"; os = "linux"}`:

- **`^host_platform`** is where steps run. pekit sets it to the machine
  it runs on, for every target it builds.
- **`^platform`** is what the build is for. It's the same as
  `^host_platform` unless something sets it.

`pekit build --platform aarch64-linux app` builds `app` for 64-bit Arm
Linux, as if the target were called with `with { platform = ... }`.
`--platform aarch64` keeps the machine's operating system. A target can
also set the platform for part of its work:

```gcsl
def build all() {
    x86_64 = build app() with { platform = {arch = "x86_64"; os = "linux"} }
    aarch64 = build app() with { platform = {arch = "aarch64"; os = "linux"} }
}
```

Every step is keyed by the machine it runs on, so a result made on one
kind of machine is never reused on another. [GCSL's context
page](~gcsl/context) explains contexts in general.

## Asking for a toolchain

A library doesn't name a compiler. It asks GCSL's `toolchain` module for
one of a kind, such as `toolchain.get(Toolchain::C)`, and gets the first
registered toolchain of that kind that runs on `^host_platform` and builds
for `^platform`.

Libraries ask in a parameter default:

```gcsl
#[hermetic]
def compile(src: File, cchain: toolchain.CToolchain = toolchain.get(Toolchain::C)) -> File {
    # ...
}
```

The default is worked out where the function is called, outside the step,
so the toolchain arrives as an ordinary argument. The step is keyed by the
toolchain's files, and changing the compiler reruns exactly the steps that
use it. [GCSL's toolchains page](~gcsl/toolchains) describes the module and
the kinds of toolchain.

## Registering toolchains

A workspace registers the toolchains it builds with in `pekitconf`, in
`workspace.gcsl`:

```gcsl
import Toolchain::* from "github.com/gcslib/gcc"
import Toolchain from toolchain

pekitconf = {toolchains = [
    Toolchain::C gcc_x86_64,
    Toolchain::C gcc_x86_64_to_aarch64,
]}
```

pekit sets the `toolchains` context from this list for every target it
builds. The list is in order of preference, and the first toolchain that
fits both platforms is used. So a workspace can register a native compiler
and a cross compiler, and each step gets the one its platforms need.

With nothing registered, the list holds one toolchain, `system_c`: the
host's own `cc`, which builds only for the host. Registering any toolchain
replaces it. If you still want the host's compiler, register `system_c`
yourself.

A registered toolchain's definition runs only when the build first asks
for its kind, and only once. A toolchain that has to be downloaded or
built costs nothing in a build that doesn't use it.

## Pinned toolchains

A pinned toolchain is fetched by its SHA-256 digest, such as the gcc
library's Bootlin compilers. Its tools run in a sandbox whose root is the
toolchain's own runtime, so they read none of the host's libraries or
headers. A build whose steps use only pinned toolchains gives the same
results on any machine of the same platform.

The host's own toolchains, such as `system_c`, run the host's programs, so
a build that uses them depends on what's installed.
[Hermetic and Strict Builds](~pekit/hermetic-and-strict-builds) describes how pekit
accounts for that, and how a strict build refuses it.
