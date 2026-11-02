---
title: Setting a Context for the Whole Workspace
type: how-to
description: Set a GCSL context that the workspace or one of its libraries
  declares once in pekitconf, so that every target pekit builds reads it,
  and override it for one target with a with block.
related:
  - pekit/getting-started/workspaces
  - pekit/libraries/writing-a-library
---

A library can declare a GCSL context to let the workspace that imports it
change how it builds, such as extra compiler flags. Set the context once in
`pekitconf.contexts`, and pekit binds it around every target it builds. You
don't need a `with` block in each build file.

## Before you start

- A workspace that imports a library declaring a context. This example
  uses a library `github.com/example/greeting` that declares one, and
  reads it in a step:

  ```gcsl
  context greeting: String = "Hello"

  #[hermetic]
  def greet(name: String) -> File {
      let greeted = exec(["sh", "-c", "echo '{^greeting}, {name}' > greeting.txt"])
      = working_dir().file("greeting.txt")
  }
  ```

  See [Libraries](~pekit/libraries) for how to make a library available.

## Set the context

1. In `workspace.gcsl`, add the context to `pekitconf.contexts`, by its
   name:

   ```gcsl
   pekitconf = {
       contexts = {greeting = "Good morning"}
   }
   ```

   `workspace.gcsl` doesn't need to import the library, as long as some
   file of the workspace does. Each value can be any GCSL expression that
   `workspace.gcsl` can evaluate, such as a call of a function the
   workspace defines.

2. Build a target that uses the library:

   ```sh
   pekit build hello
   ```

## Check the result

The step that reads `^greeting` writes `Good morning, world`. A step that
doesn't read the context keeps its key, so pekit finds it in the store
rather than running it again. A step that reads it runs again whenever the
value changes.

pekit checks each field before it builds anything:

- A name that no file of the workspace or its libraries declares as a
  context fails the build, naming the field.
- A value that isn't of the context's type fails with GCSL's type error,
  as a `with` block's value would.
- pekit sets some contexts itself, such as `host_platform` and
  `toolchains`, and `contexts` can't set those. Use their own options
  and fields instead. [Root Contexts](~pekit/root-contexts) lists them.

## Override it for one target

A `with` block in a build file binds the context for what it covers, as it
would anywhere in GCSL. It overrides the workspace's value:

```gcsl
import "github.com/example/greeting"

def build hello() {
    = greeting.greet("world")
}

def build formal() {
    = greeting.greet("world") with { greeting = "Good day" }
}
```

`pekit build formal` writes `Good day, world`. `pekit build hello` still
writes `Good morning, world`.

## Evaluation

Each value is evaluated as part of `workspace.gcsl`, before any target.
That evaluation can't read the target's own contexts, such as `^platform`.
A value that has to depend on them belongs in a `with` block in the build
files instead.
