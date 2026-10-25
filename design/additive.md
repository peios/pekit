# Layer F: the additive features

Layers A to E make pekit2 a working build system: every example runs.
Layer F makes it faster, shareable and observable. Each feature here is
additive: built on what is there, changing no result, so that stopping
after any one leaves a working system. They come in the order Jack gave
(`PLAN.md`). This note decides what each is; each gains its own as-built
notes as it lands.

## F1. Per-`exec` caching

A step that runs again, because its code changed say, reruns every
program it runs, though most see what they saw before: a step's code
identity is the source of every file its code needs, so editing a
library's comment reruns every compile it makes. A step's programs are
therefore cached each on its own.

- **Which:** each `exec` a step runs, its kin `exec_live`, and each
  `Sandbox.exec`. None after the step's sandbox binds a secret, at its start
  or when a read binds one, since nothing a secret touches is stored. A
  program is replayed only where steps are reused: not with `--rebuild`,
  which records it, nor in `--check-reproducible`'s second build, which
  neither replays nor records.
- **What a program can see:** a step's world is what its sandbox holds:
  its inputs, staged at its start, by a read or by an unpacking, the
  compositions it made, and what it can write, its own output directory,
  every sandbox's upper layer within it, and its `/tmp`.
- **Key:** the platform, as a step's receipt names it, with the host
  snapshot's fingerprint; the built-in, the program's words, working
  directory and environment, and `exec`'s options; and the world: every
  staged input by path and digest, the digests of the own directory's
  tree and of `/tmp`'s as they are before it runs, and the *view*, a chain
  folding in each effect the step had run. A program's arguments name
  files by path alone: their contents are the world's. A device is speed,
  not sight, and keys nothing, and a `#[scratch]` directory keys nothing
  either, by F2's contract, nor is it restored.
- **The view:** every effect of a step that is not a program nor one of
  those that only read, `fs.exists`, `fs.read`, `File.read`, `Dir.files`
  and `which`, folds into the view its built-in, its arguments by path,
  and what it staged by contents, as an unpacking stages. Effects run one
  at a time in a step's unit, so the view is a function of what came
  before. What such an effect wrote to the writable trees is not folded:
  it is hashed with them before the next program, since the digests the
  engine knew are forgotten when any effect but one that only reads runs.
- **Record:** its status, output and error, and the digests of the own
  directory's and `/tmp`'s trees after it, their contents in the store.
- **Replay:** a program found is not run: each writable tree whose digest
  differs from its record's is placed again from the store, whole, so the
  world is exactly as the recorded run left it, as the store keeps a tree:
  contents, links and whether a file is executable. Its result is given.
  A record whose contents are not all in the store is no hit.
- **Trust:** a replayed program leaves the world as its recorded run did,
  which holds as a step's record holds: a program's result and writes are
  a function of what it is given and can see. As the store keeps a tree,
  a replay puts back contents, links and executable bits, and not
  modification times or which files were hard links of one another, as a
  step's outputs are put back: a `make` after a replay sees every file
  new. Sealed compositions are sealed again, read-only as a run leaves
  them.
- **What can't be captured:** an entry that is neither a file, a directory
  nor a link, as an overlay's whiteout is, or one an overlay's attributes
  mark, as an opaque directory is, stops the step's programs being cached
  from then on. Attributes that only say where an entry came from
  (`origin`, `impure`) or which layer it is (`uuid`) are dropped from a
  copy harmlessly.

### F1 as built

- **Where the step writes:** the view starts from the step's own output
  directory, where its programs run, since a compiler names its working
  directory in what it writes: two targets' steps of one function and
  input find each other's programs no more than each other's results.
- **Where the work is done:** the engine keeps each step's view and the
  writable trees' digests while it knows them (`engine/programs.rs`). A
  program is sent to the pool as a `Work::Program`, and the thread that
  takes it hashes the trees if the engine did not know them, makes the
  key, finds the record and replays it, or runs the program, hashes again
  and records (`programs.rs`). No tree is hashed or placed on the engine's
  thread, which would serialise a build of many steps.
- **Hashing again cheaply:** a step's sandbox keeps a *stamp* of each file
  of its writable trees as it was last put into the store: device, inode,
  size, mode, and modification and change times (`store.rs`, "Trees seen
  before"). A file whose stamp is the same is not read again. A stamp is
  trusted only if its change time is before the put that took it began,
  by the clock of the tree's own file system, as git trusts its index, so
  a file changed in the same tick is read again. A file that is a hard link to a
  blob this process linked, by device and inode, is that blob, and not
  read. A file read is hashed before it is copied, and copied only if its
  blob is not there.
- **Sealed compositions:** a toolchain's composition in a compile step's
  own directory is some 14,000 files. Programs see a sealed composition
  read-only, so its digest is kept once walked, until the step runs an
  effect that can write, which could add to its directories, or a replay
  puts it back as copies.
- **Cost:** cold, the pinned-toolchain example took 42 s against 36 s
  without per-program caching, in the tests' unoptimised build, where a
  walk of a composition is about five times as slow as in a release
  build (50 ms for 8,400 entries). A build whose steps' code changed and
  whose programs see what they saw runs none of them.
- **Counted:** a build says how many programs its steps found, `1 step
  run (2 programs found), 0 found in the store`, and tells an
  `EffectFound` event for each, which the trace shows.
- **Tests:** `tests/programs.rs`.

## F2. `#[scratch]`

A step marked `#[scratch]` is given a directory of its own, kept between
its runs, at `/pekit-scratch`, for a tool that does less on a second run,
such as Kbuild's or Cargo's own state. It is the step's, by its definition's
directory and its name, whatever its inputs, and it keys nothing: a step
must give what it would give with an empty one, a contract it keeps, as
Bazel's persistent workers keep theirs. It is bound from
outside the step's own directory and its `/tmp`, so that F1's programs
neither capture nor put it back. Two builds running one step at once
take turns with it, by a lock. A strict build gives each run an empty one,
so a release depends on nothing a scratch directory kept.

## F3. Eviction by GreedyDual-Size

The store grows until something removes from it. A workspace says how
large its store may be, `pekitconf.local_cache = {max_size = 50GiB}`, and
each build, as it ends, evicts contents until the store fits, by
GreedyDual-Size: each blob's priority is `L + cost / size`, its cost the
time it took to make, the duration of the step or fetch that gave it, and
`L` the priority of the last evicted, so that what is cheap to make again
and large goes first, and what was used lately last. Records stay: a
record whose contents are gone is no hit, so its step runs again and puts
them back, and a downstream receipt is still computable. Scratch
directories are evicted whole, least recently used first. `pekit clean
--cache` still removes everything.

## F4. The on-disk evaluation cache

Evaluating every recipe of a large workspace on every build is slow when
nothing changed. A target's evaluation is cached on disk: its value and
the steps it reached, keyed by its definition, the digests of every file
its code needs, its root contexts, and the digest of every workspace file
its own effects read. A build whose key matches gives the value at once,
after finding each step it reached in the store, and evaluates nothing. A
target that waited for a step's result is cached with that result in its
key. Workspace files are hashed through a cache of their size, change time
and inode, so that an unchanged workspace is stat'ed, not read.

## F5. The remote cache over REAPI

Bazel's Remote Execution API is spoken, so existing servers work:
bazel-remote, Buildbarn, BuildBuddy, NativeLink. A step's record is an
action result, its receipt the action's digest, and its outputs and the
store's blobs the content-addressed store's. Manifests are action results
too, by their keys. A build finds a step locally first, then remotely,
and downloads only what it needs. Who may write is the workspace's
configuration: developers read, `upload = false`; CI writes, with
credentials of its own. A release checks what it found by rebuilding
(`--check-reproducible`).

## F6. Remote execution

REAPI's execution service runs a command, and a step is GCSL code. A step
is sent as a command running pekit's step runner, given the step's inputs
as plain data and its code's files as inputs, with the GCSL runtime among
them, so that a server runs it as any action. Only hermetic steps go; a
step's own programs run where the step runs.

## F7. Evaluation on many cores

GCSL evaluates on one thread. A build of many targets evaluates them in
several workspaces at once, each on a thread of its own, sharing the store
and the pool its programs run on, and each step is run once, by the first
to reach it, as the store's record of it says to the others.

## F8. The daemon

A daemon keeps a workspace loaded and its evaluation cache in memory, and
watches the workspace's files, so that a build with nothing changed
re-reads nothing. `pekit` talks to it over a socket in the state directory,
and runs alone when there is none. A daemon is per workspace and exits
when idle. What a store remembers for the life of a process, the blobs it
linked files to (F1), is bounded for a daemon's, by forgetting it after
each build.

## F9. The web view

`pekit view` serves a page of a build's events: its steps on a timeline,
as the Chrome trace shows them, what each read and gave, and why each ran
or was found, from the events and the store.

## F10. Release provenance

A release writes a provenance record beside what it publishes: the commit,
the lock, the toolchains, each step's receipt, and the digest of what was
published, in the SLSA provenance format, so that anyone can check what a
release was built from.

## Decisions taken without Jack (to surface)

- F1: secrets keep a program out of the per-`exec` cache; a replayed
  program puts back the whole writable world as recorded.
- F2: `/pekit-scratch`, a step's by its name, and keying nothing.
- F3: the size from `pekitconf.local_cache`, and cost as the time to make.
- F4: what keys a target's evaluation.
- F6: a step runner as REAPI's command.
- F10: SLSA provenance.
