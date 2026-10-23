# Example 10: a whole OS image, many machines

The real Peios loop, at the size it will grow to:

1. The `pkgs/` workspace builds every package: busybox, the kernel, `ppk`,
   prelude, the live-boot root, and in time hundreds more.
2. `peiso` assembles a bootable ISO from those packages.
3. A boot test starts the ISO in QEMU and checks that it comes up.

Three machines share one cache: CI, Jack's laptop, and another developer's.

This example has no sources of its own. It's about the image definition, and
about the machinery that makes a large build fast and safe across machines.

## What must happen

By hand today, with peiso's spec format (see the `peiso` docs):

```sh
pekit package --all                         # 1. every package, into an artifact dir
peiso build peios.toml --out peios.iso      # 2. the ISO, from a TOML spec that names
                                            #    packages, kernel and command line
qemu-system-x86_64 -cdrom peios.iso ...     # 3. boot it and check it comes up
```

| What happens | Must happen | Must *not* happen |
|---|---|---|
| A fresh clone on a laptop, after CI has built `main` | download the finished ISO | build anything, or download any package or intermediate file |
| You change the busybox recipe | busybox's build and package, the ISO, the boot test | any other package |
| You change only the kernel command line in the image definition | the ISO, the boot test | any package |
| `pekit build iso` with nothing changed, in a workspace of 1,000 packages | finish in well under a second | re-read 1,000 recipes |
| A developer's laptop has a broken compiler | that developer's builds fail or differ | anyone else receives those outputs from the shared cache |

## What's new

**1. The image is a step that consumes packages.** Packages come out of the
packagers from example 5, and peiso turns them into an ISO. The spec would
become GCSL, referring to package definitions directly rather than naming them
and taking "the latest". Decided: the image is an ordinary `def build`. General
build systems (Bazel, Buck2, Nix) have no image concept; only OS-specific ones
(Yocto, Buildroot) do. Booting it uses a generic `pekit run`, which runs any
target whose result can be run, and publishing it uses the packager-style
pack and publish split.

**2. Packages from outside the workspace.** An image may include packages from
`pkgs.peios.org` that aren't built here. "The latest version" is a question
about the outside world, so it isn't hermetic. That points to a lock file:
`pekit lock` records the exact versions and hashes chosen, and builds use the
lock. GCSL already plans the same thing for imports.

**3. The remote cache protocol.** Pekit could invent its own protocol, or speak
Bazel's Remote Execution API (REAPI). With REAPI, existing servers work
unchanged: bazel-remote, Buildbarn, BuildBuddy, NativeLink. REAPI has two
parts:

- a content-addressed store for files;
- an action cache, which maps a key to a result.

Our keys fit the action cache as they are. For remote *execution*, though,
REAPI expects a command line, and our steps are GCSL code. One answer is for
the command to be "run this GCSL step", with the GCSL runtime supplied as one
of the inputs.

**4. Trust in a shared cache.** If any machine can write to the shared cache,
one broken or malicious machine can hand bad outputs to everyone under correct
keys. The common answers:

- CI writes, and developers only read;
- or only remote execution writes, because the server ran the step itself.

Release builds might skip the cache, or be checked by rebuilding them
(`--check-reproducible`). Peios signs what it ships, so this matters more than
usual.

**5. Speed when nothing changed.** Re-evaluating 1,000 recipes on every run
gets slow. Bazel keeps a background server that remembers its evaluation and
watches files for changes. Our "evaluation in identifiable units" (point 4 of
the performance review) is what makes the same thing possible. When is it
worth building?

**6. Evaluation on many cores.** GCSL's evaluator is single-threaded. A thousand
recipes can be evaluated by separate evaluators, one per directory, and then
combined.

**7. Disk.** Pekit's local cache and `pekit-out/` grow forever unless something
removes old entries: a size limit, least-recently-used eviction, and
`pekit clean`.

**8. Sandbox overhead.** Thousands of tiny steps each pay to set up a sandbox.
Bazel's answers are reusing sandboxes and keeping tools such as compilers
running between steps, which it calls *persistent workers*.

## What to write

Write `build.gcsl` in this directory: the image definition, which is the GCSL
version of `peios.toml`, and the boot test. The rest is pekit machinery, not
syntax; Claude has leans on each.
