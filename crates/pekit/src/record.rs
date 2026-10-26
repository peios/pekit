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

use crate::store::{Digest, FileEntry, Store, Tree, TreeEntry};

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
    /// It holds a function, which is no data.
    NotData,
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
        Plain::Closure { .. } => return Err(StoreError::NotData),
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

/// What differs between `first` and `again`, two results of one step given
/// the same inputs, as `--check-reproducible` reports it: each file whose
/// contents or mode differ, by its path, each file a directory holds in one
/// and not the other, or differently, found through `store`'s trees, and
/// `the value` for a difference in no file. Empty if they are the same.
#[must_use]
pub fn differences(store: &Store, first: &Stored, again: &Stored) -> Vec<String> {
    let mut found = Vec::new();
    differ(store, (first, again), &mut found);
    found.dedup();
    found
}

/// Adds to `found` what differs between `first` and `again`
/// ([`differences`]).
fn differ(store: &Store, (first, again): (&Stored, &Stored), found: &mut Vec<String>) {
    match (first, again) {
        (Stored::File(path, _, one), Stored::File(other_path, _, two)) if path == other_path => {
            if one != two {
                found.push(path.clone());
            }
        }
        (Stored::Dir(path, _, one), Stored::Dir(other_path, _, two)) if path == other_path => {
            if one != two {
                trees_differ(store, path, (one, two), found);
            }
        }
        (Stored::Record(one), Stored::Record(two))
            if one.len() == two.len() && one.iter().zip(two).all(|((a, _), (b, _))| a == b) =>
        {
            for ((_, a), (_, b)) in one.iter().zip(two) {
                differ(store, (a, b), found);
            }
        }
        (Stored::List(one), Stored::List(two)) if one.len() == two.len() => {
            for (a, b) in one.iter().zip(two) {
                differ(store, (a, b), found);
            }
        }
        _ => {
            if first != again {
                found.push("the value".to_owned());
            }
        }
    }
}

/// Adds to `found` each path under `path` at which the trees `one` and
/// `two` differ: a file of one only, or of both with other contents; the
/// directory itself if either tree cannot be read.
fn trees_differ(
    store: &Store,
    path: &str,
    (one, two): (&Digest, &Digest),
    found: &mut Vec<String>,
) {
    let (Ok(one), Ok(two)) = (store.read_tree(one), store.read_tree(two)) else {
        found.push(path.to_owned());
        return;
    };
    let mut names: Vec<&str> = one
        .entries
        .iter()
        .chain(&two.entries)
        .map(TreeEntry::name)
        .collect();
    names.sort_unstable();
    names.dedup();
    for name in names {
        let at = format!("{path}/{name}");
        let find = |tree: &Tree| {
            tree.entries
                .iter()
                .find(|entry| entry.name() == name)
                .cloned()
        };
        match (find(&one), find(&two)) {
            (Some(TreeEntry::Dir { tree: a, .. }), Some(TreeEntry::Dir { tree: b, .. })) => {
                if a != b {
                    trees_differ(store, &at, (&a, &b), found);
                }
            }
            (a, b) => {
                if a != b {
                    found.push(at);
                }
            }
        }
    }
}

/// What the store keeps by a step's base, its receipt with no context
/// (see `design/engine.md`, R2): each set of context keys a run of a step
/// of that base read, sorted, by which a step's receipt is made to look it
/// up.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Manifest {
    /// Each set of keys read, each sorted, in the order first recorded.
    pub sets: Vec<Vec<String>>,
}

/// The key the manifest of the steps whose base is `base` is kept by, apart
/// from every receipt.
#[must_use]
pub fn manifest_key(base: &Digest) -> Digest {
    Digest::of(format!("pekit-manifest-1:{}", base.hex()).as_bytes())
}

/// A program a step ran, as the store keeps it by its key (see
/// `design/additive.md`, F1): what it gave, and the digests of the trees it
/// could write as it left them, the step's own output directory and its
/// `/tmp`, whose contents the store holds.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct ExecRecord {
    /// What it gave.
    pub result: Stored,
    /// The own output directory's tree after it.
    pub own: Digest,
    /// `/tmp`'s tree after it.
    pub tmp: Digest,
    /// How long it ran, in milliseconds: what finding it saves.
    #[serde(default)]
    pub duration_ms: u64,
}
