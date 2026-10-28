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

use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::mpsc::{Sender, channel};
use std::thread::{self, JoinHandle};

use crate::layers::remove_tree;

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
