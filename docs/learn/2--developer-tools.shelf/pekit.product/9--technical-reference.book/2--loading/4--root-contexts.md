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

`toolchains`, `keychain`, `packagers`, and `contexts` are the only fields
pekit reads. Before it prepares any target, pekit fails the build if:

- `pekitconf` is not a record of named fields;
- it has any other field, which a different pekit might read but this one
  would ignore; or
- it has `toolchains`, `keychain`, or `packagers` but no loaded file
  declares the context the field sets, which `import toolchain`, `import
  keychain`, or `import packaging` would.

## Contexts the workspace sets

`pekitconf.contexts` sets any other context, each of its fields the context
of its name, as [Setting a Context for the Whole
Workspace](~pekit/setting-a-context-for-the-workspace) shows. pekit binds
them in the same `with` as the fields above, so a target's own `with`
binds over them. Before it prepares any target, pekit fails the build if:

- `contexts` is not a record whose fields name contexts, such as a record
  type with a rest-field;
- a field names a context that no loaded file declares; or
- a field names one of the contexts in the table above, which pekit sets
  itself.

GCSL checks each value against its context's type, as it checks a `with`
binding, so a value of another type is a check error. Each value is
evaluated as part of `workspace.gcsl`, in the declared defaults of every
context, so it can't depend on a target's `^platform`.

## Contexts and keys

Binding a context changes no step that doesn't read it. A step is keyed by
the contexts it read as it ran, as [Receipts](~pekit/receipts) describes,
so registering another toolchain reruns no step that didn't read
`^toolchains`. A library that resolves its toolchain in a parameter default
reads `^toolchains` outside the step, and gives the step the toolchain
itself, which keys it as an argument. `^script_session` has no default, so
a step that could read it is one only a script can reach.
