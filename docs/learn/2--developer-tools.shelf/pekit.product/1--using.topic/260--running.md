---
title: Running Scripts and Programs
type: guide
description: Write scripts for tasks you run by hand, and run them or a program you built with pekit run.
---

Some tasks change the world outside the build: deploying a site, booting
an image in a virtual machine, or copying files to a device. Write each
as a script, a definition made with `script`, beside your builds:

```text
def script deploy() {
    let site = build site()
    let copied = exec(["rsync", "-a", site.path, "web:/srv/site"], interactive = true)
}
```

`pekit run deploy` runs it. A script's own commands run one after
another, in the order the script gives them, with your terminal,
environment, and network, not shut away as a step is. Pass
`interactive = true` to `exec` to give a program the terminal; otherwise
its output is captured. Whatever a script builds is built as `pekit
build` builds it: the steps of `build site()` are those of `pekit build
site`, found in the store when they've already been built.

## Giving a script words

The words after a script's name are its `^script_session.args`:

```text
def script greet() {
    let said = exec(["echo", "hello {^script_session.args}"], interactive = true)
}
```

`pekit run greet there` prints `hello ["there"]`. Pekit's own flags,
such as `--jobs`, come before the name: everything after it is the
script's.

## Scripts calling scripts

A script can run another, as `script greet()`. A build, a test, or a
package can't call a script, through any number of functions: it fails
its check with "context `script_session` has no value in this
definition". So building never deploys or boots anything.

## Running a program you built

`pekit run` also runs the program a build gives, when the build gives
one executable file:

```sh
pekit run app --verbose input.txt
```

Pekit builds `app`, runs the file it gives with the words after the
name, in the directory you ran pekit from, and exits with the program's
status. A build that gives anything else, a record of files say, isn't
run.

## Naming what to run

Name a script or a build as you name any target (see
~pekit/naming-targets): `deploy`, or `site/deploy` to say which
directory's. A build and a script of one name in one directory are an
error for every command, since `pekit run` couldn't tell which you
meant. Of one name in two directories, name the one you mean by its
directory.

## Paths in scripts

A script sees the workspace at `/w` and the build's outputs at
`/pekit-out`, as builds do, so the paths a build gives work in the
script's commands. The rest of the system is where it always is. Since
pekit gives the script that view with bubblewrap, a program that would
gain privileges, such as `sudo`, can't from a script.
