# Example 8: teaching pekit a new language

A tiny Python program built into a single runnable file, using a rule library
that pekit itself knows nothing about.

```
words/words.py    a library with no dependencies: the word "Hello"
greet/greet.py    a library that depends on words
app/__main__.py   the program; depends on greet, and through it on words
```

## What must happen

By hand:

```sh
python3 -m py_compile words/words.py greet/greet.py app/__main__.py   # 1. check each file
mkdir stage                                                          # 2. gather every source
cp words/words.py greet/greet.py app/__main__.py stage/              #    the program needs
python3 -m zipapp stage -o app.pyz                                   # 3. bundle into one file
python3 app.pyz                                                      #    prints "Hello, pekit2"
```

This was run by hand.

| You change | Must rerun | Must *not* rerun |
|---|---|---|
| `words/words.py` | its check, then the bundle | the checks of `greet.py` and `__main__.py` |
| `app/__main__.py` | its check, then the bundle | everything to do with `words` and `greet` |

## What's new

Examples 1 to 7 already established that a language is "just a library". This
example tests that claim on a language with different needs from C.

**1. A toolchain kind from outside the standard library.** Python needs an
interpreter, not a C compiler. `gcslib/python` declares its own kind, such as
`enum Toolchain::Python<{ root: Dir, python: File }>`, and a workspace
registers a Python toolchain like any other. If the extensible enum from
example 6 works, nothing in pekit or the stdlib changes.

**2. Transitive dependencies.** `app` imports only `greet`, but the bundle also
needs `words`, because `greet` imports it. So each library must hand its
users not just its own files, but everything it depends on, all the way down.
C had a milder version of this: a static library's users need its own
dependencies at link time. In Bazel, a rule passes this information on in
typed records it calls *providers*.

**3. Doing that efficiently.** The obvious way is for each library to return
`own_files + every dependency's files`. In a chain of 1,000 libraries, that
copies the list at every level, roughly half a million copies in all. And
when two paths reach the same library, as in a diamond, it appears twice.
Bazel solved this with a *depset*:

- each level adds a node that points at its children, at no extra cost;
- the full list is only built when a step actually needs it;
- duplicates are removed at that point.

One option for us is to make `FileSet` itself work like this internally, so
`a + b` costs nothing and authors never meet a new concept. Each node can also
store its own fingerprint, which keeps receipts cheap.

**4. What a rule is.** In Bazel, a language adds *target kinds*:
`py_library(name = "greet", ...)`. So far we have written every target as
`def build`, calling library functions inside it. Should `gcslib/python` add
def-types such as `def py_library greet`, or stay as functions used inside
`def build`?

**5. Type safety between rules.** A Python library's result is a typed record.
Passing a C library to `python.binary` should be a type error when the file is
checked, not a confusing failure at run time. Bazel checks providers during
analysis. We get this from GCSL's types, as long as each rule library defines
its result types.

## What to write

Write `build.gcsl` in this directory, and the `gcslib/python` side. Things to
decide:

- The Python toolchain kind, and a registered Python toolchain.
- What a Python library returns, and how transitive sources are collected.
- `python.binary`: stage every transitive source and run `zipapp`.
- Whether Python targets are `def build` or new def-types.
