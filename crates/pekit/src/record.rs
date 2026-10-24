//! Step records: what a hermetic step gave, kept by its receipt.
//!
//! # Records
//!
//! A step that succeeds is recorded (see `design/engine.md`, E3): its
//! receipt, its result, and how long it took, which eviction weighs later.
//! Every file and directory a step's result names is kept in the store
//! ([`keep_files`]), whatever else it holds, since later steps are given
//! them. Its result is then kept as a [`Stored`] value, plain data whose
//! files and directories are named by their paths and contents
//! ([`stored`]), so that a later run with the same receipt gives the result
//! back, its files at their paths ([`Stored::files`]), without running the
//! step. A result that holds a secret is no record's: a secret is never
//! stored ([`StoreError::Secret`]), so such a step runs every time, though
//! its files are kept as any step's are.

use std::collections::HashMap;
use std::hash::BuildHasher;
use std::io;
use std::path::PathBuf;

use gcsl::eval::plain::Plain;
use gcsl::value::Quantity;
use serde::{Deserialize, Serialize};

use crate::store::{Digest, FileEntry, Store};

/// A step's record (see [Records](#records)).
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct StepRecord {
    /// Its receipt.
    pub receipt: Digest,
    /// What it gave.
    pub result: Stored,
    /// How long it ran, in milliseconds.
    pub duration_ms: u64,
}

/// A step's result as a record keeps it: plain data, every file and
/// directory by its path and contents (see [Records](#records)).
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(tag = "kind", content = "value", rename_all = "lowercase")]
pub enum Stored {
    /// An `Int`.
    Int(i64),
    /// A `Float`.
    Float(f64),
    /// A `Bool`.
    Bool(bool),
    /// A `String`.
    Str(String),
    /// A `Duration`, its nanoseconds.
    Duration(i64),
    /// A `Size`, its bytes.
    Size(i64),
    /// The empty record.
    Empty,
    /// A record, its fields in its order.
    Record(Vec<(String, Stored)>),
    /// A list.
    List(Vec<Stored>),
    /// A variant: its key's, its enum's and its shown text, and its payload.
    Variant(String, String, String, Option<Box<Stored>>),
    /// A file: its path, identity and contents.
    File(String, Option<String>, FileEntry),
    /// A directory: its path, identity and tree's digest.
    Dir(String, Option<String>, Digest),
    /// A sandbox: its copy, working directory and environment.
    Sandbox(String, String, Vec<(String, String)>),
}

/// Why a result cannot be recorded.
#[derive(Debug)]
pub enum StoreError {
    /// It holds a secret, which is never stored.
    Secret,
    /// A file or directory it names was not kept ([`keep_files`]).
    Unkept(String),
    /// A file or directory it names could not be put into the store.
    Io {
        /// The path, as GCSL code names it.
        path: String,
        /// Why.
        error: io::Error,
    },
}

/// Puts every file and directory `plain`, a step's result, names into
/// `store`, from where `host` says each is on the host's file system: what
/// the store keeps of each, by its path (see [Records](#records)).
///
/// # Errors
///
/// A file or directory it names that cannot be read: one the step did not
/// make, say.
pub fn keep_files(
    plain: &Plain,
    store: &Store,
    host: &dyn Fn(&str) -> PathBuf,
) -> Result<HashMap<String, Kept>, StoreError> {
    let mut kept = HashMap::new();
    for (path, dir) in plain.files() {
        if kept.contains_key(path) {
            continue;
        }
        let at = host(path);
        let io = |error| StoreError::Io {
            path: path.to_owned(),
            error,
        };
        let entry = if dir {
            Kept::Dir(store.put_tree(&at).map_err(io)?)
        } else {
            Kept::File(store.put_file(&at).map_err(io)?)
        };
        kept.insert(path.to_owned(), entry);
    }
    Ok(kept)
}

/// `plain`, a step's result, as a record keeps it, each file and directory
/// it names by what the store keeps of it, from `kept` (see
/// [Records](#records)).
///
/// # Errors
///
/// That it holds a secret, or names a file or directory `kept` lacks.
pub fn stored<S: BuildHasher>(
    plain: &Plain,
    kept: &HashMap<String, Kept, S>,
) -> Result<Stored, StoreError> {
    let of = |path: &str| {
        kept.get(path)
            .copied()
            .ok_or_else(|| StoreError::Unkept(path.to_owned()))
    };
    Ok(match plain {
        Plain::Int(n) => Stored::Int(*n),
        Plain::Float(x) => Stored::Float(*x),
        Plain::Bool(b) => Stored::Bool(*b),
        Plain::Str(s) => Stored::Str(s.clone()),
        Plain::Quantity(Quantity::Duration, amount) => Stored::Duration(*amount),
        Plain::Quantity(Quantity::Size, amount) => Stored::Size(*amount),
        Plain::Empty => Stored::Empty,
        Plain::Record(fields) => Stored::Record(
            fields
                .iter()
                .map(|(name, value)| Ok((name.clone(), stored(value, kept)?)))
                .collect::<Result<_, StoreError>>()?,
        ),
        Plain::List(items) => Stored::List(
            items
                .iter()
                .map(|item| stored(item, kept))
                .collect::<Result<_, _>>()?,
        ),
        Plain::Variant {
            key,
            of: enum_of,
            shown,
            payload,
        } => Stored::Variant(
            key.clone(),
            enum_of.clone(),
            shown.clone(),
            match payload {
                Some(payload) => Some(Box::new(stored(payload, kept)?)),
                None => None,
            },
        ),
        Plain::Secret { .. } => return Err(StoreError::Secret),
        Plain::File { path, identity } => match of(path)? {
            Kept::File(entry) => Stored::File(path.clone(), identity.clone(), entry),
            Kept::Dir(_) => return Err(StoreError::Unkept(path.clone())),
        },
        Plain::Dir { path, identity } => match of(path)? {
            Kept::Dir(tree) => Stored::Dir(path.clone(), identity.clone(), tree),
            Kept::File(_) => return Err(StoreError::Unkept(path.clone())),
        },
        Plain::Sandbox { root, cwd, env } => {
            Stored::Sandbox(root.clone(), cwd.clone(), env.clone())
        }
    })
}

impl Stored {
    /// The plain data this is, paths as they were: what the step gave.
    #[must_use]
    pub fn plain(&self) -> Plain {
        match self {
            Stored::Int(n) => Plain::Int(*n),
            Stored::Float(x) => Plain::Float(*x),
            Stored::Bool(b) => Plain::Bool(*b),
            Stored::Str(s) => Plain::Str(s.clone()),
            Stored::Duration(amount) => Plain::Quantity(Quantity::Duration, *amount),
            Stored::Size(amount) => Plain::Quantity(Quantity::Size, *amount),
            Stored::Empty => Plain::Empty,
            Stored::Record(fields) => Plain::Record(
                fields
                    .iter()
                    .map(|(name, value)| (name.clone(), value.plain()))
                    .collect(),
            ),
            Stored::List(items) => Plain::List(items.iter().map(Stored::plain).collect()),
            Stored::Variant(key, of, shown, payload) => Plain::Variant {
                key: key.clone(),
                of: of.clone(),
                shown: shown.clone(),
                payload: payload.as_ref().map(|payload| Box::new(payload.plain())),
            },
            Stored::File(path, identity, _) => Plain::File {
                path: path.clone(),
                identity: identity.clone(),
            },
            Stored::Dir(path, identity, _) => Plain::Dir {
                path: path.clone(),
                identity: identity.clone(),
            },
            Stored::Sandbox(root, cwd, env) => Plain::Sandbox {
                root: root.clone(),
                cwd: cwd.clone(),
                env: env.clone(),
            },
        }
    }

    /// Every file and directory this names, at any depth, with what the
    /// store keeps of it: what must be put back at its path before the result
    /// is used.
    #[must_use]
    pub fn files(&self) -> Vec<(&str, Kept)> {
        let mut found = Vec::new();
        let mut pending = vec![self];
        while let Some(stored) = pending.pop() {
            match stored {
                Stored::File(path, _, entry) => found.push((path.as_str(), Kept::File(*entry))),
                Stored::Dir(path, _, tree) => found.push((path.as_str(), Kept::Dir(*tree))),
                Stored::Record(fields) => {
                    pending.extend(fields.iter().rev().map(|(_, value)| value));
                }
                Stored::List(items) => pending.extend(items.iter().rev()),
                Stored::Variant(_, _, _, Some(payload)) => pending.push(payload),
                _ => {}
            }
        }
        found
    }
}

/// What the store keeps of a file or a directory a result names.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kept {
    /// A file.
    File(FileEntry),
    /// A directory, by its tree.
    Dir(Digest),
}
