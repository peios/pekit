//! Exposure: putting a build's outputs under the output directory as what
//! is not hermetic needs them.
//!
//! # Exposure
//!
//! Each output of a build, a step's, a fetch's or an unpacking's, is kept in
//! the store by its logical path under `/pekit-out`. A step is staged with
//! the outputs it is given from the store; only what is not hermetic, a
//! target's own code and the programs it runs, reads them from the output
//! directory, `pekit-out/`. So an output is *exposed* there, hard-linked and
//! read-only, only once something may read it from the disk, and a build
//! that changed nothing writes and checks nothing there for outputs no one
//! reads:
//!
//! - **Each exposure waits** ([`Exposures::defer`]), the last of a path
//!   the one kept.
//! - **A target's `dir.files()` and `file.read()`** of a path inside an
//!   output of the build are answered from the store's tree of it
//!   ([`read_output`]), which is what the disk would show once it is
//!   exposed, but only until a target's own effect has run a program or
//!   written anything ([`Exposures::to_disk`]), which may have changed the
//!   output directory; then they read the disk, as a read of a path in no
//!   output does.
//! - **Every waiting exposure is made** before a target's own effect that
//!   may read the output directory: a read that is not answered from the
//!   store and whose path is, or leads into, the output directory, a read
//!   of a path GCSL code names as a string, a keychain's file, and any
//!   effect that runs a program or writes.
//! - **What a target gives** is copied out as the build ends: each
//!   exposure above one of its paths is made first, and those of its paths
//!   and below are dropped, the copy taking their place.
//! - **`pekit run`** makes every waiting exposure before the target's
//!   results are copied out, since the program it runs may read any of
//!   them. Any other build leaves an exposure no one needed unmade, and
//!   `pekit-out/.steps/` and `.unpack/` hold only what was.

use std::collections::BTreeMap;

use gcsl::eval::plain::Plain;

use crate::record::Kept;
use crate::store::{Digest, FileEntry, Store, TreeEntry};

/// Which outputs a build puts under the output directory (see
/// [Exposure](#exposure)).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Exposing {
    /// Those something may read from the disk.
    AsNeeded,
    /// Every one, as the build ends, for a program that may read any of
    /// them, as `pekit run`'s may.
    All,
}

/// The exposures a build has yet to make (see [Exposure](#exposure)).
#[derive(Debug, Default)]
pub struct Exposures {
    /// Each waiting exposure, by its logical path: in order, a directory
    /// before what is inside it.
    pending: BTreeMap<String, Kept>,
    /// Whether a target's own effect has run a program or written, after
    /// which reads of outputs go to the disk.
    on_disk: bool,
}

impl Exposures {
    /// Notes that the output `path` is `kept`, to be exposed once something
    /// may read it from the disk, in place of what was noted of it before.
    pub fn defer(&mut self, path: String, kept: Kept) {
        self.pending.insert(path, kept);
    }

    /// Every waiting exposure, a directory before what is inside it, none
    /// waiting any more.
    pub fn take_all(&mut self) -> Vec<(String, Kept)> {
        std::mem::take(&mut self.pending).into_iter().collect()
    }

    /// The waiting exposures of directories `path` is inside, outermost
    /// first, none of them waiting any more.
    pub fn take_above(&mut self, path: &str) -> Vec<(String, Kept)> {
        let above: Vec<String> = self
            .pending
            .keys()
            .filter(|at| within(path, at) && at.as_str() != path)
            .cloned()
            .collect();
        above
            .into_iter()
            .filter_map(|at| self.pending.remove_entry(&at))
            .collect()
    }

    /// Drops the waiting exposures of `path` and of what is inside it.
    pub fn forget_within(&mut self, path: &str) {
        self.pending.retain(|at, _| !within(at, path));
    }

    /// Whether reads of outputs may still be answered from the store.
    #[must_use]
    pub const fn answers_from_store(&self) -> bool {
        !self.on_disk
    }

    /// Notes that a target's own effect may have changed the output
    /// directory: reads of outputs go to the disk from now on.
    pub const fn to_disk(&mut self) {
        self.on_disk = true;
    }
}

/// Whether the logical path `path` is `root` or inside it.
#[must_use]
pub fn within(path: &str, root: &str) -> bool {
    path.strip_prefix(root)
        .is_some_and(|rest| rest.is_empty() || rest.starts_with('/'))
}

/// What a target reads of an output, answered from the store (see
/// [Exposure](#exposure)).
#[derive(Clone, Copy, Debug)]
pub enum OutputRead {
    /// `file.read()` of a file the store keeps as this.
    Text(FileEntry),
    /// `dir.files()` of a directory whose tree is this.
    Files(Digest),
}

/// What `read` of the output `path`, as GCSL code names it, gives, as
/// GCSL's own `file.read()` or `dir.files()` gives it of the disk: the
/// file's contents, which must be UTF-8, or every entry of the tree that is
/// no directory, a symbolic link included, by its path, sorted.
///
/// # Errors
///
/// GCSL's own message of the failure, with its note if it has one: that
/// the store cannot give it, or the file is not UTF-8.
pub fn read_output(
    store: &Store,
    read: OutputRead,
    path: &str,
) -> Result<Plain, (String, Option<String>)> {
    match read {
        OutputRead::Text(entry) => match store.read_blob(&entry.digest).map(String::from_utf8) {
            Ok(Ok(text)) => Ok(Plain::Str(text)),
            Ok(Err(_)) => Err((format!("`{path}` is not UTF-8 text"), None)),
            Err(error) => Err((format!("cannot read `{path}`"), Some(error.to_string()))),
        },
        OutputRead::Files(tree) => {
            let mut found = Vec::new();
            listed(store, &tree, path, &mut found)
                .map_err(|error| (format!("cannot list `{path}`"), Some(error.to_string())))?;
            found.sort_unstable();
            Ok(Plain::List(
                found
                    .into_iter()
                    .map(|path| Plain::File {
                        path,
                        identity: None,
                    })
                    .collect(),
            ))
        }
    }
}

/// Pushes the path of every entry of the tree `digest`, the directory
/// `under`, that is no directory onto `found`, descending into its
/// directories.
fn listed(
    store: &Store,
    digest: &Digest,
    under: &str,
    found: &mut Vec<String>,
) -> std::io::Result<()> {
    for entry in store.read_tree(digest)?.entries {
        match entry {
            TreeEntry::Dir { name, tree } => {
                listed(store, &tree, &format!("{under}/{name}"), found)?;
            }
            TreeEntry::File { name, .. } | TreeEntry::Link { name, .. } => {
                found.push(format!("{under}/{name}"));
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn file(n: u8) -> Kept {
        Kept::File(FileEntry {
            digest: Digest::of(&[n]),
            executable: false,
            size: 1,
        })
    }

    #[test]
    fn a_directory_is_exposed_before_what_is_inside_it() {
        let mut exposures = Exposures::default();
        exposures.defer("/pekit-out/a/b".to_owned(), file(1));
        exposures.defer("/pekit-out/a-b".to_owned(), file(2));
        exposures.defer("/pekit-out/a".to_owned(), file(3));
        let order: Vec<String> = exposures.take_all().into_iter().map(|(at, _)| at).collect();
        assert_eq!(order, ["/pekit-out/a", "/pekit-out/a-b", "/pekit-out/a/b"]);
    }

    #[test]
    fn a_copied_out_result_takes_what_is_above_it_and_drops_what_is_within_it() {
        let mut exposures = Exposures::default();
        for at in [
            "/pekit-out/s",
            "/pekit-out/s/r",
            "/pekit-out/s/r/x",
            "/pekit-out/s/rr",
        ] {
            exposures.defer(at.to_owned(), file(1));
        }
        let above: Vec<String> = exposures
            .take_above("/pekit-out/s/r")
            .into_iter()
            .map(|(at, _)| at)
            .collect();
        assert_eq!(above, ["/pekit-out/s"]);
        exposures.forget_within("/pekit-out/s/r");
        let left: Vec<String> = exposures.take_all().into_iter().map(|(at, _)| at).collect();
        assert_eq!(left, ["/pekit-out/s/rr"]);
    }
}
