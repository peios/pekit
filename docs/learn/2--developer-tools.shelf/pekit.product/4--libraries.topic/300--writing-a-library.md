---
title: Writing a Library
type: how-to
description: Write a pekit library of steps in a git repository, use files
  of its own, try it from a workspace with GCSL_LIBRARY_PATH, and publish it
  for other workspaces to pin.
related:
  - pekit/libraries/libraries
  - pekit/building/writing-steps
  - pekit/libraries/pinning-libraries
---

A library packages steps for other workspaces to use. It's a directory of
GCSL files in a git repository, named by an import path. This page writes a
small library that greets someone using a template file of its own, tries
it from a workspace, and pins it there.

## Before you start

- `git`.
- A workspace to try the library from. This page uses one at `uselib`,
  with an empty `workspace.gcsl`.

## Write the library

1. Make a directory for the library, and a git repository in it. This page
   names the library `example.org/greeting`, and keeps its checkout at
   `libsrc/example.org/greeting`:

   ```sh
   mkdir -p libsrc/example.org/greeting/templates
   cd libsrc/example.org/greeting
   git init -b main
   ```

   A library published at `https://example.org/greeting` is found there by
   default, as the [Go modules](https://go.dev/ref/mod) convention has it.

2. Save the template as `templates/greeting.txt`:

   ```text
   Hello, NAME, from a library
   ```

3. Save the library's code as `greeting.gcsl`:

   ```gcsl
   # Greets someone with the library's own template.

   #[track_caller]
   #[hermetic]
   def greet(name: String, template: File = file("templates/greeting.txt")) -> File {
       let made = exec(["/bin/sh", "-c", "sed 's/NAME/{name}/' {template.path} > greeting.txt"])
       = working_dir().file("greeting.txt")
   }
   ```

   A library's public names are its `def` functions, declared types, and
   fields; `fn` and `let` stay private to their file. `#[track_caller]`
   reports an error in the step at the caller's line, where the reader can
   act on it. The relative path in the default names the library's own
   file, wherever the library is used.

4. Commit it:

   ```sh
   git add -A
   git commit -m "Add the greeting library"
   ```

## Try it from a workspace

1. In `uselib/build.gcsl`, import the library and call it:

   ```gcsl
   import "example.org/greeting"

   def build hello() {
       = greeting.greet("world")
   }
   ```

2. Build with `$GCSL_LIBRARY_PATH` naming the directory that holds
   `example.org/`:

   ```sh
   GCSL_LIBRARY_PATH=/path/to/libsrc pekit build hello
   ```

   pekit finds the library in your checkout. Edit it and build again, and
   pekit uses your edit.

## Pin it

1. Tell `pekit lock` where the library's repository is. In
   `uselib/pekit.lock`, add a source:

   ```json
   {"sources": [{"prefix": "example.org/greeting", "git": "/path/to/libsrc/example.org/greeting", "dir": "", "rev": "main"}]}
   ```

   A library published at the URL its path names needs no source.

2. Pin it, without `$GCSL_LIBRARY_PATH`:

   ```sh
   pekit lock
   pekit build hello
   ```

## Check the result

`pekit lock` prints the commit it pinned:

```text
pinned example.org/greeting at 1b4e27f7c836186a4bfab3f74521ef642b953632
```

The build after it reports the step found in the store, `0 steps run, 1
found in the store`. The step is keyed by the library's path and contents,
not by where pekit found it, so the result from your checkout is reused.
`pekit-out/hello/greet/greeting.txt` holds `Hello, world, from a library`.

## Ask for a tool

A library that runs a compiler or another tool should ask for it as a
toolchain, so that each workspace chooses which one. Use an existing kind,
such as `Toolchain::C`, or declare a kind in the library, and resolve it in
a step's parameter default. [Registering
Toolchains](~pekit/registering-toolchains#define-a-toolchain-of-your-own)
shows both sides.

## Keep libraries apart from the workspace

A library sees nothing of the workspace that imports it: not its
declarations, and not its files. Take anything from the workspace as an
argument. A library can import other libraries, which `pekit lock` pins
along with it.
