//! Receipts: the key a hermetic step's result is cached by.
//!
//! # Receipts
//!
//! A step's *receipt* is a SHA-256 over a canonical encoding of everything
//! that can change what it gives (see `design/engine.md`, E3):
//!
//! - its code: the path and contents' digest of every file whose source
//!   makes it, in order;
//! - its arguments and its function's captures, and, of the context it was
//!   made in, each context it read as it ran, by its key, with its value,
//!   or that it was unbound, a default read (see `design/engine.md`, R2),
//!   as plain data, each record's fields sorted by name, since GCSL compares
//!   records so, a float by its bits, every NaN one and `-0.0` as `0.0`;
//! - each file it is given by its path *and* the digest of its contents,
//!   and each directory by its path and its tree's digest: the path stays,
//!   since it reaches command lines and names outputs, and an input is keyed
//!   by its contents, never by the step that made it, so that a rebuilt
//!   input with the same bytes at the same path keys the same;
//! - each secret by its identity and whether it is revealed, never by its
//!   value;
//! - each function in its captures or context, a registered toolchain's
//!   definition say, by the name and contents of its code's file, its number
//!   in that code, and its captures, as GCSL keys it
//!   ([`Plain::Closure`]);
//! - its own output directory, `/pekit-out/.steps/<function>/<identity>`,
//!   which names the files it makes (see [Identities](#identities));
//! - the platform it runs on.
//!
//! Its attributes are not on it: `#[retry]` and `#[timeout]` change how it
//! runs, not what it gives.
//!
//! # Identities
//!
//! A step's *identity* names its output directory,
//! `/pekit-out/.steps/<function>/<identity>`, so that a step is at the same
//! path in every build that makes it, whichever targets reach it, in
//! whatever order (see `design/engine.md`, E5). It is a SHA-256 over what
//! the step is, apart from its inputs' contents:
//!
//! - its callee: the name of its code's file and its function's number in
//!   that code, and its captures;
//! - its arguments, as plain data, every file and directory by its path
//!   alone, and each result of a step not given yet by that step's
//!   identity, so that a step is named before the steps it waits for run;
//! - the platform it runs on.
//!
//! The contexts it was made in name no step, so that binding one moves only
//! the steps that read it, as their receipts say (see `design/engine.md`,
//! R2). Two steps a build makes that only their contexts tell apart, one
//! compile for two platforms say, are named by the digest of those contexts
//! too, the later met.
//!
//! Its inputs' contents are its receipt's alone: a rebuilt input with the
//! same bytes keys the steps after it the same, and a changed one reruns
//! them at the same paths. One call made by two targets, or twice, is one
//! step, which GCSL's graph makes once.

use gcsl::eval::plain::Plain;
use sha2::{Digest as _, Sha256};

use crate::store::Digest;

/// What a receipt is made of, but its files' contents, which [`receipt`]
/// asks for.
#[derive(Clone, Copy)]
pub struct ReceiptOf<'a> {
    /// The path and contents' digest of each file whose source makes the
    /// step's code, in order.
    pub code: &'a [(String, Digest)],
    /// Its arguments, in parameter order.
    pub args: &'a [Plain],
    /// Its function's captures.
    pub captures: &'a [Plain],
    /// The contexts it read, by key, sorted, each with its value, or `None`
    /// for one unbound: none for a step's *base*, the receipt its manifest
    /// is found by.
    pub context: &'a [(String, Option<Plain>)],
    /// Its own output directory, logically.
    pub own: &'a str,
    /// The name and contents' digest of the file of each input number, by
    /// which a function in its captures or context is keyed.
    pub code_of: &'a dyn Fn(u32) -> Option<(String, Digest)>,
    /// The platform it runs on, as pekit describes it.
    pub platform: &'a str,
}

/// The receipt of a step made of `of` (see [Receipts](#receipts)),
/// `contents` giving the digest of the file, or the tree of the directory
/// if it says so, at each path the step is given.
///
/// # Errors
///
/// What `contents` fails with: a file the step is given that is not there,
/// say.
pub fn receipt(
    of: ReceiptOf<'_>,
    contents: &mut dyn FnMut(&str, bool) -> Result<Digest, String>,
) -> Result<Digest, String> {
    let mut hasher = Sha256::new();
    let none = |_: u32| None;
    let mut out = Encoder {
        hasher: &mut hasher,
        contents,
        code_of: of.code_of,
        step_of: &none,
    };
    out.text("pekit-receipt-3");
    out.count(of.code.len());
    for (path, digest) in of.code {
        out.text(path);
        out.bytes(digest.bytes());
    }
    for part in [of.args, of.captures] {
        out.count(part.len());
        for plain in part {
            out.plain(plain)?;
        }
    }
    out.count(of.context.len());
    for (key, plain) in of.context {
        out.text(key);
        // Unbound is told apart from every value by its tag.
        match plain {
            Some(plain) => {
                out.bytes(&[1]);
                out.plain(plain)?;
            }
            None => out.bytes(&[0]),
        }
    }
    out.text(of.own);
    out.text(of.platform);
    Ok(Digest::from_hasher(hasher))
}

/// What a receipt's encoding is written into.
struct Encoder<'a> {
    /// The hasher.
    hasher: &'a mut Sha256,
    /// The digests of the files given, by path.
    contents: &'a mut dyn FnMut(&str, bool) -> Result<Digest, String>,
    /// The name and contents' digest of the file of each input number.
    code_of: &'a dyn Fn(u32) -> Option<(String, Digest)>,
    /// The name of each step whose result, not given yet, is written, by its
    /// node's number: an identity's alone.
    step_of: &'a dyn Fn(u32) -> Option<String>,
}

impl Encoder<'_> {
    /// Writes `bytes`.
    fn bytes(&mut self, bytes: &[u8]) {
        self.hasher.update(bytes);
    }

    /// Writes `count`, as eight bytes.
    fn count(&mut self, count: usize) {
        self.bytes(&(count as u64).to_le_bytes());
    }

    /// Writes the length of `text`, then `text`.
    fn text(&mut self, text: &str) {
        self.count(text.len());
        self.bytes(text.as_bytes());
    }

    /// Writes the file, or the directory if `dir`, at `path` of the identity
    /// `identity`: by its path and its contents' digest.
    fn file(&mut self, path: &str, identity: Option<&str>, dir: bool) -> Result<(), String> {
        self.bytes(if dir { b"D" } else { b"F" });
        self.text(path);
        self.text(identity.unwrap_or(""));
        let digest = (self.contents)(path, dir)?;
        self.bytes(digest.bytes());
        Ok(())
    }

    /// Writes the function `function` of the code of input `input`, which
    /// captures `captures`: by its file's name and contents, its number, and
    /// its captures.
    fn closure(&mut self, input: u32, function: u32, captures: &[Plain]) -> Result<(), String> {
        let (name, digest) = (self.code_of)(input)
            .ok_or_else(|| format!("a function of no file the build has, {input}"))?;
        self.bytes(b"c");
        self.text(&name);
        self.bytes(digest.bytes());
        self.bytes(&function.to_le_bytes());
        self.count(captures.len());
        for capture in captures {
            self.plain(capture)?;
        }
        Ok(())
    }

    /// Writes `plain`, canonically (see [Receipts](#receipts)).
    fn plain(&mut self, plain: &Plain) -> Result<(), String> {
        match plain {
            Plain::Int(n) => {
                self.bytes(b"i");
                self.bytes(&n.to_le_bytes());
            }
            Plain::Float(x) => {
                self.bytes(b"f");
                let bits = if x.is_nan() {
                    f64::NAN.to_bits()
                } else if *x == 0.0 {
                    0
                } else {
                    x.to_bits()
                };
                self.bytes(&bits.to_le_bytes());
            }
            Plain::Bool(b) => self.bytes(if *b { b"t" } else { b"n" }),
            Plain::Str(s) => {
                self.bytes(b"s");
                self.text(s);
            }
            Plain::Quantity(quantity, amount) => {
                self.bytes(b"q");
                self.text(&format!("{quantity:?}"));
                self.bytes(&amount.to_le_bytes());
            }
            Plain::Empty => self.bytes(b"e"),
            Plain::Record(fields) => {
                self.bytes(b"r");
                let mut sorted: Vec<&(String, Plain)> = fields.iter().collect();
                sorted.sort_by(|a, b| a.0.cmp(&b.0));
                self.count(sorted.len());
                for (name, value) in sorted {
                    self.text(name);
                    self.plain(value)?;
                }
            }
            Plain::List(items) => {
                self.bytes(b"l");
                self.count(items.len());
                for item in items {
                    self.plain(item)?;
                }
            }
            Plain::Variant { key, payload, .. } => {
                self.bytes(b"v");
                self.text(key);
                match payload {
                    Some(payload) => {
                        self.bytes(b"1");
                        self.plain(payload)?;
                    }
                    None => self.bytes(b"0"),
                }
            }
            Plain::Secret {
                identity, revealed, ..
            } => {
                // Its identity, never its value.
                self.bytes(b"S");
                self.bytes(if *revealed { b"r" } else { b"h" });
                self.text(identity);
            }
            Plain::File { path, identity } => self.file(path, identity.as_deref(), false)?,
            Plain::Dir { path, identity } => self.file(path, identity.as_deref(), true)?,
            Plain::Closure {
                input,
                function,
                captures,
            } => self.closure(*input, *function, captures)?,
            Plain::Step(node) => {
                let name = (self.step_of)(*node).ok_or_else(|| {
                    "a step's result not given yet, which only names a step".to_owned()
                })?;
                self.bytes(b"p");
                self.text(&name);
            }
            Plain::Sandbox { root, cwd, env } => {
                self.bytes(b"x");
                self.text(root);
                self.text(cwd);
                self.count(env.len());
                for (name, value) in env {
                    self.text(name);
                    self.text(value);
                }
            }
        }
        Ok(())
    }
}

/// What a step's identity is made of (see [Identities](#identities)).
#[derive(Clone, Copy)]
pub struct IdentityOf<'a> {
    /// Its callee, a closure's plain form.
    pub callee: &'a Plain,
    /// Its arguments, in parameter order.
    pub args: &'a [Plain],
    /// The contexts that name it, by key, sorted, each with its value: none,
    /// but to tell two steps apart that only their contexts do.
    pub context: &'a [(String, Plain)],
    /// The platform it runs on, as pekit describes it.
    pub platform: &'a str,
    /// The name of the file of each input number, which a function in its
    /// callee, arguments or context is named by.
    pub code_name: &'a dyn Fn(u32) -> Option<String>,
    /// The name of each step, by its node's number, whose result it is
    /// given, not given yet, which names it.
    pub step_of: &'a dyn Fn(u32) -> Option<String>,
}

/// The identity of a step made of `of` (see [Identities](#identities)).
///
/// # Errors
///
/// An argument that is no data, or a function of no file the build has.
pub fn identity(of: IdentityOf<'_>) -> Result<Digest, String> {
    let mut hasher = Sha256::new();
    let mut by_path = |_: &str, _: bool| Ok(Digest::of(b""));
    let code_of = |input: u32| (of.code_name)(input).map(|name| (name, Digest::of(b"")));
    let mut out = Encoder {
        hasher: &mut hasher,
        contents: &mut by_path,
        code_of: &code_of,
        step_of: of.step_of,
    };
    out.text("pekit-identity-1");
    out.plain(of.callee)?;
    out.count(of.args.len());
    for arg in of.args {
        out.plain(arg)?;
    }
    out.count(of.context.len());
    for (key, plain) in of.context {
        out.text(key);
        out.plain(plain)?;
    }
    out.text(of.platform);
    Ok(Digest::from_hasher(hasher))
}

/// What a program's key is made of (see `design/additive.md`, F1).
#[derive(Clone, Copy)]
pub struct ExecOf<'a> {
    /// The step's view: the chain of what made what it can see read-only.
    pub view: &'a Digest,
    /// Each input staged by the step so far, by its path, and its contents'
    /// digest.
    pub staged: &'a [(String, Digest)],
    /// The digests of the trees it can write: its own output directory's and
    /// its `/tmp`'s.
    pub writable: (&'a Digest, &'a Digest),
    /// The built-in, by its name.
    pub builtin: &'a str,
    /// Its arguments, each file named by its path alone, since its contents
    /// are the world's.
    pub args: &'a [Plain],
}

/// The key of a program a step runs (see `design/additive.md`, F1).
///
/// # Errors
///
/// An argument that is no data.
pub fn exec_key(of: ExecOf<'_>) -> Result<Digest, String> {
    let mut hasher = Sha256::new();
    let mut by_path = |path: &str, _: bool| Ok(Digest::of(path.as_bytes()));
    let none = |_: u32| None;
    let unnamed = |_: u32| None;
    let mut out = Encoder {
        hasher: &mut hasher,
        contents: &mut by_path,
        code_of: &none,
        step_of: &unnamed,
    };
    out.text("pekit-exec-1");
    out.text(&of.view.hex());
    out.count(of.staged.len());
    for (path, digest) in of.staged {
        out.text(path);
        out.text(&digest.hex());
    }
    out.text(&of.writable.0.hex());
    out.text(&of.writable.1.hex());
    out.text(of.builtin);
    out.count(of.args.len());
    for arg in of.args {
        out.plain(arg)?;
    }
    Ok(Digest::from_hasher(hasher))
}

/// The view `view` of a step, with the effect of the built-in `builtin` on
/// `args` folded in (see `design/additive.md`, F1).
///
/// # Errors
///
/// An argument that is no data.
pub fn folded(view: &Digest, builtin: &str, args: &[Plain]) -> Result<Digest, String> {
    let none = Digest::of(b"");
    exec_key(ExecOf {
        view,
        staged: &[],
        writable: (&none, &none),
        builtin: &format!("fold:{builtin}"),
        args,
    })
}
