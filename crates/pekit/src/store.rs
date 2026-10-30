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
//! for: as git keeps its index. A file that is a
//! link to a blob this process linked, by its device and inode, is that
//! blob, and is not read. A file read is hashed before it is copied, and
//! not copied if its blob is there. A directory said to be fixed, such as a
//! sealed composition, is walked once until it is said to have changed.
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

use std::cell::RefCell;
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
    /// Each file's stamp, and what the store keeps of it.
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
}

impl Seen {
    /// Says that the directories `fixed` cannot change, until
    /// [`Seen::changed`] says they did.
    pub fn fix(&mut self, fixed: Vec<PathBuf>) {
        self.fixed = fixed;
    }

    /// Forgets each file that is no longer there.
    pub fn retain_present(&mut self) {
        self.files.retain(|path, _| path.exists());
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

impl Store {
    /// The store in the directory `root`, its directories made if they are
    /// not there.
    ///
    /// # Errors
    ///
    /// That the directories cannot be made.
    pub fn open(root: &Path) -> io::Result<Self> {
        for part in ["cas", "records", "tmp"] {
            fs::create_dir_all(root.join(part))?;
        }
        let held = File::options()
            .create(true)
            .truncate(false)
            .write(true)
            .open(root.join("lock"))?;
        // An eviction running elsewhere is waited for.
        held.lock_shared()?;
        Ok(Self {
            root: root.to_path_buf(),
            linked: Arc::default(),
            held: Arc::new(held),
            used: Arc::default(),
            trees: Arc::default(),
            complete: Arc::default(),
            materialised: Arc::default(),
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
        if let Ok(mut used) = self.used.lock() {
            used.records.insert(*key);
        }
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
        File::create(self.complete_path(digest)).map(drop)
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
        if self
            .complete
            .lock()
            .is_ok_and(|complete| complete.contains(digest))
            || self.complete_path(digest).exists()
        {
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
        let hex = digest.hex();
        let name = if executable {
            format!("{hex}.x")
        } else {
            hex.clone()
        };
        self.root.join("cas").join(&hex[..2]).join(name)
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
        let executable = fs::metadata(path)?.permissions().mode() & 0o111 != 0;
        self.put_reader(&mut File::open(path)?, executable)
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
        {
            let mut out = File::create(&temp)?;
            let mut buffer = vec![0u8; 1 << 16];
            loop {
                let read = source.read(&mut buffer)?;
                if read == 0 {
                    break;
                }
                let chunk = &buffer[..read];
                hasher.update(chunk);
                out.write_all(chunk)?;
                size += read as u64;
            }
        }
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
        File::create(&temp)?.write_all(bytes)?;
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
        if fs::hard_link(&path, at).is_err() {
            let source = self.blob_of_mode(entry)?;
            let temp = self.temp_path();
            fs::copy(&source, &temp)?;
            let mode = if entry.executable { 0o555 } else { 0o444 };
            fs::set_permissions(&temp, fs::Permissions::from_mode(mode))?;
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
        fs::copy(&other, &temp)?;
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
        for entry in fs::read_dir(path)? {
            let entry = entry?;
            let name = entry.file_name().into_string().map_err(|name| {
                io::Error::other(format!("`{}` is no UTF-8 name", name.display()))
            })?;
            let kind = entry.file_type()?;
            let at = entry.path();
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
        let mut walk = Walk { seen, refuse, now };
        let meta = fs::symlink_metadata(path)?;
        if refuse(path, &meta) {
            return Ok(None);
        }
        self.at_once(|| self.tree_seen(path, &mut walk))
    }

    /// [`Store::put_tree_seen`] of `path`, as `walk` has it.
    fn tree_seen(&self, path: &Path, walk: &mut Walk<'_>) -> io::Result<Option<Digest>> {
        let mut entries = Vec::new();
        for entry in fs::read_dir(path)? {
            let entry = entry?;
            let name = entry.file_name().into_string().map_err(|name| {
                io::Error::other(format!("`{}` is no UTF-8 name", name.display()))
            })?;
            let at = entry.path();
            let meta = fs::symlink_metadata(&at)?;
            let kind = meta.file_type();
            entries.push(if kind.is_symlink() {
                if (walk.refuse)(&at, &meta) {
                    return Ok(None);
                }
                let target = fs::read_link(&at)?.to_string_lossy().into_owned();
                TreeEntry::Link { name, target }
            } else if kind.is_dir() {
                if let Some(tree) = walk.seen.mounted.get(&at).or(walk.seen.dirs.get(&at)) {
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
        let mut walk = Walk {
            seen,
            refuse: &never,
            now,
        };
        self.file_seen(path, &meta, &mut walk)?
            .ok_or_else(|| io::Error::other("refused"))
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
        if let Some((known, entry)) = walk.seen.files.get(path)
            && *known == stamp
            && self.blob_path(&entry.digest, entry.executable).is_file()
        {
            return Ok(Some(*entry));
        }
        if (walk.refuse)(path, meta) {
            return Ok(None);
        }
        let entry = self.put_file_once(path)?;
        if stamp.changed < walk.now {
            walk.seen.files.insert(path.to_path_buf(), (stamp, entry));
        } else {
            walk.seen.files.remove(path);
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

    /// Whether the file at `at` is a link to the blob of `entry`, as
    /// [`Store::materialise_file`] makes it where it can.
    #[must_use]
    pub fn is_linked(&self, entry: &FileEntry, at: &Path) -> bool {
        let (Ok(meta), Ok(blob)) = (
            fs::symlink_metadata(at),
            fs::metadata(self.blob_path(&entry.digest, entry.executable)),
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
                            linked(self.blob_path(&file.digest, file.executable))
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
        match fs::read(self.record_path(key)) {
            Ok(bytes) => {
                self.note_record(key);
                Ok(Some(bytes))
            }
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
        let mut out = File::create(&temp)?;
        out.write_all(bytes)?;
        out.sync_all()?;
        drop(out);
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
        File::open(self.blob_path(digest, false))
            .and_then(|mut file| file.read_exact(&mut start))
            .is_ok_and(|()| start == BEGINNING)
    }
}
