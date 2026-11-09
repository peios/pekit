//! Claims: which process runs a step that several want at once.
//!
//! # Claims
//!
//! Two builds that need one step at once, two worktrees at one commit say,
//! would each run it, since neither finds it in the store until one has
//! recorded it. So a build that misses a step in the store *claims* it
//! before it looks further, in the remote cache or by running it: it holds
//! the lock file `claims/<base>` in the store's directory, by the step's
//! base receipt, with `flock`, until the step is recorded, or has failed.
//! A build that finds the step claimed waits, on a thread of its own and
//! never the pool's, so that it goes on with every other step meanwhile,
//! and holds nothing of the machine's budget. Once it has the claim, it
//! looks in the store again, and finds the step there, recorded by the
//! build that held it; or, if that build failed it, or ended first, runs it
//! itself.
//!
//! A step's base receipt is the same for every context it might read
//! (see [Receipts](crate::receipt#receipts)), so a build may wait for one
//! that read a context set apart, then miss and run its own: a wait, never
//! a wrong result. A process that ends lets go of its claims, however it
//! ends. Claims are only taken where steps are taken from the store; and
//! an eviction, which runs when no other process has the store, removes
//! their files.

use std::fs::{self, File, TryLockError};
use std::io;
use std::path::{Path, PathBuf};

use crate::store::Digest;

/// What claiming a step came to.
#[derive(Debug)]
pub enum Claimed {
    /// The claim, held.
    Held(File),
    /// Another build holds it: its lock file, to wait for.
    Busy(PathBuf),
}

/// The lock file of the claim on the step whose base receipt is `base`, in
/// the store at `store` (see [Claims](#claims)).
#[must_use]
pub fn claim_path(store: &Path, base: &Digest) -> PathBuf {
    store.join("claims").join(base.hex())
}

/// Claims the step whose base receipt is `base` in the store at `store`, if
/// no other build holds it.
///
/// # Errors
///
/// That its lock file cannot be made or locked.
pub fn try_claim(store: &Path, base: &Digest) -> io::Result<Claimed> {
    let path = claim_path(store, base);
    let file = open(&path)?;
    match file.try_lock() {
        Ok(()) => Ok(Claimed::Held(file)),
        Err(TryLockError::WouldBlock) => Ok(Claimed::Busy(path)),
        Err(TryLockError::Error(error)) => Err(error),
    }
}

/// The claim whose lock file is `path`, held, waiting for it.
///
/// # Errors
///
/// That its lock file cannot be opened or locked.
pub fn claim(path: &Path) -> io::Result<File> {
    let file = open(path)?;
    file.lock()?;
    Ok(file)
}

/// The lock file `path`, opened, made, with its directory, if it is not
/// there.
///
/// # Errors
///
/// That it cannot be.
fn open(path: &Path) -> io::Result<File> {
    if let Some(dir) = path.parent() {
        fs::create_dir_all(dir)?;
    }
    File::options()
        .create(true)
        .truncate(false)
        .write(true)
        .open(path)
}
