---
title: Compositions and Overlays
description: How pekit makes a step's composed directories without
  copying, seals them, and runs a sandbox of one as an overlay, with nested
  overlays for the trees it mounts.
---

A step composes directories with `fs.compose` and `fs.compose_dir`, and
runs programs in a `Dir.sandbox` of one, as a toolchain library does to
run a pinned compiler on its own runtime. Plain GCSL copies for both. A
step's sandbox answers both itself (`Layers`), without copying.

## Compositions

A composition places each entry under its key, as GCSL describes. pekit
places each kind of entry this way (`Layers::place_tree` and
`Layers::place_file`):

- **A directory of 64 files or more** that is a store tree is mounted: an
  empty directory of its own, the mount point, is made at its place, and
  the materialized tree is bound read-only there for every program. GCSL
  finds what it holds there too.
- **A file nothing can change,** one of a staged input, a materialized
  tree, or another sealed composition, is hard-linked.
- **Any other file** is copied.

A content a composition places twice, as a toolchain's runtime is placed
both alone and within the toolchain, is a link to the blob once and to a
numbered copy each other time. A tree mounted in a composition that holds
contents the composition already has is an *instance* of the tree whose
files of those contents link later copies. So no two files of a
composition are one, and nothing is copied anew.

Directories GCSL doesn't place whole, because an entry is placed inside
them, are made entry by entry.

## Sealing

Once a composition is made it is *sealed*: its files are made read-only on
the host, and it is bound read-only to every program of the step. No
program can change it, and so none can change a blob through a link.
GCSL's file functions refuse to write into it. Its directories keep their
modes, since an overlay's merged directory shows its lower directory's
mode, and a read-only one would refuse every write.

## Sandboxes of a composition

A `Dir.sandbox` of a directory that nothing can change, a sealed
composition or a staged input, is an overlay (`Layers::make_sandbox`):

- The directory is the *lower* layer, which no program changes.
- A directory of the step's own outputs is the *upper* layer, where
  every write goes.
- Each tree mounted in the directory is an overlay of its own, *nested* on
  its mount point, with an upper layer of its own, `.pekit-upper-<n>` in
  the step's output directory.

So a program in the sandbox can write anywhere, and nothing it writes
reaches the composition. It can't move or remove a nested overlay's mount
point, which fails with `EBUSY`, "Device or resource busy".

A `Dir.sandbox` of any other directory is copied, as plain GCSL does.

## Reading an overlay

GCSL reads a path of an overlay sandbox as the overlay shows it
(`Layers::resolve`):

- What the upper layer has, unless it is a *whiteout*, a character device
  of number 0, which hides what the lower has.
- Otherwise what the lower has, unless a directory of the upper layer on
  the way is *opaque*, marked `user.overlay.opaque`, which hides all the
  lower has below it.
- A directory both layers have is merged into a new tree of hard links, a
  snapshot of it as it is at the time.
- A path in a nested overlay is read from that overlay's layers.

GCSL writes to an overlay sandbox's path in the upper layer of the overlay
the path is in (`Layers::write_target`). A step's result can name a file or
directory of an overlay sandbox; it is kept from the merged view.
