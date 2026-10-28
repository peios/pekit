---
title: Step Sandboxes
type: concept
description: What a step can see and change while it runs, covering the
  paths of its inputs and outputs, its own directory, the host's tools, its
  environment, the network, devices, and the directories it composes.
related:
  - pekit/concepts/targets-and-steps
  - pekit/reference/paths-in-a-step
  - pekit/building/writing-steps
---

pekit runs every step in a sandbox of its own, made with bubblewrap. The
sandbox holds only what the step was given, so the step can't depend on
anything pekit doesn't know about. That is what lets pekit reuse a step's
result safely: if none of the step's inputs changed, nothing it could see
changed either.

## The layout every step sees

Every step sees the same layout, on every machine and in every checkout:

| Path | Holds |
|---|---|
| `/w` | The workspace files the step was given, at their paths in the workspace. |
| `/pekit-out` | The results of earlier steps that the step was given. |
| `/pekit-out/.steps/<function>/<identity>` | The step's own directory, where it writes what it makes. |
| `/tmp` | An empty, writable directory of the step's own. |
| `/usr`, `/bin`, `/lib`, `/lib64`, `/sbin` | The host's own, read-only, unless the build is strict. |

A file the step wasn't given isn't there. If a step is given
`file("src/main.c")` from the directory `app`, it finds `/w/app/src/main.c`,
and no other file of the workspace. A path outside what the step was given
fails with "it is outside the step".

Because the layout never depends on where your checkout is, a path a step
records, such as a path a compiler writes into debugging information, is
the same on every machine.

## The step's own directory

`working_dir()` names the step's own directory. A program the step runs
starts there, and the step gives what it made from there:

```gcsl
#[hermetic]
fn archive(files: List<File>) -> File {
    let paths = for f in files { f.path }
    let packed = exec(["tar", "-cf", "files.tar", ...paths])
    = working_dir().file("files.tar")
}
```

The directory is named after the step's function and its identity, a
digest of what the step is. So the same call makes the same directory in
every build. [Where Results Go](~pekit/where-results-go) describes how its
files reach `pekit-out/`.

A relative path in GCSL code, such as `file("main.c")`, is never relative
to the step's directory. It's relative to the directory of the build file
it's written in, as it is everywhere else. Use `working_dir().file(name)`
for a file the step made.

## Inputs are read-only

A step can't change what it was given. Its programs see their inputs
read-only, and GCSL's file functions, such as `fs.write` and `fs.copy`,
refuse to change a path in one. They also refuse to change a directory the
step composed with `fs.compose`, once it's made.

To change files it was given, a step works on a copy in its own directory.
This step copies a directory and adds a file to the copy:

```gcsl
import fs

#[hermetic]
fn stamped(source: Dir) -> Dir {
    let copied = exec(["cp", "-r", source.path, "copy"])
    fs.write(working_dir().file("copy/STAMP").path, "built by pekit\n")
    = working_dir().dir("copy")
}
```

A large directory that `fs.compose` places whole, such as a toolchain of
thousands of files, is mounted from the store rather than copied. It's mounted
in any `Dir.sandbox` made of the composition too. In a sandbox, a program can
change files inside such a directory, but can't move or remove the directory
itself: it fails with "Device or resource busy".

## The host's tools

Outside a strict build, a step can run the host's own programs: it sees the
host's `/usr`, `/bin`, `/lib`, `/lib64`, and `/sbin`, read-only. It doesn't
see `/usr/local`. Its `/etc` holds only what those programs need to load
their libraries.

A build that uses the host's tools isn't hermetic, because what a step
gives depends on what's installed. pekit says so after the build, and keys
every such step by a fingerprint of the host's installed packages, so that
upgrading a compiler doesn't leave stale results in the store. [Hermetic
and Strict Builds](~pekit/hermetic-and-strict-builds) describes the fingerprint, and how
to build without the host's tools.

## Environment and isolation

A step's programs run with only the environment variables their `exec`
call sets, and `PATH` set to `/usr/bin:/bin:/usr/sbin:/sbin` if the call
doesn't set it. Nothing of the environment you ran pekit in reaches them.

A step has no network. It has its own processes, its own users, and a
`/dev` that holds only the usual few devices, such as `/dev/null` and
`/dev/urandom`. If pekit stops, its steps' programs are stopped too.

A step that runs a virtual machine can ask for KVM with
`#[device("kvm")]`. [Writing Steps](~pekit/writing-steps) shows how, and
the other attributes a step can take.

## Sandboxes inside a step

A step can make sandboxes of its own with `Dir.sandbox`, as a build library
does to run a pinned compiler on the compiler's own runtime. Each sandbox's
programs see the sandbox's directory as their `/`, and are cut off from the
network and the host as the step's own programs are. A sandbox is a copy:
nothing a program writes in it reaches the directory it was made of.
