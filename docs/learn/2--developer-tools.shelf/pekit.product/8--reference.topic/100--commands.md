---
title: Commands
type: reference
description: The pekit commands, what each is for, the options every
  building command shares, and the page that explains each one.
related:
  - pekit/building/naming-targets
  - pekit/reference/definition-types-and-attributes
---

The `pekit` command has a subcommand for each kind of work. This page says
what each is for and where to read about it. `pekit(1)` is the authority
for every command, option, exit status, environment variable, and file,
and `pekit <command> --help` summarizes one command.

## The commands

These are the commands, in the order you're likely to meet them:

| Command | Does | Read |
|---|---|---|
| `pekit build` | Builds targets. | [Building a First Program](~pekit/building-a-first-program) |
| `pekit test` | Runs tests, every test below this directory by default. | [Writing and Running Tests](~pekit/testing) |
| `pekit run` | Runs a script, or the program a build gives. | [Running Scripts and Programs](~pekit/running-scripts-and-programs) |
| `pekit query deps` | Lists the steps and files targets reach. | [Querying What Targets Reach](~pekit/querying-what-targets-reach) |
| `pekit query rdeps` | Lists the targets that read a file. | [Querying What Targets Reach](~pekit/querying-what-targets-reach) |
| `pekit lock` | Pins the libraries the workspace imports. | [Pinning Libraries](~pekit/pinning-libraries) |
| `pekit package` | Packs packages with the workspace's packagers. | [Packaging](~pekit/packaging) |
| `pekit publish` | Packs packages and publishes them into a store. | [Packaging](~pekit/packaging) |
| `pekit release` | Builds a package from `HEAD` strictly, publishes it, and tags it. | [Releasing](~pekit/releasing) |
| `pekit clean` | Removes `pekit-out/`, and with `--cache` the store. | [Managing the Store](~pekit/managing-the-store) |

## Options the building commands share

`build`, `test`, `run`, `query`, `package`, and `publish` take the same
options for how they build:

| Option | Does | Read |
|---|---|---|
| `--jobs` | Limits how many programs run at once. | [Targets and Steps](~pekit/targets-and-steps) |
| `--platform` | Builds for another platform. | [Building for Another Platform](~pekit/building-for-another-platform) |
| `--profile` | Chooses the keychain's profile. | [Using Secrets](~pekit/using-secrets) |
| `--rebuild` | Runs every step, reusing nothing from the store. | [Reuse and the Store](~pekit/reuse-and-the-store) |
| `--check-reproducible` | Runs every step twice and compares the outputs. | [Checking That a Build Is Reproducible](~pekit/checking-reproducibility) |
| `--strict` | Builds as a release does. | [Hermetic and Strict Builds](~pekit/hermetic-and-strict-builds) |
| `--no-dynamic` | Fails a target that waits for a step's result. | [Targets and Steps](~pekit/targets-and-steps#waiting-for-a-step-s-result) |
| `--trace` | Writes a timeline of the build. | [Profiling a Build](~pekit/profiling-a-build) |
| `--gcsl-profile` | Writes a profile of the build's GCSL code. | [Profiling a Build](~pekit/profiling-a-build) |
| `--remote-cache`, `--remote-instance`, `--remote-header` | Looks for steps in a remote cache. | [Sharing a Remote Cache](~pekit/sharing-a-remote-cache) |
| `--remote-upload` | Writes the steps a build records to the remote cache. | [Sharing a Remote Cache](~pekit/sharing-a-remote-cache) |
