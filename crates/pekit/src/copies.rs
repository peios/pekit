//! Copies: what a build copied out to the output directory, kept so that a
//! later build leaves a copy that is as it was made.
//!
//! # Copies
//!
//! Once a target's value has no holes, each file and directory under
//! `/pekit-out` it names is copied out to the output directory, a copy of
//! its own, which its user may change ([Building](crate::engine#building)).
//! A copy of a package's archives, or of a tree, can be large, and a build
//! that finds every step in the store would copy each out again. So the
//! build keeps, for each copy it makes, what it copied, the digest of its
//! contents, and a digest of the stamp of everything in it, as made: its
//! files', links' and directories' devices and inodes, sizes, modes and
//! times. A later build copying the same contents to the same path walks
//! the copy, and leaves it if its stamps' digest is the one kept; one that
//! anything changed, wrote, made, removed, renamed or changed the mode of,
//! has another, and is copied again. A file system's times are as coarse as
//! a few milliseconds, so a change made within the tick a copy was walked
//! in could leave its stamps as they were: as the build ends, it waits for
//! the clock to pass the newest copy's tick, walks each copy it made again,
//! and keeps those whose stamps are still as they were.
//!
//! What is kept is in `.pekit-copies` in the output directory, which
//! removing the directory removes with the copies.

use std::collections::HashMap;
use std::fs;
use std::io;
use std::os::unix::fs::MetadataExt as _;
use std::path::Path;

use serde::{Deserialize, Serialize};
use sha2::{Digest as _, Sha256};

use crate::record::Kept;
use crate::store::{Stamp, clock_beside};

/// The file in the output directory that keeps [`Copies`].
const COPIES_FILE: &str = ".pekit-copies";

/// What each copy a build made was as it made it (see [Copies](#copies)),
/// by its logical path.
#[derive(Debug, Default, Serialize, Deserialize)]
pub struct Copies {
    /// Each copy's contents, as [`what_of`] names them, and the digest of
    /// its stamps as made.
    made: HashMap<String, (String, String)>,
    /// Whether it changed since it was read.
    #[serde(skip)]
    changed: bool,
    /// The copies made by this build, not yet kept: each logical path,
    /// where it is, what it holds, and its stamps' digest and newest change
    /// time as made.
    #[serde(skip)]
    pending: Vec<Pending>,
}

/// A copy made by this build, not yet kept (see [`Copies::keep`]).
#[derive(Debug)]
struct Pending {
    /// Its logical path.
    logical: String,
    /// Where it is.
    at: std::path::PathBuf,
    /// What it holds, as [`what_of`] names it.
    what: String,
    /// Its stamps' digest, as made.
    stamps: String,
    /// The newest change time of what is in it, as made.
    newest: (i64, i64),
}

/// What a copy of `kept` holds, as [`Copies`] names it: its contents'
/// digest, and a file's mode.
fn what_of(kept: Kept) -> String {
    match kept {
        Kept::File(entry) => {
            let mode = if entry.executable { "x" } else { "" };
            format!("file {}{mode}", entry.digest.hex())
        }
        Kept::Dir(tree) => format!("tree {}", tree.hex()),
    }
}

/// The digest of the stamp of everything at `at`, a copy, and the newest
/// of their change times; `None` if it is not there.
fn stamped(at: &Path) -> Option<(String, (i64, i64))> {
    let mut hasher = Sha256::new();
    let mut newest = (i64::MIN, 0);
    let mut pending = vec![(String::new(), at.to_path_buf())];
    while let Some((name, path)) = pending.pop() {
        let meta = fs::symlink_metadata(&path).ok()?;
        let stamp = Stamp::of(&meta);
        hasher.update(name.as_bytes());
        hasher.update([0]);
        hasher.update(format!("{stamp:?}").as_bytes());
        hasher.update([0]);
        newest = newest.max((meta.ctime(), meta.ctime_nsec()));
        if meta.is_dir() {
            let mut entries: Vec<(String, std::path::PathBuf)> = fs::read_dir(&path)
                .ok()?
                .map(|entry| {
                    let entry = entry.ok()?;
                    let name = entry.file_name().into_string().ok()?;
                    Some((name, entry.path()))
                })
                .collect::<Option<_>>()?;
            // Walked in reverse name order, so taken in name order.
            entries.sort_by(|a, b| b.0.cmp(&a.0));
            pending.extend(
                entries
                    .into_iter()
                    .map(|(child, path)| (format!("{name}/{child}"), path)),
            );
        }
    }
    let digest = hasher.finalize();
    let hex = digest.iter().fold(String::new(), |mut hex, byte| {
        use std::fmt::Write as _;
        let _ = write!(hex, "{byte:02x}");
        hex
    });
    Some((hex, newest))
}

impl Copies {
    /// What the last build of the workspace whose output directory is
    /// `outputs` kept of its copies, or nothing.
    #[must_use]
    pub fn read(outputs: &Path) -> Self {
        fs::read(outputs.join(COPIES_FILE))
            .ok()
            .and_then(|bytes| serde_json::from_slice(&bytes).ok())
            .unwrap_or_default()
    }

    /// Whether the copy at `at`, of the logical path `path`, holds `kept`,
    /// as made and unchanged since: one that need not be copied again.
    #[must_use]
    pub fn unchanged(&self, path: &str, kept: Kept, at: &Path) -> bool {
        let Some((what, stamps)) = self.made.get(path) else {
            return false;
        };
        *what == what_of(kept) && stamped(at).is_some_and(|(now, _)| now == *stamps)
    }

    /// Notes that `at`, of the logical path `path`, is a copy of `kept` just
    /// made, to be kept once its clock tick has passed ([`Copies::keep`]).
    pub fn made(&mut self, path: &str, kept: Kept, at: &Path) {
        self.changed = true;
        self.made.remove(path);
        if let Some((stamps, newest)) = stamped(at) {
            self.pending.push(Pending {
                logical: path.to_owned(),
                at: at.to_path_buf(),
                what: what_of(kept),
                stamps,
                newest,
            });
        }
    }

    /// Notes that the copy of the logical path `path` is no more, as it is
    /// removed.
    pub fn forget(&mut self, path: &str) {
        self.changed |= self.made.remove(path).is_some();
    }

    /// Keeps what it knows in the output directory `outputs`, if it changed.
    ///
    /// # Errors
    ///
    /// That it cannot be written.
    pub fn keep(&mut self, outputs: &Path) -> io::Result<()> {
        if !self.changed {
            return Ok(());
        }
        fs::create_dir_all(outputs)?;
        let path = outputs.join(COPIES_FILE);
        // Once the clock has passed the newest copy's tick, within a tenth
        // of a second, each copy still as it was walked is kept.
        if let Some(newest) = self.pending.iter().map(|pending| pending.newest).max() {
            let waited = std::time::Instant::now();
            while clock_beside(&path)? <= newest {
                if waited.elapsed() > std::time::Duration::from_millis(100) {
                    self.pending.clear();
                    break;
                }
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
        }
        for pending in std::mem::take(&mut self.pending) {
            if stamped(&pending.at).is_some_and(|(now, _)| now == pending.stamps) {
                self.made
                    .insert(pending.logical, (pending.what, pending.stamps));
            }
        }
        let temp = path.with_extension(format!("tmp-{}", crate::sweep::token()));
        fs::write(&temp, serde_json::to_vec(self).map_err(io::Error::other)?)?;
        fs::rename(&temp, &path)
    }
}
