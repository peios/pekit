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
  the step's directory under `/pekit-out` (`.steps/<function>/<identity>`), and
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
- **Cheap within the limit:** a build stats every blob, and reads each
  scratch directory's size from the `.size` file its last run wrote as
  it let go of it; if the store is within its limit, nothing more is read
  and the journals wait. Only an eviction that does something reads every
  record, and a blob is parsed as a tree only if its first bytes are a
  tree's, so a fetched archive is never read through to find it isn't.
  The stat of every blob is to become a running total if stores grow far
  larger.

  *Changed 2026-10-27 (PEI-1598).* It has: the stat of every blob was
  about 38,000 stats in each of authd's no-change builds. The store's
  `size` file keeps its blobs' size as an eviction last measured it, a
  day ago at most, with the bytes of the blobs each build wrote since
  added as it ends, under a lock of its own. The blobs are walked only
  when that, which can only be too large, is over the limit, or is a day
  old, or is not there. A build that is killed before it ends adds
  nothing, which is why a measurement is trusted for a day only.
- **Trees known complete:** a record whose contents were evicted is no
  hit, so a hit checks every blob of its trees. A tree found complete is
  marked in `complete/`, so the walk is once, not in every build, and an
  eviction removes every mark before it removes a blob.
- A lock that fails to become exclusive has been let go of, as `flock`
  changes locks, so the shared lock is taken again either way.
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

### F4, first part, as built: workspace files by their stamps

- A build hashes each workspace file a step is given through the stamps
  F1's per-program caching keeps (`Seen`), kept between builds in the
  store, `seen/<digest of the workspace's path>.json`. A file whose stamp,
  device, inode, size, mode and modification and change times, is as it
  was is not read, nor copied into the store again; its blob must still
  be there, so an eviction makes it read.
- One clock probe per build, a file made and removed in the workspace's
  root as the build begins, says when it began by the workspace's own file
  system's clock; a stamp is kept only for a file last changed before it.
  A file changed afterwards, even to the same size with its modification
  time put back, has a later change time, so it is read again.
- As a build ends, the stamps of files no longer there are dropped.
- **Tests:** `tests/stamps.rs`.

### F4, second part: measured, and deferred

Before caching evaluation, its cost was measured on the pinned-toolchain
zlib example (example 4: two Bootlin toolchains, buildenv, zlib), with a
release build, the store warm:

| | Before | After |
|---|---|---|
| `pekit query deps app` (evaluation, no step run) | 0.76 s | 0.22 s |
| `pekit build app`, nothing changed | | 0.41 s |

The 0.76 s was not GCSL's: it was the store re-exposing each unpacked
toolchain under `pekit-out/.unpack` on every build, removing and linking
again some 14,000 files, and checking every unpacked tree's contents
were there by reading each of its 5,500 tree blobs. Now:

- an exposed tree that already is the tree, every file the first of its
  blob a link to it and any other a read-only copy of its contents, each
  directory and symbolic link as the tree has it and nothing more, is
  left as it is (`Store::is_materialised`): a link can't change without
  its read-only blob, a file put in its place is another, and a copy is
  read again;
- a process reads each tree once, and checks each tree complete once,
  since nothing is evicted while it has the store.

What is left of a build that changes nothing is mostly the `stat`s of
checking those trees. GCSL's own evaluation is a small part of 0.2 s.

**Decision (to surface):** the on-disk evaluation cache is deferred.
There is no measured cost for it to remove, and a cache that keys
evaluation must be right the first time. Its sound key is known, should
it be needed: the target, its platform and evaluation settings, the
digests of its code's files, its root contexts with their files, every
workspace path its evaluation resolved by digest or absence, each fetch
pin and unpacking it reached, and each step it reached by receipt and the
result digest it saw, checked present and equal on a hit; never a target
whose own code runs `exec`, `exec_live` or `which`. Evaluation kept in
memory is the daemon's (F8), as Bazel keeps its analysis in its server.
The Peios port, which is the remaining core and the first real-scale
workspace, comes next, and will say whether evaluation needs caching.

## F5. The remote cache over REAPI

Bazel's Remote Execution API is spoken, over gRPC, so existing servers
work: bazel-remote, Buildbarn, BuildBuddy, NativeLink. A build finds a
record locally first, then remotely, and downloads only what it needs.
Who may write is a machine's or CI's configuration: developers read, CI
writes, with credentials of its own. A release checks what it found by
rebuilding (`--check-reproducible`).

- **Contents are blobs as they are.** pekit's blobs are SHA-256 digests of
  their contents, as REAPI's CAS's are, so a file is uploaded as it is.
- **A record is an action result,** keyed by an *action blob*:
  `pekit-action-1\0<key hex>`, uploaded too, its digest and real size the
  action-cache key, as Bazel keys an action by its `Action` proto's. So
  every key has a true size, and a server that checks the action exists is
  satisfied. Step records, manifests, unpackings and downloads are keyed
  so, each by its local key.
- **Sizes come with the result.** REAPI names a blob by its digest and its
  size, and a pekit tree names a directory by digest alone. So an action
  result declares the record blob in `output_files`, with each file the
  record names, and each tree the record names in `output_directories`, as
  a REAPI `Tree` proto made from the pekit tree: a `FileNode` for each
  file, with its digest, size and whether it is executable, a
  `DirectoryNode` for each directory, a `SymlinkNode` for each link. On a
  hit the result gives every blob's digest and size; the pekit trees are
  made again locally from the `Tree` protos, their digests the same since
  their entries are. A server that checks a result's outputs are all there
  (Buildbarn) or keeps what a hit touches (bazel-remote's LRU) does so for
  the whole of it.
- **Off the engine's thread.** A step missed locally is looked up remotely
  as pool work, the step waiting as it waits for an effect; found, its
  contents are put into the local store and it is taken as found locally.
  Uploads are pool work after a step is recorded; the build's end waits for
  them, and a failed upload is a warning, never a failed build.
- **Only where records are reused** (`Reuse::Store`): a host whose packages
  can't be told apart neither reads nor writes remotely, as it reads no
  local record.
- **Manifests merge.** A manifest uploaded is read, merged and written
  again; a lost race loses a read-set, which only costs a miss.
- **Program records stay local** at first: each one's trees are a step's
  whole writable world, a toolchain's 14,000 files say. Uploading them is a
  later flag.
- **Configuration.** The cache's URL and its `instance_name`, which
  Buildbarn needs, are a workspace's team's: `pekitconf.remote_cache =
  {url; instance_name}` as a default, which the environment and flags
  override (`PEKIT_REMOTE_CACHE`, `--remote-cache`). Whether to upload, and
  credentials, as headers, come from the environment or flags alone, never
  the workspace's files.
- **The client:** tonic and prost, the REAPI protos and those they import
  vendored with their licence and compiled by `protox`, so pekit's build
  needs no `protoc`; one tokio runtime per process, blocked on from pool
  threads; `GetCapabilities` first, for the batch limit, ByteStream above
  it; no compression.
- **Tests** run a pinned bazel-remote on a free port: build with upload,
  empty the local store, build again and find every step remotely, none
  run; remove a blob from the server and build again, which runs that step
  rather than failing.
- **Order, each a place to stop:** the client and the read path for step
  records; uploads; manifests, unpackings and downloads; programs behind a
  flag.

### F5 as built: the client, step records and manifests, uploads

- **The client** (`remote.rs`): `Remote::find` and `Remote::upload` by a
  record's key, as designed above. Every blob downloaded is checked against
  its digest, and every tree is made again from its `Tree` and checked
  against the digest its path names. The record is written last.
- **The protos are a crate of their own,** `crates/pekit-reapi`, which
  vendors REAPI v2.12.0 and the googleapis files it imports, with their
  licences and sources (`proto/README.md`). *Departure:* the design put
  them in pekit. Generated code doesn't meet the workspace's pedantic lints
  or missing-docs, and `#[allow]` isn't used, so the generated code is
  compiled in a crate that doesn't opt into them. Its comments are blanked
  before generation, so it passes Clippy's defaults.
- **Steps are looked up on the pool** (`engine::looked_remotely`): the
  manifest, then the receipts the merged manifest gives, then the store
  again. A found step counts as found in the store and as found remotely:
  `N found in the store (M from the remote cache)`.
- **Manifests merge on reading too.** *Departure:* the design merged them
  on upload. A remote manifest that replaced the store's would lose the
  store's sets, so a manifest fetched is merged into the store's, never
  written over it.
- **Uploads** follow each step recorded with `Reuse::Store`, as pool work.
  The cache's manifest is merged first, then the record and the manifest
  are written, sending only the blobs `FindMissingBlobs` names. The build
  waits for its uploads before it evicts.
- **A cache that is down is asked nothing more.** *Decision:* once a call
  fails, the process builds as if it had no cache, and warns once as the
  build ends, so a cache that is down costs one failed call, not one per
  step. Connecting has 10 seconds. A missing or wrong blob fails that
  lookup only.
- **Configuration** is `--remote-cache`, `--remote-instance`,
  `--remote-upload` and `--remote-header`, and `PEKIT_REMOTE_CACHE`,
  `PEKIT_REMOTE_INSTANCE`, `PEKIT_REMOTE_UPLOAD` and
  `PEKIT_REMOTE_HEADERS`, one header to a line. `grpcs://` uses TLS with
  the system's certificate authorities.
- **Tests** (`tests/remote.rs`, `tests/cli.rs`) run an in-memory REAPI
  server of the tests' own (`tests/cache/mod.rs`). It checks each blob
  against its digest and refuses a result whose blobs are missing, as
  Buildbarn does. *Departure:* the design named a pinned bazel-remote.
  Running a downloaded binary was refused in this session, so the test
  against a real server is still to write, opt-in by a path to the binary.

**Still to do:** `pekitconf.remote_cache` as the team's default, which
needs the prelude's `pekitconf` type to take it. Unpackings and downloads
are looked up remotely too. Program records stay behind a flag. The opt-in
test runs against bazel-remote.

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
- F5: an action blob as a record's action-cache key; trees as REAPI
  `Tree` protos in a result; program records local at first; the URL and
  instance from `pekitconf` with the environment overriding, uploads and
  credentials from the environment alone. As built: the protos in a crate
  of their own; manifests merged on reading as well as uploading; a cache
  that fails a call asked nothing more by the process.
- F6: a step runner as REAPI's command.
- F10: SLSA provenance.
