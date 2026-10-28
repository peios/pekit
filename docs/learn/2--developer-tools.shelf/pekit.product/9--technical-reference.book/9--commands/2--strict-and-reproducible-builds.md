---
title: Strict and Reproducible Builds
description: What --strict changes in loading, keying, sandboxes, and a
  target's effects, and how --check-reproducible runs and compares a second
  build.
---

## Strict builds

`--strict` changes four things:

- **Libraries.** Loading fails unless the workspace's libraries are exactly
  those `pekit.lock` pins, as [Libraries and the
  Lock](~pekit/libraries-and-the-lock) describes.
- **Sandboxes.** Steps are given no host snapshot: no `/usr`, `/bin`,
  `/lib`, `/lib64`, `/sbin`, or `/etc`, and no default `PATH`. A step runs
  its programs from what it is given.
- **Keys.** Every receipt's platform is the execution platform marked as
  having no host tools, `<platform> no-host-tools`, without the host's
  fingerprint. So a strict build's records are the same on every machine
  of the platform, and apart from a build's that borrowed host tools.
- **Effects.** A target's own effects are limited to fetches, unpacks, the
  keychain, and reads under `/w` and `/pekit-out`, as [A Target's Own
  Effects](~pekit/target-effects) describes. Steps marked `#[scratch]` get
  an empty directory.

## Checking reproducibility

`--check-reproducible` builds twice (`Settings::compare`):

1. The first build is an ordinary one, finding and recording steps as
   ever.
2. The workspace is checked afresh, and every step whose receipt has a
   record from the first build is run again, its programs neither found
   nor recorded. Its result and outputs are compared, byte for byte, with
   the record's. A directory is compared through its trees, so each file
   that differs is named.

Only steps given the same inputs both times are compared, so a step whose
input differed because an earlier step wasn't reproducible isn't reported
too. The second build records nothing, so a step's first record stays.

A step that differs fails the command:

```text
error: a step is not reproducible: run again with the same inputs, it gave other outputs
  .steps/stamp/7367551baf538abf
    /pekit-out/.steps/stamp/7367551baf538abf/stamp.txt
```

`pekit run` and `pekit publish` refuse `--check-reproducible`, since their
effects change the world and would be done twice.

A `--gcsl-profile` of a checked build covers both builds.
