---
title: Signing Kernel Modules
type: how-to
description: Sign a package's Linux kernel modules with the modsig library,
  using a key from the keychain, so that a kernel enforcing module
  signatures loads them and the package stays reproducible.
related:
  - pekit/shipping/packaging
  - pekit/building/using-secrets
  - pekit/shipping/releasing
---

A kernel that enforces module signatures loads a module only if it carries
a signature by a key the kernel trusts. The modsig library signs a tree of
modules after they are built, in a step of its own. It's the only step
given the key, so no build command sees it. Signing the same modules with
the same key always gives the same bytes, so a package of signed modules
stays reproducible.

## Before you start

- The modsig library, which builds its signing program with the rust
  library. See [Libraries](~pekit/libraries).
- The Rust and C toolchains registered in `workspace.gcsl`. See [Building
  Rust](~pekit/building-rust).
- A target that builds a directory of modules, each compressed with zstd
  and named `.ko.zst`.
- A PEM file holding an ML-DSA-65 private key, with its seed, followed by
  its certificate, which the kernel trusts. Keep it outside the workspace,
  and out of version control.

## Sign the modules

1. Tell pekit where the key is. Name a file in the environment, which keeps
   the path out of your workspace:

   ```sh
   export GCSL_SECRET_FILE_MODULE_SIGNING=~/.config/peios/keys/modsig.pem
   ```

   Or give it a keychain entry named `module-signing`. See [Using
   Secrets](~pekit/using-secrets).

2. In the package, give `modsig.sign` the directory of modules:

   ```gcsl
   import "github.com/gcslib/modsig"

   def package dev.example.modules() {
       version = "1.0-1"
       files = {"lib/modules" = modsig.sign(build modules())}
   }
   ```

3. Pack it:

   ```sh
   pekit package dev.example.modules
   ```

## Check the result

Decompress a module from the package, and `modinfo` shows who signed it:

```sh
zstd -d example.ko.zst
modinfo example.ko | grep -E '^(sig_id|signer)'
```

```text
sig_id:         PKCS#7
signer:         example module signing key
```

## What it changes

`modsig.sign` gives a copy of the directory. In that copy, each `.ko.zst` is
decompressed, signed, and compressed again at zstd's level 19. Everything
else is copied unchanged, so run `depmod` before or after signing, as you
prefer.

The step fails, and says why, if:

- a module is a `.ko`, `.ko.gz`, or `.ko.xz`. The signature has to be inside
  the compression, and only zstd's is supported.
- a module is signed already.
- the directory holds no module.
- the key file holds no seed, or the certificate isn't the private key's.

## How a signature is made

Each signature is deterministic pure ML-DSA-65 over the whole module, with
an empty context string and no signed attributes, in a PKCS#7 message
appended to the module. The kernel's `sign-file` can't produce this with
OpenSSL 3, which adds a signing time and randomises each signature.

A signed module's bytes also depend on zstd. The library's `Cargo.lock` pins
the version, so updating it changes every package that holds signed modules.

## How the key and the store work together

The signing step is an ordinary step given a secret, so [Using
Secrets](~pekit/using-secrets) describes how pekit protects the key. For
signing, that means:

- The directory you give `modsig.sign`, and every step that made it, stays
  unsigned in the store. Changing the key signs again without rebuilding
  them.
- The signed copy is kept in the store, under the key's identity. It holds
  no secret.
- Another machine signs for itself, because a secret's identity differs
  between machines. It packs for itself too, and gets the same package.
- `pekit release` uses the `release` profile's key. Signing twice gives the
  same bytes, so the release's reproducibility check passes.
