---
title: Building and Booting an Image
type: guide
description: Build a bootable Peios ISO from packages with the peiso library, boot it in a test with the qemu library, and run it with pekit run.
---

A bootable ISO is an ordinary build. The peiso library makes one from
packages with `peiso`, and the qemu library boots one in QEMU, in a test
or for you to use.

## The image

`peiso.iso` takes the kernel the ISO boots and the packages it carries.
Each is a package definition, which the library packs with ppk, or a
package ppk has packed already, such as one from a repository:

```text
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

Each package says whether the live system holds it, `liveboot`, which it
does unless you say otherwise, and whether the initramfs does,
`initramfs`. `initramfs` and `liveboot` arguments replace what each holds
outside `/usr`, as peiso's own spec does: symbolic links, each `{at; to}`,
with a target starting `:` in `usr`, and empty directories.

The ISO is one step, given its packages, so changing the kernel's
command line rebuilds the ISO and no package, and changing one package
rebuilds that package and the ISO.

## Packages from a repository

`ppk.from_repository(repo, name, version)` takes a package that isn't
built here from an artifact directory in the workspace, a repository, by
its name and version, for the platform being built for. It's a step
keyed by the repository's contents, so what it gives is exactly what the
repository holds. Peios doesn't serve packages over the network yet, so a
repository is a directory of the workspace for now.

## Booting it in a test

`qemu.boot(iso, until)` boots an ISO under UEFI, with no network and its
console on its first serial port, until a line holding `until` appears,
and gives what it printed, its status `0` if the line appeared. In a
test, ask for KVM so the machine runs at speed:

```text
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

## Booting it yourself

`qemu.launcher(iso)` builds a program that boots the ISO with your
terminal, so `pekit run` can run it:

```text
def build boot() { = qemu.launcher(build iso()) }
```

```sh
pekit run boot
```

## Toolchains

The libraries ask the `toolchain` module for their tools. The qemu
library's `system_qemu` is the host's `qemu-system-x86_64` and OVMF's
firmware. peiso, `mkirf`, and `mkuki` aren't published, so register the
ones you have, as ppk is registered (see ~pekit/packaging):

```text
def Toolchain::Peiso workspace_peiso() {
    let tools = dir(path.join(fileroot.this, "tools"))
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

peiso runs `mkfs.vfat`, mtools, and `xorriso` from the host.
