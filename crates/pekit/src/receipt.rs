//! Receipts: the key a hermetic step's result is cached by.
//!
//! # Receipts
//!
//! A step's *receipt* is a SHA-256 over a canonical encoding of everything
//! that can change what it gives (see `design/engine.md`, E3):
//!
//! - its code: the path and contents' digest of every file whose source
//!   makes it, in order;
//! - its arguments, its function's captures and the context it runs in,
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
//! - its own output directory, `/pekit-out/<target>/<step>`, which names
//!   the files it makes;
//! - the platform it runs on.
//!
//! Its attributes are not on it: `#[retry]` and `#[timeout]` change how it
//! runs, not what it gives.

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
    /// The context it runs in, by key, sorted.
    pub context: &'a [(String, Plain)],
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
    let mut out = Encoder {
        hasher: &mut hasher,
        contents,
        code_of: of.code_of,
    };
    out.text("pekit-receipt-2");
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
        out.plain(plain)?;
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
