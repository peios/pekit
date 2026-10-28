---
title: Secrets in Steps
description: How pekit's keychain finds a secret, identifies it without
  revealing it, gives a secret file to one step, and keeps secrets out of
  the store, records, and events.
---

GCSL's `keychain` module asks the host for a secret by name. pekit
answers (`Keychain::answer`) from the workspace's keychain configuration,
`pekitconf.keychain`, for the build's profile, or else from the
environment: `$GCSL_SECRET_FILE_<NAME>` for a file, or `$GCSL_SECRET_<NAME>`
for a string. `NAME` is the secret's name in capitals, with `_` for each
character that is not a letter or a digit. A secret found nowhere fails the
call. A string secret is the file's contents without one final newline.

## Identity

Each secret's identity is what receipts record of it:
`<profile>/<name>#<hash>`, where the hash is the first 32 hexadecimal digits
of HMAC-SHA-256 of the secret's contents under the machine's key.

The machine's key is 32 random bytes, in `keychain-key` in the state
directory, readable only by its owner. pekit makes it the first time a
build needs it, writing it beside its place and linking it into place, so
that two builds starting at once use one key.

Consequences of the identity:

- A changed secret has a new identity, and reruns the steps given it.
- Nothing about the contents can be learned from the identity without the
  key, since a guess can't be tested.
- Two machines give one secret two identities, so a step given a secret is
  found in the store only on the machine that recorded it.
- A new key gives every secret a new identity, and nothing made with the old
  identities is reused.

## A secret file in a step

A step given a secret file has the file bound read-only at
`/pekit-secrets/<profile>/<name>`, from where the keychain found it. The
path is the same on every machine. No other step has the file, and it is
never staged by copying or linking, and never put into the store.

A step can't ask the keychain itself: a `keychain` call in a step's code
fails, since what a step reads has to be among its inputs. A library
resolves a secret in a parameter default instead.

## What never holds a secret

- **The store.** A result that holds a secret is never recorded. A program
  of a step that has been given a secret is never recorded either, from the
  moment it is given one, so a program's output that reveals a secret
  isn't kept.
- **Events and the trace.** An event names a step by its name and an effect
  by its built-in and program, never the program's other arguments. A
  program named by a secret is shown as `<secret>`.
- **Errors.** A program given a secret through `exec_secret` that fails
  reports neither its status nor its output.
