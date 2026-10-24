# Layer D: libraries

Status: specified 2026-10-24 (Claude), before building. The agreed model is
in `core-model.md` (open question 4, "Secrets and signing", "Packaging"),
and the libraries' shapes in the examples' drafts, chiefly example 6's
"Consolidated" section. This note decides what layer D builds, where each
piece lives, and in what order, so that each step leaves something working.

## What layer D gives

After layer C, a build runs with whatever the host has. Layer D makes builds
look like the examples: `def build` with no fixture module, compilers
chosen by context from pinned toolchains, upstream code fetched by hash and
unpacked, and libraries (`gcc`, `python`, `ppk`) that work the same under
plain GCSL and under pekit.

## Decisions

### L1. The build declarations are in the prelude

`hermetic` and `scratch`, the `build` def-type, `Platform`, and the contexts
`host_platform` and `platform` are declared in GCSL's prelude, which every
file sees. The drafts write `def build hello()` and `#[hermetic]` with no
import, and plain GCSL must check the same files (E9). The prelude is the
one module every file already imports implicitly; a second implicit module
would be a new rule for no gain.

```gcsl
type Platform = {arch: String, os: String}
context host_platform: Platform = {arch = builtins.host_arch(); os = builtins.host_os()}
context platform: Platform = {...^host_platform}
#[attribute] #[defers] type hermetic = {on: Bool = true}
#[attribute] type scratch = {}
#[def_type] def build<T>(name: String, f: fn(): T): T { f() }
```

`host_arch` and `host_os` are new pure built-ins: the machine GCSL runs on.
Under pekit, `host_platform` is set at the root to the execution platform
(C7), so a library never reads the machine itself. The `test`, `package` and
`script` def-types come with their commands (layer E), except `package`,
which the ppk packager needs (L9).

### L2. `fetch` is a built-in that a host can answer

`fetch(url: String, sha256: String = ""): File` is impure. Plain GCSL
downloads with `curl` and checks the digest with `sha256sum`, both through
its `Effects`, as it runs every program; GCSL gains no network or hashing
dependency. An empty `sha256` downloads anyway and fails with the digest
found ("`fetch` of `<url>` is not pinned: its sha256 is `<hex>`"), which is
example 4's "what you write before you know it". A mismatch fails loudly
with both digests. The file is named by the URL's last segment.

Pekit answers a `fetch` effect node itself, never running GCSL's: the
store is keyed by SHA-256, so a pinned file the store holds is found with no
network at all, on any machine sharing the store. Otherwise pekit downloads
it on the host (`ureq`), the one place a build touches the network, checks
it, and stores it. Its path is `/pekit-out/.fetch/<sha256>/<name>`, fixed by
its contents, so every receipt that holds it is stable.

### L3. `archive.unpack` is a built-in that a host can answer

A new standard module, `archive`, binds `unpack(archive: File, strip: Int =
0): Dir`. Plain GCSL runs `tar -x` through its `Effects` into a temporary
directory. Pekit answers it natively (`tar`, with gzip, xz, bzip2 and zstd),
since the build environment that would provide `tar` is itself an archive.
It refuses entries that would leave the directory (absolute paths, `..`, or
links that point out), turns hard links into copies, keeps symbolic links,
and keeps only the executable bit of each mode. The tree is stored, and the
unpacking recorded under a key of the archive's digest and `strip`, so a
second unpack of the same archive reads the record. Its path is
`/pekit-out/.unpack/<key>/`.

### L4. `toolchain` is a standard module, as example 6 draws it

`stdlib/toolchain.gcsl` declares `Toolchain`, `Toolchain::C`,
`ToolchainDef<T>`, `Registered`, the generic `Toolchain` def-type, `context
toolchains` and `get`, exactly as example 6's consolidated section has them.
`get` is pure: the first registered toolchain of the asked kind whose
partial platforms match `^host_platform` and `^platform`, or a failure naming
the kind and both platforms. The default registry is `[Toolchain::C
system_c]`, the host's tools by `PATH`, through a new impure built-in,
`which(name): File`.

### L5. The workspace's configuration sets the registry

`workspace.gcsl` may give `pekitconf: PekitConfig`, whose `toolchains`
pekit sets as `^toolchains` at the root of every target, as it sets
`host_platform`. `PekitConfig` is declared in a standard module `pekit`
(`import pekit`), since it names `Registered` and the prelude cannot import
`toolchain`.

The value is computed in the target's own evaluation, so a registered
toolchain's `fetch` is a node of the target's graph, run only if the target
needs it. GCSL gains a way for a host to give a root context as *the value
of a top-level field of a file* (`RootValue::Field { file, path }`), which
`prepare_call` evaluates within the call's unit, as it runs the files the
call needs.

### L6. The host snapshot answers for the host's own tools

A workspace that registers no toolchain gets `system_c`, whose files are
host paths such as `/usr/bin/cc`. A hermetic step given one would fail:
it is outside the workspace. Under pekit, a host path under the host
snapshot (C5) is a valid input that is not staged, since every step binds
the snapshot already, and it is keyed by the snapshot's fingerprint and its
path, not by hashing it. `system_c` under pekit therefore names
`root = dir("/usr")`, so that its tree is in the snapshot; plain GCSL keeps
`dir("/")`. So a workspace with no toolchains builds, not hermetically, as
it did in C5, and one that registers pinned toolchains builds hermetically
with the same build files.

### L7. Libraries are found by their import path in library roots

`import "github.com/gcslib/gcc"` names a library. GCSL resolves it in
*library roots*: each directory of `$GCSL_LIBRARY_PATH`, then
`<workspace>/.gcsl/libraries/`; the library is the directory
`<root>/github.com/gcslib/gcc/`, its `.gcsl` files one module, scoped as a
standard file is (it sees the prelude and what it imports, nothing of the
workspace). A library not found is an error naming where it looked. Pekit
passes the same roots. `pekit lock` (layer E) will fetch libraries pinned by
commit into `.gcsl/libraries/`; until then they are put there, or on the
path, by hand.

The `gcslib` libraries are written in this repository, under
`gcslib/github.com/gcslib/<name>/`, so that the path is one root, until Jack
decides where they are published.

### L8. Pinned toolchains are definitions in the libraries

`gcslib/gcc` provides `Toolchain::C` definitions of Bootlin's x86-64 and
aarch64 toolchains, each fetched by URL and SHA-256 and unpacked, that a
workspace registers in `pekitconf`. `gcslib/buildenv` provides a build
environment for upstream build systems: a static busybox, fetched by hash,
for `/bin/sh` and the usual tools, and GNU make built from source with the
Bootlin toolchain inside it. `gcslib/python` provides a `Toolchain::Python`
of a standalone CPython build, fetched by hash.

### L9. `gcc`, `python` and `ppk` are the libraries the examples use

- `gcc`: `compile`, `archive` and `link`, each resolving its toolchain in a
  parameter default (`cchain = toolchain.get(Toolchain::C)`), as example 6
  draws them, with `-I` flags derived from headers' directories (example 2)
  and `configure_make` for upstream builds (example 4).
- `python`: `library` and `binary`, as example 8 draws them.
- `ppk`: the packager of example 5, with the `package` def-type and
  `Package` type declared in the prelude beside `build`. Packing is a
  hermetic step; publishing comes with `pekit publish` (layer E).

`peiso` and `qemu` (example 10) need Peios's own packages to be built
through pekit, so they come last, after the commands.

### L10. `keychain` is a standard module a host answers

`keychain.string(name)` and `keychain.file(name)` give secrets whose
identity is the name and profile. Plain GCSL reads
`$GCSL_SECRET_<NAME>` or `$GCSL_SECRET_FILE_<NAME>`; pekit reads the
workspace configuration's keychain for the current profile. Under pekit a
secret's file is staged into the one step given it and never stored (C3).

## Steps

Each leaves pekit2 working, and tested, as far as it goes.

1. **D1, the prelude's build declarations** (L1), with the fixture module
   of pekit's tests removed.
2. **D2, `fetch`** (L2), in GCSL and in pekit.
3. **D3, `archive.unpack`** (L3), in GCSL and in pekit.
4. **D4, library roots** (L7).
5. **D5, `toolchain`, `which`, the registry from `pekitconf`, and the host
   snapshot's inputs** (L4, L5, L6).
6. **D6, `gcslib/gcc` with the Bootlin toolchains** (L8, L9): examples 1, 2,
   3, 6 and 9 run as fixtures.
7. **D7, `gcslib/buildenv` and `configure_make`**: example 4 runs.
8. **D8, `keychain`** (L10).
9. **D9, `gcslib/python`**: example 8 runs.
10. **D10, `ppk`**: example 5 packs.

Examples 7 and 10 need layer E's `test` and the Peios packages.

## Decisions taken without Jack (to surface)

- L1: the build declarations in the prelude, not a second implicit module.
- L2, L3: plain GCSL's `fetch` and `unpack` run `curl`, `sha256sum` and
  `tar`, so GCSL gains no network, hashing or archive dependency; pekit
  implements both natively.
- L5: `PekitConfig` in a standard module `pekit`, and root contexts given as
  a file's field.
- L6: under pekit, `system_c` names the host snapshot's `/usr`, and host
  snapshot paths are inputs keyed by the fingerprint.
- L7: library roots, `$GCSL_LIBRARY_PATH` then `.gcsl/libraries/`, and the
  `gcslib` libraries kept in this repository for now.
