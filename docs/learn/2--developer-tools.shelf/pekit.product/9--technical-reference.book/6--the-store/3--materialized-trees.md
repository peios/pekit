---
title: Materialized Trees
description: How the store makes a tree once as a directory of links for
  steps to mount, how instances keep a composition's files apart, and when
  materialized trees are removed.
---

A step is given, and composes, whole trees, such as a toolchain of tens of
thousands of files. Linking each file into each step's staging directory
would cost as much as the step. Instead the store makes a tree's directory
once, in `trees/`, of hard links to its blobs and their copies, and every
step that needs the tree mounts that directory read-only (`store::trees`).
Nothing changes it, so every step of every build shares it.

## When a tree is mounted

A tree is mounted rather than linked file by file when it holds 64 files
and symbolic links or more, at any depth (`MOUNT_FILES`). That applies to a
directory a step is given, and to one a composition places whole.

## Instances

A tree is materialized as an *instance*: its digest, and for some of its
contents a *base*, the number of the copy its first file of that content
and mode links. The instance with no bases is named by the tree's digest,
`trees/<digest>`. Any other is named by the digest and the first 16
hexadecimal digits of a digest of its bases, `trees/<digest>-<bases>`.

A composition that mounts a tree whose contents it already holds
elsewhere mounts an instance whose files of those contents link copies the
composition doesn't use yet. So no two of the composition's files are one.

## Making one

An instance is made in `tmp/` and renamed into `trees/` whole, so an
instance there is complete. Within a process, each instance has its own
lock, so two threads never make one twice. A process that loses a race with
another finds the other's instance there, and drops its own.

pekit learns what is in an instance from the tree, not by walking its
directory: the digest of a directory inside it, and which blob or copy each
of its files links.

## Removal

An instance holds links to blobs, so an eviction removes every
materialized tree before it removes any blob (`Store::forget_materialised`).
The next step that needs a tree materializes it again.
