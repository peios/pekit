# Example 3: a tool built during the build

A program, `app`, prints a message. The message isn't written in C. It lives in
`data/message.txt`, and a small tool, `embed`, turns it into C source during
the build. `embed` is itself built during the same build.

```
gen/embed.c       the tool: turns a file into NAME.h and NAME.c
data/message.txt  the message
main.c            the program; includes message.h, which doesn't exist yet
```

## What must happen

By hand, the build is five commands:

```sh
cc -o embed gen/embed.c                        # 1. build the tool
./embed data/message.txt message out/          # 2. run it: makes out/message.h
                                               #    and out/message.c
cc -c -Iout -o main.o main.c                   # 3. compile the program
cc -c -o message.o out/message.c               # 4. compile the generated code
cc -o app main.o message.o                     # 5. link
```

The order is 1, then 2, then 3 and 4 in parallel, then 5.

| You change | Must rerun | Must *not* rerun |
|---|---|---|
| `main.c` | 3, 5 | 1, 2, 4 |
| `data/message.txt` | 2, 4, 5 | 1; and 3, because `message.h` comes out identical (early cutoff) |
| `gen/embed.c` | 1, 2, and then only what depends on outputs that actually changed | |

## What's new

**1. A step runs a program the build just made.** In examples 1 and 2, every
step ran a program from the toolchain (`cc`, `ar`). Step 2 runs `embed`, which
step 1 produced. In the pekit model this should need nothing special: the tool
is a `File` output of one step, passed into another step like any other input.

**2. A generated header.** In example 2, `g.headers` were files from the
workspace, so they were ready at once. Here `message.h` doesn't exist until
step 2 has run. So `main.c` can't compile until then, and pekit has to work
that out from the build file.

**3. Two machines that happen to be the same machine.** `embed` runs *during
the build*, on the machine doing the build. `app` runs *wherever it's
deployed*. Today they're the same computer, so nothing visibly differs.
Example 6 builds for another architecture, say an ARM `app` on an x86 machine.
Then `embed` must still be compiled for x86, because it runs here, while `app`
is compiled for ARM. Bazel calls these the *exec* and *target* configurations.
You don't need to solve this now. But if the build file can't tell which
compiles are "for the build machine", example 6 means rewriting it.

## What to write

Write `build.gcsl` in this directory. Things to decide, though any answer is
fine:

- How the `embed` tool target is written, and how step 2 runs it: a hermetic
  function in the build file, or in a library.
- What step 2 returns, and how `main` gets both the generated header and the
  generated `.c` file.
- Whether anything marks `embed` as "runs on the build machine". Even a
  comment saying where that would go is enough for now.
