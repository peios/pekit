# Example 7: tests as cached steps

Example 2's library and program, with two tests:

```
lib/greet.h, lib/greet.c   the library, with a new greeting() function
lib/internal.h
main.c                     the program
test/greeting_test.c       a unit test: links the library, checks greeting()
test/expected_output.txt   what the program should print
```

- **`greeting_test`** is a *unit test*: a small program that links the library,
  checks one function, and exits 0 for pass and 1 for fail.
- **The output test** is a *golden test*: it runs `app` and compares what it
  prints with `test/expected_output.txt`.

## What must happen

By hand, after building example 2's library and program:

```sh
cc -c -Ilib -o greeting_test.o test/greeting_test.c    # 1. build the unit test
cc -o greeting_test greeting_test.o libgreet.a
./greeting_test                                        # 2. run it: exit 0 is a pass

./app > actual.txt                                     # 3. run the program
diff test/expected_output.txt actual.txt               # 4. same output is a pass
```

All of this was run by hand, and both tests pass.

The point is that a test result is cached like any other output:

| You change | Must rerun | Must *not* rerun |
|---|---|---|
| nothing | | both tests: pekit reports "passed (cached)" |
| `main.c` | `app`, then the output test | the unit test, which never touches `main.c` |
| `lib/greet.c` | the library, then both tests | |
| `test/expected_output.txt` | the output test only | every build step |
| a comment in `main.c`, giving an identical `app` | `app`'s compile | the output test, through early cutoff |

## What's new

**1. A failing test is a result, not a broken build.** If running a test
counted as a failed step, a failing test would stop the build, and pekit
couldn't cache the fact that it failed. So a test step should always
*succeed*, and its output records what happened: passed or failed, the exit
code, and the log. `pekit test` then reports the results and exits non-zero
if any failed.

**2. Should a failure be cached?** If a test is hermetic and deterministic, it
fails the same way every time, so rerunning it unchanged is wasted work.
But people rerun failing tests precisely because they suspect they aren't
deterministic.

**3. Flaky tests.** Some tests sometimes pass and sometimes fail on the same
inputs, because of timing, races or resource limits. The options include
retrying N times, marking a test as known-flaky, and running a test many times
to find out whether it's flaky. A cached "pass" from a flaky test is also a
pass that may not hold next time.

**4. Tests that can't be hermetic.** Some tests need the network, real
hardware or a running service. They can't be cached safely, because their
result depends on things outside their inputs. They need a way to say so,
and then they run every time. Note that Peios's integration tests, which boot
a VM, can be hermetic: QEMU plus an image built in the graph has no outside
inputs. So they can be cached too.

**5. Timeouts and resources.** A test that hangs must be stopped. A timeout
should not change the cache key, but a test that only passes with more time
is a sign of flakiness.

**6. Choosing what to run.** `pekit test` with no target, a single test, a
directory of tests, or in CI, only the tests affected by a change. The last
needs "what depends on this file", which is the query question from the
performance review.

## What to write

Write `build.gcsl` in this directory. Use the toolchain design from example 6,
so no compiler is passed anywhere. Things to decide:

- How a test is defined: probably `def test name()`, alongside `def build`
  and `def package`.
- What a test returns, and how pass or fail is decided: an exit code, a
  comparison, or something else.
- How the golden comparison is written.
- What happens to failures and flaky tests, and how a non-hermetic test says
  so.
