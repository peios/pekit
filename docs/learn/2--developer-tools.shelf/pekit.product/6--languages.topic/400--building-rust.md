---
title: Building Rust
type: how-to
description: Build Rust programs hermetically from the crates Cargo.lock
  pins with the rust library, one crate at a time or with Cargo whole,
  including tests, git crates, C dependencies, bindgen, and the pinned
  Rust toolchain.
related:
  - pekit/languages/building-c
  - pekit/testing-and-running/testing
  - pekit/libraries/libraries
---

The rust library builds Rust programs from the crates your `Cargo.lock`
pins, with no network and nothing of the host. It builds a Cargo workspace
one crate at a time, as Bazel's Rust rules do, or whole, as Cargo does.
Import it as `github.com/gcslib/rust`.

## Before you start

- The rust, gcc, buildenv, and python libraries. See [Libraries](~pekit/libraries).
- A Cargo workspace with a `Cargo.lock`.

## Register the toolchain

The library asks the `toolchain` module for a `Toolchain::Rust`. Its
`rust_1_98` is Rust 1.98.1 from static.rust-lang.org, each part pinned by
its SHA-256, with the standard libraries for `x86_64-unknown-linux-gnu` and
`x86_64-unknown-linux-musl`. It runs on the gcc library's Bootlin sysroot
and links with its compiler. Register both in `workspace.gcsl`:

```gcsl
import Toolchain::C gcc_x86_64 from "github.com/gcslib/gcc"
import Toolchain::Rust rust_1_98 from "github.com/gcslib/rust"
import Toolchain from toolchain

pekitconf = {toolchains = [Toolchain::C gcc_x86_64, Toolchain::Rust rust_1_98]}
```

rustc needs zlib, which the sysroot lacks. The toolchain builds zlib from
its pinned source with the buildenv library's pinned environment, so a
first build takes longer.

## Build one crate at a time

1. Plan the workspace with `rust.workspace`, then build what you need of
   it:

   ```gcsl
   import "github.com/gcslib/rust"

   fn crates() -> rust.RustWorkspace {
       = rust.workspace(dir("."), rust.vendored(file("Cargo.lock")))
   }

   def build server() {
       = rust.bin(crates(), "server")
   }

   def build client_library() {
       = rust.library(crates(), "client")
   }
   ```

2. Build a program:

   ```sh
   pekit build server
   ```

`rust.bin(ws, package, name)` gives a member's program, by default the one
named after the member. `rust.library(ws, package)` gives its shared or
static library.

## Check the result

pekit prints the program's path. Change one source file and build again:
pekit compiles that file's crate, and the crates that depend on it, again,
and finds every other crate in the store.

## How planning works

Cargo plans the build: what each member needs, unit by unit, as `cargo
build --unit-graph` lists it. Each crate, with the features Cargo resolves
for the member you build, is compiled as a step of its own, and so is each
build script and each run of one. Two programs that use the same crate
with the same features share the one step.

Planning reads the manifests, `Cargo.lock`, and which files exist, never
what the sources say. Editing a source plans nothing again.

Features follow the member. A program built from a member that asks for a
crate's feature gets it, and a program from a member that doesn't never
does, even when you build both at once. Cargo unifies features across
everything one `cargo build` names; pekit plans each member on its own.

Cargo's plans leave out some of what it takes from the manifests: the
`[lints]` table, which targets have `harness = false`, and the `--check-cfg`
flags it gives every crate. The library reads each member's manifest for
them with the python library's pinned Python, and compiles the member's
crates with them, as Cargo does. Link-time optimization follows Cargo too:
the programs a profile's `lto` names are optimized whole, and the libraries
they link carry what that needs.

## Crates that read outside their directory

Each step's sandbox holds the toolchain, the crate's own directory, and
what it is given. A crate that reads a file outside its directory needs it
named. Give `workspace` the settings of such packages in `crates`:

```gcsl
rust.workspace(
    dir("."),
    rust.vendored(file("Cargo.lock")),
    crates = [
        {package = "policy"; data = [{at = "registry.d"; place = dir("registry.d")}]},
        {
            package = "sdk-sys"
            env = [{key = "SDK_DIR"; value = "/opt/sdk"}]
            given = [{at = "/opt/sdk"; place = build sdk()}]
        },
    ],
)
```

Each setting does one thing:

- **`data`** places files and directories under the workspace, where the
  crate's compiles and build script find them.
- **`env`** sets variables for its build script and its compiles.
- **`given`** places files and directories at absolute paths, for its build
  script and its compiles, and for every crate that depends on it. A
  native library that a build script links goes here, so that the programs
  that use the crate link it too.

## Tests

`rust.tests(ws, package)` gives a member's test programs, its unit tests
and each integration test, as `cargo test -p` builds them. Give
`features` too if the tests need them. `rust.run_test(test)` runs one as a
step, in a sandbox of the toolchain's runtime with a writable `/tmp`:

```gcsl
def test server_tests(tests: List<rust.RustTest> = rust.tests(crates(), "server")) {
    let ran = for each in tests { = rust.run_test(each) }
}
```

Give `run_test` what a test reads in `given`, its variables in `env`, and
its arguments in `args`, such as a filter and `--test-threads=1`. A test
whose program didn't change is found in the store.

A test target with `harness = false` in its manifest is compiled as a
program of its own, as Cargo compiles it.

## Documentation, its tests, and Clippy

`rust.doctests(ws, package)` tests a member's documentation, as `cargo test
--doc` does: rustdoc compiles and runs each crate's examples as a step. It
takes `features`, `given`, and `env` as `run_test` does, and gives no tests
for a member with no library.

`rust.clippy(ws, package)` checks a member with Clippy, as `cargo clippy
--all-targets` does: its library, programs, build script, tests, benchmarks,
and examples, each crate a step. `flags` come after the member's lints, so
`flags = ["-Dwarnings"]` denies every warning. A check fails when a lint it
denies fires.

`rust.docs(ws, package)` makes a member's library documentation, as `cargo
doc --no-deps` does, as a step, and gives the directory rustdoc wrote.
`flags` come after the member's lints, as they do for Clippy, so `flags =
["-Dwarnings"]` fails it when rustdoc warns, such as about a broken link.

All three are steps that the rust library makes, so make them in a test's
parameter defaults, where a test plans and builds what it needs:

```gcsl
def test clippy(checked: List<List<{}>> = rust.clippy(crates(), "server", flags = ["-Dwarnings"])) {}

def test doctests(ran: List<List<{}>> = rust.doctests(crates(), "server")) {}

def test docs(made: Dir = rust.docs(crates(), "server", flags = ["-Dwarnings"])) {}
```

## Build with Cargo, whole

`rust.cargo_build` builds the workspace with Cargo, in one step:

```gcsl
import "github.com/gcslib/rust"

def build ppk() {
    = rust.cargo_build(dir("src"), rust.vendored(file("src/Cargo.lock")), bins = ["ppk"])
}
```

`rust.cargo_build(src, vendor, bins)` builds the Cargo workspace `src` with
`cargo build --release --frozen --offline`, from the crates of `vendor`. It
gives a directory of the binaries `bins`, each by its name. `packages`
names the workspace's packages to build, as Cargo's `-p` does.

The whole of `cargo build` is one step, so changing one source file runs it
again. Cargo's target directory is kept between runs in the step's
[scratch directory](~pekit/writing-steps#keep-state-between-runs), so the
crates of `vendor` aren't compiled again. The workspace's own crates, and
crates it takes by their paths, are always compiled again. Cargo judges
them by their files' modification times, which a file put back as it was
can make look older than the last build.

Either way of building takes its crates from `rust.vendored`, so no crate
is fetched again.

## Where crates come from

`rust.vendored(lock)` fetches every crate of crates.io that `lock` pins,
each checked against the checksum `Cargo.lock` records for it, and lays
them out as `cargo vendor` does. Each crate's fetch and unpacking is kept
in the store.

A crate from a git repository is pinned by the commit `Cargo.lock` records
for it. `rust.vendored` fetches the repository at that commit with
`fetch_git`, once for every crate it holds. It gives each crate's directory
as the repository has it, so a crate that takes another of the same
repository by its path still finds it.

These crates are errors:

- A git crate whose manifest takes values from its workspace's, such as
  `version.workspace = true`. Its manifest would need rewriting to build on
  its own.
- A git crate from a repository the workspace also imports a library from,
  at another commit than `pekit.lock` pins the library at.
- A crate from anywhere else, since nothing pins it.

## Targets and flags

By default `cargo_build` builds for `x86_64-unknown-linux-musl`, as static,
position-independent programs that need nothing of the system they run on.
Give `target = "x86_64-unknown-linux-gnu"` for programs linked against
glibc instead.

The flags follow the target: a glibc build takes none unless you give
`rustflags`. The library's flags are Cargo's `build.rustflags`, so a
project's own configuration of its target's flags, or of its linker, takes
precedence over them, as Cargo intends.

## C, libraries, and bindings

C that a crate compiles through the `cc` crate, such as `zstd-sys`, is
compiled for the target by `target_c`. By default that's the gcc library's
Bootlin musl toolchain, `gcc_x86_64_musl()`, for a musl target, which a
static musl program needs, and the registered C toolchain for any other.

`cargo_build` takes these as well:

- **`libs`** names libraries a crate builds to give besides the binaries,
  such as a `staticlib`: `libs = ["libpeios.a"]`.
- **`beside`** places directories next to the source, at `../NAME`, for a
  crate that takes another by a path outside its own tree.
- **`env`** gives every build script variables of its own, each a
  `{key; value}`, as Cargo's `[env]` table does.
- **`libclang`** is for a build script that runs bindgen: a list of one
  directory holding `libclang.so`, such as
  `libclang = [clang.libclang_18()]`. The clang library's `libclang_18` is
  a pinned build from PyPI. The build is given `LIBCLANG_PATH`, and
  `BINDGEN_EXTRA_CLANG_ARGS` names the target C toolchain's own headers and
  sysroot, so bindgen parses as that toolchain's compiler would.

Everything is pinned, so a strict build gives the same program, byte for
byte, on any machine. See [Hermetic and Strict
Builds](~pekit/hermetic-and-strict-builds).
