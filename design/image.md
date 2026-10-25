# Example 10: an OS image

Example 10 is the real Peios loop: build every package, assemble a bootable
ISO from them with `peiso`, and boot it in QEMU to check that it comes up.
This note designs how it runs through pekit2: what pekit2 gains for it,
what the libraries gain, and what of Peios it can build today. Jack's draft
is `examples/10/build.gcsl`. As built, the fixture is
`tests/fixtures/examples/10/`.

## Spikes

Three facts the design rests on, checked by hand before writing it:

1. **An ISO boots with busybox alone.** `peiso build` makes an ISO from
   `pkgs/_artifacts`' kernel and busybox, with `[initramfs] symlinks =
   {"init" = ":bin/busybox"}` and `dirs = ["proc", "sys", "tmp", "dev"]`, in
   under a second. Booted under OVMF with KVM, busybox's init reaches
   "Please press Enter to activate this console." on `ttyS0`. So a real
   boot test needs neither prelude nor the live-boot hook, which aren't
   built anywhere yet.
2. **QEMU runs in a step's sandbox.** `qemu-system-x86_64 -enable-kvm`
   boots that ISO inside a bubblewrap with `--unshare-all`, the host's
   `/usr` read-only, and `--dev-bind /dev/kvm /dev/kvm`. OVMF's firmware is
   `/usr/share/OVMF/`, in the host snapshot.
3. **A package's identity is its manifest's digest.** `ppk fetch --from DIR
   NAME@VERSION?arch=ARCH --artifact-dir OUT` copies a package from one
   artifact directory into another and prints `sha256:<digest>`, the
   digest of its manifest, which names every blob by its digest. The
   manifest is TOML, which GCSL can't read.

`mkirf` and `mkuki`, which peiso runs, are peiosutils' `pu_mkirf` and
`pu_mkuki`, built here with Cargo into `local/pekit2-data/`.

## Decisions

### I1. The image is a build, and its tools are toolchains

As Jack decided, the ISO is an ordinary `def build`. A library,
`gcslib/peiso`, gives `peiso.iso(...)`: a hermetic step given the packed
packages, which collects them into one artifact directory with `ppk fetch`,
writes peiso's TOML spec from the GCSL description, and runs `peiso build`.
The spec's keys are GCSL's: a kernel package and command line, packages
each with `liveboot` and `initramfs`, and the layouts.

peiso's tools are a toolchain, `Toolchain::Peiso`, of `peiso`, `mkirf` and
`mkuki`, as ppk is `Toolchain::Ppk`. Nothing publishes them yet, so the
library pins none, and a workspace registers the ones it has, as the ppk
example does. Building them from source waits for a Rust library (I6).

### I2. QEMU is a toolchain, and a device is an attribute

`gcslib/qemu` declares `Toolchain::Qemu`: the system emulator and the UEFI
firmware's code and variables. `system_qemu` is the host's, under `/usr`
and so in the host snapshot. A pinned QEMU comes later, as the pinned C
toolchains did after `system_c`.

A step that needs `/dev/kvm` says so with `#[device("kvm")]`, an
attribute the prelude declares. It changes how fast a step runs, never
what it gives, so it is not on the receipt, as `#[retry]` and `#[timeout]`
are not. A machine with `/dev/kvm` binds it into the sandbox; one without
runs the step anyway, and the library falls back to emulation. A device
pekit doesn't know is an error.

### I3. The boot test is a test, and `pekit run` boots interactively

The boot test is a `def test` given the ISO and QEMU, which runs QEMU
until busybox's prompt appears, with no network and a timeout. It is
hermetic, so it reruns only when the ISO or QEMU changes. `qemu.launcher`
builds a small program that boots an ISO with the terminal, for `pekit run
boot`, which runs any build that gives a program.

### I4. Packages from outside come from a repository in the workspace

An image takes packages it doesn't build from an artifact directory, a
*repository*. Today that is a directory in the workspace:
`ppk.from_repository(repo, NAME, VERSION)` is a hermetic step given the
repository's tree, so it is keyed by its contents, and it gives the
package's own artifact directory, as `ppk.pack` gives one. Jack's draft
names `https://pkgs.peios.org`, which doesn't exist yet, and peiso refuses
a repository by URL until Peios can serve one. When it can, `pekit lock`
pins each package from it by name, version and manifest digest in a
`packages` section of `pekit.lock`, and a build fetches the manifest and
each blob it names by digest, as `fetch` fetches a file. That design needs
a repository protocol, ppk's to define, so it waits for one.

### I5. Which of example 10's rows the fixture checks

| Row | Checked by |
|---|---|
| A fresh clone, after CI built `main`, downloads the ISO | layer F: the remote cache (F5) |
| A change to one package rebuilds it, the ISO and the boot test, and no other package | the fixture, with a package built in the workspace standing for busybox |
| A change to the kernel command line rebuilds the ISO and the boot test, and no package | the fixture |
| Nothing changed in 1,000 packages finishes in under a second | layer F: the evaluation cache and the daemon (F4, F8) |
| A broken compiler on one laptop reaches no one else | layer F: who may write the shared cache (F5) |

### I6. Peios's own packages are a phase of their own

Building prelude, ppk, the live-boot root and peiosutils needs a Rust
library: a pinned `rustc` with the musl target, and each crate a fetch
pinned by its checksum in `Cargo.lock`, as Bazel's `rules_rust` does.
prelude and peiosutils build with Bazel today, and ppk with Cargo from a
vendored tree. The kernel builds through its own Docker-based runner,
which can't run in a step: how it is to build under pekit2 is Jack's to
decide. Until then the kernel comes from a repository. The recipes are
ported under the example 10 fixture, since `pkgs` is Jack's.

## Steps

1. **X1, `#[device]`:** the prelude's attribute, and pekit binding
   `/dev/kvm` when it is there.
2. **X2, `gcslib/qemu`:** `Toolchain::Qemu`, `system_qemu`, and
   `launcher`.
3. **X3, `gcslib/peiso`:** `Toolchain::Peiso` and `peiso.iso`.
4. **X4, packages from a repository:** `ppk.from_repository`.
5. **X5, the fixture:** a workspace package, the kernel and busybox from a
   repository seeded from `local/pekit2-data`, the ISO, the boot test and
   `pekit run boot`, and the rows of I5 it checks.
6. **X6, a Rust library and Peios's packages,** with the kernel waiting on
   Jack.

## Decisions taken without Jack (to surface)

- I1: peiso's tools registered by the workspace, as ppk's are.
- I2: `#[device("kvm")]`, a string naming the device, rather than the
  draft's bare `kvm`, since an attribute's argument is a value.
- I4: a repository in the workspace for now, and the lock's `packages`
  waiting for a repository protocol.
- I6: the kernel from a repository until its build under pekit2 is
  decided.

## As built

- X1: `#[device("kvm")]`, `type device = {name: String}` in GCSL's
  prelude, binds `/dev/kvm` with `--dev-bind` into the step's sandbox, and
  into each `Dir.sandbox` it makes, where the machine has it; `kvm` is the
  only device known, another failing the step at its call.
- X2: `gcslib/qemu`: `QemuToolchain = {root, runtime, system, firmware,
  variables}`, `system_qemu` the host's under `/usr`; `boot(iso, until)`
  with `-accel kvm -accel tcg`, so a machine without KVM emulates, `-nic
  none`, and the firmware's variables a snapshot, so the template is
  never written. QEMU puts a snapshot in `/var/tmp`, which a step lacks,
  when `TMPDIR` is `/tmp`, so `boot` gives it `/tmp/qemu-snapshots`.
  `launcher(iso)` writes a `#!/bin/sh` program that boots the ISO with the
  terminal, naming QEMU and the ISO by their paths, which `pekit run`'s
  host view has.
- X3: `gcslib/peiso`: `PeisoToolchain = {root, runtime, peiso, mkirf,
  mkuki}`; `iso(kernel, packages, initramfs, liveboot, output)`, each
  package a `Package` the library packs with `ppk.packed` or a
  `ppk.PpkPackage`, narrowed with `is`; a hermetic step gathers them with
  `ppk fetch` into one repository, writes peiso's TOML spec, and runs
  `peiso build`. A layout's links are a list of `{at, to}`, since GCSL
  can't list a record's fields.
- X4: `ppk.from_repository(repo, name, version)`, a hermetic step given the
  repository's tree, and `ppk.packed(pkg)`, both giving a `PpkPackage =
  {name, version, arch, artifacts}`.
- X5: `tests/fixtures/examples/10/`: the kernel and busybox from a
  repository seeded into `local/pekit2-data/peios-repository` by `ppk
  fetch` from `pkgs/_artifacts`, and a package built here, booted to
  busybox's prompt by a test in about two seconds with KVM. The rows of
  I5 it checks hold: a change to the package built here runs three steps
  and finds the repository's two; a change to the command line alone runs
  the ISO's step and finds four; each reruns the boot test.
- X6a: `gcslib/rust`: `rust_1_98`, Rust 1.98.1's rustc, Cargo and the
  `x86_64-unknown-linux-gnu` and `-musl` standard libraries, pinned by the
  SHA-256s prelude's `MODULE.bazel` pins, composed into one install by a
  step; rustc needs `libz.so.1`, which the Bootlin sysroot lacks, so the
  toolchain builds zlib 1.3.1 with buildenv's pinned environment and puts
  its libraries beside rustc's, found through rustc's `$ORIGIN/../lib`.
  `crates(lock)` reads `Cargo.lock` line by line, since GCSL reads no
  TOML; `vendored(lock)` fetches each crate by its checksum and lays them
  out with `.cargo-checksum.json` records that name no file, which Cargo
  accepts; `cargo_build` runs Cargo in a sandbox of the runtime, with
  prelude's musl flags, `-Crelocation-model=pic -Clink-self-contained=yes
  -Clink-arg=-static-pie`, by default, linking with Bootlin's gcc. ppk
  builds from its committed tree in about half a minute from nothing,
  and the same program strictly, byte for byte. Cargo is one step: per
  crate steps, as `rules_rust` makes, are a follow-up.
- X6b: example 10's image carries `dev.peios.ppk`, built from ppk's
  committed tree with the rust library, and `dev.peios.live-root`, its
  hook from ppk's `boot/`. With busybox as init the hook is carried, not
  run. Found with it: a step a library's definition made, buildenv's
  toolchain's say, was its target's, since a definition made with a
  variant is in no def-type's listing, so each target that resolved the
  toolchain made it again. GCSL now finds such a definition by its cell,
  `Tool::C gcc`, and pekit puts a library's under `.libraries/` and a
  variant's under its def-type, `.libraries/github.com/gcslib/buildenv/
  Toolchain.BuildEnv/busybox_x86_64/assemble`.
- Next of I6: prelude needs the Peios SDK, built from `peios-sdk` at a
  pinned commit by prelude's `tools/prepare-sdk.py`, a crate from a git
  repository, `peios-rs` pinned by commit, which the rust library refuses
  until it pins git crates, and libclang, for bindgen, which wants a
  pinned LLVM. peiosutils, for `mkirf` and `mkuki`, is a large Cargo
  workspace. The kernel waits on Jack.
