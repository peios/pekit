//! The content store: files by the digest of their contents, directories as
//! trees of digests, and step records apart from both.
//!
//! # The store
//!
//! A store is a directory, by default `~/.cache/pekit`, which every
//! workspace and every pekit process shares:
//!
//! - `cas/` holds each file's contents once, by its SHA-256 digest, in a
//!   directory named by the digest's first two hex digits, read-only. Being
//!   executable is no part of contents, but a file's mode is shared by every
//!   link to it, so an executable copy, `DIGEST.x`, sits beside the plain
//!   one when one is needed. So do numbered copies of a blob, `DIGEST.r1`
//!   and on, `DIGEST.x.r1` for an executable one, made the first time they
//!   are needed and kept: a tree or a composition that holds one content at
//!   several paths has each a file of its own, the first a link to the blob
//!   and each other a link to a copy, so that placing it copies nothing
//!   that was copied before ([`Store::link_copy`]). No record names a copy,
//!   so an eviction removes them first, as costing nothing to make again;
//! - a directory is a *tree*: its entries, sorted by name, each a file by
//!   digest and executability, a directory by its own tree's digest, or a
//!   symbolic link by its target, encoded canonically and stored as a blob
//!   itself, so that a tree's digest is its whole content's, as REAPI's
//!   directories are;
//! - `records/` holds each step's record by its receipt, and is never
//!   evicted with contents (see `design/engine.md`, E4);
//! - `tmp/` holds what is being written: a blob or a record is written
//!   there in full and renamed into place, so that a reader, or another
//!   process, never sees one in part;
//! - `complete/` marks each tree found to have every content it names, at
//!   any depth, by its digest, so that a record's tree is walked for that
//!   once, not in every build that finds the record ([`Store::holds_tree`]).
//!   An eviction removes every mark before it removes a blob;
//! - `trees/` holds trees made once as directories, of links to their
//!   blobs, for steps to mount (see [Materialised
//!   trees](trees#materialised-trees)), which an eviction removes before
//!   it removes a blob too.
//!
//! A file is put into the store by hashing it as it is copied ([`Store::put_file`]),
//! and taken out by a hard link to its blob, or a copy where a link cannot
//! be made, as across file systems ([`Store::materialise_file`]).
//!
//! # Trees seen before
//!
//! A tree put into the store again and again, as a step's writable trees
//! are around each of its programs, is mostly what it was: [`Store::put_tree_seen`]
//! keeps what each file was, its [`Stamp`], when it was last put, and reads
//! again only a file whose stamp differs, or whose blob is gone. A file
//! changed since has another stamp, since its change time is the time of
//! the change, unless it was changed in the same tick of the file system's
//! clock as it was stamped, which a stamp taken in the tick a put begins in,
//! or after, by the clock of the tree's own file system, is not trusted
//! for: as git keeps its index. A file trusted by its stamp has its blob
//! looked for once the walk is over, and only if the tree is not marked
//! complete; all found, the tree is marked so, and the next walk to come to
//! the same tree looks for none. A file that is a
//! link to a blob this process linked, by its device and inode, is that
//! blob, and is not read. A file read is hashed before it is copied, and
//! not copied if its blob is there. A directory said to be fixed, such as a
//! sealed composition, is walked once until it is said to have changed.
//!
//! # Stamps kept
//!
//! A build keeps the stamps of a workspace's files, and of its libraries'
//! files, in the store's `seen/` ([`stamps_path`]), to read again before
//! the next: a hundred thousand of them for a catalog, which a build reads
//! whole before it can walk anything. So they are kept as bytes, not text:
//! `PKSTAMP1`, how many there are, and each in its path's order, the length
//! its path shares with the one before and the rest of it, then its stamp,
//! its contents' digest and size, and whether it is executable, each number
//! little-endian. Stamps that cannot be read, from a pekit that kept them
//! otherwise, say, are none, and a build that finds none reads each file
//! again. Those an older pekit kept as JSON beside them are read once and
//! kept anew.
//!
//! # Reading ahead
//!
//! A walk spends most of its time asking the file system for each entry's
//! stamp, one at a time, and a catalog's libraries are a hundred thousand
//! files. So before a walk, its tree is read ahead ([`read_ahead`]): level
//! by level, each directory of the level listed, then each entry of the
//! level stamped, by several threads where there are enough of them. The
//! walk then takes each directory's listing and each entry's metadata as
//! read ahead, and reads only what was not. What changes between the two is
//! what changes while a walk reads, which [Trees read while they
//! change](#trees-read-while-they-change) covers.
//!
//! # Putting a tree at once
//!
//! A blob is flushed to the disk before it is renamed into place, so that a
//! blob under its digest's name is whole even after the machine loses
//! power, and is trusted as it is found. Flushed one by one, as a tree is
//! walked, each waits for a commit of the file system's journal of its own,
//! which a tree of many small files, a kernel's build, spends hours on. So
//! a tree is put at once ([`Store::put_tree_mounted`],
//! [`Store::put_tree_seen_at`]): each blob it holds that the store lacks,
//! and each of its trees' encodings, is written whole into `tmp/` as the
//! tree is walked, then all are flushed together, by several threads, so
//! that one commit serves many, and only then renamed into place, in the
//! order they were written, a tree's entries before it. A put that fails
//! removes what it wrote.
//!
//! # Trees read while they change
//!
//! A walk reads a tree one entry at a time, so a tree another process
//! changes as it is walked, as a package repository is while a package is
//! published into it, can be read in part before the change and in part
//! after: an entry listed and gone before it is read, or a file that names
//! another the walk passed before it was written. [`Store::put_tree_steady`]
//! and [`Store::put_file_steady`] keep what the walk saw, the stamp of the
//! root and of each entry and the names each directory listed, and look
//! again once it is over. If anything differs, or is gone, the tree changed
//! as it was read, and what was read is not given, even if the walk failed
//! because of it: [`Steady::Changed`] says where, for the caller to read it
//! again. A walk that failed while nothing changed fails. A file changed in
//! place, keeping its size, in the same tick of the file system's clock as
//! it was stamped, and so keeping its stamp, is not seen; a writer that
//! renames each file into place, as ppk does, is always seen.

use std::cell::{Cell, RefCell};
use std::collections::{HashMap, HashSet};
use std::fs::{self, File};
use std::io::{self, Read, Write};
use std::os::unix::fs::{MetadataExt, PermissionsExt, symlink};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use serde::{Deserialize, Serialize};
use sha2::{Digest as _, Sha256};

pub mod trees;

/// What a file was when it was put into the store (see [Trees seen
/// before](#trees-seen-before)): its device and inode, size, mode, and the
/// times it was modified and changed.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Stamp {
    /// Its device and inode.
    node: (u64, u64),
    /// Its size.
    size: u64,
    /// Its mode.
    mode: u32,
    /// When it was modified, in seconds and nanoseconds.
    modified: (i64, i64),
    /// When it was changed, in seconds and nanoseconds.
    changed: (i64, i64),
}

impl Stamp {
    /// The stamp of the file whose metadata is `meta`.
    #[must_use]
    pub fn of(meta: &fs::Metadata) -> Self {
        Self {
            node: (meta.dev(), meta.ino()),
            size: meta.size(),
            mode: meta.mode(),
            modified: (meta.mtime(), meta.mtime_nsec()),
            changed: (meta.ctime(), meta.ctime_nsec()),
        }
    }
}

/// What each file was when it was last put into the store, by its path,
/// and the digests of the directories that cannot change, while they can't
/// (see [Trees seen before](#trees-seen-before)).
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct Seen {
    /// Each file's stamp, and what the store keeps of it: kept in its
    /// paths' order, so that the same stamps are kept as the same bytes.
    #[serde(serialize_with = "in_order")]
    files: HashMap<PathBuf, (Stamp, FileEntry)>,
    /// The directories that cannot change.
    #[serde(skip)]
    fixed: Vec<PathBuf>,
    /// The digest of each of them put, by its path.
    #[serde(skip)]
    dirs: HashMap<PathBuf, Digest>,
    /// The digest of the tree mounted on each mount point, by where it is,
    /// which a walk does not read (see [Materialised
    /// trees](trees#materialised-trees)).
    #[serde(skip)]
    mounted: HashMap<PathBuf, Digest>,
    /// Each file a walk found there since this was read, whose stamp is
    /// known to be of a file present.
    #[serde(skip)]
    visited: HashSet<PathBuf>,
    /// Whether a file's stamp was added, changed or forgotten since this
    /// was read, so that it is to be kept again.
    #[serde(skip)]
    edited: bool,
}

impl Seen {
    /// Says that the directories `fixed` cannot change, until
    /// [`Seen::changed`] says they did.
    pub fn fix(&mut self, fixed: Vec<PathBuf>) {
        self.fixed = fixed;
    }

    /// Forgets each file that is no longer there: one a walk did not find
    /// since this was read is looked for.
    pub fn retain_present(&mut self) {
        let unvisited: Vec<&PathBuf> = self
            .files
            .keys()
            .filter(|path| !self.visited.contains(*path))
            .collect();
        let gone: HashSet<PathBuf> = unvisited
            .iter()
            .zip(in_parallel(&unvisited, |path| path.exists()))
            .filter(|(_, there)| !there)
            .map(|(path, _)| (*path).clone())
            .collect();
        if !gone.is_empty() {
            self.files.retain(|path, _| !gone.contains(path));
            self.edited = true;
        }
    }

    /// Forgets each file no walk found since this was read: of trees each
    /// walked whole, those no longer there.
    pub fn retain_visited(&mut self) {
        let (count, visited) = (self.files.len(), &self.visited);
        self.files.retain(|path, _| visited.contains(path));
        self.edited |= self.files.len() != count;
    }

    /// Notes that the file at `path`, there, has the stamp and contents
    /// `known`.
    fn note(&mut self, path: &Path, known: (Stamp, FileEntry)) {
        self.visited.insert(path.to_path_buf());
        self.edited |= self.files.insert(path.to_path_buf(), known) != Some(known);
    }

    /// Forgets the stamp of the file at `path`.
    fn forget(&mut self, path: &Path) {
        self.edited |= self.files.remove(path).is_some();
    }

    /// Says that the directories said to be fixed could have changed.
    pub fn changed(&mut self) {
        self.dirs.clear();
    }

    /// Says that the tree of each digest of `mounted` is mounted on the
    /// mount point it is by.
    pub fn mount(&mut self, mounted: HashMap<PathBuf, Digest>) {
        self.mounted = mounted;
    }
}

/// The entries of the directory at `path`, each its name, kind and path,
/// read whole and the directory closed: a walk that goes down a level
/// holds no descriptor open per level above, so a tree deeper than this
/// process may open descriptors can be read. A `DirEntry` kept would keep
/// its directory open.
fn listing(path: &Path) -> io::Result<Vec<(std::ffi::OsString, fs::FileType, PathBuf)>> {
    fs::read_dir(path)?
        .map(|entry| {
            let entry = entry?;
            Ok((entry.file_name(), entry.file_type()?, entry.path()))
        })
        .collect()
}

/// The most threads a walk reads ahead on, or looks for changes on.
const MOST_THREADS: usize = 8;

/// The fewest stamps a thread is given to take, so that it is worth its
/// start.
const PER_THREAD: usize = 256;

/// `each` of every one of `items`, in their order, on as many threads as
/// there are [`PER_THREAD`] of them, to [`MOST_THREADS`] and the machine's
/// processors (see [Reading ahead](#reading-ahead)).
fn in_parallel<T: Sync, U: Send>(items: &[T], each: impl Fn(&T) -> U + Sync) -> Vec<U> {
    in_parallel_by(items, PER_THREAD, each)
}

/// [`in_parallel`], each thread given at least `per` of `items`.
pub(crate) fn in_parallel_by<T: Sync, U: Send>(
    items: &[T],
    per: usize,
    each: impl Fn(&T) -> U + Sync,
) -> Vec<U> {
    let processors = std::thread::available_parallelism().map_or(1, usize::from);
    let threads = (items.len() / per.max(1)).clamp(1, MOST_THREADS.min(processors));
    if threads == 1 {
        return items.iter().map(each).collect();
    }
    let each = &each;
    std::thread::scope(|scope| {
        let parts: Vec<_> = items
            .chunks(items.len().div_ceil(threads))
            .map(|part| scope.spawn(move || part.iter().map(each).collect::<Vec<U>>()))
            .collect();
        parts
            .into_iter()
            .flat_map(|part| {
                part.join()
                    .unwrap_or_else(|panic| std::panic::resume_unwind(panic))
            })
            .collect()
    })
}

/// An entry of a directory as a walk reads it: its name, its path, and
/// its metadata, if it was read ahead ([`read_ahead`]).
type Listed = (
    std::ffi::OsString,
    PathBuf,
    Option<io::Result<fs::Metadata>>,
);

/// The tree at `root` read ahead of a walk (see [Reading
/// ahead](#reading-ahead)): each directory's entries with their metadata,
/// by its path, but those under a directory `skip` says to leave, or that
/// could not be listed, which the walk lists itself.
fn read_ahead(root: &Path, skip: &(dyn Fn(&Path) -> bool + Sync)) -> HashMap<PathBuf, Vec<Listed>> {
    let mut ahead = HashMap::new();
    let mut level = vec![root.to_path_buf()];
    while !level.is_empty() {
        let listed: Vec<(PathBuf, Vec<(std::ffi::OsString, PathBuf)>)> =
            in_parallel(&level, |dir| {
                let names = fs::read_dir(dir).and_then(|entries| {
                    entries
                        .map(|entry| entry.map(|entry| (entry.file_name(), entry.path())))
                        .collect::<io::Result<Vec<_>>>()
                });
                Some((dir.clone(), names.ok()?))
            })
            .into_iter()
            .flatten()
            .collect();
        let paths: Vec<&PathBuf> = listed
            .iter()
            .flat_map(|(_, names)| names.iter().map(|(_, at)| at))
            .collect();
        let mut metas = in_parallel(&paths, |at| fs::symlink_metadata(at)).into_iter();
        level = Vec::new();
        for (dir, names) in listed {
            let entries: Vec<Listed> = names
                .into_iter()
                .zip(metas.by_ref())
                .map(|((name, at), meta)| {
                    if meta.as_ref().is_ok_and(fs::Metadata::is_dir) && !skip(&at) {
                        level.push(at.clone());
                    }
                    (name, at, Some(meta))
                })
                .collect();
            ahead.insert(dir, entries);
        }
    }
    ahead
}

/// The bytes free to write on the file system `path` is on, as one who is
/// not its administrator may.
///
/// # Errors
///
/// That the file system cannot be asked.
pub fn free_space(path: &Path) -> io::Result<u64> {
    let stat = rustix::fs::statvfs(path)?;
    Ok(stat.f_bavail.saturating_mul(stat.f_frsize))
}

/// Now, as the clock of the file system `path` is on tells it, as coarsely
/// as that file system keeps times: the change time of a file made beside
/// it, and removed.
///
/// # Errors
///
/// That no file can be made beside it.
pub fn clock_beside(path: &Path) -> io::Result<(i64, i64)> {
    let probe = path.with_file_name(format!(".pekit-probe-{}", crate::sweep::token()));
    File::create(&probe)?;
    let now = fs::symlink_metadata(&probe).map(|meta| (meta.ctime(), meta.ctime_nsec()));
    fs::remove_file(&probe)?;
    now
}

/// A walk of a tree being put into the store by [`Store::put_tree_seen`].
struct Walk<'a> {
    /// What each file was when it was last put.
    seen: &'a mut Seen,
    /// Whether an entry makes the tree one that can't be kept.
    refuse: &'a dyn Fn(&Path, &fs::Metadata) -> bool,
    /// When the put began, by the file system's clock.
    now: (i64, i64),
    /// What is no part of the tree, by where it is.
    left_out: &'a [PathBuf],
    /// What the walk saw, if it is to be looked at again (see [Trees read
    /// while they change](#trees-read-while-they-change)).
    watch: Option<Watch>,
    /// Each file taken by its stamp to hold what it held, its blob not yet
    /// looked for (see [Trees seen before](#trees-seen-before)).
    trusted: Vec<(PathBuf, FileEntry)>,
    /// Each tree taken as put before, unwalked: a fixed directory's, or a
    /// mounted one's.
    unwalked: Vec<Digest>,
    /// What of the tree was read ahead of the walk, which it takes (see
    /// [Reading ahead](#reading-ahead)).
    ahead: HashMap<PathBuf, Vec<Listed>>,
}

impl Walk<'_> {
    /// A walk of what `seen` knows, begun at `now`.
    fn new<'a>(
        (seen, now): (&'a mut Seen, (i64, i64)),
        refuse: &'a dyn Fn(&Path, &fs::Metadata) -> bool,
        left_out: &'a [PathBuf],
        watch: Option<Watch>,
    ) -> Walk<'a> {
        Walk {
            seen,
            refuse,
            now,
            left_out,
            watch,
            trusted: Vec::new(),
            unwalked: Vec::new(),
            ahead: HashMap::new(),
        }
    }

    /// Reads the tree at `root` ahead of the walk, but what it leaves out
    /// and the directories it knows.
    fn read_ahead(&mut self, root: &Path) {
        let (left_out, seen) = (self.left_out, &*self.seen);
        let skip = |at: &Path| {
            left_out.iter().any(|out| out == at)
                || seen.mounted.contains_key(at)
                || seen.dirs.contains_key(at)
        };
        self.ahead = read_ahead(root, &skip);
    }

    /// Forgets what the walk found, to walk again.
    fn restart(&mut self) {
        self.trusted.clear();
        self.unwalked.clear();
        self.ahead.clear();
        self.seen.dirs.clear();
        if let Some(watch) = &mut self.watch {
            *watch = Watch::default();
        }
    }
}

/// Whether the files a walk trusted by their stamps have their blobs (see
/// [`Store::trusted_held`]).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Held {
    /// Known so without looking: none was trusted, or the tree is
    /// complete.
    Known,
    /// Looked for, and each found or put again.
    Checked,
    /// One is not what its stamp said: the tree is to be walked again.
    Again,
}

/// What a read of a tree, or a file, that another process may be changing
/// came to (see [Trees read while they change](#trees-read-while-they-change)).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Steady<T> {
    /// What was read, which nothing changed as it was read.
    Read(T),
    /// The first path found changed, or gone, since it was read.
    Changed(PathBuf),
}

/// What a walk saw, to be looked at again once it is over (see [Trees
/// read while they change](#trees-read-while-they-change)): each
/// directory's names, which an entry made, removed or renamed changes, and
/// each other entry's stamp, which a file written or renamed over changes.
/// A directory's own stamp is not kept, since it changes as much when an
/// entry left out of the tree is made.
#[derive(Debug, Default)]
struct Watch {
    /// Each directory's names but those left out, sorted, by its path.
    listings: Vec<(PathBuf, Vec<std::ffi::OsString>)>,
    /// Each file's and link's stamp, by its path.
    stamps: Vec<(PathBuf, Stamp)>,
}

impl Watch {
    /// The first path whose names or stamp differ from what the walk saw,
    /// or that is gone, if any, what is at `left_out` no part of a
    /// directory's names.
    fn changed(&self, left_out: &[PathBuf]) -> Option<PathBuf> {
        let listed = self
            .listings
            .iter()
            .find(|(path, names)| names_in(path, left_out).ok().as_ref() != Some(names));
        if let Some((path, _)) = listed {
            return Some(path.clone());
        }
        let differ = in_parallel(&self.stamps, |(path, stamp)| {
            fs::symlink_metadata(path).map(|meta| Stamp::of(&meta)).ok() != Some(*stamp)
        });
        self.stamps
            .iter()
            .zip(differ)
            .find_map(|((path, _), differs)| differs.then(|| path.clone()))
    }
}

/// The names of the entries of the directory at `path`, sorted, but those
/// at `left_out`.
fn names_in(path: &Path, left_out: &[PathBuf]) -> io::Result<Vec<std::ffi::OsString>> {
    let mut names = Vec::new();
    for entry in fs::read_dir(path)? {
        let entry = entry?;
        if !left_out.contains(&entry.path()) {
            names.push(entry.file_name());
        }
    }
    names.sort();
    Ok(names)
}

/// A SHA-256 digest, of a file's contents, a tree or a receipt.
#[derive(Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(into = "String", try_from = "String")]
pub struct Digest([u8; 32]);

impl Digest {
    /// The digest of `bytes`.
    #[must_use]
    pub fn of(bytes: &[u8]) -> Self {
        Self(Sha256::digest(bytes).into())
    }

    /// The digest a finished hasher holds.
    #[must_use]
    pub fn from_hasher(hasher: Sha256) -> Self {
        Self(hasher.finalize().into())
    }

    /// The digest of the bytes `bytes`, as [`Digest::bytes`] gives them.
    #[must_use]
    pub fn from_bytes(bytes: [u8; 32]) -> Self {
        Self(bytes)
    }

    /// Its 64 lower-case hex digits.
    #[must_use]
    pub fn hex(&self) -> String {
        let mut hex = String::with_capacity(64);
        for byte in self.0 {
            hex.push(char::from(b"0123456789abcdef"[usize::from(byte >> 4)]));
            hex.push(char::from(b"0123456789abcdef"[usize::from(byte & 0xf)]));
        }
        hex
    }

    /// The digest whose hex digits are `hex`, if they are 64 of them.
    #[must_use]
    pub fn from_hex(hex: &str) -> Option<Self> {
        let bytes = hex.as_bytes();
        if bytes.len() != 64 {
            return None;
        }
        let digit = |byte: u8| match byte {
            b'0'..=b'9' => Some(byte - b'0'),
            b'a'..=b'f' => Some(byte - b'a' + 10),
            _ => None,
        };
        let mut digest = [0u8; 32];
        for (at, pair) in digest.iter_mut().zip(bytes.chunks(2)) {
            *at = digit(pair[0])? << 4 | digit(pair[1])?;
        }
        Some(Self(digest))
    }

    /// Its bytes.
    #[must_use]
    pub const fn bytes(&self) -> &[u8; 32] {
        &self.0
    }
}

impl std::fmt::Debug for Digest {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Digest({})", self.hex())
    }
}

impl std::fmt::Display for Digest {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.hex())
    }
}

impl From<Digest> for String {
    fn from(digest: Digest) -> Self {
        digest.hex()
    }
}

impl TryFrom<String> for Digest {
    type Error = String;

    fn try_from(hex: String) -> Result<Self, Self::Error> {
        Digest::from_hex(&hex).ok_or_else(|| format!("`{hex}` is no SHA-256 digest"))
    }
}

/// A file's contents and mode, as the store keeps it.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct FileEntry {
    /// Its contents' digest.
    pub digest: Digest,
    /// Its size, in bytes.
    pub size: u64,
    /// Whether it is executable.
    pub executable: bool,
}

/// One entry of a tree (see [The store](#the-store)).
#[derive(Clone, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "lowercase")]
pub enum TreeEntry {
    /// A file.
    File {
        /// Its name in the directory.
        name: String,
        /// Its contents and mode.
        file: FileEntry,
    },
    /// A directory, by its tree's digest.
    Dir {
        /// Its name in the directory.
        name: String,
        /// Its tree's digest.
        tree: Digest,
    },
    /// A symbolic link, by its target.
    Link {
        /// Its name in the directory.
        name: String,
        /// What it points to, as written.
        target: String,
    },
}

impl TreeEntry {
    /// Its name in its directory.
    #[must_use]
    pub fn name(&self) -> &str {
        match self {
            TreeEntry::File { name, .. }
            | TreeEntry::Dir { name, .. }
            | TreeEntry::Link { name, .. } => name,
        }
    }
}

/// A directory's entries, sorted by name (see [The store](#the-store)).
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Tree {
    /// Its entries, sorted by name.
    pub entries: Vec<TreeEntry>,
}

/// The number of the next temporary file this process writes.
static NEXT_TEMP: AtomicU64 = AtomicU64::new(0);

/// How many threads flush the blobs of a tree put at once.
const FLUSHERS: usize = 16;

thread_local! {
    /// The blobs this thread has written whole into `tmp/` and not yet
    /// flushed or put in place, while it puts a tree at once (see [Putting
    /// a tree at once](#putting-a-tree-at-once)).
    static AT_ONCE: RefCell<Option<Vec<Unsettled>>> = const { RefCell::new(None) };
    /// Whether this thread puts files the store may take as its own (see
    /// [`Store::adopting`]).
    static ADOPTING: Cell<bool> = const { Cell::new(false) };
}

/// Puts this thread back to adopting as it was, as it is dropped.
struct Adopting(bool);

impl Drop for Adopting {
    fn drop(&mut self) {
        ADOPTING.set(self.0);
    }
}

/// A blob written whole into `tmp/`, not yet flushed or in place.
struct Unsettled {
    /// Where it is written.
    temp: PathBuf,
    /// Its digest.
    digest: Digest,
    /// Whether it is executable.
    executable: bool,
    /// Whether it is flushed even under `PEKIT_NO_SYNC`.
    always: bool,
}

/// This thread's put of a tree at once, while it lasts: what it wrote is
/// removed unless it is put in place, so that a put that fails, or panics,
/// leaves neither the thread putting at once nor its files behind.
struct AtOnce;

impl AtOnce {
    /// Begins a put at once on this thread: `None` if one is under way,
    /// which this put is part of.
    fn begin() -> Option<Self> {
        AT_ONCE.with(|at_once| {
            let mut at_once = at_once.borrow_mut();
            if at_once.is_some() {
                return None;
            }
            *at_once = Some(Vec::new());
            Some(Self)
        })
    }

    /// What the put wrote, its end.
    fn take(self) -> Vec<Unsettled> {
        let written = AT_ONCE.with(|at_once| at_once.borrow_mut().take().unwrap_or_default());
        std::mem::forget(self);
        written
    }
}

impl Drop for AtOnce {
    fn drop(&mut self) {
        let written = AT_ONCE.with(|at_once| at_once.borrow_mut().take().unwrap_or_default());
        for each in written {
            let _ = fs::remove_file(each.temp);
        }
    }
}

/// The blobs, and the copies of blobs, a process linked a file to.
#[derive(Debug, Default)]
struct Linked {
    /// Each, by the device and inode it shares with every link: its
    /// contents and mode, and which it is, 0 the blob and any other the copy
    /// of that number.
    nodes: HashMap<(u64, u64), (FileEntry, u32)>,
    /// Each, by its contents and mode and which it is: those whose device
    /// and inode are known already.
    entries: HashSet<(FileEntry, u32)>,
}

/// A content store (see [The store](#the-store)).
#[derive(Clone, Debug)]
pub struct Store {
    /// Its directory.
    root: PathBuf,
    /// Each blob this process linked a file to (see [Trees seen
    /// before](#trees-seen-before)).
    linked: Arc<Mutex<Linked>>,
    /// Its lock, held shared for as long as it is open, which an eviction
    /// takes exclusively (see `crate::evict`).
    held: Arc<File>,
    /// What it was used for since it was opened: each record found or
    /// written, by its key, and each scratch directory held, by its name.
    used: Arc<Mutex<Used>>,
    /// Each tree this process read, by its digest: a tree's blob never
    /// changes.
    trees: Arc<Mutex<HashMap<Digest, Tree>>>,
    /// Each tree this process found it holds every content of: nothing is
    /// evicted while a process has the store open.
    complete: Arc<Mutex<HashSet<Digest>>>,
    /// What it knows of the materialised trees (see [Materialised
    /// trees](trees#materialised-trees)).
    materialised: Arc<Mutex<trees::Materialised>>,
    /// Each blob this process asked where it is, put, or took by a stamp:
    /// what an eviction during its build keeps (see
    /// [`crate::evict::make_room`]).
    touched: Arc<Mutex<HashSet<Digest>>>,
    /// Where this process publishes what it uses, for an eviction while it
    /// has the store open (see [Publishing what is
    /// used](crate::using#publishing-what-is-used)).
    publisher: Option<Arc<crate::using::Publisher>>,
}

/// How a tree was found complete (see [`Store::holds_tree`]).
enum Found {
    /// This process found it so, or its mark says so.
    Known,
    /// A walk of it just found it so.
    Walked,
}

/// What a store was used for (see [`Store::used`]).
#[derive(Debug, Default)]
pub struct Used {
    /// Each record found or written, by its key.
    pub records: HashSet<Digest>,
    /// Each scratch directory held, by its name.
    pub scratch: HashSet<String>,
    /// The bytes of the blobs it wrote.
    pub added: u64,
}

/// `files` serialized in its keys' order.
fn in_order<S: serde::Serializer>(
    files: &HashMap<PathBuf, (Stamp, FileEntry)>,
    serializer: S,
) -> Result<S::Ok, S::Error> {
    let ordered: std::collections::BTreeMap<&PathBuf, &(Stamp, FileEntry)> = files.iter().collect();
    serde::Serialize::serialize(&ordered, serializer)
}

/// Where the store at `store` keeps stamps of the workspace at `root`, in
/// `seen/`, by a digest of its path: its own files' if `suffix` is empty,
/// and its libraries' if it is `-libraries` (see [Stamps
/// kept](#stamps-kept)).
#[must_use]
pub fn stamps_path(store: &Path, root: &Path, suffix: &str) -> PathBuf {
    let name = Digest::of(root.as_os_str().as_encoded_bytes()).hex();
    store.join("seen").join(format!("{name}{suffix}.stamps"))
}

/// What begins a file of stamps kept (see [Stamps kept](#stamps-kept)).
const STAMPS_MAGIC: &[u8; 8] = b"PKSTAMP1";

/// The bytes of each file's stamp and contents of `files`, in its paths'
/// order (see [Stamps kept](#stamps-kept)).
fn stamps_encoded(files: &HashMap<PathBuf, (Stamp, FileEntry)>) -> Vec<u8> {
    let mut ordered: Vec<(&[u8], &(Stamp, FileEntry))> = files
        .iter()
        .map(|(path, known)| (path.as_os_str().as_encoded_bytes(), known))
        .collect();
    ordered.sort_unstable_by(|a, b| a.0.cmp(b.0));
    let mut bytes = Vec::with_capacity(16 + ordered.len() * 128);
    bytes.extend_from_slice(STAMPS_MAGIC);
    bytes.extend_from_slice(&(ordered.len() as u64).to_le_bytes());
    let mut last: &[u8] = &[];
    for (path, (stamp, entry)) in ordered {
        let shared = path.iter().zip(last).take_while(|(a, b)| a == b).count();
        let rest = &path[shared..];
        for number in [shared, rest.len()] {
            bytes.extend_from_slice(&u32::try_from(number).unwrap_or(u32::MAX).to_le_bytes());
        }
        bytes.extend_from_slice(rest);
        for number in [stamp.node.0, stamp.node.1, stamp.size] {
            bytes.extend_from_slice(&number.to_le_bytes());
        }
        bytes.extend_from_slice(&stamp.mode.to_le_bytes());
        for number in [
            stamp.modified.0,
            stamp.modified.1,
            stamp.changed.0,
            stamp.changed.1,
        ] {
            bytes.extend_from_slice(&number.to_le_bytes());
        }
        bytes.extend_from_slice(&entry.digest.0);
        bytes.extend_from_slice(&entry.size.to_le_bytes());
        bytes.push(u8::from(entry.executable));
        last = path;
    }
    bytes
}

/// The stamps `bytes` hold, as [`stamps_encoded`] writes them; `None` if
/// they are no such bytes, or cut short.
fn stamps_decoded(bytes: &[u8]) -> Option<HashMap<PathBuf, (Stamp, FileEntry)>> {
    struct Reader<'a>(&'a [u8]);
    impl<'a> Reader<'a> {
        fn take(&mut self, count: usize) -> Option<&'a [u8]> {
            let (taken, rest) = self.0.split_at_checked(count)?;
            self.0 = rest;
            Some(taken)
        }
        fn array<const N: usize>(&mut self) -> Option<[u8; N]> {
            self.take(N)?.try_into().ok()
        }
        fn u32(&mut self) -> Option<u32> {
            self.array().map(u32::from_le_bytes)
        }
        fn u64(&mut self) -> Option<u64> {
            self.array().map(u64::from_le_bytes)
        }
        fn i64(&mut self) -> Option<i64> {
            self.array().map(i64::from_le_bytes)
        }
    }
    let mut reader = Reader(bytes);
    if reader.take(STAMPS_MAGIC.len())? != STAMPS_MAGIC {
        return None;
    }
    let count = usize::try_from(reader.u64()?).ok()?;
    let mut files = HashMap::with_capacity(count.min(bytes.len() / 64));
    let mut path: Vec<u8> = Vec::new();
    for _ in 0..count {
        let shared = reader.u32()? as usize;
        let rest = reader.u32()? as usize;
        path.truncate(shared);
        path.extend_from_slice(reader.take(rest)?);
        let stamp = Stamp {
            node: (reader.u64()?, reader.u64()?),
            size: reader.u64()?,
            mode: reader.u32()?,
            modified: (reader.i64()?, reader.i64()?),
            changed: (reader.i64()?, reader.i64()?),
        };
        let entry = FileEntry {
            digest: Digest(reader.array()?),
            size: reader.u64()?,
            executable: reader.take(1)? == [1],
        };
        // Written by this process's kind from paths it read, so the same
        // kind of bytes as any path.
        let os = std::os::unix::ffi::OsStrExt::from_bytes(path.as_slice());
        let os: &std::ffi::OsStr = os;
        files.insert(PathBuf::from(os), (stamp, entry));
    }
    reader.0.is_empty().then_some(files)
}

/// The stamps the store at `store` keeps of the workspace at `root`, as
/// [`stamps_path`] names them, or none. The workspace's path is noted
/// beside them, in `seen/<digest>.root`, if it is not, so that the stamps
/// of a workspace gone are found ([`crate::sweep::sweep_seen`]).
#[must_use]
pub fn read_stamps(store: &Path, root: &Path, suffix: &str) -> Seen {
    let path = stamps_path(store, root, suffix);
    let noted = stamps_path(store, root, "").with_extension("root");
    if !noted.exists() {
        let temp = noted.with_extension(format!("tmp-{}", crate::sweep::token()));
        let _ = fs::create_dir_all(store.join("seen"))
            .and_then(|()| fs::write(&temp, root.as_os_str().as_encoded_bytes()))
            .and_then(|()| fs::rename(&temp, &noted));
    }
    if let Some(files) = fs::read(&path)
        .ok()
        .and_then(|bytes| stamps_decoded(&bytes))
    {
        return Seen {
            files,
            ..Seen::default()
        };
    }
    // Kept as JSON by an older pekit: read, and kept anew as the next build
    // ends, the JSON then removed.
    let json = path.with_extension("json");
    let mut seen: Seen = fs::read(&json)
        .ok()
        .and_then(|bytes| serde_json::from_slice(&bytes).ok())
        .unwrap_or_default();
    seen.edited |= json.exists();
    seen
}

/// Keeps `seen`, stamps of files, at `path`, if any was added, changed or
/// forgotten since it was read: a large workspace's run to tens of
/// megabytes, mostly as the last build left them.
///
/// # Errors
///
/// That `path` cannot be written.
pub fn keep_stamps(path: &Path, seen: &Seen) -> io::Result<()> {
    if !seen.edited {
        return Ok(());
    }
    let bytes = stamps_encoded(&seen.files);
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let temp = path.with_extension(format!("tmp-{}", crate::sweep::token()));
    fs::write(&temp, bytes)?;
    fs::rename(&temp, path)?;
    // What an older pekit kept, as JSON, is this now.
    match fs::remove_file(path.with_extension("json")) {
        Err(error) if error.kind() != io::ErrorKind::NotFound => Err(error),
        _ => Ok(()),
    }
}

/// What the store would keep of the file at `path`, executable if
/// `executable`, read and hashed where it is.
///
/// # Errors
///
/// That it cannot be read.
fn hashed(path: &Path, executable: bool) -> io::Result<FileEntry> {
    let mut hasher = Sha256::new();
    let mut size = 0u64;
    let mut file = File::open(path)?;
    let mut buffer = vec![0u8; 1 << 16];
    loop {
        let read = file.read(&mut buffer)?;
        if read == 0 {
            break;
        }
        if let Some(chunk) = buffer.get(..read) {
            hasher.update(chunk);
        }
        size += read as u64;
    }
    Ok(FileEntry {
        digest: Digest::from_hasher(hasher),
        size,
        executable,
    })
}

/// `written`, what writing the temporary file `temp` came to, with `temp`
/// removed if it failed: a write cut short, by a full disk say, leaves
/// nothing in the store's `tmp/` to fill it further.
fn or_removed<T>(temp: &Path, written: io::Result<T>) -> io::Result<T> {
    if written.is_err() {
        let _ = fs::remove_file(temp);
    }
    written
}

impl Store {
    /// The store in the directory `root`, its directories made if they are
    /// not there.
    ///
    /// # Errors
    ///
    /// That the directories cannot be made.
    pub fn open(root: &Path) -> io::Result<Self> {
        fs::create_dir_all(root)?;
        // An eviction that waits for the store alone holds its gate, so
        // that no build opens it meanwhile: this one waits for it there.
        let gate = Self::lock_file(root, "gate")?;
        gate.lock_shared()?;
        let held = Self::lock_file(root, "lock")?;
        // An eviction running elsewhere is waited for.
        held.lock_shared()?;
        // Its file of what it uses is made before the gate is let go, so
        // that no eviction beside it finds it holding the store unpublished.
        let store = Self::with_held(root, held);
        drop(gate);
        store
    }

    /// The file `name` in the store's directory `root`, opened to be locked,
    /// made if it is not there.
    ///
    /// # Errors
    ///
    /// That it cannot be opened.
    pub(crate) fn lock_file(root: &Path, name: &str) -> io::Result<File> {
        File::options()
            .create(true)
            .truncate(false)
            .write(true)
            .open(root.join(name))
    }

    /// The store in the directory `root`, which `held`, its lock, holds as
    /// the caller took it, its directories made if they are not there.
    ///
    /// # Errors
    ///
    /// That the directories cannot be made.
    pub(crate) fn with_held(root: &Path, held: File) -> io::Result<Self> {
        for part in ["cas", "records", "tmp"] {
            fs::create_dir_all(root.join(part))?;
        }
        Ok(Self {
            root: root.to_path_buf(),
            linked: Arc::default(),
            held: Arc::new(held),
            used: Arc::default(),
            trees: Arc::default(),
            complete: Arc::default(),
            materialised: Arc::default(),
            touched: Arc::default(),
            publisher: crate::using::publisher(root),
        })
    }

    /// The store's lock, which this process holds shared while the store is
    /// open.
    #[must_use]
    pub fn held(&self) -> &File {
        &self.held
    }

    /// Notes that the scratch directory `name` was held.
    pub fn note_scratch(&self, name: &str) {
        if let Ok(mut used) = self.used.lock() {
            used.scratch.insert(name.to_owned());
        }
    }

    /// Notes that the record `key` was found or written.
    pub fn note_record(&self, key: &Digest) {
        if let Some(publisher) = &self.publisher {
            publisher.publish(crate::using::Kind::Record, key);
        }
        if let Ok(mut used) = self.used.lock() {
            used.records.insert(*key);
        }
    }

    /// The bytes of the blobs this process wrote since it last asked,
    /// taken: what [`crate::evict::ledger_written`] adds to the store's
    /// size as a build goes.
    #[must_use]
    pub fn take_added(&self) -> u64 {
        self.used
            .lock()
            .map(|mut used| std::mem::take(&mut used.added))
            .unwrap_or_default()
    }

    /// What the store was used for since it was opened, or since this was
    /// last asked, which it forgets.
    #[must_use]
    pub fn used(&self) -> Used {
        self.used
            .lock()
            .map(|mut used| std::mem::take(&mut *used))
            .unwrap_or_default()
    }

    /// Whether the tree `digest` is known complete, by what this process
    /// found or by its mark, without a walk.
    fn marked_complete(&self, digest: &Digest) -> bool {
        // Published before it is looked for, as all it names is then relied
        // on (see [Publishing what is
        // used](crate::using#publishing-what-is-used)).
        if let Some(publisher) = &self.publisher {
            publisher.publish(crate::using::Kind::Tree, digest);
        }
        self.complete
            .lock()
            .is_ok_and(|complete| complete.contains(digest))
            || self.complete_path(digest).exists()
    }

    /// Whether the store holds every content of the tree `digest`, at any
    /// depth: marked so in `complete/` once it is found to.
    #[must_use]
    pub fn holds_tree(&self, digest: &Digest) -> bool {
        match self.complete_by(digest) {
            Some(Found::Walked) => {
                let _ = self.mark_complete(digest);
                true
            }
            Some(Found::Known) => true,
            None => false,
        }
    }

    /// Where the mark that the tree `digest` is complete is.
    fn complete_path(&self, digest: &Digest) -> PathBuf {
        self.root.join("complete").join(digest.hex())
    }

    /// Marks the tree `digest` complete.
    fn mark_complete(&self, digest: &Digest) -> io::Result<()> {
        fs::create_dir_all(self.root.join("complete"))?;
        File::create(self.complete_path(digest))?;
        if let Ok(mut complete) = self.complete.lock() {
            complete.insert(*digest);
        }
        Ok(())
    }

    /// Removes every mark that a tree is complete, as an eviction does
    /// before it removes a blob.
    ///
    /// # Errors
    ///
    /// That they cannot be removed.
    pub fn forget_complete(&self) -> io::Result<()> {
        if let Ok(mut complete) = self.complete.lock() {
            complete.clear();
        }
        match fs::remove_dir_all(self.root.join("complete")) {
            Err(error) if error.kind() != io::ErrorKind::NotFound => Err(error),
            _ => Ok(()),
        }
    }

    /// How the store was found to hold every content of the tree `digest`,
    /// if it does: by what this process found, or by its mark, or by a walk
    /// of the tree.
    fn complete_by(&self, digest: &Digest) -> Option<Found> {
        if self.marked_complete(digest) {
            if let Ok(mut complete) = self.complete.lock() {
                complete.insert(*digest);
            }
            return Some(Found::Known);
        }
        let tree = self.read_tree(digest).ok()?;
        let holds = tree.entries.iter().all(|entry| match entry {
            TreeEntry::File { file, .. } => self.has(&file.digest),
            TreeEntry::Dir { tree, .. } => self.complete_by(tree).is_some(),
            TreeEntry::Link { .. } => true,
        });
        if !holds {
            return None;
        }
        if let Ok(mut complete) = self.complete.lock() {
            complete.insert(*digest);
        }
        Some(Found::Walked)
    }

    /// The store's directory.
    #[must_use]
    pub fn root(&self) -> &Path {
        &self.root
    }

    /// Where the blob of `digest` is, executable if `executable`.
    #[must_use]
    pub fn blob_path(&self, digest: &Digest, executable: bool) -> PathBuf {
        self.touch(digest);
        self.cas_path(digest, executable)
    }

    /// [`Store::blob_path`], the blob not noted as this process's to use:
    /// for an eviction, which looks at every blob.
    #[must_use]
    pub fn cas_path(&self, digest: &Digest, executable: bool) -> PathBuf {
        let hex = digest.hex();
        let name = if executable {
            format!("{hex}.x")
        } else {
            hex.clone()
        };
        self.root.join("cas").join(&hex[..2]).join(name)
    }

    /// Notes that this process uses the blob `digest`, which an eviction
    /// during its build then keeps.
    fn touch(&self, digest: &Digest) {
        match &self.publisher {
            Some(publisher) => publisher.publish(crate::using::Kind::Blob, digest),
            None => {
                if let Ok(mut touched) = self.touched.lock() {
                    touched.insert(*digest);
                }
            }
        }
    }

    /// Each blob this process used, each record it found or wrote, and each
    /// scratch directory it held, so far: what an eviction during its
    /// build keeps (see [`crate::evict::make_room`]).
    #[must_use]
    pub fn in_use(&self) -> crate::evict::Keep {
        let blobs = self
            .touched
            .lock()
            .map(|touched| touched.clone())
            .unwrap_or_default();
        let (records, scratch) = self
            .used
            .lock()
            .map(|used| (used.records.clone(), used.scratch.clone()))
            .unwrap_or_default();
        // Each tree it took as complete, all of which it relies on.
        let trees = self
            .complete
            .lock()
            .map(|complete| complete.clone())
            .unwrap_or_default();
        crate::evict::Keep {
            blobs,
            records,
            scratch,
            trees,
        }
    }

    /// Whether the store holds the contents of `digest`.
    #[must_use]
    pub fn has(&self, digest: &Digest) -> bool {
        self.blob_path(digest, false).is_file() || self.blob_path(digest, true).is_file()
    }

    /// Where the store holds the contents of `digest`, plain or executable,
    /// and their size, if it does.
    #[must_use]
    pub fn held_blob(&self, digest: &Digest) -> Option<(PathBuf, u64)> {
        [false, true].into_iter().find_map(|executable| {
            let path = self.blob_path(digest, executable);
            let size = fs::metadata(&path)
                .ok()
                .filter(fs::Metadata::is_file)?
                .len();
            Some((path, size))
        })
    }

    /// A new temporary file's path in the store's `tmp/`, which no other
    /// writer, of this process or another, uses.
    fn temp_path(&self) -> PathBuf {
        let number = NEXT_TEMP.fetch_add(1, Ordering::Relaxed);
        self.root
            .join("tmp")
            .join(format!("{}-{number}", crate::sweep::token()))
    }

    /// Puts the file at `path`, read, into the store, hashing it as it is
    /// copied: its contents' digest, size and mode.
    ///
    /// # Errors
    ///
    /// That it cannot be read, or the store written.
    pub fn put_file(&self, path: &Path) -> io::Result<FileEntry> {
        let meta = fs::metadata(path)?;
        let executable = meta.permissions().mode() & 0o111 != 0;
        if ADOPTING.get() && meta.is_file() && meta.nlink() == 1 {
            return self.adopt_file(path, executable);
        }
        // Hashed first: a file whose contents the store holds is read once
        // and written nowhere, as a workspace's or a library's unchanged
        // files are each build. One it lacks is read again as it is copied.
        if meta.is_file() {
            let entry = hashed(path, executable)?;
            if self.blob_path(&entry.digest, executable).is_file() {
                return Ok(entry);
            }
        }
        self.put_reader(&mut File::open(path)?, executable)
    }

    /// Runs `put`, which puts what is the store's to take into it, such as a
    /// step's results, about to be removed with its staging: a file of one
    /// link that `put` puts is hashed where it is, and, unless the store
    /// holds its contents, which writes nothing, linked into place as its
    /// blob rather than copied (see [`Store::put_file`]). Never for a file
    /// someone may change after, as a workspace's are: it would be the blob.
    pub fn adopting<T>(&self, put: impl FnOnce() -> T) -> T {
        let _was = Adopting(ADOPTING.replace(true));
        put()
    }

    /// Puts the file at `path`, of one link and executable if `executable`,
    /// into the store as the store's own (see [`Store::adopting`]).
    ///
    /// # Errors
    ///
    /// That it cannot be read, or the store written.
    fn adopt_file(&self, path: &Path, executable: bool) -> io::Result<FileEntry> {
        let entry = hashed(path, executable)?;
        let digest = entry.digest;
        if self.blob_path(&digest, executable).is_file() {
            return Ok(entry);
        }
        let temp = self.temp_path();
        if fs::hard_link(path, &temp).is_err() {
            // On another file system than the store's: copied.
            return self.put_reader(&mut File::open(path)?, executable);
        }
        self.finish(temp, digest, executable, false)?;
        Ok(entry)
    }

    /// [`Store::put_file`], a file of a materialised tree not read, but
    /// taken as its tree has it.
    ///
    /// # Errors
    ///
    /// That it cannot be read, or the store written.
    pub fn put_file_known(&self, path: &Path) -> io::Result<FileEntry> {
        match self.file_in(path) {
            Some((entry, _)) => Ok(entry),
            None => self.put_file(path),
        }
    }

    /// Puts what `source` reads into the store as a blob, executable if
    /// `executable` says, hashing it as it goes: what it keeps of it.
    ///
    /// # Errors
    ///
    /// That `source` cannot be read or the store written.
    pub fn put_reader(&self, source: &mut dyn Read, executable: bool) -> io::Result<FileEntry> {
        let temp = self.temp_path();
        let mut hasher = Sha256::new();
        let mut size = 0u64;
        let copied = (|| -> io::Result<()> {
            let mut out = File::create(&temp)?;
            let mut buffer = vec![0u8; 1 << 16];
            loop {
                let read = source.read(&mut buffer)?;
                if read == 0 {
                    return Ok(());
                }
                let chunk = &buffer[..read];
                hasher.update(chunk);
                out.write_all(chunk)?;
                size += read as u64;
            }
        })();
        or_removed(&temp, copied)?;
        let digest = Digest::from_hasher(hasher);
        self.finish(temp, digest, executable, false)?;
        Ok(FileEntry {
            digest,
            size,
            executable,
        })
    }

    /// Puts what `write` writes into the store as a blob, not executable,
    /// hashing it as it goes: what it keeps of it. A blob arriving in parts,
    /// as a download does, is written so, never held whole.
    ///
    /// # Errors
    ///
    /// That `write` fails, or the store cannot be written.
    pub fn put_written(
        &self,
        write: impl FnOnce(&mut dyn Write) -> io::Result<()>,
    ) -> io::Result<FileEntry> {
        /// A file that hashes and counts what is written to it.
        struct Hashing {
            /// The file.
            out: File,
            /// The hash of what was written.
            hasher: Sha256,
            /// How much was written.
            size: u64,
        }
        impl Write for Hashing {
            fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
                let written = self.out.write(bytes)?;
                self.hasher.update(&bytes[..written]);
                self.size += written as u64;
                Ok(written)
            }
            fn flush(&mut self) -> io::Result<()> {
                self.out.flush()
            }
        }
        let temp = self.temp_path();
        let mut hashing = Hashing {
            out: File::create(&temp)?,
            hasher: Sha256::new(),
            size: 0,
        };
        if let Err(error) = write(&mut hashing) {
            let _ = fs::remove_file(&temp);
            return Err(error);
        }
        let Hashing { hasher, size, .. } = hashing;
        let digest = Digest::from_hasher(hasher);
        self.finish(temp, digest, false, false)?;
        Ok(FileEntry {
            digest,
            size,
            executable: false,
        })
    }

    /// What the store keeps of the blob `digest`, not executable, if it holds
    /// it: what a file of those contents is, without reading it again.
    #[must_use]
    pub fn entry(&self, digest: &Digest) -> Option<FileEntry> {
        let meta = fs::metadata(self.blob_path(digest, false)).ok()?;
        Some(FileEntry {
            digest: *digest,
            size: meta.len(),
            executable: false,
        })
    }

    /// Puts `bytes` into the store as a blob, not executable.
    ///
    /// # Errors
    ///
    /// That the store cannot be written.
    pub fn put_bytes(&self, bytes: &[u8]) -> io::Result<Digest> {
        let digest = Digest::of(bytes);
        if self.blob_path(&digest, false).is_file() {
            return Ok(digest);
        }
        let temp = self.temp_path();
        or_removed(&temp, File::create(&temp)?.write_all(bytes))?;
        self.finish(temp, digest, false, true)?;
        Ok(digest)
    }

    /// Puts `temp`, written whole with the contents of `digest`, in place
    /// as its blob, executable if `executable`, flushed first unless
    /// `PEKIT_NO_SYNC` is set and it is not to be flushed `always`; or, while
    /// this thread puts a tree at once, keeps it to be (see [Putting a tree
    /// at once](#putting-a-tree-at-once)).
    fn finish(
        &self,
        temp: PathBuf,
        digest: Digest,
        executable: bool,
        always: bool,
    ) -> io::Result<()> {
        let unsettled = Unsettled {
            temp,
            digest,
            executable,
            always,
        };
        let kept = AT_ONCE.with(|at_once| match at_once.borrow_mut().as_mut() {
            Some(written) => {
                written.push(unsettled);
                None
            }
            None => Some(unsettled),
        });
        match kept {
            Some(unsettled) => self.settle_all(vec![unsettled]),
            None => Ok(()),
        }
    }

    /// Flushes each of `written` that is to be, by several threads, and
    /// then puts each in place, in their order; or, if any can't be
    /// flushed, removes them all.
    fn settle_all(&self, written: Vec<Unsettled>) -> io::Result<()> {
        let syncing = std::env::var_os("PEKIT_NO_SYNC").is_none();
        let flushed: Vec<&Unsettled> = written
            .iter()
            .filter(|each| syncing || each.always)
            .collect();
        let share = flushed.len().div_ceil(FLUSHERS).max(1);
        let outcome = std::thread::scope(|scope| {
            let flushers: Vec<_> = flushed
                .chunks(share)
                .map(|part| {
                    scope.spawn(move || {
                        part.iter()
                            .try_for_each(|each| File::open(&each.temp)?.sync_all())
                    })
                })
                .collect();
            flushers.into_iter().try_for_each(|flusher| {
                flusher
                    .join()
                    .unwrap_or_else(|_| Err(io::Error::other("a flush panicked")))
            })
        });
        if let Err(error) = outcome {
            for each in written {
                let _ = fs::remove_file(each.temp);
            }
            return Err(error);
        }
        let mut left = written.into_iter();
        while let Some(each) = left.next() {
            if let Err(error) = self.settle(&each.temp, &each.digest, each.executable) {
                for rest in left {
                    let _ = fs::remove_file(rest.temp);
                }
                return Err(error);
            }
        }
        Ok(())
    }

    /// `put`, writing what it puts into the store at once (see [Putting a
    /// tree at once](#putting-a-tree-at-once)): as part of the put under way
    /// on this thread if there is one.
    fn at_once<T>(&self, put: impl FnOnce() -> io::Result<T>) -> io::Result<T> {
        let Some(at_once) = AtOnce::begin() else {
            return put();
        };
        let value = put()?;
        self.settle_all(at_once.take())?;
        Ok(value)
    }

    /// Moves `temp`, a finished copy of the contents of `digest`, into place
    /// as its blob, executable if `executable`, read-only; or drops it if
    /// the blob is there already, written by another writer.
    fn settle(&self, temp: &Path, digest: &Digest, executable: bool) -> io::Result<()> {
        let target = self.blob_path(digest, executable);
        if target.is_file() {
            return fs::remove_file(temp);
        }
        if let Some(parent) = target.parent() {
            fs::create_dir_all(parent)?;
        }
        let mode = if executable { 0o555 } else { 0o444 };
        fs::set_permissions(temp, fs::Permissions::from_mode(mode))?;
        let size = fs::metadata(temp)?.len();
        fs::rename(temp, &target)?;
        if let Ok(mut used) = self.used.lock() {
            used.added += size;
        }
        Ok(())
    }

    /// The contents of the blob of `digest`.
    ///
    /// # Errors
    ///
    /// That the store does not hold it, or it cannot be read.
    pub fn read_blob(&self, digest: &Digest) -> io::Result<Vec<u8>> {
        let plain = self.blob_path(digest, false);
        let path = if plain.is_file() {
            plain
        } else {
            self.blob_path(digest, true)
        };
        fs::read(path)
    }

    /// Makes the file `entry` at `at`, a path that does not exist: a hard
    /// link to its blob, of its mode, or a copy where a link cannot be made.
    /// A link *is* the blob: only a view nothing writes through, a sandbox's
    /// read-only inputs, may be one; [`Store::copy_file`] makes a file that
    /// may be changed.
    ///
    /// # Errors
    ///
    /// That the store does not hold it, or `at` cannot be written.
    pub fn materialise_file(&self, entry: &FileEntry, at: &Path) -> io::Result<()> {
        self.materialise_as(entry, at, 0)
    }

    /// Makes the file `entry` at `at`, a path that does not exist: a hard
    /// link to its blob if `copy` is 0, else to its copy of that number
    /// ([`Store::link_copy`]), or a read-only copy of its own if no link can
    /// be made.
    fn materialise_as(&self, entry: &FileEntry, at: &Path, copy: u32) -> io::Result<()> {
        // At once, if its blob and `at`'s directory are there, as they are
        // for every file of a tree but the first of its directory.
        if copy == 0 {
            let blob = self.blob_path(&entry.digest, entry.executable);
            if fs::hard_link(&blob, at).is_ok() {
                self.note_linked((entry, 0), &blob);
                return Ok(());
            }
        }
        let blob = self.blob_of_mode(entry)?;
        if let Some(parent) = at.parent() {
            fs::create_dir_all(parent)?;
        }
        let linked = if copy == 0 {
            fs::hard_link(&blob, at).map(|()| self.note_linked((entry, 0), &blob))
        } else {
            self.link_copy(entry, copy, at)
        };
        if linked.is_ok() {
            return Ok(());
        }
        fs::copy(&blob, at)?;
        let mode = if entry.executable { 0o555 } else { 0o444 };
        fs::set_permissions(at, fs::Permissions::from_mode(mode))
    }

    /// Makes `at`, a path that does not exist in a directory that does, a
    /// hard link to the copy numbered `copy`, from 1, of the blob of
    /// `entry`, made from the blob the first time it is needed (see [The
    /// store](#the-store)).
    ///
    /// # Errors
    ///
    /// That the store does not hold the blob, or the copy or the link
    /// cannot be made.
    pub fn link_copy(&self, entry: &FileEntry, copy: u32, at: &Path) -> io::Result<()> {
        let path = self.copy_path(entry, copy);
        if let Err(error) = fs::hard_link(&path, at) {
            // No copy is made that could never be linked there: on another
            // file system, the caller copies the blob instead.
            if error.kind() == io::ErrorKind::CrossesDevices || self.across_devices(at) {
                return Err(io::Error::from(io::ErrorKind::CrossesDevices));
            }
            let source = self.blob_of_mode(entry)?;
            let temp = self.temp_path();
            let mode = if entry.executable { 0o555 } else { 0o444 };
            let copied = fs::copy(&source, &temp)
                .and_then(|_| fs::set_permissions(&temp, fs::Permissions::from_mode(mode)));
            or_removed(&temp, copied)?;
            // A copy another process made first is as good.
            if fs::hard_link(&temp, &path).is_ok()
                && let Ok(mut used) = self.used.lock()
            {
                used.added += entry.size;
            }
            fs::remove_file(&temp)?;
            fs::hard_link(&path, at)?;
        }
        self.note_linked((entry, copy), &path);
        Ok(())
    }

    /// Whether `at`'s directory is on another file system than the store,
    /// where no link to a blob can be made; `false` if either can't be told.
    fn across_devices(&self, at: &Path) -> bool {
        let device = |path: &Path| fs::metadata(path).ok().map(|meta| meta.dev());
        match (at.parent().and_then(device), device(&self.root)) {
            (Some(there), Some(store)) => there != store,
            _ => false,
        }
    }

    /// Where the copy numbered `copy` of the blob of `entry` is, or would be
    /// (see [The store](#the-store)).
    fn copy_path(&self, entry: &FileEntry, copy: u32) -> PathBuf {
        let blob = self.blob_path(&entry.digest, entry.executable);
        let mut name = blob.file_name().unwrap_or_default().to_os_string();
        name.push(format!(".r{copy}"));
        blob.with_file_name(name)
    }

    /// Which blob, or copy of a blob, the file of device and inode `node` is
    /// a link to, if this process linked a file to it: its contents and
    /// mode, and 0 for the blob or the copy's number.
    #[must_use]
    pub fn origin(&self, node: (u64, u64)) -> Option<(FileEntry, u32)> {
        self.linked.lock().ok()?.nodes.get(&node).copied()
    }

    /// Notes that a file was linked to `path`, the blob of `entry` if `copy`
    /// is 0, else its copy of that number, unless its device and inode are
    /// known already.
    fn note_linked(&self, (entry, copy): (&FileEntry, u32), path: &Path) {
        let Ok(mut linked) = self.linked.lock() else {
            return;
        };
        if linked.entries.contains(&(*entry, copy)) {
            return;
        }
        if let Ok(meta) = fs::metadata(path) {
            linked
                .nodes
                .insert((meta.dev(), meta.ino()), (*entry, copy));
            linked.entries.insert((*entry, copy));
        }
    }

    /// Makes the file `entry` at `at`, a path that does not exist, as a copy
    /// of its blob, its own, writable by its owner: what someone may change,
    /// as a target's result under `pekit-out/` is.
    ///
    /// # Errors
    ///
    /// That the store does not hold it, or `at` cannot be written.
    pub fn copy_file(&self, entry: &FileEntry, at: &Path) -> io::Result<()> {
        let blob = self.blob_of_mode(entry)?;
        if let Some(parent) = at.parent() {
            fs::create_dir_all(parent)?;
        }
        fs::copy(&blob, at)?;
        let mode = if entry.executable { 0o755 } else { 0o644 };
        fs::set_permissions(at, fs::Permissions::from_mode(mode))
    }

    /// The blob of `entry`'s contents of its mode, made from the blob of the
    /// other mode if only that is there.
    fn blob_of_mode(&self, entry: &FileEntry) -> io::Result<PathBuf> {
        let wanted = self.blob_path(&entry.digest, entry.executable);
        if wanted.is_file() {
            return Ok(wanted);
        }
        let other = self.blob_path(&entry.digest, !entry.executable);
        if !other.is_file() {
            return Err(io::Error::new(
                io::ErrorKind::NotFound,
                format!("the store holds no contents of {}", entry.digest),
            ));
        }
        let temp = self.temp_path();
        or_removed(&temp, fs::copy(&other, &temp))?;
        self.settle(&temp, &entry.digest, entry.executable)?;
        Ok(wanted)
    }

    /// Puts the directory at `path` into the store, every file, directory
    /// and symbolic link under it, never following a link: its tree's digest.
    ///
    /// # Errors
    ///
    /// That something under it cannot be read, a name is not UTF-8, or the
    /// store cannot be written.
    pub fn put_tree(&self, path: &Path) -> io::Result<Digest> {
        self.put_tree_mounted(path, &HashMap::new())
    }

    /// [`Store::put_tree`], neither a directory that is a mount point of
    /// `mounted`, by where it is, nor one of a materialised tree, read, but
    /// its tree's digest taken (see [Materialised
    /// trees](trees#materialised-trees)).
    ///
    /// # Errors
    ///
    /// That something under it cannot be read, a name is not UTF-8, or the
    /// store cannot be written.
    pub fn put_tree_mounted<S: std::hash::BuildHasher>(
        &self,
        path: &Path,
        mounted: &HashMap<PathBuf, Digest, S>,
    ) -> io::Result<Digest> {
        self.at_once(|| self.tree_mounted(path, mounted))
    }

    /// [`Store::put_tree_mounted`], as part of a put at once.
    fn tree_mounted<S: std::hash::BuildHasher>(
        &self,
        path: &Path,
        mounted: &HashMap<PathBuf, Digest, S>,
    ) -> io::Result<Digest> {
        if let Some(digest) = mounted.get(path).copied().or_else(|| self.tree_in(path)) {
            return Ok(digest);
        }
        let mut entries = Vec::new();
        for (name, kind, at) in listing(path)? {
            let name = name.into_string().map_err(|name| {
                io::Error::other(format!("`{}` is no UTF-8 name", name.display()))
            })?;
            entries.push(if kind.is_symlink() {
                let target = fs::read_link(&at)?.to_string_lossy().into_owned();
                TreeEntry::Link { name, target }
            } else if kind.is_dir() {
                TreeEntry::Dir {
                    name,
                    tree: self.tree_mounted(&at, mounted)?,
                }
            } else {
                TreeEntry::File {
                    name,
                    file: self.put_file_known(&at)?,
                }
            });
        }
        entries.sort_by(|a, b| a.name().cmp(b.name()));
        self.put_tree_of(&Tree { entries })
    }

    /// [`Store::put_tree`], reading only the files `seen` does not know
    /// unchanged, or links to blobs, and asking `refuse` of every other
    /// entry whether the tree can't be kept: `None` if it can't (see [Trees
    /// seen before](#trees-seen-before)).
    ///
    /// # Errors
    ///
    /// That something under `path` cannot be read, or the store written.
    pub fn put_tree_seen(
        &self,
        path: &Path,
        seen: &mut Seen,
        refuse: &dyn Fn(&Path, &fs::Metadata) -> bool,
    ) -> io::Result<Option<Digest>> {
        let now = clock_beside(path)?;
        self.put_tree_seen_at(path, (seen, now), refuse)
    }

    /// [`Store::put_tree_seen`], the put begun at `now`, by the clock of the
    /// file system the tree is on ([`clock_beside`]).
    ///
    /// # Errors
    ///
    /// That something under `path` cannot be read, or the store written.
    pub fn put_tree_seen_at(
        &self,
        path: &Path,
        (seen, now): (&mut Seen, (i64, i64)),
        refuse: &dyn Fn(&Path, &fs::Metadata) -> bool,
    ) -> io::Result<Option<Digest>> {
        self.put_tree_seen_without(path, (seen, now), refuse, &[])
    }

    /// [`Store::put_tree_seen_at`], what is at the paths `left_out` no part
    /// of the tree.
    ///
    /// # Errors
    ///
    /// That something under `path` cannot be read, or the store written.
    pub fn put_tree_seen_without(
        &self,
        path: &Path,
        (seen, now): (&mut Seen, (i64, i64)),
        refuse: &dyn Fn(&Path, &fs::Metadata) -> bool,
        left_out: &[PathBuf],
    ) -> io::Result<Option<Digest>> {
        let mut walk = Walk::new((seen, now), refuse, left_out, None);
        let meta = fs::symlink_metadata(path)?;
        if refuse(path, &meta) {
            return Ok(None);
        }
        self.put_walked(path, &mut walk)
    }

    /// [`Store::put_tree_seen_without`] of a tree another process may be
    /// changing as it is read: [`Steady::Changed`] if it changed, whether
    /// or not the walk failed because of it (see [Trees read while they
    /// change](#trees-read-while-they-change)).
    ///
    /// # Errors
    ///
    /// That `path` is not there, or that something under it cannot be read
    /// though nothing changed, or that the store cannot be written.
    pub fn put_tree_steady(
        &self,
        path: &Path,
        (seen, now): (&mut Seen, (i64, i64)),
        refuse: &dyn Fn(&Path, &fs::Metadata) -> bool,
        left_out: &[PathBuf],
    ) -> io::Result<Steady<Option<Digest>>> {
        let meta = fs::symlink_metadata(path)?;
        if refuse(path, &meta) {
            return Ok(Steady::Read(None));
        }
        let mut walk = Walk::new((seen, now), refuse, left_out, Some(Watch::default()));
        let put = self.put_walked(path, &mut walk);
        match walk.watch.and_then(|watch| watch.changed(left_out)) {
            Some(changed) => Ok(Steady::Changed(changed)),
            None => put.map(Steady::Read),
        }
    }

    /// The tree at `path` put into the store, as `walk` has it, at once
    /// (see [Putting a tree at once](#putting-a-tree-at-once)), and the
    /// blobs of the files it trusted by their stamps looked for: the tree
    /// walked again if one is not what its stamp said, and marked complete
    /// if they were looked for and found.
    fn put_walked(&self, path: &Path, walk: &mut Walk<'_>) -> io::Result<Option<Digest>> {
        // Part of a put under way, what this one writes is in place only
        // once that one ends: too soon to mark the tree complete.
        let settles = AT_ONCE.with(|at_once| at_once.borrow().is_none());
        loop {
            walk.read_ahead(path);
            let walked = self.at_once(|| {
                let Some(tree) = self.tree_seen(path, walk)? else {
                    return Ok(None);
                };
                Ok(Some((tree, self.trusted_held(walk, &tree)?)))
            })?;
            match walked {
                None => return Ok(None),
                Some((_, Held::Again)) => walk.restart(),
                Some((tree, held)) => {
                    if settles
                        && held == Held::Checked
                        && walk.unwalked.iter().all(|tree| self.holds_tree(tree))
                    {
                        let _ = self.mark_complete(&tree);
                    }
                    return Ok(Some(tree));
                }
            }
        }
    }

    /// Whether each file `walk` trusted by its stamp has its blob, the
    /// tree `tree`: known if the tree is complete, or looked for, and a
    /// blob gone put again from its file. [`Held::Again`] if a file is not
    /// what its stamp said, each such then forgotten.
    fn trusted_held(&self, walk: &mut Walk<'_>, tree: &Digest) -> io::Result<Held> {
        if walk.trusted.is_empty() {
            return Ok(Held::Known);
        }
        if self.marked_complete(tree) {
            walk.trusted.clear();
            return Ok(Held::Known);
        }
        let mut held = Held::Checked;
        for (path, entry) in std::mem::take(&mut walk.trusted) {
            if self.blob_path(&entry.digest, entry.executable).is_file() {
                continue;
            }
            if self.put_file_once(&path)? != entry {
                walk.seen.forget(&path);
                held = Held::Again;
            }
        }
        Ok(held)
    }

    /// [`Store::put_tree_seen`] of `path`, as `walk` has it.
    fn tree_seen(&self, path: &Path, walk: &mut Walk<'_>) -> io::Result<Option<Digest>> {
        let mut entries = Vec::new();
        let listed: Vec<Listed> = match walk.ahead.remove(path) {
            Some(listed) => listed,
            None => listing(path)?
                .into_iter()
                .map(|(name, _, at)| (name, at, None))
                .collect(),
        };
        if let Some(watch) = &mut walk.watch {
            let mut names: Vec<_> = listed
                .iter()
                .filter(|(_, at, _)| !walk.left_out.contains(at))
                .map(|(name, _, _)| name.clone())
                .collect();
            names.sort();
            watch.listings.push((path.to_path_buf(), names));
        }
        for (name, at, ahead) in listed {
            let name = name.into_string().map_err(|name| {
                io::Error::other(format!("`{}` is no UTF-8 name", name.display()))
            })?;
            if walk.left_out.contains(&at) {
                continue;
            }
            let meta = match ahead {
                Some(meta) => meta?,
                None => fs::symlink_metadata(&at)?,
            };
            let kind = meta.file_type();
            if let Some(watch) = &mut walk.watch
                && !kind.is_dir()
            {
                watch.stamps.push((at.clone(), Stamp::of(&meta)));
            }
            entries.push(if kind.is_symlink() {
                if (walk.refuse)(&at, &meta) {
                    return Ok(None);
                }
                let target = fs::read_link(&at)?.to_string_lossy().into_owned();
                TreeEntry::Link { name, target }
            } else if kind.is_dir() {
                if let Some(tree) = walk.seen.mounted.get(&at).or(walk.seen.dirs.get(&at)) {
                    walk.unwalked.push(*tree);
                    TreeEntry::Dir { name, tree: *tree }
                } else {
                    if (walk.refuse)(&at, &meta) {
                        return Ok(None);
                    }
                    let Some(tree) = self.tree_seen(&at, walk)? else {
                        return Ok(None);
                    };
                    if walk.seen.fixed.contains(&at) {
                        walk.seen.dirs.insert(at, tree);
                    }
                    TreeEntry::Dir { name, tree }
                }
            } else {
                let Some(file) = self.file_seen(&at, &meta, walk)? else {
                    return Ok(None);
                };
                TreeEntry::File { name, file }
            });
        }
        entries.sort_by(|a, b| a.name().cmp(b.name()));
        self.put_tree_of(&Tree { entries }).map(Some)
    }

    /// [`Store::put_file`], reading the file only if `seen` does not know it
    /// unchanged, the put begun at `now`, by the clock of the file system the
    /// file is on ([`clock_beside`]) (see [Trees seen
    /// before](#trees-seen-before)).
    ///
    /// # Errors
    ///
    /// That it cannot be read, or the store written.
    pub fn put_file_seen_at(
        &self,
        path: &Path,
        (seen, now): (&mut Seen, (i64, i64)),
    ) -> io::Result<FileEntry> {
        let meta = fs::metadata(path)?;
        let never = |_: &Path, _: &fs::Metadata| false;
        let mut walk = Walk::new((seen, now), &never, &[], None);
        let entry = self
            .file_seen(path, &meta, &mut walk)?
            .ok_or_else(|| io::Error::other("refused"))?;
        if walk.trusted.is_empty() || self.blob_path(&entry.digest, entry.executable).is_file() {
            return Ok(entry);
        }
        let put = self.put_file_once(path)?;
        if put != entry {
            walk.seen.forget(path);
        }
        Ok(put)
    }

    /// [`Store::put_file_seen_at`] of a file another process may be
    /// changing as it is read: [`Steady::Changed`] if its stamp, taken
    /// through any symbolic link, differs once it is read, or it is gone,
    /// whether or not the read failed because of it (see [Trees read while
    /// they change](#trees-read-while-they-change)).
    ///
    /// # Errors
    ///
    /// That `path` is not there, or cannot be read though it did not
    /// change, or that the store cannot be written.
    pub fn put_file_steady(
        &self,
        path: &Path,
        (seen, now): (&mut Seen, (i64, i64)),
    ) -> io::Result<Steady<FileEntry>> {
        let before = Stamp::of(&fs::metadata(path)?);
        let put = self.put_file_seen_at(path, (seen, now));
        if fs::metadata(path).map(|meta| Stamp::of(&meta)).ok() != Some(before) {
            return Ok(Steady::Changed(path.to_path_buf()));
        }
        put.map(Steady::Read)
    }

    /// The file at `path`, of metadata `meta`, put into the store, unread if
    /// it is a link to a blob or `walk` knows it unchanged.
    fn file_seen(
        &self,
        path: &Path,
        meta: &fs::Metadata,
        walk: &mut Walk<'_>,
    ) -> io::Result<Option<FileEntry>> {
        if let Some(entry) = self.blob_linked(meta) {
            return Ok(Some(entry));
        }
        let stamp = Stamp::of(meta);
        // Its blob is looked for once the walk is over, and only if the
        // tree is not known complete: published then, as the tree or as
        // itself ([`Store::trusted_held`]), not here.
        if let Some((known, entry)) = walk.seen.files.get(path)
            && *known == stamp
        {
            let entry = *entry;
            walk.seen.visited.insert(path.to_path_buf());
            walk.trusted.push((path.to_path_buf(), entry));
            return Ok(Some(entry));
        }
        if (walk.refuse)(path, meta) {
            return Ok(None);
        }
        let entry = self.put_file_once(path)?;
        if stamp.changed < walk.now {
            walk.seen.note(path, (stamp, entry));
        } else {
            walk.seen.forget(path);
        }
        Ok(Some(entry))
    }

    /// The blob the file of metadata `meta` is a link to, if this process
    /// linked it, and it is that blob still.
    fn blob_linked(&self, meta: &fs::Metadata) -> Option<FileEntry> {
        if meta.nlink() < 2 {
            return None;
        }
        let node = (meta.dev(), meta.ino());
        let (entry, 0) = *self.linked.lock().ok()?.nodes.get(&node)? else {
            return None;
        };
        let blob = fs::metadata(self.blob_path(&entry.digest, entry.executable)).ok()?;
        ((blob.dev(), blob.ino()) == node).then_some(entry)
    }

    /// [`Store::put_file`], hashing the file first, and copying it only if
    /// the store lacks its blob.
    ///
    /// # Errors
    ///
    /// That it cannot be read, or the store written.
    pub fn put_file_once(&self, path: &Path) -> io::Result<FileEntry> {
        let executable = fs::metadata(path)?.permissions().mode() & 0o111 != 0;
        let mut hasher = Sha256::new();
        let mut size = 0u64;
        let mut source = File::open(path)?;
        let mut buffer = vec![0u8; 1 << 16];
        loop {
            let read = source.read(&mut buffer)?;
            if read == 0 {
                break;
            }
            hasher.update(&buffer[..read]);
            size += read as u64;
        }
        let digest = Digest::from_hasher(hasher);
        if self.blob_path(&digest, executable).is_file() {
            return Ok(FileEntry {
                digest,
                size,
                executable,
            });
        }
        self.put_file(path)
    }

    /// Puts `tree` into the store as its canonical encoding: its digest.
    ///
    /// # Errors
    ///
    /// That the store cannot be written.
    pub fn put_tree_of(&self, tree: &Tree) -> io::Result<Digest> {
        let bytes = serde_json::to_vec(tree).map_err(io::Error::other)?;
        self.put_bytes(&bytes)
    }

    /// The tree of `digest`.
    ///
    /// # Errors
    ///
    /// That the store does not hold it, or holds no tree under it.
    pub fn read_tree(&self, digest: &Digest) -> io::Result<Tree> {
        if let Some(tree) = self
            .trees
            .lock()
            .ok()
            .and_then(|trees| trees.get(digest).cloned())
        {
            return Ok(tree);
        }
        let bytes = self.read_blob(digest)?;
        let tree: Tree = serde_json::from_slice(&bytes).map_err(io::Error::other)?;
        if let Ok(mut trees) = self.trees.lock() {
            trees.insert(*digest, tree.clone());
        }
        Ok(tree)
    }

    /// The tree `digest`, as [`Store::read_tree`] gives it, but not kept for
    /// later: what an eviction, which reads every tree of the store once,
    /// reads them by, so that it holds no more than it needs of them.
    ///
    /// # Errors
    ///
    /// That the store does not hold it, or holds no tree under it.
    pub fn read_tree_once(&self, digest: &Digest) -> io::Result<Tree> {
        if let Some(tree) = self
            .trees
            .lock()
            .ok()
            .and_then(|trees| trees.get(digest).cloned())
        {
            return Ok(tree);
        }
        // Read as an eviction reads it, not noted as this process's to use.
        let plain = self.cas_path(digest, false);
        let path = if plain.is_file() {
            plain
        } else {
            self.cas_path(digest, true)
        };
        let bytes = fs::read(path)?;
        serde_json::from_slice(&bytes).map_err(io::Error::other)
    }

    /// Whether the file at `at` is a link to the blob of `entry`, as
    /// [`Store::materialise_file`] makes it where it can.
    #[must_use]
    pub fn is_linked(&self, entry: &FileEntry, at: &Path) -> bool {
        // Compared only, not relied on: a link keeps its contents if the
        // blob is evicted, so it is not published.
        let (Ok(meta), Ok(blob)) = (
            fs::symlink_metadata(at),
            fs::metadata(self.cas_path(&entry.digest, entry.executable)),
        ) else {
            return false;
        };
        meta.is_file() && (meta.dev(), meta.ino()) == (blob.dev(), blob.ino())
    }

    /// Whether the directory at `at` is the tree `digest` as
    /// [`Store::materialise_tree`] makes it, and nothing more: each file
    /// the first of its blob a link to it, any other a link to the copy of
    /// it of its number, each directory and symbolic link as the tree has
    /// it. A link can't be changed without its blob or copy, which is
    /// read-only, and a file put in its place is another; a file of its own,
    /// made where no link could be, is read again.
    #[must_use]
    pub fn is_materialised(&self, digest: &Digest, at: &Path) -> bool {
        self.materialised_with(digest, at, &mut HashMap::new())
            .unwrap_or(false)
    }

    /// [`Store::is_materialised`], `linked` how many files of the tree are
    /// links to each blob, by its contents and mode, already.
    fn materialised_with(
        &self,
        digest: &Digest,
        at: &Path,
        linked: &mut HashMap<FileEntry, u32>,
    ) -> io::Result<bool> {
        let tree = self.read_tree(digest)?;
        let mut names = HashSet::new();
        for entry in fs::read_dir(at)? {
            names.insert(entry?.file_name());
        }
        if names.len() != tree.entries.len() {
            return Ok(false);
        }
        for entry in &tree.entries {
            let path = at.join(entry.name());
            if !names.contains(path.file_name().unwrap_or_default()) {
                return Ok(false);
            }
            let meta = fs::symlink_metadata(&path)?;
            let same = match entry {
                TreeEntry::File { file, .. } => {
                    let count = linked.entry(*file).or_default();
                    let copy = *count;
                    *count += 1;
                    let linked = |source: PathBuf| {
                        fs::metadata(source)
                            .is_ok_and(|blob| (blob.dev(), blob.ino()) == (meta.dev(), meta.ino()))
                    };
                    meta.is_file()
                        && (meta.permissions().mode() & 0o111 != 0) == file.executable
                        && meta.permissions().mode() & 0o222 == 0
                        && meta.len() == file.size
                        && if copy == 0 {
                            // Compared only, as in `is_linked`.
                            linked(self.cas_path(&file.digest, file.executable))
                        } else {
                            // A copy of its own where no link to the
                            // store's could be made.
                            linked(self.copy_path(file, copy))
                                || (meta.nlink() == 1
                                    && self
                                        .put_file_once(&path)
                                        .is_ok_and(|read| read.digest == file.digest))
                        }
                }
                TreeEntry::Dir { tree, .. } => {
                    meta.is_dir() && self.materialised_with(tree, &path, linked)?
                }
                TreeEntry::Link { target, .. } => {
                    meta.file_type().is_symlink()
                        && fs::read_link(&path)
                            .is_ok_and(|read| read.as_os_str() == target.as_str())
                }
            };
            if !same {
                return Ok(false);
            }
        }
        Ok(true)
    }

    /// Makes the directory of the tree `digest` at `at`, a path that does
    /// not exist, every file materialised ([`Store::materialise_file`]), but
    /// that each of its files is a file of its own, as in the tree it was
    /// made from: the first file of a blob is a link to it, and any other of
    /// the same contents and mode a link to a copy of it, the second to the
    /// first copy and on ([`Store::link_copy`]). Two identical files of
    /// the tree are never one file, which `cp` refuses to copy between and
    /// `tar` would archive as a hard link.
    ///
    /// # Errors
    ///
    /// That the store does not hold it, or `at` cannot be written.
    pub fn materialise_tree(&self, digest: &Digest, at: &Path) -> io::Result<()> {
        self.materialise_tree_into(digest, at, &mut HashMap::new())
    }

    /// [`Store::materialise_tree`], `linked` how many files of the tree are
    /// links to each blob, by its contents and mode, already.
    fn materialise_tree_into(
        &self,
        digest: &Digest,
        at: &Path,
        linked: &mut HashMap<FileEntry, u32>,
    ) -> io::Result<()> {
        fs::create_dir_all(at)?;
        for entry in self.read_tree(digest)?.entries {
            match entry {
                TreeEntry::File { name, file } => {
                    let count = linked.entry(file).or_default();
                    let copy = *count;
                    *count += 1;
                    self.materialise_as(&file, &at.join(name), copy)?;
                }
                TreeEntry::Dir { name, tree } => {
                    self.materialise_tree_into(&tree, &at.join(name), linked)?;
                }
                TreeEntry::Link { name, target } => symlink(target, at.join(name))?,
            }
        }
        Ok(())
    }

    /// Makes the directory of the tree `digest` at `at`, a path that does not
    /// exist, every file a copy of its own ([`Store::copy_file`]): what
    /// someone may change.
    ///
    /// # Errors
    ///
    /// That the store does not hold it, or `at` cannot be written.
    pub fn copy_tree(&self, digest: &Digest, at: &Path) -> io::Result<()> {
        self.copy_tree_mounted(digest, at, &HashMap::new())
    }

    /// [`Store::copy_tree`], but each directory that is a mount point of
    /// `mounted`, by where it is, of its tree, made empty, for its tree to
    /// be mounted on it again.
    ///
    /// # Errors
    ///
    /// That the store does not hold it, or `at` cannot be written.
    pub fn copy_tree_mounted<S: std::hash::BuildHasher>(
        &self,
        digest: &Digest,
        at: &Path,
        mounted: &HashMap<PathBuf, Digest, S>,
    ) -> io::Result<()> {
        fs::create_dir_all(at)?;
        if mounted.get(at) == Some(digest) {
            return Ok(());
        }
        for entry in self.read_tree(digest)?.entries {
            match entry {
                TreeEntry::File { name, file } => self.copy_file(&file, &at.join(name))?,
                TreeEntry::Dir { name, tree } => {
                    self.copy_tree_mounted(&tree, &at.join(name), mounted)?;
                }
                TreeEntry::Link { name, target } => symlink(target, at.join(name))?,
            }
        }
        Ok(())
    }

    /// Where the record of `receipt` is.
    fn record_path(&self, receipt: &Digest) -> PathBuf {
        let hex = receipt.hex();
        self.root.join("records").join(&hex[..2]).join(hex)
    }

    /// The record of the step whose receipt is `receipt`, if the store has
    /// one, as `T`.
    ///
    /// # Errors
    ///
    /// That it cannot be read, or is no `T`.
    pub fn record<T: for<'a> Deserialize<'a>>(&self, receipt: &Digest) -> io::Result<Option<T>> {
        self.record_bytes(receipt)?
            .map(|bytes| serde_json::from_slice(&bytes).map_err(io::Error::other))
            .transpose()
    }

    /// The record kept by `key`, if the store has one, as it is written.
    ///
    /// # Errors
    ///
    /// That it cannot be read.
    pub fn record_bytes(&self, key: &Digest) -> io::Result<Option<Vec<u8>>> {
        // Published before it is read (see [Publishing what is
        // used](crate::using#publishing-what-is-used)).
        self.note_record(key);
        match fs::read(self.record_path(key)) {
            Ok(bytes) => Ok(Some(bytes)),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(error),
        }
    }

    /// Writes `record`, the record of the step whose receipt is `receipt`,
    /// whole, replacing any.
    ///
    /// # Errors
    ///
    /// That the store cannot be written.
    pub fn put_record<T: Serialize>(&self, receipt: &Digest, record: &T) -> io::Result<()> {
        let bytes = serde_json::to_vec(record).map_err(io::Error::other)?;
        self.put_record_bytes(receipt, &bytes)
    }

    /// Writes `bytes` as the record kept by `key`, whole, replacing any.
    ///
    /// # Errors
    ///
    /// That the store cannot be written.
    pub fn put_record_bytes(&self, key: &Digest, bytes: &[u8]) -> io::Result<()> {
        let target = self.record_path(key);
        if let Some(parent) = target.parent() {
            fs::create_dir_all(parent)?;
        }
        let temp = self.temp_path();
        let written = File::create(&temp).and_then(|mut out| {
            out.write_all(bytes)?;
            out.sync_all()
        });
        or_removed(&temp, written)?;
        fs::rename(temp, target)?;
        self.note_record(key);
        Ok(())
    }

    /// Whether the blob `digest` begins as a tree's encoding does, read no
    /// further: a file's contents are not read to find they aren't one.
    #[must_use]
    pub fn is_tree_blob(&self, digest: &Digest) -> bool {
        const BEGINNING: &[u8] = b"{\"entries\":";
        let mut start = [0u8; BEGINNING.len()];
        File::open(self.cas_path(digest, false))
            .and_then(|mut file| file.read_exact(&mut start))
            .is_ok_and(|()| start == BEGINNING)
    }
}
