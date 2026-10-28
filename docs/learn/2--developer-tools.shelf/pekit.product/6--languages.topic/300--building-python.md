---
title: Building Python
type: how-to
description: Check Python sources and bundle a program and its libraries
  into one runnable archive with the python library, using a pinned CPython
  or the host's.
related:
  - pekit/languages/building-c
  - pekit/building/registering-toolchains
---

The python library builds a Python program into one file, an archive that
`python3 app.pyz` runs. It checks that each source compiles, as a step of
its own, so changing one file rechecks only that file. Import it as
`github.com/gcslib/python`.

## Before you start

- The python library. See [Libraries](~pekit/libraries).
- A Python interpreter registered in `pekitconf`. This page registers the
  library's pinned CPython.

## Build a program

1. Register an interpreter in `workspace.gcsl`:

   ```gcsl
   import Toolchain::Python python_3_14 from "github.com/gcslib/python"
   import Toolchain from toolchain

   pekitconf = {toolchains = [Toolchain::Python python_3_14]}
   ```

2. In a build file, declare a library for each group of sources, and a
   binary for the program:

   ```gcsl
   import "github.com/gcslib/python"

   def build words() {
       = python.library([file("words/words.py")])
   }

   def build greet() {
       = python.library([file("greet/greet.py")], deps = [build words()])
   }

   def build app() {
       = python.binary(file("app/__main__.py"), deps = [build greet()], output = "app.pyz")
   }
   ```

3. Build the program:

   ```sh
   pekit build app
   ```

## Check the result

Run the archive pekit printed with `python3`. It holds `app/__main__.py`
as its `__main__.py`, and the sources of `greet` and `words` beside it.

## Libraries and binaries

`python.library(srcs, deps)` gives a `python.Library`: its own sources,
and every source of its dependencies, all the way down. `app` depends on
`greet` alone, and still gets `words`.

`python.binary(main, deps, output)` bundles `main`, as `__main__.py`, and
every source of its dependencies into one archive, each at the top by its
name. Two different sources with one name are an error. A library reached
by two paths is bundled once.

The archive's entries are sorted and carry a fixed date, so the same
sources always give the same bytes. A step that uses the archive reruns
only when a source really changed.

## Interpreters

The library asks the `toolchain` module for a `Toolchain::Python`. It
defines two:

| Toolchain | Is |
|---|---|
| `python_3_14` | CPython 3.14 from python-build-standalone, pinned by its SHA-256, running in a sandbox whose root is the gcc library's Bootlin runtime. A build with it reads nothing of the host's Python. |
| `system_python` | The host's own `python3`, found on `PATH`. |
