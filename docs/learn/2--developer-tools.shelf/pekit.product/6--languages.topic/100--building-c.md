---
title: Building C
type: how-to
description: Compile, archive, and link C programs and static libraries
  with the gcc library, give each compile exactly its headers, and build
  with Bootlin's pinned compilers for the host or for aarch64.
related:
  - pekit/languages/building-upstream-software
  - pekit/building/registering-toolchains
  - pekit/building/building-for-another-platform
---

The gcc library builds C programs and static libraries. Each of its
functions is one step, so pekit reruns only the compiles whose sources or
headers changed. Import it as `github.com/gcslib/gcc`.

## Before you start

- The gcc library. See [Libraries](~pekit/libraries) for how pekit finds
  one.
- Optional: Bootlin's pinned compilers registered in `pekitconf`. Without
  them, the library uses the host's `cc`.

## Build a program

1. In a build file, compile each source and link the objects:

   ```gcsl
   import "github.com/gcslib/gcc"

   def build hello() {
       = gcc.link([gcc.compile(file("hello.c"))], output = "hello")
   }
   ```

2. Build it:

   ```sh
   pekit build hello
   ```

## Check the result

pekit prints the program's path, `pekit-out/hello/link/hello`. Run it.

## The library's functions

The library has three steps:

| Function | Makes |
|---|---|
| `compile(src, headers, output, flags)` | The object of one C source, by default its name with `.o` for `.c`. |
| `archive(objs, output)` | A static library of objects, by `ar rcsD`, with no times or owners recorded. |
| `link(objs, libraries, output, flags)` | A program of objects and static libraries. |

`flags` go to the compiler as they are. `output` names the file the step
makes.

## Give each compile its headers

A compile's sandbox holds its source and the `headers` you give it, and
nothing else of the workspace. pekit passes each header's directory to the
compiler with `-I`. Give a compile exactly the headers it includes:

- A header it includes but isn't given fails the compile.
- A header it's given but doesn't include reruns it for nothing when the
  header changes.

## Build a library and a program that uses it

A library target can give its archive and its public headers together, so
that a program compiles against the public headers alone:

```gcsl
import "github.com/gcslib/gcc"

def build greet() {
    let obj = gcc.compile(file("lib/greet.c"), headers = [file("lib/greet.h"), file("lib/internal.h")])
    archive = gcc.archive([obj], output = "libgreet.a")
    headers = [file("lib/greet.h")]
}

def build app() {
    let g = build greet()
    let obj = gcc.compile(file("main.c"), headers = g.headers)
    = gcc.link([obj], libraries = [g.archive], output = "app")
}
```

Editing `lib/internal.h` reruns the library's compile, its archive, and the
link, but never the program's compile, which wasn't given that header.

## Build with pinned compilers

The gcc library defines toolchains of Bootlin's compilers. Each is fetched
by its SHA-256 digest and unpacked once, and its compiler runs in a sandbox
whose root is the toolchain's own runtime. A build with them reads nothing
of the host's compiler or libraries. Register them in `workspace.gcsl`:

```gcsl
import Toolchain::* from "github.com/gcslib/gcc"
import Toolchain from toolchain

pekitconf = {toolchains = [
    Toolchain::C gcc_x86_64,
    Toolchain::C gcc_x86_64_to_aarch64,
]}
```

The toolchains are:

| Toolchain | Runs on | Builds for |
|---|---|---|
| `gcc_x86_64` | x86-64 Linux | x86-64 Linux, with glibc |
| `gcc_x86_64_to_aarch64` | x86-64 Linux | aarch64 Linux, with glibc |

`gcc_x86_64_musl()` gives a Bootlin toolchain that builds for x86-64 Linux
with musl. It isn't registered, but a library can pass it to a step, as
the rust library does for static musl programs.

With both registered, `pekit build --platform aarch64 hello` builds the
program for aarch64. See [Building for Another
Platform](~pekit/building-for-another-platform).

## Tools built during the build

A program the build makes and then runs, such as a code generator, is a
file like any other: pass it to the step that runs it. Build it for the
machine steps run on, and link it statically, so that it runs in a step's
sandbox with nothing of the toolchain that built it:

```gcsl
def build embed() {
    = gcc.link([gcc.compile(file("gen/embed.c"))], output = "embed", flags = ["-static"])
}

def build app() {
    let tool = build embed() with { platform = ^host_platform }
    # Runs the tool in a step, then compiles what it wrote.
}
```
