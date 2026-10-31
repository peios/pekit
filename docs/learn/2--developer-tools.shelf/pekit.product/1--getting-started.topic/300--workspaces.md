---
title: Workspaces
type: concept
description: How a pekit workspace is laid out, what workspace.gcsl and its
  pekitconf hold, where build files go, and which files pekit adds and you
  commit or ignore.
related:
  - pekit/building/naming-targets
  - pekit/libraries/pinning-libraries
  - pekit/concepts/where-results-go
---

A workspace is the directory tree that pekit builds from. Its root holds a
file called `workspace.gcsl`, and every directory below the root belongs to
it. pekit finds the workspace from the directory you run it in, by looking
upward for the nearest `workspace.gcsl`, so you can run pekit from any
directory inside.

A pekit workspace is a GCSL workspace, so GCSL's rules for how files see
each other apply unchanged. [Workspaces](~gcsl/workspaces) in the GCSL
documentation describes them.

## Build files

Any `.gcsl` file in the workspace can declare targets. A common layout puts
a `build.gcsl` in each directory beside the sources it builds:

```text
workspace.gcsl
pekit.lock
app/
  build.gcsl
  main.c
lib/
  build.gcsl
  greet.c
  greet.h
```

The files of one directory share their public declarations, and a
directory sees the public declarations of the directories above it. So a
helper declared in `workspace.gcsl` is available everywhere, and a target in
`app/build.gcsl` can call another in `app/package.gcsl` without an import.
To use a declaration from a sibling directory, import it by its path from
the root, as `import lib`.

A definition's full name is its directory and its name: `app/hello` is the
definition `hello` in `app`. [Naming Targets](~pekit/naming-targets)
explains how you name one on the command line.

A relative path in a build file, such as `file("main.c")`, is relative to
the directory of the file it's written in. It's never relative to the
directory you run pekit from.

## What pekit leaves out

pekit reads build files from every directory of the workspace, except:

- A directory or file whose name starts with a dot, such as `.git`.
- A directory below the root that has a `workspace.gcsl` of its own. A
  nested workspace is a separate workspace, built on its own.
- A path that `.gcslignore` at the root names. Use it for `.gcsl` files that
  are data, such as test fixtures. Each line names one file or directory,
  relative to the root, and lines that start with `#` are comments.

These rules decide only which `.gcsl` files declare definitions. A build
can read any file under the root, hidden or not, and a directory listing
such as `dir(".").files()` includes them all. The one exception is
`pekit-out/`, the output directory: no listing includes it, and no build
reads it as part of the workspace.

A step can take the whole workspace, by being given `dir(".")` at the root.
It receives everything under the root except `.git` and `pekit-out/`, so a
commit doesn't rerun the step, but any other file there does, whether or not
Git tracks it. Listing only what a step needs avoids those reruns.

## `workspace.gcsl` and `pekitconf`

`workspace.gcsl` is an ordinary GCSL file. It can be empty, and it can
declare helpers for the whole workspace. pekit also reads one value from
it, `pekitconf`, a record of the workspace's settings:

```gcsl
import Toolchain::* from "github.com/gcslib/gcc"
import Toolchain from toolchain
import keychain

pekitconf = {
    toolchains = [Toolchain::C gcc_x86_64]
    keychain = [{profile = "dev"; name = "signing"; file = "keys/test.pem"}]
}
```

Each field is optional:

- **`toolchains`.** The compilers and other tools the workspace builds
  with. See [Platforms and Toolchains](~pekit/platforms-and-toolchains).
- **`keychain`.** Where each secret the build uses is kept. See [Using
  Secrets](~pekit/using-secrets).
- **`packagers`.** The package formats `pekit package` packs into. See
  [Packaging](~pekit/packaging).

pekit reads a field only when the workspace imports the module the setting
belongs to: `toolchain` for `toolchains`, `keychain` for `keychain`, and
`packaging` for `packagers`. A library's `import` of the module counts, so
importing the gcc library is enough for `toolchains`.

## Files pekit adds

pekit adds these to a workspace:

| Path | Holds | In version control |
|---|---|---|
| `pekit-out/` | What each target built, and the files of every step that ran. | Ignore it. |
| `.gcsl/libraries/` | The libraries `pekit lock` pinned, exported from their repositories. | Ignore it. Any build puts back a pinned library that's missing. |
| `pekit.lock` | The commit each library is pinned at. | Commit it, so every checkout builds with the same libraries. |

Add `pekit-out/` and `.gcsl/` to your `.gitignore`. Everything else pekit
keeps, such as its store, is outside the workspace, in your cache
directory. [Reuse and the Store](~pekit/reuse-and-the-store) describes it.
