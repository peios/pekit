---
title: Naming Targets
type: concept
description: How pekit finds the definition a name means, how a full name
  picks one of several with the same name, and how globs select many
  targets at once.
related:
  - pekit/getting-started/workspaces
  - pekit/concepts/targets-and-steps
---

Every pekit command that builds takes targets: definitions in your
workspace, named by their name. `pekit build hello` builds the `build`
definition called `hello`, and `pekit test greeting` runs the `test`
definition called `greeting`. The command decides the kind; the name
decides which definition of that kind.

## When a name is defined more than once

Directories often reuse names: several can each have a `build main`.
pekit picks one this way:

1. If only one definition of the kind has the name, pekit uses it, from
   any directory.
2. If several do, pekit uses the one in the directory you ran pekit from.
3. Otherwise, pekit lists each one by its full name and asks you to name
   one.

A full name puts the definition's directory first, relative to the
workspace's root: `pekit build app/hello`. A definition in the root
directory is `/hello`. A full name always means the same definition,
wherever you run pekit from.

If no definition has the name, pekit says so and suggests the closest
names it has.

## Several targets at once

A command takes as many targets as you give it. A glob selects every
definition of the command's kind that it matches, relative to the
directory you run pekit from. A glob reads as a full name does: its last
part is the name, and the parts before it are directories.

| Glob | Selects |
|---|---|
| `app/*` | Every definition in `app`. |
| `app/test_*` | Each definition in `app` whose name begins with `test_`. |
| `*` or `.` | Every definition in this directory. |
| `pkgs/*/*` | Every definition in each directory directly below `pkgs`. |
| `pkgs/**` | Every definition in `pkgs` and every directory below it. |
| `**` | Every definition in this directory and every directory below it. |

In a part, `*` matches any run of characters, as in `net.*`. `**` matches
any number of directories, including none, and as the last part it matches
every name in them. A glob that starts with `/` is relative to the
workspace's root.

Quote a glob, so that your shell doesn't expand it first:

```sh
pekit build hello 'pkgs/**'
```

Several targets are one build. A step that two of them need runs once,
and their steps run at the same time. A definition that several names or
globs select is built once. A target that fails doesn't stop the others,
and pekit exits with status 1 if any did.

With several targets, pekit reports each one, then the steps for the
whole build:

```text
built /stamp
  pekit-out/stamp/stamped/copy
built /tarred
  pekit-out/tarred/archive/files.tar
2 steps run, 0 found in the store
```

## Commands that default to everything

`pekit test` and `pekit package` don't need a name. Given none, each takes
every definition of its kind in the directory you run it from and every
directory below, as `**` would. `pekit build` always needs at least one
target.
