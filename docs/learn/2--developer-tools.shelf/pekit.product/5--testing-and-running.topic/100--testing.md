---
title: Writing and Running Tests
type: how-to
description: Write tests as test definitions that pekit runs as cached
  steps, give each test what it tests, run them with pekit test, and read
  the results, retries, and flaky tests.
related:
  - pekit/concepts/targets-and-steps
  - pekit/testing-and-running/querying-what-targets-reach
  - pekit/languages/building-rust
---

A test is a definition made with `test`, beside your builds. pekit runs
each test's body as a step, so a test that passed doesn't run again until
something it tests changes. A failing test is a result, not a broken
build: the other tests still run.

## Before you start

- A workspace with a target to test. This page uses a C program, `app`,
  that prints a greeting, and a test program, `greeting_test`, built from
  the same library.

## Write tests

1. In a build file, declare a test for each check. A test passes if its
   body succeeds, and fails if its body fails: an `assert` that doesn't
   hold, a program that exits with an error, or any other failure.

   ```gcsl
   def test greeting(prog: File = build greeting_test()) {
       let ran = exec([prog.path])
   }

   def test output(prog: File = build app(), expected: File = file("test/expected_output.txt")) {
       let ran = exec([prog.path])
       assert ran.stdout == expected.read()
   }
   ```

   Give a test what it tests through its parameters' defaults, as these do.
   pekit builds the defaults first, outside the test, and gives the test's
   step what they built. The step holds nothing else, so a test can't read
   a file it wasn't given.

2. Run every test below the current directory:

   ```sh
   pekit test
   ```

## Check the result

`pekit test` reports each test as it ended, then a count:

```text
passed           example/greeting
passed           example/output
2 passed, 0 failed
```

It exits with status 0 if every test passed, and 1 if any failed.

## Read a failure

A failing test shows its failure's message under its name:

```text
passed (cached)  example/greeting
FAILED           example/output
    assertion failed: `ran.stdout == expected.read()`
1 passed, 1 failed
```

Each test is reported with one of four results:

| Result | Means |
|---|---|
| `passed` | The test ran and passed. |
| `passed (cached)` | The test passed before with the same inputs, so it didn't run. |
| `flaky` | The test failed, then passed when run again. |
| `FAILED` | The test failed, after any retries. The message follows. |

A test whose program fails to build fails too, with the build's error.

## Run some tests

Name tests to run only those, or select them with a glob, as for builds:

```sh
pekit test output 'lib/**'
```

[Naming Targets](~pekit/naming-targets) describes names and globs.

## When tests run again

A test that passed isn't run again until something it was given changes.
Change `main.c`, and `output` runs again with the rebuilt program, while
`greeting`, which never saw `main.c`, is `passed (cached)`. A change that
rebuilds a program to the same bytes, such as a new comment, runs no test
at all.

A test that failed runs again every time, in case it was flaky.

## Retry flaky tests

`#[retry(3)]` runs a failing test up to three more times, each time from
the start in a fresh sandbox. A test that passes only on a retry is
reported `flaky`:

```gcsl
#[retry(3)]
def test reconnects(prog: File = build client()) {
    let ran = exec([prog.path, "--self-test"])
}
```

`#[timeout(5m)]` fails a test that runs for longer than five minutes, all
its attempts together.

## Tests that can't be sandboxed

A test that needs something a step can't have, such as the network, can
say `#[hermetic(false)]`. It runs every time, outside a sandbox, and its
result is never reused:

```gcsl
#[hermetic(false)]
def test mirror_answers() {
    let ran = exec(["curl", "--fail", "https://example.com/"])
}
```

Prefer a hermetic test wherever you can, so that a test that passed costs
nothing on the next run.
