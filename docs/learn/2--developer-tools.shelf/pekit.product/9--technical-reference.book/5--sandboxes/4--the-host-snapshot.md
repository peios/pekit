---
title: The Host Snapshot
description: The host directories steps see outside a strict build, how
  pekit fingerprints them, and what happens on a host it can't identify.
---

Outside a strict build, every step sees the host's system directories, so
that it can run the host's programs: the *host snapshot* (`HostSnapshot`).
A strict build gives steps none of it.

## What it binds

Each of these that exists is bound read-only at its own path:

- `/usr`, `/bin`, `/lib`, `/lib64`, and `/sbin`.
- `/etc/alternatives`, where Debian's `cc` and similar names lead.
- `/etc/ld.so.cache`, the dynamic linker's cache.

`/usr/local` is hidden by an empty `tmpfs`, since no package manager
manages it, and so its changes would change no fingerprint. Nothing else
of `/etc` is there.

## The fingerprint

The snapshot is an input of every step, and too large to hash on every
build. It is identified instead by a *fingerprint*, a digest of these:

- `/etc/os-release`
- The package manager's database, which changes whenever a package is
  installed, upgraded, or removed
- What the bound parts of `/etc` say, which an administrator can change
  without a package: each alternative's target, and the linker's cache

The databases pekit knows are dpkg's `/var/lib/dpkg/status`, rpm's
`rpmdb.sqlite` in `/var/lib/rpm/` or `/usr/lib/sysimage/rpm/`, apk's
`/lib/apk/db/installed`, and pacman's `/var/lib/pacman/local/`, whose
entries name each package and version.

The fingerprint is part of the platform every receipt names. A step
recorded before a compiler was upgraded is not found after it.

## A host it can't identify

On a host with none of those databases, the snapshot has no fingerprint.
Its steps are recorded, but never found in the store, since nothing would
say when its tools changed. Every command that builds warns:

```text
warning: the host's packages cannot be identified, so no step is reused from the store
```

A strict build doesn't use the snapshot, so it reuses steps on any host.

## The note after a build

After a build that ran at least one step and wasn't strict, pekit notes
that steps ran with the host's own tools, so the build isn't hermetic.
Every step of such a build could see the snapshot, whether or not it used
it.
