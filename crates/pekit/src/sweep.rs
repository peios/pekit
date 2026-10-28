//! Sweeping: removing what a build is done with while it goes on.
//!
//! # Sweeping
//!
//! A step's staging directory holds a link or a copy of every file it was
//! given and made, often tens of thousands, and removing it takes as long as
//! staging it. A build removes none on the way to its next step: it renames
//! the directory, which takes no time, and a thread of its own removes it,
//! while the build goes on. The build waits for that thread before it ends,
//! so that it leaves nothing behind, and fails as it would have had it
//! removed the directory itself if one cannot be removed.
//!
//! # Names of its own
//!
//! Every pekit process names what it makes in a directory others share,
//! the store's `tmp/` and the scratch directory, with its *token*, 16 hex
//! digits drawn at random once ([`token`]): never its process's number,
//! which another process sharing the store, in another PID namespace say,
//! may have too.
//!
//! What a process was killed before it removed is removed by another
//! ([`sweep_stale`]), once it holds the store's lock exclusively, as an
//! eviction does: then no other process has the store open, so nothing in
//! the store's `tmp/`, and no staging directory, swept directory, unpacking
//! or checkout in the scratch directory, is any but its own.

use std::fmt::Write as _;
use std::fs::{self, TryLockError};
use std::io::{self, Read as _};
use std::path::{Path, PathBuf};
use std::sync::OnceLock;
use std::sync::mpsc::{Sender, channel};
use std::thread::{self, JoinHandle};

use crate::layers::remove_tree;
use crate::store::{Digest, Store};

/// This process's token (see [Names of its own](#names-of-its-own)).
#[must_use]
pub fn token() -> &'static str {
    static TOKEN: OnceLock<String> = OnceLock::new();
    TOKEN.get_or_init(|| {
        let mut bytes = [0u8; 8];
        let drawn =
            fs::File::open("/dev/urandom").and_then(|mut random| random.read_exact(&mut bytes));
        if drawn.is_err() {
            // As unlikely to be another's: this process, and now.
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default();
            let seed = format!(
                "{} {} {:?}",
                std::process::id(),
                now.as_nanos(),
                thread::current().id()
            );
            bytes.copy_from_slice(&Digest::of(seed.as_bytes()).bytes()[..8]);
        }
        bytes.iter().fold(String::new(), |mut hex, byte| {
            let _ = write!(hex, "{byte:02x}");
            hex
        })
    })
}

/// The kinds of what a build makes in the scratch directory, which it
/// names `KIND-TOKEN…`.
const SCRATCH_KINDS: [&str; 5] = ["step-", "swept-", "kept-", "unpack-", "git-"];

/// Removes what processes that are gone left in `store`'s `tmp/` and in the
/// scratch directory `scratch`, if no other process has the store open
/// (see [Names of its own](#names-of-its-own)): whether it could look.
///
/// # Errors
///
/// That the store's lock could not be taken or given back, or something
/// left could not be removed.
pub fn sweep_stale(store: &Store, scratch: &Path) -> io::Result<bool> {
    match store.held().try_lock() {
        Ok(()) => {}
        Err(TryLockError::WouldBlock) => {
            store.held().lock_shared()?;
            return Ok(false);
        }
        Err(TryLockError::Error(error)) => {
            store.held().lock_shared()?;
            return Err(error);
        }
    }
    let swept = sweep_held(&store.root().join("tmp"), scratch);
    // Held shared again, as every process that has the store holds it.
    store.held().lock_shared()?;
    swept.map(|()| true)
}

/// Removes what is not this process's in `tmp` and of a build's kinds in
/// `scratch`, the store's lock held exclusively.
fn sweep_held(tmp: &Path, scratch: &Path) -> io::Result<()> {
    let own = token();
    let entries = |dir: &Path| -> io::Result<Vec<fs::DirEntry>> {
        match fs::read_dir(dir) {
            Ok(entries) => entries.collect(),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(Vec::new()),
            Err(error) => Err(error),
        }
    };
    let remove = |path: &Path| -> io::Result<()> {
        if fs::symlink_metadata(path)?.is_dir() {
            remove_tree(path)
        } else {
            fs::remove_file(path)
        }
    };
    for entry in entries(tmp)? {
        if !entry.file_name().to_string_lossy().contains(own) {
            remove(&entry.path())?;
        }
    }
    for entry in entries(scratch)? {
        let name = entry.file_name().to_string_lossy().into_owned();
        if SCRATCH_KINDS.iter().any(|kind| name.starts_with(kind)) && !name.contains(own) {
            remove(&entry.path())?;
        }
    }
    Ok(())
}

/// A thread removing the directories a build is done with (see
/// [Sweeping](#sweeping)).
#[derive(Debug)]
pub struct Sweeper {
    /// Where it renames each directory to, before it is removed.
    swept: PathBuf,
    /// How many it has renamed.
    count: u64,
    /// Where the directories go, until it is finished.
    sender: Option<Sender<PathBuf>>,
    /// The thread, which ends with the first directory it could not
    /// remove, and why.
    thread: Option<JoinHandle<io::Result<()>>>,
}

impl Sweeper {
    /// A sweeper that renames each directory to a name beginning `swept`,
    /// which must be on their file system, before it removes it.
    #[must_use]
    pub fn new(swept: PathBuf) -> Self {
        let (sender, directories) = channel::<PathBuf>();
        let thread = thread::spawn(move || {
            let mut outcome = Ok(());
            for dir in directories {
                // Every directory is removed that can be, whatever the
                // first that could not.
                let removed = remove_tree(&dir);
                if outcome.is_ok() {
                    outcome = removed;
                }
            }
            outcome
        });
        Self {
            swept,
            count: 0,
            sender: Some(sender),
            thread: Some(thread),
        }
    }

    /// Removes the directory at `dir`, if there is one: at once, if it
    /// cannot be renamed, else on the sweeper's thread.
    ///
    /// # Errors
    ///
    /// That it could neither be renamed nor removed.
    pub fn remove(&mut self, dir: &Path) -> io::Result<()> {
        if fs::symlink_metadata(dir).is_err() {
            return Ok(());
        }
        let renamed = self.swept.with_file_name(format!(
            "{}-{}",
            self.swept
                .file_name()
                .map_or_else(String::new, |name| name.to_string_lossy().into_owned()),
            self.count
        ));
        self.count += 1;
        let Some(sender) = &self.sender else {
            return remove_tree(dir);
        };
        if fs::rename(dir, &renamed).is_err() {
            return remove_tree(dir);
        }
        if let Err(unsent) = sender.send(renamed) {
            return remove_tree(&unsent.0);
        }
        Ok(())
    }

    /// Waits for every directory sent to be removed.
    ///
    /// # Errors
    ///
    /// That one could not be removed.
    pub fn finish(&mut self) -> io::Result<()> {
        self.sender = None;
        match self.thread.take().map(JoinHandle::join) {
            Some(Ok(outcome)) => outcome,
            Some(Err(_)) => Err(io::Error::other("the thread removing directories stopped")),
            None => Ok(()),
        }
    }
}

impl Drop for Sweeper {
    /// Waits for every directory sent to be removed, as when the build
    /// fails.
    fn drop(&mut self) {
        let _ = self.finish();
    }
}
