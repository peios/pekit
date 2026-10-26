---
title: Building Rust
type: guide
description: Build Rust programs with Cargo from the crates Cargo.lock pins, hermetically, with gcslib's rust library and its pinned Rust.
---

The rust library builds Rust programs with Cargo, from the crates your
`Cargo.lock` pins, with no network and nothing of the host:

```text
import "github.com/gcslib/rust"

def build ppk() {
    = rust.cargo_build(dir("src"), rust.vendored(file("src/Cargo.lock")), bins = ["ppk"])
}
```

`rust.vendored(lock)` fetches every crate of crates.io that `lock` pins,
each checked against the checksum `Cargo.lock` records for it, and lays
them out as `cargo vendor` does. `rust.cargo_build(src, vendor, bins)`
then builds the Cargo workspace `src` with `cargo build --release
--frozen --offline` from those crates, and gives a directory of the
binaries `bins`, each by its name.

A crate from a git repository is pinned by the commit `Cargo.lock`
records for it. `rust.vendored` fetches the repository at that commit
with `fetch_git`, once for every crate it holds, and gives each crate's
directory as the repository has it, so a crate that takes another of the
same repository by its path still finds it. A git crate whose manifest
takes values from its workspace's, such as `version.workspace = true`,
is an error, since its manifest would need rewriting to build on its
own. A crate from anywhere else is an error, since nothing pins it.

## Targets

By default `cargo_build` builds for `x86_64-unknown-linux-musl`, as
static, position-independent programs that need nothing of the system
they run on. Give `target = "x86_64-unknown-linux-gnu"` for programs
linked against glibc instead: the flags follow the target, so a glibc
build takes none unless you give `rustflags`. `packages` names the
workspace's packages to build, as Cargo's `-p` does. The library's flags
are Cargo's `build.rustflags`, so a project's own configuration of its
target's flags, or of its linker, takes precedence over them, as Cargo
intends.

## C, libraries and bindings

C that a crate compiles through the `cc` crate, such as `zstd-sys`, is
compiled for the target by `target_c`: by default the gcc library's
Bootlin musl toolchain, `gcc_x86_64_musl()`, for a musl target, which a
static musl program needs, and the registered C toolchain for any
other.

`cargo_build` gives the binaries `bins` by default. To give a library a
crate builds as well, such as a `staticlib`, name its file in `libs`,
for example `libs = ["libpeios.a"]`. `beside` places directories next to
the source, at `../NAME`, for a crate that takes another by a path
outside its own tree. `env` gives every build script variables of its
own, each a `{name; value}`, as Cargo's `[env]` table does.

A build script that runs bindgen needs libclang. Give `libclang = [dir]`,
a directory holding `libclang.so`, such as the clang library's
`clang.libclang_18()`, a pinned build from PyPI. The build is then given
`LIBCLANG_PATH`, and `BINDGEN_EXTRA_CLANG_ARGS` names the target C
toolchain's own headers and sysroot, so bindgen parses as that
toolchain's compiler would.

## The toolchain

The library asks the `toolchain` module for a `Toolchain::Rust`. Its
`rust_1_98` is Rust 1.98.1 from static.rust-lang.org, each part pinned by
its SHA-256, with the standard libraries for both targets. It runs on the
gcc library's Bootlin sysroot and links with its compiler, and gives
rustc the zlib it needs, built from zlib's pinned source with the
buildenv library's pinned environment. Register it with the gcc
library's toolchain:

```text
import Toolchain::C gcc_x86_64 from "github.com/gcslib/gcc"
import Toolchain::Rust rust_1_98 from "github.com/gcslib/rust"
import Toolchain from toolchain

pekitconf = {toolchains = [Toolchain::C gcc_x86_64, Toolchain::Rust rust_1_98]}
```

Everything is pinned, so a strict build (see ~pekit/building-a-target)
builds the same program, byte for byte.

## What is a step

Each crate's fetch and unpacking is its own, kept in the store. The
vendored directory is one step, and the whole of `cargo build` another,
so changing one source file of the workspace builds the workspace again,
as Cargo would, but no crate is fetched again.
