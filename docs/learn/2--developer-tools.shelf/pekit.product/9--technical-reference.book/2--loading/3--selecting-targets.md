---
title: Selecting Targets
description: How pekit turns the names and globs on its command line into
  definitions, and how several selected targets become one build.
---

A target is a definition made with a definition type: `build hello` is the
definition `hello` of the definition type `build`. The command decides the
definition type: `build` for `pekit build`, `test` for `pekit test`,
`package` for `pekit package`, `pekit publish`, and `pekit release`, the
`--kind` option's for `pekit query`, and `script`, or else `build`, for
`pekit run`. Selection is `target::select`.

## Names

A name is a definition's path alone, `hello`, or qualified by its directory
relative to the root, `app/hello`, with `/hello` for one in the root
directory. A path alone resolves in order:

1. The one definition of that path in the workspace, wherever pekit runs
   from.
2. Of several, the one in the directory pekit runs from.
3. Otherwise none. The command fails with "`build hello` is defined in
   several places", listing each by its qualified name.

A qualified name means the definition in that directory, or none. A name
that matches nothing fails with "no `build` is named `hello`", with the
closest names there are.

## Globs

A name with `*` in it is a glob. It selects every definition of the
definition type that it matches, relative to the directory pekit runs from,
or to the root if it starts with `/`. Its last part matches the
definition's name, and the parts before it match directories:

- `*` in a part matches any run of characters within the part.
- `**` matches any number of directories, including none. As the last part,
  it matches every name in them too.
- `.` matches the directory itself, and every name in it.

So `app/*` is every definition of `app`, `pkgs/*/*` every definition of a
directory directly below `pkgs`, and `**` every definition at or below the
current directory.

`pekit test`, `pekit package`, and `pekit query rdeps` select `**` when
given no names.

## Several targets

What several names and globs select is selected once, and the targets are
sorted by their qualified names. They are evaluated as one build, against
one action graph: each target is a unit of its own, and a step two units
reach is one node, run once. Reports come in the sorted order.

For `pekit run`, a definition of one name in one directory is a script or a
build, never both, by the check of
[Finding and Checking the Workspace](~pekit/finding-the-workspace). Of one
name in two directories, `pekit run` asks for the directory, as any
command does.
