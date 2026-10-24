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
//!   one when one is needed;
//! - a directory is a *tree*: its entries, sorted by name, each a file by
//!   digest and executability, a directory by its own tree's digest, or a
//!   symbolic link by its target, encoded canonically and stored as a blob
//!   itself, so that a tree's digest is its whole content's, as REAPI's
//!   directories are;
//! - `records/` holds each step's record by its receipt, and is never
//!   evicted with contents (see `design/engine.md`, E4);
//! - `tmp/` holds what is being written: a blob or a record is written
//!   there in full and renamed into place, so that a reader, or another
//!   process, never sees one in part.
//!
//! A file is put into the store by hashing it as it is copied ([`Store::put_file`]),
//! and taken out by a hard link to its blob, or a copy where a link cannot
//! be made, as across file systems ([`Store::materialise_file`]).

use std::fs::{self, File};
use std::io::{self, Read, Write};
use std::os::unix::fs::{PermissionsExt, symlink};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use serde::{Deserialize, Serialize};
use sha2::{Digest as _, Sha256};

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

/// A content store (see [The store](#the-store)).
#[derive(Clone, Debug)]
pub struct Store {
    /// Its directory.
    root: PathBuf,
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
        Ok(Self {
            root: root.to_path_buf(),
        })
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

    /// A new temporary file's path in the store's `tmp/`, which no other
    /// writer, of this process or another, uses.
    fn temp_path(&self) -> PathBuf {
        let number = NEXT_TEMP.fetch_add(1, Ordering::Relaxed);
        self.root
            .join("tmp")
            .join(format!("{}-{number}", std::process::id()))
    }

    /// Puts the file at `path`, read, into the store, hashing it as it is
    /// copied: its contents' digest, size and mode.
    ///
    /// # Errors
    ///
    /// That it cannot be read, or the store written.
    pub fn put_file(&self, path: &Path) -> io::Result<FileEntry> {
        let executable = fs::metadata(path)?.permissions().mode() & 0o111 != 0;
        let mut source = File::open(path)?;
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
            out.sync_all()?;
        }
        let digest = Digest::from_hasher(hasher);
        self.settle(&temp, &digest, executable)?;
        Ok(FileEntry {
            digest,
            size,
            executable,
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
        let mut out = File::create(&temp)?;
        out.write_all(bytes)?;
        out.sync_all()?;
        drop(out);
        self.settle(&temp, &digest, false)?;
        Ok(digest)
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
        fs::rename(temp, &target)
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
        let blob = self.blob_of_mode(entry)?;
        if let Some(parent) = at.parent() {
            fs::create_dir_all(parent)?;
        }
        if fs::hard_link(&blob, at).is_ok() {
            return Ok(());
        }
        fs::copy(&blob, at)?;
        let mode = if entry.executable { 0o555 } else { 0o444 };
        fs::set_permissions(at, fs::Permissions::from_mode(mode))
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
                    tree: self.put_tree(&at)?,
                }
            } else {
                TreeEntry::File {
                    name,
                    file: self.put_file(&at)?,
                }
            });
        }
        entries.sort_by(|a, b| a.name().cmp(b.name()));
        self.put_tree_of(&Tree { entries })
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
        let bytes = self.read_blob(digest)?;
        serde_json::from_slice(&bytes).map_err(io::Error::other)
    }

    /// Makes the directory of the tree `digest` at `at`, a path that does
    /// not exist, every file materialised ([`Store::materialise_file`]).
    ///
    /// # Errors
    ///
    /// That the store does not hold it, or `at` cannot be written.
    pub fn materialise_tree(&self, digest: &Digest, at: &Path) -> io::Result<()> {
        fs::create_dir_all(at)?;
        for entry in self.read_tree(digest)?.entries {
            match entry {
                TreeEntry::File { name, file } => self.materialise_file(&file, &at.join(name))?,
                TreeEntry::Dir { name, tree } => self.materialise_tree(&tree, &at.join(name))?,
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
        match fs::read(self.record_path(receipt)) {
            Ok(bytes) => serde_json::from_slice(&bytes)
                .map(Some)
                .map_err(io::Error::other),
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
        let target = self.record_path(receipt);
        if let Some(parent) = target.parent() {
            fs::create_dir_all(parent)?;
        }
        let temp = self.temp_path();
        let mut out = File::create(&temp)?;
        out.write_all(&bytes)?;
        out.sync_all()?;
        drop(out);
        fs::rename(temp, target)
    }
}
