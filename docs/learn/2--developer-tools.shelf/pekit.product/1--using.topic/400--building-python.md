---
title: Building Python
type: guide
description: Check Python sources and bundle a program into one runnable archive with the python library.
---

The python library builds Python programs into one file that
`python3 app.pyz` runs. Import it as `github.com/gcslib/python`.

```text
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

## The rules

`python.library(srcs, deps)` gives a `python.Library`: its own sources and
every source of its dependencies, all the way down. `app` above depends
on `greet` alone, and still gets `words`. Each source is checked to
compile, a step of its own, so changing one file reruns its own check and
nothing else's.

`python.binary(main, deps, output)` bundles `main`, as `__main__.py`, and
every source of its dependencies into one archive, each at the top by its
name. Two different sources with one name are an error; a library reached
by two paths is bundled once. The archive's entries are sorted and carry
a fixed date, so the same sources always give the same bytes, and a step
that uses the archive reruns only when a source really changed.

## Interpreters

The rules ask the `toolchain` module for a `Toolchain::Python`, so a
workspace registers one, as it does a C compiler (see
~pekit/building-a-target):

```text
import Toolchain::Python python_3_14 from "github.com/gcslib/python"
import Toolchain from toolchain

pekitconf = {toolchains = [Toolchain::Python python_3_14]}
```

`python_3_14` is CPython 3.14 from python-build-standalone, pinned by its
SHA-256, running in a sandbox whose root is the gcc library's Bootlin
runtime, so a build reads nothing of the host's Python. `system_python`
is the host's own `python3`, found on `PATH`.
