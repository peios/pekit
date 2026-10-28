---
title: Terminology
description: The terms this manual uses with a meaning of pekit's own.
---

The terms in this manual have these meanings. GCSL's own terms, such as
context, effect, and definition type, have the meanings GCSL's
documentation gives them.

| Term | Meaning |
|---|---|
| Blob | A file's contents in the store, named by their SHA-256 digest. |
| Composition | A directory a step makes with `fs.compose` or `fs.compose_dir`, sealed once made. |
| Effect | A built-in call that touches the world, such as `exec` or `fs.write`, run as a job on the pool. |
| Execution platform | The machine steps run on: the machine pekit runs on, named as `<arch>-<os>`. |
| Exposure | Writing an output into `pekit-out/` on the host, once something that is not hermetic might read it there. |
| Host snapshot | The host's system directories that steps see outside a strict build, and the fingerprint that identifies them. |
| Host view | The effect context of a target's own code: the host, with the logical layout mapped onto it. |
| Identity | A 64-bit digest of what a step is, apart from its inputs' contents, naming its output directory. |
| Logical layout | The paths every GCSL value and sandbox uses: `/w` for the workspace, `/pekit-out` for outputs. |
| Manifest | For a step's base receipt, every set of contexts a run of it has read. |
| Materialized tree | A store tree made once as a directory of links to its blobs, for steps to mount. |
| Pool | The threads, `--jobs` of them, that run effects and pekit's own work for steps. |
| Program record | The record of one program a step ran, keyed by what the program could see. |
| Receipt | The SHA-256 key of a step: everything that can change what it gives. |
| Record | What a step gave, kept in the store under its receipt. |
| Scratch directory | A directory kept between a step's runs for a step marked `#[scratch]`. |
| Staging directory | The directory on the host that a step's sandbox is made of. |
| Step | A call of a hermetic function made by a target's code, run in a sandbox and recorded. |
| Store | The content-addressed store under the cache directory, shared by every workspace and process on the machine. |
| Target | A definition named on the command line, evaluated as a unit. |
| Token | The 16 random hexadecimal digits a process names its temporary files and directories with. |
| Tree | A directory in the store, kept as a blob of its sorted entries in a canonical encoding. |
| Unit | A GCSL evaluation that stops at each result it needs and runs again from there: a target's, or a step's body. |
