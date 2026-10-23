# Example 5: shipping the outputs as ppk packages

This takes example 2's outputs and ships them as two ppk packages:

- `dev.example.greet`, the program, installed as `/usr/bin/greet`;
- `dev.example.greet-dev`, for developers who want to use the library:
  `/usr/include/greet.h` and `/usr/lib/libgreet.a`.

One build makes two packages. These are *split packages*, as in the kernel
recipe in `pkgs/`. This example contains a copy of example 2's sources and
`build.gcsl`. The packages are in `package.gcsl`, beside it.

## What must happen

By hand, after building example 2:

```sh
mkdir -p greet/bin greet-dev/include greet-dev/lib       # 1. stage each package's
cp app greet/bin/greet                                   #    files, laid out as
cp lib/greet.h greet-dev/include/                        #    they will be under
cp libgreet.a greet-dev/lib/                             #    /usr

ppk pack 'dev.example.greet@1.0-1?arch=x86_64' greet \
    --artifact-dir artifacts                             # 2. pack each one
ppk pack 'dev.example.greet-dev@1.0-1?arch=x86_64' greet-dev \
    --artifact-dir artifacts
```

This was run by hand. Packing is deterministic: the same files packed into a
fresh artifact directory give the same identity
(`sha256:7b27eb46…`). So a pack can be cached like any other step.

| You change | Must rerun | Must *not* rerun |
|---|---|---|
| `main.c` | `app`'s compile and link, then the `greet` pack | the `greet-dev` pack, because its files didn't change |
| `lib/greet.c` | everything, then both packs | |
| only the package version | both packs | every compile and link |
| a comment in `main.c`, giving an identical `app` | `app`'s compile | everything after it, through early cutoff |

## What's new

**1. Packing is a step; publishing isn't.** `ppk pack` writes into an artifact
directory, a store shared with everything else that writes there. A hermetic
step can't write into a store outside its sandbox. So:

- The pack step packs into a fresh, empty artifact directory in its own
  sandbox and returns it as its output. That output is cached and
  deterministic.
- Copying it into a real store, such as yours, the system's or
  `pkgs.peios.org`, is a separate action that changes the world outside the
  build.

The copy itself is easy, because blobs and manifests are content-addressed.
The exception is refs: ppk refuses a version whose content has changed.

The open question is what command does that copy, and how it's written. Bazel
separates `bazel build`, which never touches the outside world, from
`bazel run`, which does something with the result. Here it might be a
different def-type, such as `def publish`, run with something like
`pekit publish`.

**2. Staging is a map from `/usr` paths to files.** That's the same shape as
`fs.compose_dir` from example 4, so it may be the same function.

**3. Versions.** ppk never lets a version change its content, but pekit
rebuilds whenever an input changes. If you edit `main.c` and rebuild without
changing the version, the pack step produces a different package with the
same version. That's fine in `pekit-out/`, but publishing it would be refused.
Today's pekit handles this by distinguishing `release` from development builds
and marking the latter `+dirty`. Where should the version of a development
build come from?

**4. ppk is a library, not part of pekit.** Something like `gcslib/ppk` wraps
`ppk pack` in a hermetic function. The `ppk` tool is an input `FileSet`, like
the compiler. This example tests that nothing here needs pekit to know what
ppk is.

**5. Using the build's targets.** The canonical layout (Jack) is a
`build.gcsl` and a `package.gcsl` in the same directory. Files in one
directory see each other's definitions, so `package.gcsl` can write
`build main()` with no import.

**6. On pekit's side: the executable bit.** ppk records which files are
executable. You can see it in the artifact directory, where blob names end in
`.exec`. So a `File`'s fingerprint must include whether it's executable, not
just its bytes. Bazel's remote cache protocol does the same. No syntax is
needed.

The architecture is fixed as `x86_64` for now. Example 6 is about that.

## What to write

Write `package.gcsl` in this directory. Things to decide:

- How a package is written: its name, version and files.
- The pack step, if you want to write the library side.
- How publishing is written and triggered, or leave it out and just say how
  you'd want it.
