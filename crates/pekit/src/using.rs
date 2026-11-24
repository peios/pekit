//! What each process uses of the store, published, so that an eviction while
//! other processes have the store open keeps it.
//!
//! # Publishing what is used
//!
//! Each process that opens a store keeps a file of its own in the store's
//! `using/`, `<pid>-<token>`, and holds a lock on it, exclusively, for as
//! long as it lives, and removes it as it exits ([`retire`]): a file whose
//! lock can be taken is a dead process's, and is removed. Before it relies on anything of the store, a process
//! appends it there, once: each record it reads, before it reads it; each
//! blob it asks where it is, before it looks for it or links it; and each
//! tree it takes as complete. Each entry is a kind byte and a digest.
//!
//! An eviction while other processes have the store open (see [Shared
//! eviction](crate::evict#shared-eviction)) keeps everything any live
//! process has published, and runs only if every process that holds the
//! store publishes: one that does not, an older pekit say, could be using
//! anything. Which processes hold the store the kernel says, in
//! `/proc/locks`, by their process numbers.

use std::collections::{HashMap, HashSet};
use std::fs::{self, File, TryLockError};
use std::io::{self, Read as _, Write as _};
use std::os::unix::fs::MetadataExt as _;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, OnceLock, PoisonError};

use crate::store::Digest;

/// What an entry of a file of `using/` is of.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Kind {
    /// A blob, by its contents' digest.
    Blob,
    /// A record, by its key.
    Record,
    /// A tree taken as complete, by its digest: every blob it names, at any
    /// depth.
    Tree,
}

impl Kind {
    /// Its byte in a file.
    fn byte(self) -> u8 {
        match self {
            Kind::Blob => b'b',
            Kind::Record => b'r',
            Kind::Tree => b't',
        }
    }

    /// The kind of `byte`, if it is one's.
    fn of(byte: u8) -> Option<Self> {
        match byte {
            b'b' => Some(Kind::Blob),
            b'r' => Some(Kind::Record),
            b't' => Some(Kind::Tree),
            _ => None,
        }
    }
}

/// The size of an entry: its kind's byte and its digest.
const ENTRY: usize = 33;

/// A process's file of what it uses of one store (see [Publishing what is
/// used](#publishing-what-is-used)).
#[derive(Debug)]
pub struct Publisher {
    /// The file, appended to, and locked exclusively for the process's life.
    file: Mutex<File>,
    /// Where it is.
    path: PathBuf,
    /// What it has published, so that each is published once; and, once a
    /// write failed, what it uses, unpublished.
    published: Mutex<HashSet<(Kind, Digest)>>,
    /// Whether a write failed: then the file is gone, nothing more is
    /// written, and an eviction beside this process tells it publishes
    /// nothing.
    broken: std::sync::atomic::AtomicBool,
}

impl Publisher {
    /// Publishes that this process uses `digest`, of the kind `kind`, if it
    /// has not: before it relies on it. Should the write fail, the disk
    /// full say, the file is removed and nothing more is published, so that
    /// no eviction beside this process trusts what it did not get to write.
    pub fn publish(&self, kind: Kind, digest: &Digest) {
        use std::sync::atomic::Ordering;
        let new = self
            .published
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .insert((kind, *digest));
        if !new || self.broken.load(Ordering::Acquire) {
            return;
        }
        let mut entry = [0u8; ENTRY];
        entry[0] = kind.byte();
        entry[1..].copy_from_slice(digest.bytes());
        let mut file = self.file.lock().unwrap_or_else(PoisonError::into_inner);
        if file.write_all(&entry).is_err() {
            self.broken.store(true, Ordering::Release);
            let _ = fs::remove_file(&self.path);
        }
    }

    /// Each blob this process uses, as it published them.
    #[must_use]
    pub fn blobs(&self) -> HashSet<Digest> {
        self.published
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .iter()
            .filter(|(kind, _)| *kind == Kind::Blob)
            .map(|(_, digest)| *digest)
            .collect()
    }

    /// Stops publishing, as a failed write does: for a test of what an
    /// eviction beside such a process does.
    #[doc(hidden)]
    pub fn break_for_test(&self) {
        self.break_off();
    }

    /// Publishes no more, and removes the file.
    fn break_off(&self) {
        let file = self.file.lock().unwrap_or_else(PoisonError::into_inner);
        self.broken
            .store(true, std::sync::atomic::Ordering::Release);
        let _ = fs::remove_file(&self.path);
        drop(file);
    }
}

/// Each store's publisher of this process, by the store's directory.
static PUBLISHERS: OnceLock<Mutex<HashMap<PathBuf, Arc<Publisher>>>> = OnceLock::new();

/// This process's publisher of what it uses of the store at `root`, made
/// once: none if its file cannot be made, which an eviction while this
/// process holds the store then tells by it. The file is locked before it
/// is put where others look, so that none takes it for a dead process's.
#[must_use]
pub fn publisher(root: &Path) -> Option<Arc<Publisher>> {
    let mut publishers = PUBLISHERS
        .get_or_init(Mutex::default)
        .lock()
        .unwrap_or_else(PoisonError::into_inner);
    if let Some(publisher) = publishers.get(root) {
        return Some(Arc::clone(publisher));
    }
    let dir = root.join("using");
    fs::create_dir_all(&dir).ok()?;
    let name = format!("{}-{}", std::process::id(), crate::sweep::token());
    let made = dir.join(format!(".{name}"));
    let file = File::options().create(true).append(true).open(&made).ok()?;
    file.try_lock().ok()?;
    let path = dir.join(name);
    fs::rename(&made, &path).ok()?;
    let publisher = Arc::new(Publisher {
        file: Mutex::new(file),
        path,
        published: Mutex::default(),
        broken: std::sync::atomic::AtomicBool::new(false),
    });
    publishers.insert(root.to_path_buf(), Arc::clone(&publisher));
    Some(publisher)
}

/// Removes this process's file of what it uses of each store it opened, and
/// publishes no more: for when it is done with the stores, as it exits.
/// One left by a process that died is removed by the next process that
/// looks at `using/` with the store to itself, or evicts beside others.
pub fn retire() {
    let Some(publishers) = PUBLISHERS.get() else {
        return;
    };
    let publishers = publishers.lock().unwrap_or_else(PoisonError::into_inner);
    for publisher in publishers.values() {
        publisher.break_off();
    }
}

/// What a live process has published that it uses of a store.
#[derive(Debug, Default)]
pub struct Used {
    /// The process's number.
    pub pid: u32,
    /// Each blob.
    pub blobs: HashSet<Digest>,
    /// Each record.
    pub records: HashSet<Digest>,
    /// Each tree taken as complete.
    pub trees: HashSet<Digest>,
}

/// What each live process has published that it uses of the store at
/// `root`; the file of each dead one removed.
///
/// # Errors
///
/// That `using/` cannot be listed.
pub fn live(root: &Path) -> io::Result<Vec<Used>> {
    let dir = root.join("using");
    let entries = match fs::read_dir(&dir) {
        Ok(entries) => entries,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(error) => return Err(error),
    };
    let mut found = Vec::new();
    for entry in entries {
        let entry = entry?;
        let path = entry.path();
        // One being made, not yet locked.
        if entry.file_name().to_string_lossy().starts_with('.') {
            continue;
        }
        let Ok(mut file) = File::open(&path) else {
            continue;
        };
        match file.try_lock() {
            // No process holds it: its process is dead.
            Ok(()) => {
                let _ = fs::remove_file(&path);
                continue;
            }
            Err(TryLockError::WouldBlock) => {}
            Err(TryLockError::Error(_)) => continue,
        }
        let name = entry.file_name().to_string_lossy().into_owned();
        let pid = name
            .split('-')
            .next()
            .and_then(|pid| pid.parse().ok())
            .unwrap_or(0);
        let mut bytes = Vec::new();
        file.read_to_end(&mut bytes)?;
        let mut used = Used {
            pid,
            ..Used::default()
        };
        for entry in bytes.as_chunks::<ENTRY>().0 {
            let Some(digest) = entry
                .get(1..)
                .and_then(|bytes| <[u8; 32]>::try_from(bytes).ok())
                .map(Digest::from_bytes)
            else {
                continue;
            };
            match Kind::of(entry[0]) {
                Some(Kind::Blob) => used.blobs.insert(digest),
                Some(Kind::Record) => used.records.insert(digest),
                Some(Kind::Tree) => used.trees.insert(digest),
                None => false,
            };
        }
        found.push(used);
    }
    Ok(found)
}

/// The numbers of the processes that hold the lock file `lock`, by the
/// kernel's `/proc/locks`; none if it cannot be read.
fn holders(lock: &Path) -> Option<Vec<u32>> {
    let meta = fs::metadata(lock).ok()?;
    let dev = meta.dev();
    let node = format!(
        "{:02x}:{:02x}:{}",
        rustix::fs::major(dev),
        rustix::fs::minor(dev),
        meta.ino()
    );
    let text = fs::read_to_string("/proc/locks").ok()?;
    let mut pids = Vec::new();
    for line in text.lines() {
        // A lock waited for, under the one that holds it, is no holder.
        if line.contains("->") {
            continue;
        }
        let fields: Vec<&str> = line.split_whitespace().collect();
        if fields.get(1) == Some(&"FLOCK") && fields.get(5) == Some(&node.as_str()) {
            pids.push(fields.get(4).and_then(|pid| pid.parse().ok())?);
        }
    }
    Some(pids)
}

/// Whether every process that holds the store at `root`, `live` those that
/// publish what they use, publishes: none that does not, an older pekit say,
/// could be using what an eviction removes.
#[must_use]
pub fn all_publish(root: &Path, live: &[Used]) -> bool {
    let Some(holders) = holders(&root.join("lock")) else {
        return false;
    };
    // This process holds it too: if the kernel's list does not say so, it
    // is not read right, and says nothing of the others.
    if !holders.contains(&std::process::id()) {
        return false;
    }
    let publishing: HashSet<u32> = live.iter().map(|used| used.pid).collect();
    holders
        .iter()
        .all(|pid| *pid != 0 && publishing.contains(pid))
}
