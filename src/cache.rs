//! What `pekit package` keeps between runs, in pekit's cache.
//!
//! Each install tree a build gives is kept under a key: the SHA-256 of
//! everything pekit gives the build, its recipe files, its source's content
//! and timestamp, and the architecture. A build whose key has a tree does
//! not run again. What the key cannot see, such as the host's tools, is
//! not in it; `--rebuild` is for when those change.
//!
//! Each build also has a work directory of its own, kept between runs:
//! `source` is its copy of the source, kept up to date by syncing (see
//! [`crate::sync`]), and `out` is emptied before each run. Anything a build
//! leaves in either, or beside them, is there for its next run, so that a
//! build such as Kbuild redoes only what changed. `pekit release` uses
//! neither: it builds in a new directory, so that what it packs comes from
//! the source alone.

use std::fs::{self, File, TryLockError};
use std::io;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use sha2::{Digest, Sha256};
use tempfile::TempDir;

use crate::sync::{Record, hex};

/// The cache's layout and the key's form. Changing either changes this, so
/// that nothing is read as what it is not.
const FORMAT: &str = "pekit-cache 1";

/// The work directory's files that are pekit's own.
const LOCK: &str = "pekit-lock";
const RECORD: &str = "pekit-synced";
const UNPACKED: &str = "pekit-unpacked";

/// A build's work directory, used by this run alone until it is dropped.
pub struct WorkDir {
    path: PathBuf,
    /// Held until the directory is dropped.
    _lock: Option<File>,
    /// Removes the directory when it is dropped.
    _temp: Option<TempDir>,
}

impl WorkDir {
    /// The work directory build `build`, of the workspace at `root`, keeps
    /// for `arch` in the cache at `cache`, locked against other runs, which
    /// are waited for. If `fresh`, everything in it is removed first.
    pub fn kept(cache: &Path, root: &Path, build: &str, arch: &str, fresh: bool) -> Result<Self> {
        // Two workspaces can each have a build of one name.
        let workspace = &hex(&Sha256::digest(root.as_os_str().as_encoded_bytes()))[..16];
        let path = cache.join("work").join(workspace).join(build).join(arch);
        fs::create_dir_all(&path).with_context(|| path.display().to_string())?;
        let lock_path = path.join(LOCK);
        let lock = File::create(&lock_path).with_context(|| lock_path.display().to_string())?;
        match lock.try_lock() {
            Ok(()) => {}
            Err(TryLockError::WouldBlock) => {
                eprintln!("pekit: waiting for another run building {build}");
                lock.lock()
                    .with_context(|| lock_path.display().to_string())?;
            }
            Err(TryLockError::Error(error)) => {
                return Err(error).with_context(|| lock_path.display().to_string());
            }
        }
        if fresh {
            for entry in fs::read_dir(&path).with_context(|| path.display().to_string())? {
                let entry = entry.with_context(|| path.display().to_string())?;
                if entry.file_name() == LOCK {
                    continue;
                }
                let found = entry.path();
                let removed = if entry.file_type()?.is_dir() {
                    fs::remove_dir_all(&found)
                } else {
                    fs::remove_file(&found)
                };
                removed.with_context(|| found.display().to_string())?;
            }
        }
        Ok(WorkDir {
            path,
            _lock: Some(lock),
            _temp: None,
        })
    }

    /// A new, empty work directory in the cache at `cache`, removed when it
    /// is dropped.
    pub fn new(cache: &Path) -> Result<Self> {
        let dir = cache.join("work");
        fs::create_dir_all(&dir).with_context(|| dir.display().to_string())?;
        let temp = tempfile::Builder::new()
            .prefix("build-")
            .tempdir_in(&dir)
            .with_context(|| dir.display().to_string())?;
        Ok(WorkDir {
            path: temp.path().to_path_buf(),
            _lock: None,
            _temp: Some(temp),
        })
    }

    /// The build's copy of its source.
    pub fn source(&self) -> PathBuf {
        self.path.join("source")
    }

    /// What the last sync into [`WorkDir::source`] placed.
    pub fn record(&self) -> Result<Record> {
        Record::read(&self.path.join(RECORD))
    }

    pub fn write_record(&self, record: &Record) -> Result<()> {
        record.write(&self.path.join(RECORD))
    }

    /// A new, empty directory to unpack a fetched source into, removed by
    /// [`WorkDir::out`].
    pub fn unpacked(&self) -> Result<PathBuf> {
        let path = self.path.join(UNPACKED);
        remove_dir(&path)?;
        Ok(path)
    }

    /// The build's `out_path`, empty.
    pub fn out(&self) -> Result<PathBuf> {
        remove_dir(&self.path.join(UNPACKED))?;
        let out = self.path.join("out");
        remove_dir(&out)?;
        fs::create_dir(&out).with_context(|| out.display().to_string())?;
        Ok(out)
    }
}

fn remove_dir(path: &Path) -> Result<()> {
    match fs::remove_dir_all(path) {
        Err(error) if error.kind() != io::ErrorKind::NotFound => {
            Err(error).with_context(|| path.display().to_string())
        }
        _ => Ok(()),
    }
}

/// What a build's key is made of.
pub struct KeyParts<'a> {
    pub build: &'a str,
    pub arch: &'a str,
    /// Each file the build's recipe needs, by name, with its text.
    pub recipe: &'a [(String, String)],
    /// What the source is: a fetched archive's SHA-256, or a local tree's
    /// digest, and its timestamp.
    pub source: &'a str,
}

/// The key of a build of `parts` by this pekit, in lower-case hex.
pub fn key(parts: &KeyParts) -> String {
    let mut hasher = Sha256::new();
    let mut field = |value: &[u8]| {
        hasher.update(value.len().to_string());
        hasher.update(":");
        hasher.update(value);
    };
    field(FORMAT.as_bytes());
    field(env!("CARGO_PKG_VERSION").as_bytes());
    field(parts.build.as_bytes());
    field(parts.arch.as_bytes());
    field(parts.source.as_bytes());
    for (name, text) in parts.recipe {
        field(name.as_bytes());
        field(text.as_bytes());
    }
    hex(&hasher.finalize())
}

/// The install tree kept under `key` in the cache at `cache`, if there is
/// one.
pub fn kept(cache: &Path, key: &str) -> Option<PathBuf> {
    let path = cache.join("builds").join(key);
    path.is_dir().then_some(path)
}

/// Keeps the install tree `tree`, which a build in `work` gave, under `key`
/// in the cache at `cache`, replacing any kept there, and returns where it
/// is kept.
pub fn keep(cache: &Path, key: &str, tree: &Path, work: &WorkDir) -> Result<PathBuf> {
    let dir = cache.join("builds");
    fs::create_dir_all(&dir).with_context(|| dir.display().to_string())?;
    // Put together beside where it goes, so that only a whole tree is ever
    // there.
    let pending = tempfile::Builder::new()
        .prefix("pending-")
        .tempdir_in(&dir)
        .with_context(|| dir.display().to_string())?;
    let new = pending.path().join("tree");
    // A tree in out is moved, since out is emptied before the next run; any
    // other, such as the copy of the source, is copied.
    let tree = &fs::canonicalize(tree).with_context(|| tree.display().to_string())?;
    let out = fs::canonicalize(work.path.join("out"))?;
    if tree.starts_with(&out) {
        fs::rename(tree, &new).with_context(|| tree.display().to_string())?;
    } else {
        crate::stage::copy(tree, &new)?;
    }
    let path = dir.join(key);
    if path.exists() {
        fs::rename(&path, pending.path().join("old"))
            .with_context(|| path.display().to_string())?;
    }
    fs::rename(&new, &path).with_context(|| path.display().to_string())?;
    Ok(path)
}
