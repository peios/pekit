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
4. **Defaults that are expressions** (`9a93d47`; see below).
5. **Duration and size literals** (`c9b8040`; see below).
6. **Host-declared attributes** (`b57331c`; see below), and **retry and
   timeout** (`a705b27`).
7. **Receiver methods** (`51947a8`, `0bf0d9b`, `a894aff`, `354021a`; see
   below).
8. **Generics** (`ecbec8a`, `79f5461`, `7180d8a`; see below). Generic
   functions with inferred type parameters, and generic `type`
   declarations.

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
- **Parameter types (follow-up `fix(calls)` commit).** A default function's
  copies of the earlier parameters take the types its function resolved,
  before inference, instead of resolving the type expressions again. That
  reported an erroneous type once per default, and gave a memoised
  function's default either-shaped parameters where its body has present
  ones.
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
  the new frame's earlier slots, copied up the stack in place so that a
  call allocates nothing once the stack has grown, passing the call's caller
  and its own depth budget.
- **Index.** References, rename and hover treat a parameter read in a
  default as the parameter itself, with no change needed. Completion in a
  default offers only the parameters before it.
- **Documentation.** The checker's "Defaults" and "Calls", the parser, the
  AST, lowering, the IR and the evaluator's "Calls"; Learn's functions page;
  gcsl(5).


## 5. Duration and size literals (done)

### Rule

Two new primitive types, `Duration` and `Size`, with literals written as a
number glued to a unit. A number glued to letters is an error today, so no
existing program changes meaning.

- **Duration literals.** One or more components in strictly decreasing
  unit order, each unit at most once: `30s`, `500ms`, `1h30m`, `2d`. The
  units are `d` (exactly 24h; there is no calendar), `h`, `m`, `s`, `ms`,
  `us` and `ns`. Only the last component may have a decimal fraction
  (`1.5h`, `1m0.5s`), and the total must be a whole number of
  nanoseconds. A `Duration` is an `i64` count of nanoseconds, which covers
  about ±292 years. A literal beyond that is "duration literal out of range
  for Duration".
- **Size literals.** One component: `B`, the decimal units `KB` `MB` `GB`
  `TB` `PB` (powers of 1000), or the binary units `KiB` `MiB` `GiB` `TiB`
  `PiB` (powers of 1024). Examples are `512B`, `2GiB` and `1.5MB`. A
  fraction must make a whole number of bytes. A `Size` is an `i64` count of
  bytes; one beyond that is "size literal out of range for Size". There
  are no exabyte units, because `1E5` is a float exponent.
- Digits may use `_` separators, as in other number literals. There is no
  exponent. A unit is case-sensitive, and an unknown unit is "unknown unit
  `x`". The error lists the units of the literal's type, or of both types
  when the unit matches neither.
- **Negative values** are written with unary minus: `-30s`. Minus applied
  directly to one of these literals is a constant, as for numbers, so
  `timeout: Duration = 30s` is a constant default.
- **Arithmetic**, with `Q` standing for either type:
  - `Q + Q` and `Q - Q` give `Q`; `-Q` gives `Q`;
  - `Q * Int` and `Int * Q` give `Q`;
  - `Q // Int` gives `Q`, floored;
  - `Q // Q` gives `Int`, and `Q % Q` gives `Q`, both floored as for `Int`.
  
  Overflow is a run-time error, as for `Int` ("duration overflow", "size
  overflow"), and dividing by zero is too. `/` is an error on these types,
  noted "use `//`; a Duration is a whole number of nanoseconds" or "of
  bytes". Mixing `Duration` with `Size`, or either with `Float`, is a type
  error. There is no implicit conversion from `Int`, so `0` is not a
  duration; write `0s`.
- **Comparison and equality.** `==`, `!=`, `<`, `<=`, `>` and `>=` work
  within one type.
- **Shown form, the canonical literal.**
  - A duration is shown as its non-zero components from `d` down to `ns`,
    for example `1m30s` or `1s500ms`; zero is `0s`, and a negative duration
    is `-` followed by the form of its magnitude.
  - A size is shown in the largest unit, decimal or binary, that divides it
    exactly: `2GiB`, `1500B`, `1MB`; zero is `0B`.

  Every shown form is a literal of the same value. `show`, interpolation
  and `gcsl run` use it.
- **Types.** The new types join `Int`, `Float`, `Bool` and `String`
  everywhere a primitive goes: annotations, unions, `is` and `as`, record
  fields, and constants in defaults and field defaults.
- **Conversions** to and from numbers, such as `d.seconds()` and
  `size.bytes()`, wait for receiver methods (feature 7) and the prelude's
  move to methods (feature 16). Adding them now as global functions would be
  churn. A host reads `Value::Quantity` directly.

### Implementation, as built

- **Lexer.** A number becomes a quantity only when the letters glued to it
  start with a unit's name (`unit_at`). Any other letters lex as they
  always did, a number and then a name, so a module path's `pkgs/7zip` and
  `pkgs/0ad` keep working. Where an expression is expected, the parser
  reports `30sec` as "unknown unit `sec`" (`Parser::unitless`). This
  departs from the plan, under which the lexer reported every unknown
  unit, and would have broken directory names that start with a digit.
  `Lexer::quantity` computes values exactly: it cancels the fraction's
  power of ten against the unit's factors of 2 and 5, so there is no
  rounding. A fraction with more than 38 significant digits is an honest
  "too many digits" error. The unit tables live in `value.rs`, shared by
  the lexer and the shown form.
- **One quantity variant per layer.** The enum is `Quantity { Duration,
  Size }`. It is used as `TyKind::Quantity(Quantity)`,
  `ValueKind::Quantity(Quantity)`, `Value::Quantity(Quantity, i64)`,
  `Constant::Quantity(Quantity, i64)` and `IrKind::Quantity(Quantity,
  IrId)`, rather than as two variants each. ("Measure" already means
  indentation in the lexer and parameter demand in the checker.)
- **IR and evaluator.** Arithmetic and comparison reuse the `Int` nodes,
  whose operands may be quantities. Lowering lowers each operand of an
  integer operator as its own kind, because `Q // Q` is an `Int` with
  quantity operands. The `Int` walker takes amounts by the node's kind
  (`amount_taken`). An overflow in a quantity's own operator is "duration
  overflow" or "size overflow", and error labels show operands as literals.
  A refactor before this (`shared_as`) moved the typed walkers' any-kind
  arms into one helper, to keep the walkers exhaustive and within the lint.
- **Stack.** The `for`-depth end-to-end test needs between 63 and 64 MB,
  against the 64 MB `STACK_SIZE`, both before this feature and after it.
  The margin was already under 2%: two temporaries in the general walker
  were enough to overflow it, until they moved out of line. This needs
  Jack's decision: raise `STACK_SIZE`, which is public and costs only a
  reservation, or slim the walkers.
- **Host API.** `Given::Quantity`, and a `Duration` or `Size` invocation
  parameter read from text by the lexer itself, as in `--timeout 30s`.
  Invocation parameters can now be of six types.
- **Errors.** `//` and `%` with a quantity that does not fit name both
  operands, with a note on what a quantity divides by.
- Zero is shown as `0s` and `0B`. The most negative duration is the one
  shown form that is no literal, as for `Int`.

### Decisions to confirm with Jack

- The names `Duration` and `Size`. Pkl uses `Duration` and `DataSize`.
  `Size` is shorter and reads well in `memory: Size = 2GiB`.
- `d` as exactly 24 hours, with no `w` and no months.
- No exabyte units.
- No float arithmetic on quantities.

## 6. Host-declared attributes with typed arguments (done)

### Rule

An attribute's arguments are a record, and declaring an attribute means
declaring the record type of its arguments, marked as an attribute:

```gcsl
#[attribute] type hermetic = {on: Bool = true}
#[attribute] type scratch = {}
#[attribute] type device = {name: String}
```

- **Declaration.** `#[attribute]` is a new built-in attribute, allowed only
  on a `type` item. The type must be a record type, written or named,
  whose fields are all of constant-able types: `Int`, `Float`, `Bool`,
  `String`, `Duration`, `Size`, or lists and records of those, with no
  rest-field. Otherwise the error is "an attribute's arguments are a record
  of constants", at the type. An attribute is still a type, usable as one;
  it is simply also an attribute.
- **Namespace.** Attributes are types, so they live in the types'
  namespace and follow its rules: directories, views, member and `*`
  imports, sessions and conflicts. A host's attributes come from an
  importable module, for example `import hermetic from pekit/attributes`,
  that plain GCSL accepts. A misspelt attribute is still an error, "unknown
  attribute `x`", with a suggestion from the attributes in view. A type that
  is not marked `#[attribute]` gives "`PackageDef` is not an attribute",
  noted "an attribute is a `type` marked `#[attribute]`".
- **Built-in attributes.** `shadowable`, `def_type`, `no_prelude`,
  `attribute`, `retry` and `timeout` are built in, and an `#[attribute]`
  type may not be named like one. `retry` is `{times: Int}` and `timeout`
  is `{limit: Duration}`. They are core GCSL, so they are not prelude
  types: a user's `type retry = ...` is still a type, and does not
  conflict.
- **Use.** `#[NAME]` or `#[NAME(ARGS)]`. The arguments are constants, by the
  rule for constant defaults. They are named, `#[device(name = "kvm")]`,
  or, for an attribute of exactly one field, one positional argument:
  `#[retry(3)]`, `#[timeout(30s)]`, `#[hermetic(false)]`. Each argument is
  checked against its field's type, as a constant default is. Fields left
  out take their defaults, and a field with no default left out is
  "`device` needs `name`". `#[hermetic()]` is the same as `#[hermetic]`.
  Bare words, `#[device(kvm)]`, wait for extensible enums (feature 9), so
  for now one is an ordinary unknown name. Qualified names, `#[pk.hermetic]`,
  are out of scope, since the lexer's shape is a single identifier.
  Arguments on an inner attribute, `#![x(...)]`, are an error.
- **Targets.** A named `fn` item at any level, a `def`, a definition made
  with a def-type, and a def-type itself. Anything else (a `let`, a field,
  a function expression, an import, a `type` other than through
  `#[attribute]`) is "attributes apply to functions: a `fn`, a `def` or a
  definition". One attribute written twice on one item is "`#[retry]` is
  written twice".
- **Def-type defaults.** Attributes on a def-type are defaults for its
  definitions. A definition's own attribute of the same name replaces the
  def-type's whole record, not field by field, so `#[hermetic(false)]` on a
  test overrides the `test` def-type's `#[hermetic]`.
- **Pass-through.** Every function's code carries its resolved attributes,
  each an `Attribute { id: AttributeId, args: Constant }`. `AttributeId`
  identifies the declaration, not just the name: the declaring module's
  path and the type's name, or a built-in. A host reads them from any
  closure (`Closure::attributes()`), and in layer B from every action-graph
  node. Plain GCSL ignores host attributes.
- **Retry and timeout semantics (6b).** The design says plain GCSL honours
  `#[retry]` and `#[timeout]`. Feature 6 declares, checks and passes them
  through, and they are inert until 6b. Under 6b, an impure function's call
  that fails with a panic is run again up to `times` more times. A call that
  runs longer than `limit` fails, with any child process killed. Retry on a
  pure function is a warning, since rerunning it cannot change the result.

### Implementation, as built

- **Lexer.** `#[NAME(` lexes as `TokenKind::AttributeArgs` through the name,
  and its `[` stays open on the bracket stack until the `]` after the
  arguments, which lex as ordinary tokens. The shape lives in
  `attribute_at`, which the scan shares.
- **Parser.** Each use gets a synthetic `TypeExprKind::Named` at its name.
  The existing type machinery then resolves and expands it with no new
  resolution code (`type_reads` walks every type expression), and the
  index's references and rename cover attribute uses for free. Uses are
  kept per item (`Ast::attribute_uses`). An unbalanced bracket in the
  arguments recovers as an unbalanced list does.
- **Store and workspace.** `TypeStore` records which declarations are
  attributes, keyed by declaration number with their `AttributeId`, so
  sessions keep them. The checker asks `Context::attribute(name)` through
  the file's view of types.
- **Checker.** `check_attribute_uses` runs before the body. Arguments are
  checked with `check_expr_expecting`, as constant defaults are, so
  mismatch errors read as usual and every argument expression is typed;
  the slot allocation is given back afterwards. `Attribute { id, args }`
  holds the args by field name text, so a host needs no names table. A
  definition takes its def-type's attributes through the def-type's
  function, `KnownParams` carrying them across files and session inputs.
- **Lowering.** `FnCode::attributes`, and `Closure::attributes()` for hosts.
- **Departure.** `retry` and `timeout` are not in the prelude (see below).
  A user's `type retry` is unaffected, and only `#[attribute] type retry`
  is an error.

### Implementation outline (planned)

- **Lexer.** Besides `#[NAME]`, the shape `#[NAME(` is an attribute with
  arguments: a `TokenKind::AttributeArgs(name)` token covering `#[NAME`.
  The lexer pushes a `[` on its bracket stack, so newlines inside are
  insignificant and the closing `]` pops it. The arguments are ordinary
  tokens. The inner form is `#![NAME(`. The shape lives in `attribute_at`,
  which the scan shares, and for the argument form the scan ends the
  attribute at the `(`.
- **Parser.** Built-in attributes keep their item rules, and
  `#[attribute]` is new and applies to a `type` item. Any other outer name
  is a *use*, kept in a side table by item: its name, span and argument
  range, parsed as call arguments, with constants checked later.
- **Workspace.** `DeclaredType` records whether it is an attribute. Each
  use's name resolves through the file's view of types, as a type name does,
  to a declaration, or to a built-in. The tree is expanded with the
  attribute's record type, as a type is.
- **Checker.** Checks the uses on each function item: the target, the
  arguments as constants against the record type, defaults, duplicates and
  def-type defaults. Records each function's attributes as constants.
- **Lowering, IR and values.** `FnCode::attributes`, and
  `Closure::attributes()`.

### Decisions to confirm with Jack

- An attribute as a record type marked `#[attribute]`, rather than a new
  item, `attribute hermetic(on: Bool = true)`, which could later become
  sugar for the same thing.
- Attribute types are usable as types.
- `retry` and `timeout` built in, not declared in the prelude.
- A positional argument only for a one-field attribute.

## 6b. Retry and timeout in plain GCSL (done)

### Rule

- **`#[retry(times)]`.** When a call of the function fails with a runtime
  error, its body runs again with the same arguments, up to `times` more
  times. A runtime error is a panic, a failed `exec` and any other error the
  program's run raises. A bug in GCSL itself, an internal error, is never
  retried. The arguments, defaults included, are not computed again, and the
  body starts afresh each time. If every attempt fails, the error is the
  last attempt's, noted "`f` was tried 3 times". Retrying repeats a
  function's effects: the attribute is the author's statement that doing so
  is safe. On a pure function, retrying cannot change the result, so it is
  a warning: "retrying a pure function cannot change its result". `times`
  must not be negative.
- **`#[timeout(limit)]`.** A call that runs for longer than `limit`, from its
  body's start, fails: "`f` timed out after 30s", at the call. A program
  `exec` is running when the time runs out is killed (the program itself,
  not its own children). An enclosing call's earlier deadline still applies
  inside, and a timeout of an enclosing call is no error of an inner call,
  so an inner `#[retry]` never retries it. A function's own timeout is
  retried by its own `#[retry]`, each attempt timed afresh. `limit` must be
  positive.
- **Memoised functions.** Retry and timeout wrap the body's run inside the
  memoised call. Only a success is recorded, as now.

### Implementation

- **Lowering.** `FnCode` gains `retry: u32` and `timeout: Option<Duration>`,
  read from the function's built-in attributes.
- **As built.** The timeout error is no new `Unwind` variant: a call
  boundary turns every unwind into a `Diagnostic`, so the timeout travels
  as an error carrying a private `Mark` (a `u32` in `Diagnostic`'s padding:
  none, internal, or a deadline's number). Numbers are unique per run, so
  an error never matches a later, unrelated deadline. The error points
  where evaluation was when time ran out, so input stamping across
  sessions and files stays correct. Checks on the recursion path are a
  boolean test followed by a tail call into a cold function, so walker
  frames gain no temporary.
- **Evaluator.** A call of a function with either takes a guarded path
  (`Evaluator::guarded_body`), charged one more level against the depth
  budget. The unguarded path is unchanged. Deadlines are kept in a stack in
  the run's shared state, each its effective instant, the minimum of its
  own and its enclosing ones. Loop iterations, calls and `exec` check it,
  but only while it is non-empty. When a deadline passes, the evaluator
  raises `Unwind::Timeout { depth }` for the outermost expired deadline,
  and the frame that pushed that deadline turns it into the error. `exec`
  under a deadline spawns the program and polls it, killing it at the
  deadline.

## Order: generics (8) before methods (7)

Methods with generic receivers (`def (xs: List<T>) unique(): List<T>`), and
moving the prelude to methods, need type parameters. Methods also need new
workspace machinery for their visibility, while generics stay inside the
checker. So generics come first, and methods are built on them.

## 8. Generics

### Rule

- **Generic functions.** `fn NAME<T, U>(PARAMS) MARKER RET { BODY }`, and
  the same for `def`, so `def get<T>(kind: fn(T): Toolchain): T`. A type
  parameter is a name in the function's signature and body, written where
  a type is, and it shadows a declared type of its name there. One
  parameter written twice is "type parameter `T` is declared twice".
- **Calls infer.** Type arguments are never written at a call. Each type
  parameter is bound by matching the arguments' types against the
  parameters' types, structurally (`List<T>` against `List<Int>` binds
  `T = Int`). A parameter bound more than once takes the join of its
  bindings. Every argument is then checked against the parameter types
  with the bindings substituted, by the ordinary rules, so `id("a")` is a
  `String`. A type parameter that nothing binds is "cannot infer `T` for
  this call of `f`", noted "`T` appears in no parameter's type given an
  argument here". An argument whose type mentions `T` is checked bottom
  up, with no expected type, so that it can bind `T`.
- **Bodies are checked once, for every `T`.** Inside the body a value of
  type `T` is opaque. It can be passed, returned, and stored in records
  and lists, but nothing else: no operator, comparison (`===` included,
  since `T` may be a function), field, index, call or interpolation. A `T`
  is a subtype only of itself, and of a union that has it as a member. A
  `T` may be bound to any type, a lazy one included: it is
  laziness-transparent, so bareness never wraps a `T`, and a pure
  function's `T` parameter is not inferred.
- **Generic types.** `type NAME<T, ...> = TYPE`, used as
  `NAME<Int, ...>`, its parameters substituted into its definition when
  the tree is expanded. The wrong number of arguments is "type `Pair`
  takes 2 type arguments, given 1"; a generic type used bare is
  "type `Pair` needs its type arguments".
- **Values.** A generic function used as a value, rather than called, is
  "a generic function must be called" for now. Instantiating one at an
  expected function type comes later, with methods if they need it.
- **Generic def-types** (`def Toolchain<T>(...)`) come with feature 9,
  where the definition's header binds `T`.

### Implementation

- **Parser.** Type parameters after a function item's name or a `type`
  item's name, and the type arguments of a named type, which the parser
  reads already (`TypeExprKind::Applied`). `FnDecl` gains its type
  parameters.
- **Types.** `FnTy` gains `generics: u8`. `TyKind::Param(u8)` stands for
  the n-th type parameter of the function type it occurs in; the
  generic function types themselves are never nested. A substitution maps
  `Param`s to types (`TypeTable::substitute`), and matching
  (`TypeTable::bind`) binds them from an argument's type.
- **Checker.** Resolving a function's signature declares its type
  parameters, `Param(i)`, in a scope of type names. Calls of a generic
  signature infer, then substitute.
- **Lowering.** A node whose type is or holds a `Param` at the top lowers
  with kind `Deferred`, which the general walker evaluates and which may
  hold any value. A call's result is lowered with the caller's substituted
  kind.
- **Workspace.** A generic `type` declaration is expanded per use, its
  parameters substituted.

### Implementation, as built

- **Generic functions** (`ecbec8a`). `Generics { owner, params, types }`
  records a function's or type's parameters with the range of type
  expressions where they are in scope; `Ast::type_param` picks the
  innermost. The type table has `TyKind::Param { generic, index }` of a
  `GenericId` made once per owner, and `FnTy.generic`. A parameter is
  opaque, a subtype only of itself, `Present` to the await rules and of
  value kind `Deferred`, so lazy bindings pass through untouched (tested
  with `lazy` arguments and `->>`). Calls infer in `Checker::instantiate`:
  the arguments filling a parameter that mentions a type parameter are
  checked bottom up and bind structurally (`TypeTable::bind`), then the
  call is bound against the substituted signature, prechecked arguments
  only expected to fit. `[]` and `{}` bind nothing; an integer literal
  binds last, only what nothing else binds or what it does not fit, so
  `first([1.5], 0)` is a `Float`. A binding widened to a union warns, "the
  arguments for `T` have different types" (`79f5461`), as a mixed list
  does. Interpolating a `T` is an error (it may be an unawaited future).
- **Generic types** (`7180d8a`). Applied types take argument lists and
  qualified names may be applied (`TypeExprKind::PathApplied`). The store
  holds a generic definition's parameters as `TypeExprKind::Param` and a
  use in another definition as `TypeExprKind::Instance`; settling and tree
  expansion substitute (`TypeArena::substitute`), making nodes only above
  a parameter, so nesting costs linear space. The workspace checks arity
  when linking ("needs its type arguments", "takes 2 type arguments,
  given 1"); the store keeps arities for later session inputs.
- **Divergence from the outline above.** No `generics: u8` count on `FnTy`
  and no positional `Param(u8)` in the type table: parameters belong to a
  `GenericId`, so nested generic signatures (a generic function's type
  inside another's) can never confuse whose parameter is whose.

### Decisions to confirm with Jack

- A binding widened to a union is a warning, not an error, matching list
  literals and `if` branches.
- `TyKind` grew from 32 to 40 bytes for the generic's id.
- Type-parameter lists take no trailing comma, as type-argument lists
  don't.
- A generic attribute type is an error.
- Spreading a type parameter in a generic type is an error, since the
  definition is merged before its arguments are known.

## 7. Receiver methods

### Rule

- **Declaration.** `fn (RECV: TYPE) NAME<T, ...>(PARAMS) MARKER RET { BODY }`
  and the same with `def`. The receiver is the method's first parameter,
  with every rule a parameter has (bareness included). Type parameters
  are declared after the name, as for any generic function, and are in
  scope in the receiver: `def (xs: List<T>) unique<T>(): List<T>`. A method
  is a top-level item only: "a method is declared only at the top level of
  a file" elsewhere.
- **Visibility.** A method binds no name: it is in no namespace of
  variables, fields or types, so `files.file(...)` never collides with the
  global `file(...)`. A `fn` method is visible in its own file only. A
  `def` method is visible where a `def` of its file would be seen: in its
  own file, in every file of its directory and of the directories below,
  and in every file that imports its module in any form (whole, member or
  `*`, of its file or of its directory), as extension methods are.
- **Calls.** `VALUE.NAME(ARGS)`, where `VALUE` is a value (not a module or
  a reference to a top-level field) and its static type has no field
  `NAME`, calls a method. A method *applies* when the value's static type,
  its lazy layers stripped, fits its receiver type, binding the method's
  type parameters the receiver mentions as a call binds them (feature 8).
  Of the methods that apply, the one whose receiver type fits every
  other's is called: `List<File>.flatten()` is chosen over
  `List<T>.flatten()` for a `List<File>`. None applying is "no method
  `NAME` for type T", noting the receivers of the visible methods of the
  name, or "no field or method `NAME` in T" with no visible method of the
  name. Several applying with no most specific one is "method `NAME` is
  ambiguous for type T", labelling each.
- **Calling.** The receiver is the first argument, then the call's
  arguments bind as they do for any call, defaults included: a method
  call is a call by the function's own name. The method's effect applies
  as a function's does.
- **Conflicts.** Two methods of one name whose receiver types are the same
  type are an error: in one file at the second, and at any call that sees
  both ("method `NAME` is declared twice for type T"). A value whose static
  type has a field `NAME` that a visible method `NAME` also applies to is
  "`NAME` is both a field of T and a method", at the call. A method whose
  receiver is a record type with a field of its name is an error at the
  declaration.
- **A method is no value.** `xs.unique` without a call reads a field, as
  today; a method is never read as a value.

### Implementation

- **Parser and tree.** `fn (` or `def (` at the top level starts a method.
  A new `ItemKind::Method { name, name_span, function, public }`; the
  function is an ordinary `FnDecl` whose first parameter is the receiver,
  with `receiver: true` on it. Generics are recorded with a type range
  that starts before the receiver, so a parameter is in scope there.
- **Mentions.** Each file records its methods (name, item, public) and the
  names of its method-shaped calls, `VALUE.NAME(...)`: the call of a field
  on anything other than a reference into the top-level tree or an import.
- **Order (the cost).** Which method a call means depends on the
  receiver's static type, which the scan, ordering files before any is
  typed, does not have. So a method-shaped call of `NAME` makes its file
  need every file that declares a `def` method `NAME` visible to it. That
  is conservative: a file that calls a record field `get(...)` needs a
  file that declares a visible method `get`, and if that file needs it
  back, the two are "files that need each other". Only `def` methods in
  directories above or imported modules can cause this. The alternatives,
  typing receivers in the scan or checking files in an order found while
  checking, are much larger changes.
- **Workspace.** Methods stay out of the field trees, as types do: each
  file has a flat method view, name to declarations, merged from its own
  file, its directory chain's files and every imported module, and a
  session's earlier inputs for an input. Each method has a cell of its own
  after the merged nodes, as a value file has, holding its closure once
  its file runs. The context gives the checker the candidates of a name:
  their cells, checked function types and what a call needs of their
  parameters, as for an imported `def`.
- **Checker.** In `check_call`, a callee `Field { expr, name }` that is no
  reference has its receiver checked first. Then field or method is
  decided by the rules above. A method call binds the receiver against
  parameter 0 (for a generic method, binding first) and then the call's
  arguments against the rest. The call records its target for lowering,
  and argument positions are offset by one.
- **Lowering.** A method call is an ordinary `IrKind::Call` of the
  method's cell or closure, with the receiver as the first argument.
- **Index.** A method's name at a call links to its declaration, for
  go-to-definition and hover. Completion after `.` needs the static type
  at the cursor and comes with the language server's completion work.

### Implementation, as built

- **Bindings.** A method is an ordinary top-level function binding under a
  name no source can write, `<method twice@0>` (first free ordinal in the
  shared names table, so session inputs never collide), recorded in
  `Ast::methods`. Hoisting, grouping, captures, defaults, lowering and
  session persistence are a `fn`'s, unchanged. The plan's
  `ItemKind::Method` was not needed.
- **Calls.** `Checker::check_field_call` types the receiver first, then
  the field or the most specific applying method; `call_method` binds the
  receiver as parameter 0 (generics first) and the rest from parameter 1
  (`Bound` offsets). Lowering calls the binding or slot with the receiver
  as argument 0 (`Resolution::method_receiver`).
- **Workspace.** Flat method views (`File::method_view`) from the
  directory chain and every imported module; needs per
  `method_needs`, conservative as specified, the cycle label saying "may
  call the method `get` of `lib.gcsl` here"; checked methods passed to
  later files by slot, type and known parameters (`KnownCallee::Method`).
- **Language server.** Hover (`fn (x: Int) twice(): Int`), definition,
  references and rename across files; completion after `.` of the
  methods whose receiver may fit (`TypeTable::may_receive`).
- **Limits found.** `[]` cannot be a receiver: it is typed before a method
  is chosen. The REPL can import only standard modules today, so it sees
  workspace methods only through those.

### Decisions to confirm with Jack

- Type parameters are declared explicitly, `unique<T>()`. An implicit `T`
  would change meaning silently when a `type T` is in scope.
- `fn` methods are private to their file and `def` methods visible, as for
  functions.
- Methods are top-level only.
- Duplicates are reported in one file and at calls, not for every view
  that could see both.
- The conservative order rule above.

## 9. Extensible enums, `Partial<T>` and generic def-types

Needed by toolchains (example 6, "Consolidated"): `enum Toolchain
extensible<{ *: File | Dir }>`, `enum Toolchain::C<{...}>`, the generic
def-type `Toolchain<T>`, `def Toolchain::C gcc_x86_64()`, and
`toolchain.get(Toolchain::C)`. Built in three steps, each usable alone:
9a enums, 9b `Partial<T>`, 9c generic def-types.

### The gap in the draft

The draft types `get`'s key as `kind: fn(T): Toolchain`. A function type
cannot do what `get` needs: GCSL closures can be neither compared nor
inspected, so `get` could never tell whether a registered toolchain is of
the variant `kind`, nor take out its payload. Jack's own sketch had
`class: Empty<Toolchain<T>>`, a key of its own kind. So a variant is a
value of its own type, `Variant<E, T>`, which can still be called as a
`fn(T): E`.

### Rule (9a: enums)

- **Declarations.** `enum NAME extensible<BOUND>` declares a nominal type
  `NAME` in the file's namespace of types, as `type` does, whose values
  are variants that any module may add. `enum NAME extensible` has no
  bound. `enum NAME::VARIANT<PAYLOAD>` adds a variant to the enum `NAME`
  (a name of the file's view of types, or qualified, `t.NAME::V`), whose
  payload is of type `PAYLOAD`; `enum NAME::VARIANT` has the payload `{}`.
  The payload must fit the bound: "the payload of `Toolchain::C` must
  fit `{*: File | Dir}`". Both are top-level items only.
- **Nominal.** Two enums are different types whatever their bounds, and a
  value of an enum is of no other type. An enum is not generic.
- **Visibility of variants.** A variant is visible where a `def` method
  is: its own file, its directory chain, and every file importing its
  module. Two visible variants of one enum with one name are an error at
  any use of the name ("variant `Toolchain::C` is declared in two
  files").
- **Variants as values.** `NAME::VARIANT` in an expression is the variant,
  of type `Variant<NAME, PAYLOAD>`. A `Variant<E, T>` is a subtype of
  `fn(T): E`, so `Toolchain::C(layout)` makes a value of `Toolchain`, and
  a variant can be passed where a function is expected.
- **Inspecting.** `kind.matches(value)` is whether `value`, an `E`, is of
  the variant `kind`; `kind.payload(value)` is its payload, a `T`, and a
  runtime error ("`value` is `Toolchain::Rust`, not `Toolchain::C`") if
  it is another variant's. They are methods of the prelude on
  `Variant<E, T>`, built in. `value is Toolchain` tests that a value is of
  the enum.
- **Equality and display.** Two values of an enum are equal if they are
  of one variant and their payloads are equal. One shows as
  `Toolchain::C({...})`.

### Rule (9b: `Partial<T>`)

- `Partial<T>`, for a record type `T` with no rest-field, is the type of
  records holding any of `T`'s fields, each of its type. `T` and any
  record type with some of `T`'s fields, of fitting types, fit
  `Partial<T>`, so `{ arch = "x86_64" }` is a `Partial<Platform>`.
- A field of a `Partial<T>` cannot be read by name: it may be absent
  ("field `arch` of `Partial<Platform>` may be absent").
  `partial.matches(full)`, a built-in method of the prelude, is whether
  every field `partial` holds equals `full`'s of its name. That is how a
  toolchain's platforms are matched.
- `Partial<T>` takes defaults from no field.

### Rule (9c: generic def-types)

- A def-type may be generic, `#[def_type] def Toolchain<T>(name: String,
  kind: Variant<Toolchain, T>, f: fn(): ToolchainDef<T>): Registered`. Its
  type parameters are bound by a definition's header: `def Toolchain::C
  gcc()` passes the variant `Toolchain::C` as the def-type's parameter
  of type `Variant<E, T>`, binding `T` to its payload, and the body is
  checked against `f`'s type with `T` bound. A reference,
  `Toolchain::C gcc`, names the definition the same way.
- A generic def-type has exactly one `Variant` parameter, the one a header
  fills. A generic def-type used without `::VARIANT` is "def-type
  `Toolchain` needs a variant: `def Toolchain::V name()`".

### Implementation (planned)

- **Lexer and parser.** `::` is a token. `enum` and `extensible` are
  contextual: `enum NAME ...` at an item's start, as `type` is read.
  `ItemKind::Enum` and `ItemKind::Variant`; `ExprKind::Variant`, the enum
  a type expression so that the workspace's expansion resolves it;
  `Variant<E, T>` and `Partial<T>` are built-in applied types.
- **Types.** `TyKind::Enum { decl }`, nominal by its declaration's number
  in the type store, shown by its name. `TyKind::Variant { decl, payload
  }`, whose function type, `fn(payload: T): E`, is what a call of it is
  checked against. `TyKind::Partial(record)`.
- **No file order.** A variant is static data: `Toolchain::C` lowers to a
  constant tag, and its payload type is resolved with the `type` items,
  before any file is checked. So, unlike a method, using a variant makes
  a file need nothing, and the conservative order rule of methods does
  not apply. Enum and variant declarations live in the type store beside
  `type` declarations; each file has a view of variants, as of methods.
- **Runtime.** `Value::Tag` (a variant as a value, callable) and
  `Value::Variant` (a tag and its payload), compared by the variant's
  number and shown `Toolchain::C(...)`. The built-ins `variant_matches`,
  `variant_payload` and `partial_matches` have generic signatures, which
  the prelude's methods `matches` and `payload` call.

### As built (9a, 9b)

GCSL `c8012a7` and `0b68623`. Differences from the plan above:

- **Identity by key, not by declaration number.** An enum is
  `TyKind::Enum { key, name }`. Its key is a name no source can write,
  `<enum NAME@ORIGIN>`, where the origin is the declaring file's path, or
  `input N` for a session input. Each variant has a key of its own too.
  Declaration numbers change between scans and session inputs, so they
  would have made one enum several types.
- **Bounds are checked closed.** A payload fits its bound by its record
  type as written (`TypeTable::fits_bound`). Under the ordinary rest-field
  rule, `{side: Int}` would not fit `{*: Int}`.
- **Equality.** `==` on values of an enum compares them whole, as `===`
  does. It is refused for an enum without a bound, whose payloads may
  hold functions.
- **`is E`** tests a value's enum (`ValueTest::Enum`).
- **Errors in the prelude.** The runtime error of `payload` points into
  the prelude's method, since GCSL reports no call stack. A stack would
  improve it; recorded as a possible later improvement.
- **Hidden fields.** A payload's hidden fields are outside its bound's
  check. To document if it matters for toolchains.
- `Partial<T>` field reads are "field `arch` of `Partial<Platform>` may
  be absent", noting `.matches(...)`.

### Decisions to confirm with Jack

- A variant is a value of `Variant<E, T>`, a callable subtype of
  `fn(T): E`, not a plain function, so that `get` can match and unwrap.
- Only extensible enums for now; closed enums are not needed yet.
- Variants are visible as `def` methods are.
- `Partial<T>` fields are not readable by name, only matched.
- A generic def-type's type parameters come only from a variant header.

## Later features

Specified in detail when their turn comes: 7 receiver methods; 8
generics; 9 extensible enums and `Partial<T>`; 10 context (`context`, `^`,
`with`); 11 `recover` and `assert`; 12 `Secret<T>`; 13 persistent lists;
14 `File`, `Dir`, `Sandbox` and `exec` options; 15 wildcard def-type
imports; 16 the prelude moved to methods.
