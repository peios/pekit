---
title: Building and Booting an Image
type: how-to
description: Build a bootable Peios ISO from packages with the peiso
  library, boot it in a test with the qemu library, and boot it yourself
  with pekit run.
related:
  - pekit/shipping/packaging
  - pekit/testing-and-running/testing
  - pekit/testing-and-running/running-scripts-and-programs
---

A bootable ISO is an ordinary build. The peiso library makes one from
packages with the `peiso` tool, and the qemu library boots one in QEMU, in
a test or for you to use.

## Before you start

- The peiso, ppk, and qemu libraries. See [Libraries](~pekit/libraries).
- The `peiso`, `mkirf`, and `mkuki` programs, and `ppk`, to register as
  toolchains.
- On the host: `mkfs.vfat`, mtools, and `xorriso`, which peiso runs, and
  QEMU with OVMF's firmware for booting.

## Register the tools

The libraries ask the `toolchain` module for their tools. The qemu
library's `system_qemu` is the host's `qemu-system-x86_64` and OVMF's
firmware. peiso, `mkirf`, and `mkuki` aren't published, so register the
ones you have, as ppk is registered in [Packaging](~pekit/packaging):

```gcsl
def Toolchain::Peiso workspace_peiso() {
    let tools = dir("tools")
    host_platform = ^host_platform
    platform = {}
    provides = {
        root = tools
        runtime = dir("/")
        peiso = tools.file("peiso")
        mkirf = tools.file("mkirf")
        mkuki = tools.file("mkuki")
    }
}
```

## Build the image

1. In a build file, call `peiso.iso` with the kernel the ISO boots and the
   packages it carries:

   ```gcsl
   import "github.com/gcslib/peiso"
   import "github.com/gcslib/ppk"

   def build iso() {
       = peiso.iso(
           kernel = {
               package = ppk.from_repository(dir("repository"), "dev.peios.kernel", "0.25.0-indev-1")
               cmdline = "console=ttyS0"
           },
           packages = [
               {package = ppk.from_repository(dir("repository"), "net.busybox.busybox", "1.37.0-1"); initramfs = true},
               {package = package dev.example.motd()},
           ],
       )
   }
   ```

   Each package is a package definition, which the library packs with ppk,
   or a package ppk has packed already, such as one from a repository.

2. Build it:

   ```sh
   pekit build iso
   ```

## Check the result

pekit prints the ISO's path. The ISO is one step, given its packages, so
changing the kernel's command line rebuilds the ISO and no package.
Changing one package rebuilds that package and the ISO.

## Choose where each package goes

Each package says whether the live system holds it, `liveboot`, which it does
unless you say otherwise, and whether the initramfs does, `initramfs`. The
`initramfs` and `liveboot` arguments of `peiso.iso` replace what each holds
outside `/usr`, as peiso's own specification does. Each holds symbolic links,
each `{at; to}`, with a target starting `:` in `usr`, and empty directories.

## Take packages from a repository

`ppk.from_repository(repo, name, version)` takes a package that isn't built
here from an artifact directory in the workspace, by its name and version,
for the platform being built for. It's a step keyed by the repository's
contents, so what it gives is exactly what the repository holds. The
repository has to be a directory of the workspace; the ppk library doesn't
fetch one over the network.

## Boot the image in a test

`qemu.boot(iso, until)` boots an ISO under UEFI, with no network and its
console on its first serial port, until a line holding `until` appears. It
gives what the machine printed, with status `0` if the line appeared. In a
test, ask for KVM so that the machine runs at speed:

```gcsl
import "github.com/gcslib/qemu"

#[device("kvm")]
#[timeout(5m)]
def test boots(iso: File = build iso()) {
    let run = qemu.boot(iso, until = "Please press Enter to activate this console.")
    assert run.status == 0, run.stdout
}
```

The test is a step given the ISO, so it boots again only when the ISO
changes. Without KVM, QEMU emulates the machine, slowly but the same.

## Boot the image yourself

`qemu.launcher(iso)` builds a program that boots the ISO with your
terminal, so that `pekit run` can run it:

```gcsl
def build boot() { = qemu.launcher(build iso()) }
```

```sh
pekit run boot
```
