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

## 4. Defaults that are expressions

### Rule

A parameter's default may be any expression, not only a constant. It is
evaluated **on the caller's side of the call**, each time a call leaves the
parameter out, before the callee's body starts, so that under pekit a
default such as `cchain = toolchain.get(Toolchain::C)` is resolved outside
a hermetic step and arrives as an ordinary argument.

- **Scope.** A default sees the function's lexical scope, where the function
  is written, not the caller's, and the parameters before it.
  `fn f(a: Int, b: Int = a * 2)` is allowed; a default cannot see its own or
  later parameters.
- **Dynamic context** (feature 10) is read at the call, so `^arch` in a
  default is the caller's.
- **Effect.** A default may do what its function may do: a pure function's
  defaults are checked as pure code, and an impure or memoised function's as
  impure. No new effect inference is needed: a pure caller can call an impure
  function only where it could anyway.
- **Order.** The explicit arguments are evaluated first, in source order, as
  now; then each parameter left out is given its default, in parameter
  order, each seeing the values of all parameters before it, explicit or
  defaulted.
- **Known callees only.** As now, only a call that knows its callee gives
  defaults. A function's type says nothing of defaults.
- **Constants stay constants.** A default that is a constant by today's rule
  keeps today's path, a `Constant` copied in at the call. Record-field
  defaults stay constants: they are part of a type.

### Implementation

- **Parser.** For each parameter default of a function expression that is
  not syntactically constant, the parser also makes a *default function*: a
  `FnDecl` whose parameters are copies of the earlier parameters (names and
  type expressions), whose return type is the parameter's type expression,
  whose effect is the function's (`Memo` becomes `Impure`), and whose body is
  the default expression, as a `lazy`'s body is any expression. `Param`
  gains `default_fn: Option<FnId>`. There is no expression node of its own:
  the default expression is the body.
- **Checker.** `check_fn` checks each default function before pushing the
  function's own `Function`, as `check_lazy` checks a `lazy`: so the
  default's captures are computed in the defining scope. Its resolution goes
  in `fn_resolutions` like any function's. `FnResolution::defaults` becomes
  per parameter: `Constant(Constant)` or `Function { capture: u32 }`, the
  index among the closure's captures where the default function's closure
  is. The function's `captures` list gains, after its own, one entry for
  each default function, read in the defining frame: the default function's
  closure. `KnownParams` carries the same, so defaults reach other files and
  later session inputs.
- **Lowering.** A function's `MakeClosure` items gain, after its captures, a
  `MakeClosure` of each default function. A call that leaves a parameter
  with a default function out lowers an `Arg` whose value is a new node,
  `IrKind::DefaultArg { param, capture }`, after the explicit arguments, in
  parameter order.
- **Evaluator.** In `call`, after the explicit arguments are in the new
  frame's slots, each `DefaultArg` reads the callee closure's capture
  `capture`, calls it with the values of the new frame's slots `0..param`,
  and stores the result in slot `param`. The call's caller and context are
  the call site's.
- **Index, mentions, formatter.** The default expression is visited as part
  of its parameter, as now; the default function adds no syntax.

### Errors

- The "a parameter's default must be a constant" error goes; a default that
  does not fit its parameter's type is the usual mismatch, at the default.
- A pure function's default that calls an impure function is the usual
  error for impure code in a pure function, at the call, with a note that
  a default runs with its function's effect.
- A default reading a later parameter, or its own, is "`b` is not in scope
  here: a default sees only the parameters before it".

### Documentation

`check.rs` "Calls" and "Defaults", `lower.rs` and `ir.rs` on calls, and
`eval.rs` "Calls"; Learn's functions page; gcsl(5).

## Later features

Specified in detail when their turn comes: 5 duration and size literals; 6
host-declared attributes with typed arguments; 7 receiver methods; 8
generics; 9 extensible enums and `Partial<T>`; 10 context (`context`, `^`,
`with`); 11 `recover` and `assert`; 12 `Secret<T>`; 13 persistent lists;
14 `File`, `Dir`, `Sandbox` and `exec` options; 15 wildcard def-type
imports; 16 the prelude moved to methods.
