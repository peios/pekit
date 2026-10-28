---
title: Registering Toolchains
type: how-to
description: Register the toolchains a workspace builds with in pekitconf,
  choose among a library's toolchains, keep the host's compiler, and
  define a toolchain of your own for a tool no library provides.
related:
  - pekit/concepts/platforms-and-toolchains
  - pekit/building/building-for-another-platform
  - pekit/languages/building-c
---

A workspace chooses the compilers and tools its steps use by registering
toolchains in `pekitconf`. Register pinned toolchains to make a build
independent of the machine it runs on, or define your own for a tool that
no library provides.

## Before you start

- A workspace that imports a library whose steps ask for a toolchain, such
  as the gcc library. See [Libraries](~pekit/libraries) for how to make one
  available.

## Register a library's toolchains

1. In `workspace.gcsl`, import the toolchains the library defines, and the
   `Toolchain` type:

   ```gcsl
   import Toolchain::* from "github.com/gcslib/gcc"
   import Toolchain from toolchain
   ```

   `import Toolchain::* from` a library brings in every toolchain it
   defines, and nothing else of it. To bring in one, name it, as
   `import Toolchain::C gcc_x86_64 from "github.com/gcslib/gcc"`.

2. List them in `pekitconf.toolchains`, in order of preference:

   ```gcsl
   pekitconf = {toolchains = [
       Toolchain::C gcc_x86_64,
       Toolchain::C gcc_x86_64_to_aarch64,
   ]}
   ```

   Each entry names a toolchain's kind and its definition. For each step,
   pekit uses the first toolchain of the kind it asks for that runs on the
   machine and builds for the target platform.

3. Build a target that uses the library:

   ```sh
   pekit build hello
   ```

## Check the result

The steps that ask for a C toolchain run with the one you registered.
`pekit build --platform aarch64 hello` builds the same target with
`gcc_x86_64_to_aarch64`, and `file pekit-out/hello/link/hello` reports an
`ARM aarch64` executable. A `get` that finds no toolchain fails with an
error naming the kind and both platforms.

## Keep the host's compiler

Registering any toolchain replaces the default list, which holds only
`system_c`, the host's own C compiler. To keep it as well, register it
yourself. It's defined in the `toolchain` module:

```gcsl
import * from toolchain

pekitconf = {toolchains = [Toolchain::C system_c]}
```

A step that uses `system_c` runs the host's `cc`, so its result depends on
what's installed. A strict build refuses it.

## Define a toolchain of your own

A toolchain can hold any tool, not only a compiler. This workspace keeps a
tool called `greet` in `tools/greeter/bin/`, and defines a toolchain kind
for it.

1. In `workspace.gcsl`, declare the kind, with the layout of files and
   directories it provides:

   ```gcsl
   import * from toolchain

   enum Toolchain::Greeter<{root: Dir, greet: File}>
   ```

   A kind's layout is a record whose every field is a file or a
   directory, so that a toolchain can be passed to a step.

2. Define a toolchain of that kind. Its `host_platform` and `platform` say
   where it runs and what it builds for; a field you leave out matches any
   platform:

   ```gcsl
   def Toolchain::Greeter workspace_greeter() {
       let tools = dir("tools/greeter")
       host_platform = {os = "linux"}
       platform = {}
       provides = {root = tools; greet = tools.file("bin/greet")}
   }
   ```

   A toolchain fetched from the internet would fetch and unpack its files
   here, as the gcc library's do.

3. Register it, beside any others:

   ```gcsl
   pekitconf = {toolchains = [Toolchain::C system_c, Toolchain::Greeter workspace_greeter]}
   ```

4. In a build file, ask for it in a step's parameter default:

   ```gcsl
   import toolchain
   import Toolchain from toolchain

   #[hermetic]
   fn greeting(name: String, greeter: {root: Dir, greet: File} = toolchain.get(Toolchain::Greeter)) -> File {
       let greeted = exec([greeter.greet.path, name, "greeting.txt"])
       = working_dir().file("greeting.txt")
   }

   def build hello() {
       = greeting("world")
   }
   ```

`pekit build hello` runs the tool in the step, and
`pekit-out/hello/greeting/greeting.txt` holds `Hello, world`. Because the
tool is an argument, changing it reruns every step that uses it.
