# GCSL language changes for pekit2

This is the working specification of every GCSL change that layer A of the
plan makes (see `PLAN.md`). The agreed design behind each is in
`core-model.md` and on Acta PEI-1594; implementation progress is on
PEI-1595. Each section states the rule precisely enough to implement and
test, and is marked when done. Once a feature is done, the GCSL module
documentation and Learn pages are the authority; this file records why.

GCSL's conventions apply throughout: every behaviour is documented in the
module documentation of the stage that implements it, in prose, with every
error message quoted; tests cover each rule and each error; the formatter,
the language server and Learn follow every syntax change.

## Done

1. **Slash-path imports** (`bdf69b5`). `import lib/helpers`; segments keep
   dots, `-`, `+` and digits; a whole import whose last segment is no name
   needs `as`; old dotted paths get a note and a slashed suggestion.
2. **Definitions named by directory** (`1be34de`). The language already
   scoped def-type definitions by directory; the host API now does too.
   `Definition::dir`, qualified paths `dir/path` and `/path`,
   `RequestError::Ambiguous`, duplicates only among directories that see
   each other.
3. **Named-argument shorthand** (`d0d3c72`). After a named argument, a name
   alone is `NAME = NAME`. Positional arguments after named ones stay legal
   (an existing, tested feature); only a variable passed positionally by its
   name alone after a named argument changes meaning.
4. **Defaults that are expressions** (commit after `d0d3c72`; see below).

## 4. Defaults that are expressions: as built

The rule is as designed. A default may be any expression and is evaluated
on the caller's side of the call. That happens each time a call leaves the
parameter out, after the written arguments and before the body. So under
pekit, `cchain = toolchain.get(Toolchain::C)` is resolved outside a hermetic
step. A default sees the function's lexical scope and the parameters
before it, and has the function's effect. Only known callees give defaults.
Constants keep their old path, and record-field defaults stay constants.

How it is built, and where it departs from the plan:

- **Parser.** `Param::default_fn` holds the *default function*. It is a
  `FnDecl` with no node of its own: its parameters copy the earlier ones,
  its return type is the parameter's, its effect is the function's (memo
  becomes impure), and its body is the default. Its **marker is the
  function's own**, not the default's span, so every effect error's "this
  function is pure" label points at the function's `:`. That replaces the
  planned extra note, which would have had to be added at six error sites.
  `Param` grows from 24 to 32 bytes, and the size guard is updated.
- **Checker.** `check_default_fns` checks each default function before
  the function's own frame is pushed, so its captures come from the defining
  scope. `ParamDefault::{Constant, Function { capture }}` replaces
  `Constant` in `FnResolution::defaults`, `KnownParams` and `call_defaults`.
  The capture index is `captures.len() + k`.
- **Inference (not in the plan; found while testing).** A pure function's
  bare parameter is present only if something measures it. Without a fix, a
  future could reach a default that measures the parameter, leaving a
  mistyped slot. Now a default function's copied bare parameters are
  inferred as any pure function's are, and what the default measures, the
  function measures. So `fn f(a: Int, b: Int = a * 2): {v: Int} { v = b }`
  takes a present `a`, and the call-site error for a future points at the
  measuring use in the default.
- **Settling (found while testing).** `settle_function` finds nested
  functions by their `Fn` or `Lazy` nodes, so it now also settles each
  function's default functions' captures.
- **Top-level copies.** A top-level function's default functions' copies
  join its own (`DefaultsChecked::copies`), which decide where its closure
  is made.
- **Mentions (found while testing).** `Walker::function` walks each default,
  with the parameters before it in scope. Before this, a top-level field read
  only in a default was not ordered before its users.
- **Error wording.** A later or own parameter keeps the general message
  "undefined variable `b`" (tools match on that prefix). The error adds the
  label "`b` is a parameter from here on" and the note "a parameter's default
  sees only the parameters before it", in place of the planned new message.
- **IR.** The planned new node `IrKind::DefaultArg` became `ArgValue` in
  `IrKind::Arg { param, value: ArgValue }`, which is `Node(IrId)` or
  `Default { capture, kind }`. A default can then appear only as an
  argument, and no evaluator walker needs a new arm. A function's
  `MakeClosure` gains a `MakeClosure` of each default function after its
  captures.
- **Lowering.** Shadowed declarations stub their default functions too.
- **Evaluator.** `Evaluator::default_arg` calls the closure's capture with
  the new frame's earlier slots, passing the call's caller and its own
  depth budget.
- **Index.** References, rename and hover treat a parameter read in a
  default as the parameter itself, with no change needed. Completion in a
  default offers only the parameters before it.
- **Documentation.** The checker's "Defaults" and "Calls", the parser, the
  AST, lowering, the IR and the evaluator's "Calls"; Learn's functions page;
  gcsl(5).


## Later features

Specified in detail when their turn comes: 5 duration and size literals; 6
host-declared attributes with typed arguments; 7 receiver methods; 8
generics; 9 extensible enums and `Partial<T>`; 10 context (`context`, `^`,
`with`); 11 `recover` and `assert`; 12 `Secret<T>`; 13 persistent lists;
14 `File`, `Dir`, `Sandbox` and `exec` options; 15 wildcard def-type
imports; 16 the prelude moved to methods.
