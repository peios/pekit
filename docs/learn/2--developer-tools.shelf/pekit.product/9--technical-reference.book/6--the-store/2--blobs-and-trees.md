---
title: Blobs and Trees
description: How the store keeps files as blobs with executable and
  numbered copies, directories as trees, and how it avoids reading a tree
  it has seen before.
---

The store keeps a file's contents once, as a *blob*, and a directory as a
*tree* of its entries. Both are named by SHA-256 digests, so the same
contents are kept once, whichever step, fetch, or workspace they came from.

## Blobs

A blob is `cas/<xx>/<digest>`, where `<xx>` is the digest's first two
hexadecimal digits. It is read-only.

Whether a file is executable is not part of its contents, but a file's
mode is shared by every hard link to it. So an executable file's blob is a
copy beside the plain one, `<digest>.x`, made the first time it is needed.

A tree or a composition that holds one content at several paths needs a
separate file for each, so that no two of its files are one. The store
keeps numbered copies for this, `<digest>.r1`, `<digest>.r2`, and on, and
`<digest>.x.r1` and on for an executable one, each made the first time it
is needed and kept (`Store::link_copy`). The first path is a link to the
blob, and each other path a link to the next copy. No record names a copy,
so an eviction removes copies first, as costing nothing to make again.

A blob is taken out of the store by a hard link (`Store::materialise_file`),
or by a copy where a link can't be made, as across file systems.

## Trees

A tree is a directory's entries, sorted by name. Each entry is one of:

- A file, by its contents' digest and whether it is executable
- A directory, by its own tree's digest
- A symbolic link, by its target, kept as it is

A tree is encoded canonically as JSON and stored as a blob itself, so a
tree's digest covers its whole contents at any depth, as the directories
of the Remote Execution API's content store do. Names have to be UTF-8; a
directory holding another name can't be put into the store.

A tree found to have every blob and tree it names, at any depth, is marked
in `complete/` by its digest (`Store::holds_tree`), so that it is walked
for that once, not in every build that finds a record naming it.

## Putting a tree it has seen

A tree put into the store again and again, as a step's writable directories
are around each of its programs, is mostly what it was. `Store::put_tree_seen`
keeps each file's stamp, its device, inode, size, mode, and times, from
when it was last put, and reads again only a file whose stamp differs, or
whose blob is gone.

A file changed since it was stamped has another change time, unless it
changed in the same tick of the file system's clock as it was stamped. So a
stamp taken in the tick a put begins in, or later, by the clock of the
tree's own file system, isn't trusted, as git treats its index.

A file that is a hard link to a blob this process linked, by its device and
inode, is that blob, and isn't read. A file that is read is hashed before
it is copied, and isn't copied if its blob is there. A directory known not
to change, such as a sealed composition, is walked once. A mount point's
directory takes the mounted tree's digest, unread.
