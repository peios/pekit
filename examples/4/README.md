# Example 4: building someone else's code

A program, `app`, uses zlib, the compression library. zlib isn't in the
repository. The build downloads it from zlib.net, checks it against a pinned
hash, builds it with zlib's own build system, and links it into `app`.

This is the kind of work the recipes in `pkgs/` do today.

```
main.c   compresses a string with zlib, uncompresses it and prints it
```

## What must happen

By hand:

```sh
curl -O https://zlib.net/fossils/zlib-1.3.1.tar.gz     # 1. download
sha256sum zlib-1.3.1.tar.gz                            #    and check it is exactly:
# 9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23
tar xzf zlib-1.3.1.tar.gz                              # 2. unpack
cd zlib-1.3.1
./configure --static --prefix=OUT                      # 3. zlib's own build:
make -j                                                #    configure, make,
make install                                           #    install into OUT
cd ..
cc -c -IOUT/include -o main.o main.c                   # 4. compile the program
cc -o app main.o OUT/lib/libz.a                        # 5. link
```

The program then prints:

```
zlib 1.3.1: 56 bytes -> 19 bytes -> "pekit2 pekit2 ..."
```

| What happens | Must rerun | Must *not* rerun |
|---|---|---|
| You change `main.c` | 4, 5 | 1, 2, 3 |
| You change the pinned hash and URL, to upgrade zlib | everything | |
| A fresh machine, with a shared cache that already has zlib | 4, 5 | 1, 2, 3, including the download |
| zlib.net quietly changes the file behind the same URL | *fail loudly* at 1, because the hash no longer matches | never silently build different code |

## What's new

**1. The network.** The only thing allowed to touch the network is `fetch`.
Even that is only allowed because the hash pins the result: the same hash means
the same bytes, whenever and wherever the build runs. Step 3 must not reach
the network. Its sandbox simply has none. One practical question: how do you
find the hash in the first place? For example, you could leave it empty and
have pekit tell you what it found.

**2. Someone else's build system, as one big step.** There are two ways to
build zlib:

- **Fine-grained:** call `gcc.compile` on each of zlib's 15 `.c` files, as in
  example 2. Pekit sees every compile, so changing one file rebuilds one file.
  This is Bazel's way. But you have to reproduce what zlib's own build does.
- **Coarse:** run zlib's own `configure` and `make` as a single hermetic step.
  This is how your `pkgs/` recipes work. It is far less to write and uses
  exactly upstream's build. Pekit can't see inside the step, so any change
  rebuilds all of zlib.

For third-party code that rarely changes, coarse is usually right. Pekit must
support both, and this example should pick one.

**3. Upstream build systems misbehave in a sandbox.** Two things turned up
when this was run by hand:

- `configure` looked for a compiler on `PATH` and wrote `CC=gcc` into the
  Makefile. In pekit, `PATH` is empty, so the toolchain has to be handed to it
  explicitly, for example as `CC=<path to cc in the sandbox>`.
- `make install` wrote the absolute install path into
  `lib/pkgconfig/zlib.pc`. If the sandbox path differs on each run, the output
  differs on each run. That defeats early cutoff and makes the build
  unreproducible. So pekit should give sandboxes a stable path, such as
  always `/work`. This is a pekit decision, not a build-file one, but it is
  worth knowing why.

**4. Parallelism inside a step.** `make -j` runs many compiles inside one step.
If pekit runs 8 such steps at once, each with `-j8`, the machine gets 64 jobs.
Pekit needs to share cores out between steps, as `make`'s jobserver does.
This is also pekit-side, with no syntax needed.

**5. The zlib target returns the same shape as `greet`:** headers and an
archive. So `app` should look just like example 2.

## What to write

Write `build.gcsl` in this directory. Things to decide:

- The zlib target: fetch, unpack and build, either coarse (`configure` and
  `make` inside a hermetic function) or fine-grained (`gcc.compile` per file).
- Where unpacking happens: inside `fetch`, a library function, or a step of
  its own.
- How the toolchain is handed to `configure`.
- What you write for the hash before you know it.
