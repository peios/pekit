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
