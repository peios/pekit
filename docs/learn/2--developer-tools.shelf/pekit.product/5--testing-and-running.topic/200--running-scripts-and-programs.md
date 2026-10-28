---
title: Running Scripts and Programs
type: how-to
description: Write scripts for tasks you run by hand, such as deploying or
  booting an image, run them with pekit run, and run a program a build
  gives with its arguments.
related:
  - pekit/concepts/targets-and-steps
  - pekit/shipping/building-an-image
---

Some tasks change things outside the build: deploying a site, booting an
image in a virtual machine, or copying files to a device. Write each as a
script, a definition made with `script`, beside your builds, and run it
with `pekit run`. A script can build what it needs first, exactly as
`pekit build` would.

## Write and run a script

1. In a build file, declare the script:

   ```gcsl
   def script deploy() {
       let site = build site()
       let copied = exec(["rsync", "-a", site.path, "web:/srv/site"], interactive = true)
   }
   ```

   A script's commands run one after another, in the order the script gives
   them, with your terminal, environment, and network. They aren't shut
   away as a step is. Pass `interactive = true` to `exec` to give a program
   the terminal; otherwise pekit captures its output.

2. Run it:

   ```sh
   pekit run deploy
   ```

## Check the result

The script's programs run, and `pekit run` exits with status 0 if the script
succeeded. Whatever a script builds is built as `pekit build` builds it. The
steps of `build site()` are those of `pekit build site`, and are found in the
store if they've been built before.

## Give a script arguments

The words after a script's name are its `^script_session.args`:

```gcsl
def script greet() {
    let said = exec(["echo", "hello {^script_session.args}"], interactive = true)
}
```

`pekit run greet there` prints `hello ["there"]`. pekit's own options, such
as `--jobs`, come before the name. Everything after the name belongs to the
script.

## Run a program you built

`pekit run` also runs the program a build gives, when the build gives one
executable file:

```sh
pekit run app --verbose input.txt
```

pekit builds `app`, then runs the file it gives with the words after the
name, in the directory you ran pekit from. It exits with the program's
status. A build that gives anything else, such as a record of files, can't
be run.

## Name what to run

Name a script or a build as you name any target: `deploy`, or
`site/deploy` for the one in `site`. See [Naming
Targets](~pekit/naming-targets).

A build and a script of one name in one directory are an error for every
command, since `pekit run` couldn't tell which you meant. If two
directories each have one, name the one you mean by its directory.

## Scripts and builds stay apart

A script can run another script, as `script greet()`. A build, a test, or a
package can't call a script, through any number of functions. The check
fails before anything runs:

```text
error: context `script_session` has no value in this definition
```

So building never deploys or boots anything. Only `pekit run` runs a
script.

## Paths in scripts

A script sees the workspace at `/w` and the build's outputs at
`/pekit-out`, as steps do, so the paths a build gives work in the script's
commands. The rest of the system is where it always is. pekit gives the
script that view with bubblewrap, so a program that would gain privileges,
such as `sudo`, can't do so from a script.

Before a script or a program runs, pekit writes every output of the build
to `pekit-out/`, since the program might read any of them.
