---
title: Naming Targets
type: concept
description: Name the definition you want built, from anywhere in the workspace.
---

A target is a definition in your workspace, such as `def build hello()`.
You name it by its kind and its name: `pekit build hello` asks for the
`build` definition called `hello`.

A workspace is the directory tree under a `workspace.gcsl` file. Pekit
finds it from the directory you run it in, by looking upwards for that
file, so you can run it from any directory inside.

## When a name is defined more than once

Directories often reuse names: many of them can have a `build main`.
Pekit picks one this way:

- If only one definition has the name, it's that one, wherever you are.
- If several do, it's the one in the directory you run pekit from.
- Otherwise pekit lists each one, by its full name, and asks you to name
  one.

A full name puts the definition's directory first, relative to the
workspace root: `pekit build pekit2/examples/1/hello`. A definition in the
root directory is `/hello`. A full name always means the same
definition, wherever you run pekit from.

If no definition has the name, pekit says so and suggests the closest
names it has.

## Several targets at once

A command takes as many targets as you give it, and a *directory glob*
selects every definition of its kind in the directories it matches,
relative to where you run pekit:

| Glob | Selects the definitions in |
|---|---|
| `.` | this directory |
| `*` | each directory directly below this one |
| `pkgs/*` | each directory directly below `pkgs` |
| `pkgs/**` | `pkgs` and every directory below it |
| `**` | this directory and every directory below it |

`*` matches one directory's name, or part of one, as in `net.*`, and `**`
any number of directories, none included. A definition that several names
or globs select is built once.

```sh
pekit build hello 'pkgs/**'
```

Quote a glob, so your shell doesn't expand it first. Several targets are
one build: a step two of them need runs once, and their steps run at the
same time. A target that fails doesn't stop the others, and pekit exits
with status 1 if any did.
