# pekit2 implementation plan

This is the plan for implementing pekit2 and the GCSL changes it needs. The
design is in `core-model.md` and `examples/`, and the decisions behind it are
on Acta task PEI-1594, and implementation is tracked on PEI-1595.

## Priorities (Jack, 2026-10-24)

1. **Correctness above everything.** No code written just to get something
   working at the expense of correctness, robustness or a sound
   architecture. If a component needs days to be solid, powerful and
   efficient, it gets days.
2. **The goal is total implementation.** Everything designed gets built.
3. **Build horizontally, foundations first.** Each layer is completed
   properly before the next depends on it. Do not make vertical slices to
   get something vaguely functional early. Additive features, such as remote
   caching, the daemon and the web view, come after the foundations, layered
   so that stopping part way through still leaves a functional system. As a
   guide, the system should be usable at about 60%: when the language, the
   engine, the libraries and the commands are in.
4. **Never block on Jack.** Try the correct approach first. If it needs Jack,
   take the highest-quality route that doesn't, or work on something else,
   and record what was decided and why on Acta. For example, a pinned build
   environment is preferred, with the host snapshot as the fallback; a
   pinned standalone Python is preferred, with the host `python3` where a
   test needs it.

## Ground rules

- **Repositories:**
  - pekit2 is a new repository, worked on directly on `main` (Jack).
  - GCSL changes went on a branch, `feat/1595-pekit2`, in a worktree of
    `gcsl` at `local/worktrees/gcsl-1595/`, made from `main` at `72edfde`.
    Jack had it merged into GCSL's `main` at `185a924` on 2026-10-26, and
    pekit2 now builds against the `gcsl` checkout itself. Further GCSL work
    goes on a new branch in a worktree of its own.
  - Never push, never rebase, and never merge a GCSL branch into its `main`
    unless Jack says to. Jack reviews and merges.
- **Leave `pekit` and `pkgs` alone.** Both are on `feat/1593-modsig`. Today's
  pekit keeps serving `pkgs/` until pekit2 replaces it.
- **Leave `examples/*/build.gcsl` alone.** They are Jack's design drafts.
  Runnable versions live under `tests/fixtures/`.
- **Follow PSC.** Conventional Commits with `Refs: PEI-…` footers, and the
  coding style for the language concerned.
- **Document behaviour as it is built:**
  - GCSL changes go in `gcsl/docs/learn/`;
  - pekit2 gets its own `docs/learn/` tree, joined to the site later.

  Nothing is published.
- **Record progress on Acta** at each meaningful milestone: what works, how
  it was tested, and what is left.

## Facts checked before starting

- **Sandboxing:** raw `unshare --user` is blocked by Ubuntu's AppArmor
  restriction (`kernel.apparmor_restrict_unprivileged_userns = 1`). `bwrap`
  works, including overlays and `--unshare-all` with no network. Pekit's
  sandbox therefore uses bubblewrap.
- **Toolchains:** Bootlin publishes prebuilt, relocatable musl toolchains
  with SHA-256 hashes, for x86-64
  (`x86-64--musl--stable-2025.08-1.tar.xz`, 82 MB,
  `09fca3aa89540f1b01b5f4210d488cbeb00f522044c53e9989b1dd8a38076912`) and
  for x86-64→aarch64.
- **Host tools present:** `qemu-system-x86_64`, `python3`, `gcc` 15, `cargo`
  and `bwrap`.

## Layers

Each layer is designed in detail, built completely, tested thoroughly and
documented before the next one builds on it. Within a layer, work proceeds in
whatever order is soundest.

### A. GCSL: the language

Everything the design added to the language:

- **Names and imports:**
  - slash-path imports;
  - def-type definitions scoped by directory;
  - wildcard def-type imports (`import package * from *`, `**`).
- **Methods:**
  - receiver methods (`def (x: T) name(...)`), resolved by static type;
  - the visibility and conflict rules;
  - moving the prelude to methods.
- **Calls:**
  - non-constant defaults, evaluated at the call site;
  - the bare named-argument shorthand.
- **Context:** `context`, `^`, `with`, and inferred requirements.
- **Types:**
  - generic functions and def-types;
  - extensible enums with bounded payloads;
  - `Partial<T>`;
  - persistent lists with cheap concatenation;
  - duration and size literals.
- **Attributes:** host-declared, with typed arguments, passed through to the
  host, allowed on `fn` and `def`, and with def-type defaults that a
  definition can override.
- **Failure:** `recover` and `assert`.
- **Secrets:** `Secret<T>`, contagious, with `reveal()` returning `Revealed<T>`
  and `unsafe_value()`.
- **Files:** `File`, `Dir`, `Sandbox` and `FileSet`, and `exec` with `cwd`,
  `env`, `until` and `interactive`.

### B. GCSL: the action-graph evaluator and embedding API

- **The evaluation mode:**
  - pure code folds immediately;
  - impure calls become nodes, with data and order edges and their
    attributes;
  - a function pauses where a result is first used and resumes later, and
    the graph grows as results arrive.
- **The host interface:**
  - the host supplies a node's value, or asks GCSL to evaluate the node in
    an effect context that routes `exec` and `fs`;
  - context values are captured in a node's identity;
  - evaluation is organised in identifiable units, so that results can be
    cached later.

### C. pekit2: the engine

- **Workspace loading and target resolution.**
- **The content store and step records:**
  - receipts are the function identity plus the content hashes of the
    arguments, plus the identities of any secrets;
  - early cutoff;
  - records are kept separately from contents.
- **The scheduler:** a build graph derived from the action graph, opt-in
  parallelism, program order for nodes that are not hermetic, and a pool of
  CPU slots shared with make's jobserver.
- **The bubblewrap sandbox:**
  - stable paths and no network;
  - inputs mounted, and outputs captured from the returned `File`s;
  - `Dir.sandbox` implemented with overlays.
- **Execution platform selection,** cache-aware in development and fixed in
  strict mode.
- **The event stream and Chrome trace output.**

### D. Libraries

- **Stdlib modules:**
  - `toolchain`, with its kinds and registry;
  - `keychain`;
  - `archive`;
  - `fs.compose_dir`.
- **`gcslib` libraries:**
  - `gcc`;
  - the `ppk` packager;
  - `python`;
  - `peiso`;
  - `qemu`.
- **Pinned toolchains:**
  - the Bootlin x86-64 and aarch64 toolchains;
  - a build environment providing `sh` and `make`;
  - Python.

### E. Commands

`build`, `test`, `package`, `publish`, `release`, `run` (with `def script`),
`lock`, `query` and `clean`, with the shared flags. That covers
`--platform`, `--rebuild`, `--strict`, `--check-reproducible`,
`--no-dynamic`, `--jobs` and `--trace`.

With layers A to E done, every example from 1 to 10 runs as a test fixture.
This is the point at which the system is functional.

### F. Additive features

These come after the functional system, roughly in this order, as
`design/additive.md` designs them:

1. per-`exec` caching;
2. `#[scratch]`;
3. GreedyDual-Size eviction;
4. the on-disk evaluation cache;
5. the remote cache over REAPI;
6. remote execution;
7. many-core evaluation;
8. the daemon;
9. the web view and its trace views;
10. release provenance records.

## Done

Everything above is implemented, tested and documented, examples 1 to 10
run as fixtures, and Acta records what was built, how it was verified, and
every place the implementation departs from the design.
