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
neither capture nor put it back. Two runs of one step at once don't
share it: one holds it by a lock, and the other is given an empty one of
its own, which the contract allows, so that no run waits. A strict build
gives each run an empty one, so a release depends on nothing a scratch
directory kept.

### F2 as built

- **Where:** `<store>/scratch/<digest>`, the digest of the platform and
  the step's directory under `/pekit-out` (its target's and its name), and
  a lock file beside it, held with `File::try_lock` for as long as the
  step runs, through its retries, and touched when taken, for F3's
  eviction to go by. `pekit clean --cache` removes them with the store.
  Keying by the platform as well keeps two architectures' builds from
  thrashing one directory, though either would be correct.
- **An empty one:** under the build's scratch directory, removed when
  the step's run ends.
- **Bound:** at `/pekit-scratch` in the step's sandbox and in the
  sandboxes it makes, and named so to the step's own effects.
- **Asked for only to run:** a step found in the store takes no lock.
- **Tests:** `tests/scratch.rs`.

## F3. Eviction by GreedyDual-Size

The store grows until something removes from it. A store may be as
large as its machine says, `PEKIT_CACHE_LIMIT` (`50GiB` by default, `0`
for no limit), and each build, as it ends, evicts contents until the
store is below nine tenths of it, by GreedyDual-Size.

- **The limit is the machine's.** The store is shared by every workspace
  of a user, so a workspace's `pekitconf` can't say how large it may be:
  two would disagree. (The first design said `pekitconf.local_cache`.)
- **What is evicted is a record's contents, together.** Each record, a
  step's, a program's, an unpacking's or a fetch's, names the blobs and
  trees it holds; a record is *live* while every one is there. Records
  stay when their contents go: a record whose contents are not all there
  is no hit, so its step, program, unpacking or fetch runs again and puts
  them back, and a downstream receipt is still computable. A blob is
  removed once no live record names it. Blobs no record names, a
  workspace's files hashed for a build, say, cost nothing to make again,
  and go first.
- **The order:** each record's priority is `L + cost / size`, set when it
  is used: its cost the time its step, program, unpacking or download
  took, its size its contents', and `L` the priority of the last evicted,
  so that what is cheap to make again and large goes first, and what was
  used lately last. A scratch directory is evicted whole, by the same
  order, its cost nothing, since a step must do without it, and never
  while a run holds it.
- **Used:** a store notes each record found or written, and a build
  appends them to a journal in the store as it ends; the eviction reads
  every journal, sets the priorities, and keeps them, with `L`, in the
  store.
- **Concurrency:** every process holds the store by a shared lock while
  it has it open. An eviction takes it exclusively, without waiting: if
  another process has the store, this build evicts nothing, and a later
  one will.
- **A miss, not a failure:** a step found in the store whose outputs are
  not all there runs again, as does an unpacking whose tree isn't.

### F3 as built

- `evict.rs`. A store notes each record found or written and each
  scratch directory held (`Store::used`); a build with a limit journals
  them to `usage/` as it ends and calls `evict`.
- A record's blobs are found generically: every 64-hex-digit string in
  its JSON that names a blob, and, for a tree, every blob under it. So
  every kind of record weighs the same way, and a string that happens to
  name a blob only keeps it longer. Its cost is its `duration_ms`:
  programs, unpackings and downloads now record one, and a download has
  a record of its own (`fetch.rs`), so a fetched archive isn't taken for
  unreferenced and fetched again.
- Priorities are integers, cost in 2^-20 ms per byte, so no float casts.
- A step record whose outputs aren't all there, and an unpacking whose
  tree isn't, are misses (`holds_all`, `Store::holds_tree`); a program's
  record whose trees aren't is a miss already, as its replay fails.
- Every scan reads every blob's size, and, over the limit, every record:
  fine for a store of thousands of records, and to be kept as a running
  total if stores grow far larger.
- **Tests:** `tests/evict.rs`.

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
- F3: the size from `PEKIT_CACHE_LIMIT`, the machine's, not a
  workspace's `pekitconf`, defaulting to 50 GiB; cost as the time to make;
  a scratch directory as costing nothing.
- F4: what keys a target's evaluation.
- F6: a step runner as REAPI's command.
- F10: SLSA provenance.
