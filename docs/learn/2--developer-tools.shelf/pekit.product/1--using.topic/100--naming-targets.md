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

A command takes as many targets as you give it, and a *glob* selects
every definition of its kind that it matches, relative to where you run
pekit. A glob reads as a full name does: its last part is the name, and
the parts before it the directory.

| Glob | Selects |
|---|---|
| `app/*` | every definition in `app` |
| `app/test_*` | those in `app` whose names begin `test_` |
| `*` or `.` | every definition in this directory |
| `pkgs/*/*` | every definition in each directory directly below `pkgs` |
| `pkgs/**` | every definition in `pkgs` and every directory below it |
| `**` | every definition in this directory and every directory below it |

In a part, `*` matches any run of characters, as in `net.*`. `**` matches
any number of directories, none included, and as the last part, every
name in them. A glob that starts with `/` is from the workspace's root. A
definition that several names or globs select is built once.

```sh
pekit build hello 'pkgs/**'
```

Quote a glob, so your shell doesn't expand it first. Several targets are
one build: a step two of them need runs once, and their steps run at the
same time. A target that fails doesn't stop the others, and pekit exits
with status 1 if any did.
