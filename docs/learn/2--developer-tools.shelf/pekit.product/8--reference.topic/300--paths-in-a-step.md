---
title: Paths in a Step
type: reference
description: Every path a step's sandbox has, what each holds, whether the
  step can write it, and when it's there.
related:
  - pekit/concepts/step-sandboxes
  - pekit/concepts/where-results-go
---

A step's sandbox has the same layout on every machine and in every
checkout. This table lists each path a step can have, in the order a step
is likely to use them. [Step Sandboxes](~pekit/step-sandboxes) explains
why the layout is as it is.

| Path | Holds | Writable | There when |
|---|---|---|---|
| `/w/<path>` | A workspace file or directory the step was given, at its path in the workspace. | No | The step was given it. |
| `/w/.gcsl/libraries/<library>/` | A library's own files the step was given. | No | The step was given one. |
| `/pekit-out/.steps/<function>/<identity>/` | The step's own directory, where its programs start and its outputs go. | Yes | Always. |
| `/pekit-out/<path>` | Another step's output the step was given. | No | The step was given it. |
| `/pekit-out/.fetch/<sha256>/<name>` | A fetched file the step was given. | No | The step was given it. |
| `/pekit-out/.unpack/<key>/` | An unpacked archive the step was given. | No | The step was given it. |
| `/pekit-out/.git/<key>/<name>/` | A git commit's files the step was given. | No | The step was given them. |
| `/tmp` | An empty directory of the step's own. | Yes | Always. |
| `/pekit-scratch` | A directory pekit keeps between the step's runs. | Yes | The step is marked `#[scratch]`. |
| `/pekit-secrets/<profile>/<name>` | A secret file. | No | The step was given the secret. |
| `/usr`, `/bin`, `/lib`, `/lib64`, `/sbin` | The host's own programs and libraries. | No | The build isn't strict. |
| `/etc/alternatives`, `/etc/ld.so.cache` | What the host's programs need to start. | No | The build isn't strict. |
| `/dev` | `null`, `zero`, `full`, `random`, `urandom`, `tty`, and the standard streams. | Yes, as devices allow | Always. |
| `/dev/kvm` | The machine's KVM device. | Yes | The step is marked `#[device("kvm")]` and the machine has KVM. |
| `/proc` | The step's own processes. | No | Always. |

A program the step runs starts in the step's own directory, with `PATH`
set to `/usr/bin:/bin:/usr/sbin:/sbin` unless its `exec` sets one.
