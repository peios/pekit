---
title: Root Contexts
description: The GCSL contexts pekit binds around every target, where each
  value comes from, and when pekit binds it.
---

pekit evaluates every target as if wrapped in a `with` block that binds
some GCSL contexts (`root_contexts` and `root_fields`). It binds a context
only where the workspace declares it: where some loaded module, the
standard library's included, declares the context. A workspace that never
imports `keychain`, for example, has no `^keychain_profile` to bind.

| Context | Module | Value |
|---|---|---|
| `host_platform` | prelude | The execution platform, as `{arch; os}`. |
| `platform` | prelude | The platform `--platform` names, if given. Otherwise unbound, and its default is `^host_platform`. |
| `toolchains` | `toolchain` | `pekitconf.toolchains` of `workspace.gcsl`, if it has the field. |
| `keychain_keys` | `keychain` | `pekitconf.keychain` of `workspace.gcsl`, if it has the field. |
| `keychain_profile` | `keychain` | The `--profile` option's value, `dev` by default, or `release` in `pekit release`. |
| `packagers` | `packaging` | `pekitconf.packagers` of `workspace.gcsl`, if it has the field. |
| `pinned_libraries` | `libraries` | The libraries `pekit.lock` pins, each as `{commit; dir; git; path}`. |
| `script_session` | prelude | For `pekit run` of a script only: `{args}`, the words after the script's name. |

The fields of `pekitconf` are evaluated as part of `workspace.gcsl`, in
GCSL, so a mistake in one is a check error of that file. A `pekitconf`
without one of these fields leaves the context at its default: the
`toolchain` module's `[Toolchain::C system_c]`, and an empty keychain and
packager list.

## Contexts and keys

Binding a context changes no step that doesn't read it. A step is keyed by
the contexts it read as it ran, as [Receipts](~pekit/receipts) describes,
so registering another toolchain reruns no step that didn't read
`^toolchains`. A library that resolves its toolchain in a parameter default
reads `^toolchains` outside the step, and gives the step the toolchain
itself, which keys it as an argument. `^script_session` has no default, so
a step that could read it is one only a script can reach.
