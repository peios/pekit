# Example 1: one C file to one binary

The bare minimum. There is one source file, `hello.c`, and the goal is a
program called `hello`.

## What must happen

By hand, the build is one command:

```sh
cc -o hello hello.c
```

In build-system terms:

- **One step:** run the C compiler.
- **Input:** `hello.c`, a file. The compiler itself is also an input, but
  ignore that for now. It returns in example 6.
- **Output:** `hello`, a file.
- **What someone types:** something like `pekit build hello`. Pekit runs the
  step and puts `hello` somewhere they can find it.
- **Run it again with nothing changed:** pekit sees the same code and the same
  `hello.c` content and reuses the earlier `hello`. The compiler does not run.
- **Change `hello.c` and run again:** the content hash differs, so the step
  runs again.

## What to write

Write a `BUILD.gcsl`, or whatever you think the file should be called, in this
directory. Show how you would like to describe this build. Don't worry about
whether the syntax exists yet; this is about taste.

Things worth deciding as you write it, though any answer is fine for now:

- What you name the thing being built, and how someone asks for it.
- Where the hermetic function lives: written in the build file, or in a library
  the build file imports.
- How the file `hello.c` is passed in, and how the output `hello` comes back.
