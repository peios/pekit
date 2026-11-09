---
title: Signing Firmware
type: how-to
description: Sign a package's device firmware, or any file that isn't ELF,
  with the pipsig library and a key from the keychain, so that the Peios
  kernel trusts it and the package stays reproducible.
related:
  - pekit/shipping/packaging
  - pekit/building/using-secrets
  - pekit/shipping/signing-kernel-modules
---

The Peios kernel loads device firmware only if the file carries a signature
by a key in the kernel's trusted computing base. A file that isn't ELF
carries its signature in its `security.peios.sig` attribute, which a package
can't hold. So the package carries the signature as a file beside the one it
signs, `<file>.peios.sig`, and the package's consumer sets the attribute from
it. The pipsig library makes those files after the build, in a step of its
own. It's the only step given the key, so no build command sees it. Signing
the same files with the same key always gives the same bytes, so the package
stays reproducible.

For the format of the signature and what the kernel checks, see [Signature
Format](~specifications/pspk/binary-signing-and-pip/signature-format) and
[Files the Kernel
Loads](~specifications/pspk/binary-signing-and-pip/kernel-loaded-files). For
how a package carries one, see [Payload
Entries](~specifications/pspu/package-format-and-repository-protocol/payload-entries#detached-signatures).

## Before you start

- The pipsig library, which builds its signing program with the rust
  library. See [Libraries](~pekit/libraries).
- The Rust and C toolchains registered in `workspace.gcsl`. See [Building
  Rust](~pekit/building-rust).
- A target that builds a directory holding the files to sign.
- A PEM file holding an ML-DSA-65 private key with its seed, such as
  `openssl genpkey -algorithm ML-DSA-65` writes, whose public key the kernel
  trusts. Keep it outside the workspace, and out of version control.

## Sign the files

1. Tell pekit where the key is. Name a file in the environment, which keeps
   the path out of your workspace:

   ```sh
   export GCSL_SECRET_FILE_TCB_SIGNING=~/.config/peios/keys/tcb.pem
   ```

   Or give it a keychain entry named `tcb-signing`. See [Using
   Secrets](~pekit/using-secrets).

2. In the package, give `pipsig.sign` the directory and the patterns of the
   files to sign:

   ```gcsl
   import "github.com/gcslib/pipsig"

   def package dev.example.firmware() {
       version = "1.0-1"
       files = {"." = pipsig.sign(build firmware(), take = ["usr/lib/firmware/**"])}
   }
   ```

3. Pack it:

   ```sh
   pekit package dev.example.firmware
   ```

## Check the result

Each file the patterns take has its signature beside it, of 3310 bytes:

```sh
wc -c usr/lib/firmware/example.bin.zst.peios.sig
```

```text
3310 usr/lib/firmware/example.bin.zst.peios.sig
```

## What it changes

`pipsig.sign` gives a copy of the directory. In that copy, each regular file
the patterns take has a signature beside it, with mode `0644`. Everything
else, the files themselves included, is copied unchanged.

A pattern is matched against the whole of a path in the directory. `**`
matches any characters, `*` matches any characters but `/`, and every other
character matches itself. So `usr/lib/firmware/**` takes every file under
`usr/lib/firmware`, and `*/usr/lib/firmware/**` takes those of each directory
the tree holds.

A link that a pattern takes isn't signed. The kernel checks the file a link
names, so that file's signature serves the link.

The step fails, and says why, if:

- a file it takes is ELF. An ELF file carries its signature in a section
  instead.
- a file it takes is a signature, or already has one beside it.
- a pattern takes no file, or `take` is empty.
- the tree holds something that is neither a file, a directory nor a link.
- the key file holds no seed, or its expanded key isn't its seed's.

## How a signature is made

Each signature is a version byte, `1`, followed by a deterministic pure
ML-DSA-65 signature, with an empty context string, of the SHA-256 of the
whole file as it is on disk. A compressed file is signed as it is
compressed, since the kernel checks the file before it decompresses it.

## How the key and the store work together

The signing step is an ordinary step given a secret, so [Using
Secrets](~pekit/using-secrets) describes how pekit protects the key. For
signing, that means:

- The directory you give `pipsig.sign`, and every step that made it, stays
  unsigned in the store. Changing the key signs again without rebuilding
  them.
- The signed copy is kept in the store, under the key's identity. It holds
  no secret.
- Another machine signs for itself, because a secret's identity differs
  between machines. It packs for itself too, and gets the same package.
- `pekit release` uses the `release` profile's key. Signing twice gives the
  same bytes, so the release's reproducibility check passes.
