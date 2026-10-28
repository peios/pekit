---
title: Exposure
description: When pekit writes a build's outputs into pekit-out on the
  host, how a target's code reads outputs before then, and how a target's
  results are copied out and linked under its name.
---

Every output of a build, a step's, a fetch's, or an unpacking's, is kept in
the store by its logical path under `/pekit-out`. Steps are staged from the
store, so only what is not hermetic reads outputs from `pekit-out/` on the
disk: a target's own code, the programs it runs, and `pekit run`. An output
is *exposed* there, as read-only hard links to the store's files, only
once something might read it from the disk (`exposure`). A build that
changed nothing writes nothing there for outputs no one reads.

## When an output is exposed

- **Each exposure waits.** A kept output is noted, and a later note of the
  same path replaces an earlier one.
- **A target's reads are answered from the store.** A target's
  `dir.files()` or `file.read()` of a path inside an output of the build is
  answered from the store's tree of it, which is what the disk would show
  once it's exposed. That holds until a target's own effect runs a program
  or writes, which might have changed the output directory; from then on,
  reads go to the disk, as a read of a path in no output does.
- **Every waiting exposure is made** before a target's own effect that
  might read the output directory: a read that isn't answered from the
  store and whose path is, or leads into, the output directory; a read of a
  path that GCSL code names as a string; a keychain's file; and any effect
  that runs a program or writes.
- **`pekit run`** makes every waiting exposure before the program or script
  runs, since it might read any of them.

Any other build leaves an exposure no one needed unmade, so
`pekit-out/.steps/` and `pekit-out/.unpack/` hold only what was.

## What a target gives

As a build ends, each file and directory a target's value names under
`/pekit-out` is copied out to `pekit-out/` on the host. Each exposure above
one of its paths is made first, and those at or below its paths are
dropped, the copy taking their place. The copy is the user's: writable,
and a file of its own, not a link to the store.

Each of a target's results under `.steps/<function>/<identity>/` is then
linked under the target's name (`target_view`), as
`pekit-out/<target>/<function>/<rest>`, a symbolic link to the step's
copy. The second step of one function a target's value names is under
`<function>.2`, and so on, numbered in the order the value names them.

A step found in the store has its outputs placed the same way, so a
target's results are there after `pekit clean`, or in a release's fresh
tree, without running anything.

## What the output directory isn't

`pekit-out/` is not part of the workspace. GCSL's directory listings of the
workspace leave it out, and a step can't be given a file of it by its
workspace path; outputs are named by their `/pekit-out` paths.
