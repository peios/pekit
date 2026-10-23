# Example 2: a library, a program and headers

A small library, `greet`, and a program, `app`, that uses it.

```
lib/greet.h     the library's public header: what users of the library see
lib/internal.h  a private header that only the library's own code uses
lib/greet.c     the library's code, which includes both headers
main.c          the program, which includes greet.h only
```

## What must happen

By hand, the build is four commands:

```sh
cc -c -o greet.o lib/greet.c            # 1. compile the library's code
ar rcs libgreet.a greet.o               # 2. bundle it into a static library
cc -c -Ilib -o main.o main.c            # 3. compile the program
cc -o app main.o libgreet.a             # 4. link them into one binary
```

Steps 1 and 3 don't need each other, so they run in parallel. Step 2 needs
step 1, and step 4 needs steps 2 and 3.

This is the first example where *what changed* decides *what reruns*:

| You change | Must rerun | Must *not* rerun |
|---|---|---|
| `main.c` | 3, 4 | 1, 2 |
| `lib/greet.c` | 1, 2, 4 | 3 |
| `lib/greet.h` | 1, 2, 3, 4 | |
| `lib/internal.h` | 1, 2, 4 | 3, because `main.c` never sees it |
| a comment in `greet.c`, giving an identical `greet.o` | 1 | 2, 4, through early cutoff |

## The new problem: headers

Nobody passes headers to the compiler. `cc` finds them itself by following
`#include` lines. A hermetic step only sees what it was given, so:

- step 3 needs `greet.h` in its sandbox, or it fails;
- step 3 must not depend on `internal.h`, or editing it would rebuild `main.o`
  for nothing.

So the build file has to say which headers go where, or pekit has to discover
them. That is the main thing this example is about.

## The other new thing: a library is a target with more than one output

`app` needs two things from `greet`: the compiled `libgreet.a` for linking,
and the public header `greet.h` for compiling. A library target therefore hands
its users more than one `File`, and possibly different things to different
steps.

## What to write

Write `build.gcsl` in this directory, using example 1's style. Things to
decide, though any answer is fine:

- How a library target and a program target are written, and how `app` says
  that it uses `greet`.
- What a library target returns, so that `app` can get both the archive and
  the public headers.
- How headers get into the right sandboxes: listed explicitly, found by
  convention (for example, everything in `lib/*.h`), or discovered from the
  compiler's own report of what it included.
- How the public/private split between `greet.h` and `internal.h` is
  expressed, if at all.
- How much of this belongs in `gcslib/gcc` and how much in the build file.
  Write the library side too, as you did in example 1, if it helps.
