---
title: Libraries
type: concept
description: How a pekit build imports libraries, where pekit finds them,
  how a library's steps are shared between workspaces, how a library builds
  from files of its own, and which libraries gcslib provides.
related:
  - pekit/libraries/pinning-libraries
  - pekit/libraries/writing-a-library
  - pekit/languages/building-c
---

A library is GCSL code kept apart from any workspace, such as the gcc
library, which compiles and links C. A build file imports a library by a
path in quotes, and calls its functions, which are usually steps:

```gcsl
import "github.com/gcslib/gcc"

def build hello() {
    = gcc.link([gcc.compile(file("hello.c"))], output = "hello")
}
```

A library sees only its own files, the standard library, and the libraries
it imports, never anything of the workspace that uses it. So it means the
same in every workspace. GCSL's [Workspaces](~gcsl/workspaces) page
describes imports in full.

## Where pekit finds a library

pekit looks for a library's directory, by its import path, in two places,
and uses the first it finds:

1. Each directory of `$GCSL_LIBRARY_PATH`, in order.
2. `.gcsl/libraries/` at the workspace's root, where `pekit lock` puts the
   libraries it pins.

`$GCSL_LIBRARY_PATH` is for working on a library: point it at your
checkout, and every workspace on your machine builds with your copy. For
everything else, pin libraries with `pekit lock`, so that every checkout
builds with the same ones. See [Pinning
Libraries](~pekit/pinning-libraries).

## Steps from a library are shared

pekit places every library at `/w/.gcsl/libraries/<path>` in the layout
steps see, wherever it found the library. A step a library makes is keyed
by the library's path and contents, never by where it was on disk. So a
library's steps give the same results from the store whether the library
came from `$GCSL_LIBRARY_PATH` or from `pekit lock`, and in every workspace
that imports the same version.

## Libraries with files of their own

A library can keep other files beside its `.gcsl` files: sources to build,
templates, or tools to run. A relative path in a library's code names its
own files, such as `file("templates/greeting.txt")`, wherever the library
is used. The Peios SDK's library builds libpeios this way, from the
sources in its own repository.

Because every library is at the same path in every workspace, a step that
builds from a library's files is the same step everywhere, and its results
are shared through the store.

## What the lock tells a build

A build's code can read what `pekit.lock` pins, through the GCSL standard
library's `libraries` module: each pinned library's path, repository,
directory, and commit. A library that takes something the same repository also
gives another way can check that the two agree. The gcslib rust library uses
it to check `Cargo.lock`. A build fails if `Cargo.lock` takes a git crate from
a repository at another commit than the one `pekit.lock` pins a library of
that repository at.

## The gcslib libraries

gcslib is a set of libraries for building common kinds of software with
pekit:

| Library | Builds |
|---|---|
| `github.com/gcslib/gcc` | C programs and static libraries, with Bootlin's pinned compilers. See [Building C](~pekit/building-c). |
| `github.com/gcslib/buildenv` | Software with its own build system, such as `./configure` and `make`, in a pinned environment. See [Building Upstream Software](~pekit/building-upstream-software). |
| `github.com/gcslib/python` | Python programs, bundled into one archive. See [Building Python](~pekit/building-python). |
| `github.com/gcslib/rust` | Rust programs, one crate at a time or with Cargo. See [Building Rust](~pekit/building-rust). |
| `github.com/gcslib/go` | Go programs, one package at a time. See [Building Go](~pekit/building-go). |
| `github.com/gcslib/clang` | A pinned libclang, for tools such as bindgen. |
| `github.com/gcslib/ppk` | Peios packages. See [Packaging](~pekit/packaging). |
| `github.com/gcslib/peiso` | Bootable Peios images. See [Building and Booting an Image](~pekit/building-an-image). |
| `github.com/gcslib/qemu` | Programs and tests that boot an image in QEMU. |

The first six are in repositories of their own, which `pekit lock` finds
by their paths. ppk, peiso, and qemu are in pekit's own repository, under
`gcslib/`. To pin them, give their paths a source in `pekit.lock`, as
[Pinning Libraries](~pekit/pinning-libraries#take-libraries-from-elsewhere)
shows.
