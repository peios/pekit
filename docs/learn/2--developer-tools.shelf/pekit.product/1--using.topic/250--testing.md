---
title: Testing
type: guide
description: Write tests that pekit runs as cached steps, and run them with pekit test.
---

A test is a definition made with `test`, beside your builds. It passes if
its body succeeds and fails if it fails: an `assert` that doesn't hold, a
program that exits with an error, or any other failure.

```text
def test greeting(prog: File = build greeting_test()) {
    let ran = exec([prog.path])
}

def test output(prog: File = build app(), expected: File = file("test/expected_output.txt")) {
    let ran = exec([prog.path])
    assert ran.stdout == expected.read()
}
```

Give a test what it tests with its parameters' defaults, as these do.
Pekit builds them first, and runs the test's body as a step of its own,
in a sandbox that holds only what it was given.

## Running tests

`pekit test` runs every test below the current directory. Name tests to
run only those, or select them with a glob, as for builds (see
~pekit/naming-targets):

```sh
pekit test
pekit test output 'lib/**'
```

Each test is reported as it ended:

```text
passed (cached)  example/greeting
FAILED           example/output
    assertion failed: `ran.stdout == expected.read()`
1 passed, 1 failed
```

`pekit test` exits with status 1 if any test failed. A failing test is a
result, not a broken build: the other tests still run.

## Cached results

A test that passed isn't run again until something it was given changes.
Change `main.c`, and the golden test reruns with the rebuilt program,
while the unit test, which never saw `main.c`, is `passed (cached)`. A
change that rebuilds the program to the same bytes, such as a comment,
reruns no test at all. A test that failed runs again every time, in case
it was flaky.

`#[retry(3)]` reruns a failing test up to three more times. One that
passes only on a retry is reported `flaky`. A test that can't run in a
sandbox, because it needs the network say, says `#[hermetic(false)]`: it
runs every time, and its result is never reused.
