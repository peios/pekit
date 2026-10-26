//! Scratch directories: what a step marked `#[scratch]` keeps between its
//! runs (see `design/additive.md`, F2).
//!
//! # Scratch directories
//!
//! A step marked `#[scratch]` sees a directory of its own at
//! [`SCRATCH`], kept between its runs, for a tool that does less on a
//! second run: Kbuild's or Cargo's state, or a compiler cache. It is the
//! step's by the platform it runs for and its directory under `/pekit-out`,
//! whatever its inputs, and keys nothing: a step must give what it would
//! give with an empty one, as Bazel's persistent workers must. So a run
//! that can't have it is given an empty one of its own, deleted when it
//! ends: a strict build's, so that what it gives depends on nothing a
//! scratch directory kept, and a run whose directory another run, of this
//! process or another, holds, so that no run waits for one.
//!
//! The directories are in the store's directory, under `scratch/`, each
//! named by a digest of whose it is, with a lock file beside it that a run
//! holds while it has the directory, its modification time when it was
//! last held. `pekit clean --cache` removes them with the store.

use std::fs::{self, File, TryLockError};
use std::io;
use std::path::{Path, PathBuf};
use std::time::SystemTime;

use crate::layers::remove_tree;
use crate::store::Digest;

/// Where a step sees its scratch directory.
pub const SCRATCH: &str = "/pekit-scratch";

/// A step's scratch directory, for as long as a run of it has it (see
/// [Scratch directories](#scratch-directories)).
#[derive(Debug)]
pub struct Scratch {
    /// The directory, on the host.
    dir: PathBuf,
    /// The lock it is held by, if it is the one kept between runs; else it
    /// is the run's own, and deleted when the run ends.
    lock: Option<File>,
}

impl Scratch {
    /// The scratch directory of the step whose directory under `/pekit-out`
    /// is `own`, for the platform `platform`, kept in the store at `store`:
    /// the one kept between its runs, unless `strict`, or another run holds
    /// it, in which case an empty one of its own at `fresh`.
    ///
    /// # Errors
    ///
    /// That a directory or its lock cannot be made.
    pub fn of(
        store: &Path,
        (platform, own): (&str, &str),
        fresh: &Path,
        strict: bool,
    ) -> io::Result<Self> {
        if !strict {
            let name = Digest::of(format!("pekit-scratch-1\0{platform}\0{own}").as_bytes()).hex();
            let kept = store.join("scratch");
            fs::create_dir_all(&kept)?;
            let lock = File::options()
                .create(true)
                .truncate(false)
                .write(true)
                .open(kept.join(format!("{name}.lock")))?;
            match lock.try_lock() {
                Ok(()) => {
                    let dir = kept.join(name);
                    fs::create_dir_all(&dir)?;
                    lock.set_modified(SystemTime::now())?;
                    return Ok(Self {
                        dir,
                        lock: Some(lock),
                    });
                }
                Err(TryLockError::WouldBlock) => {}
                Err(TryLockError::Error(error)) => return Err(error),
            }
        }
        remove_tree(fresh)?;
        fs::create_dir_all(fresh)?;
        Ok(Self {
            dir: fresh.to_path_buf(),
            lock: None,
        })
    }

    /// The directory, on the host.
    #[must_use]
    pub fn dir(&self) -> &Path {
        &self.dir
    }

    /// Whether it is the one kept between the step's runs.
    #[must_use]
    pub fn kept(&self) -> bool {
        self.lock.is_some()
    }
}

impl Drop for Scratch {
    fn drop(&mut self) {
        if self.lock.is_none() {
            let _ = remove_tree(&self.dir);
        }
    }
}
