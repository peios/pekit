---
title: Fetching Files
type: how-to
description: Download files and git commits into a build, pin each by its
  digest or commit, unpack archives, and pass what was fetched to the steps
  that need it.
related:
  - pekit/concepts/reuse-and-the-store
  - pekit/building/writing-steps
  - pekit/languages/building-upstream-software
---

Fetching is the only part of a pekit build that uses the network. Every
fetch is pinned: a file by its SHA-256 digest, and a git repository by a
commit. So a fetch gives the same files on every machine and in every
build, and pekit downloads each one only once.

## Fetch a file

1. In a target, call `fetch` with the file's URL and no digest:

   ```gcsl
   def build zlib_source() {
       = fetch("https://zlib.net/fossils/zlib-1.3.1.tar.gz")
   }
   ```

2. Build the target. pekit downloads the file, and fails with the digest
   it found, ready to paste in:

   ```text
   error: `fetch` of `https://zlib.net/fossils/zlib-1.3.1.tar.gz` is not pinned: its sha256 is `9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23`
   ```

   Check the digest against one the file's publisher gives, if there is
   one. A digest pins whatever the server sent.

3. Add the digest:

   ```gcsl
   def build zlib_source() {
       = fetch("https://zlib.net/fossils/zlib-1.3.1.tar.gz", sha256 = "9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23")
   }
   ```

4. Build again. pekit downloads the file, checks it against the digest, and
   keeps it in the store.

## Check the result

`pekit build zlib_source` prints the file's path, under
`pekit-out/.fetch/<sha256>/`. The next build finds the file in the store
and doesn't use the network. So does any workspace on the machine that
fetches a file with the same digest, from any URL.

A fetch whose download doesn't match its digest fails, and pekit keeps
nothing of it.

## Unpack an archive

`archive.unpack` unpacks a tar archive, compressed with gzip, xz, bzip2, or
zstd or not compressed, or a zip archive such as a Python wheel. `strip`
leaves off the first parts of each path, as `tar --strip-components` does:

```gcsl
import archive

def build zlib_source() {
    let tarball = fetch("https://zlib.net/fossils/zlib-1.3.1.tar.gz", sha256 = "9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23")
    = archive.unpack(tarball, strip = 1)
}
```

pekit unpacks archives itself, so it needs no `tar`, and keeps what it
unpacked in the store, so each archive is unpacked only once. An entry
whose path would leave the directory fails the unpacking, so an archive
can never write outside it. Of each file's permissions, only whether it is
executable is kept.

## Fetch a git commit

`fetch_git` gives the files of a git repository at one commit, without
`.git` or submodules:

```gcsl
def build gcc_library() {
    = fetch_git("https://github.com/gcslib/gcc", "72dae8c75eabc34c63615380a9197e1021a39952")
}
```

The commit has to be named in full, by its 40 hexadecimal digits. A branch
or a tag can move, so neither can pin a fetch. pekit keeps a clone of each
repository in its cache, and fetches only commits it doesn't have.

pekit runs git without your or the system's git configuration, so a
setting such as a line-ending conversion can't change the files a commit
gives.

## Fetch a private repository

pekit fetches a private repository with the credentials your own git uses
for its URL. Set up git so that `git clone` of the repository works, and
`fetch_git` works too. For a GitHub repository, for example:

```sh
gh auth setup-git
```

pekit asks your git for credentials only; no other setting of yours
reaches its git. It never stores or erases credentials, and it never
prompts. Without credentials, the fetch fails:

```text
error: `fetch_git` could not fetch `…` of `https://github.com/…`: fatal: could not read Username for 'https://github.com': terminal prompts disabled
```

A commit gives the same files whoever fetches it, so credentials don't
change what pekit builds or what it keeps. A step never sees them.

## Pass fetched files to a step

A step has no network, so it can't fetch. A fetch in a step's code fails:

```text
error: `fetch` runs outside hermetic steps: a step has no network; fetch in a parameter default, or in the caller, and pass the file in
```

Fetch in the code that calls the step, and pass what you fetched as an
argument. A library usually fetches in a parameter default, which runs
where the step is called:

```gcsl
import archive

#[hermetic]
fn header(source: Dir = archive.unpack(fetch("https://zlib.net/fossils/zlib-1.3.1.tar.gz", sha256 = "9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23"), strip = 1)) -> File {
    let copied = exec(["cp", source.file("zlib.h").path, "zlib.h"])
    = working_dir().file("zlib.h")
}

def build zlib_header() {
    = header()
}
```

`archive.unpack` works in a step too, on an archive the step was given.
