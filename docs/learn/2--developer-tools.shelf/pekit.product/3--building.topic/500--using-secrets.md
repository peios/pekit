---
title: Using Secrets
type: how-to
description: Give a step a signing key or another secret through the
  keychain, configure where each secret is kept for each profile, and
  understand how pekit keys and protects steps given secrets.
related:
  - pekit/getting-started/workspaces
  - pekit/shipping/releasing
---

Some steps need a secret, such as a key to sign a package. pekit's
keychain gives a step a secret without the secret entering the store, the
build files, or a program's arguments. A workspace says where each secret
is kept, and can keep different secrets for development and for releases.

## Before you start

- The secret, in a file only you can read. This page uses a test key at
  `keys/test.pem` in the workspace. Keep a real key outside the workspace,
  and out of version control.

## Give a step a secret

1. In `workspace.gcsl`, say where the secret is for the `dev` profile, the
   one builds use by default:

   ```gcsl
   import keychain

   pekitconf = {keychain = [
       {profile = "dev"; name = "signing"; file = "keys/test.pem"},
   ]}
   ```

   A relative `file` is relative to the workspace's root.

2. In a build file, ask for the secret in a step's parameter default, so
   that the step is given it as an argument:

   ```gcsl
   import keychain

   #[hermetic]
   fn sign(message: File, key: Secret<File> = keychain.file("signing")) -> File {
       let signed = exec_secret(
           ["/bin/sh", "-c", "sha256sum \"$KEY\" {message.path} > signature.txt"],
           secrets = {KEY = key.reveal().path},
       )
       = working_dir().file("signature.txt")
   }

   def build signature() {
       = sign(file("message.txt"))
   }
   ```

   `exec_secret` gives the program the secret in an environment variable,
   never in its arguments, which any user of the machine can read.
   `keychain.string(name)` gives a secret string instead of a file.

3. Build the target:

   ```sh
   pekit build signature
   ```

## Check the result

`pekit-out/signature/sign/signature.txt` names the key's path as
`/pekit-secrets/dev/signing`: the step found the secret there, read-only.
Every machine gives the step the same path, whatever the key's real
location, so a program that records the path records the same one
everywhere.

## Keep release secrets apart

Each keychain entry belongs to a profile. Add the release key under its
own profile:

```gcsl
pekitconf = {keychain = [
    {profile = "dev"; name = "signing"; file = "keys/test.pem"},
    {profile = "release"; name = "signing"; file = "/run/secrets/signing.pem"},
]}
```

`pekit build --profile release signature` then uses the release key.
`pekit release` always uses the `release` profile.

A secret with no entry for the profile comes from the environment, as in
plain GCSL: `$GCSL_SECRET_FILE_SIGNING` names its file, and
`$GCSL_SECRET_SIGNING` holds a secret string. Without either, the build
fails at the call:

```text
error: no secret `signing` for the profile `release`: configure it in the keychain, or set `$GCSL_SECRET_FILE_SIGNING`
```

## How pekit protects secrets

pekit handles a step given a secret differently from other steps:

- **Only that step sees it.** No other step has the file, and pekit never
  copies a secret into the store. What the step makes with it, such as a
  signature, is kept like any output. - **A step can't ask for a secret
  itself.** A `keychain` call inside a step's code fails. Give the step its
  secrets as arguments instead. - **Its key doesn't hold the secret.** pekit
  identifies a secret by its profile, its name, and a hash of its contents
  keyed by a random key of this machine's. The hash tells nothing of the
  secret to anyone without the machine's key. Changing a secret's contents
  runs the steps given it again. - **Its programs aren't recorded.** From the
  moment a step is given a secret, pekit records none of the programs it runs.
  So a program's output that might reveal the secret never reaches the store.

The machine's key is in pekit's state directory: `$PEKIT_STATE_DIR` if you
set it, otherwise `pekit` under `$XDG_STATE_HOME`, otherwise
`~/.local/state/pekit`. Two machines hash the same secret differently, so a
step given a secret runs on each machine that builds it. A changed or lost
key reuses nothing made with the old one. `pekit clean` never removes the
state directory.

A step whose result holds a secret, such as one that gives a
`Secret<String>`, is never recorded, and runs on every build.
