//! Eviction: keeping the store within the size its machine allows, by
//! GreedyDual-Size (see `design/additive.md`, F3).
//!
//! # Eviction
//!
//! A store may be as large as `PEKIT_CACHE_LIMIT` says ([`limit_of`]). As
//! a build ends, [`evict`] journals what the build used, and, if the store
//! is larger, evicts until it is below nine tenths of it:
//!
//! - **What is evicted** is a record's contents, together: every blob and
//!   tree its record names, a step's, a program's, an unpacking's or a
//!   download's. A record stays when its contents go, and is no hit, so
//!   what made it is done again. A blob is removed once no *live* record,
//!   one whose contents are all there, names it at any depth; a blob no
//!   record names goes first, as costing nothing to make again. Each blob
//!   counts what names it, live records directly and live trees as an
//!   entry, so that a record's contents are never listed whole: evicting a
//!   record lets go of what it names, a blob let go of by everything is
//!   removed, and a tree removed lets go of its entries in turn. A store's
//!   trees share their subtrees, so what an eviction holds grows with the
//!   store's blobs and trees' entries, not with what its records name.
//! - **The order** is each record's priority, `L + cost / size`, set when it
//!   was last used, its cost the time it took to make in milliseconds, its
//!   size its contents' in bytes, and `L` the priority of the last evicted.
//!   A scratch directory is evicted whole by the same order, as costing
//!   nothing, and never while a run holds it.
//! - **What was used** is journalled in `usage/`, one file per build; the
//!   eviction reads every journal and keeps each record's priority, with
//!   `L`, in `eviction.json`, and which records it evicted.
//! - **The store's size** is its blobs' and its scratch directories'. Its
//!   blobs' is kept in `size` ([`Ledger`]): what an eviction last measured,
//!   walking every blob, a day ago at most, and the bytes of the blobs each
//!   build wrote since, added as each of its steps is recorded and as it
//!   ends ([`ledger_written`]). The blobs are walked only when that, which
//!   can only be too large, is above the limit, or is older, or is not
//!   there; what a build killed wrote after its last step is added by
//!   none, which the next walk sets right.
//! - **Concurrency:** every process holds the store's lock shared while it
//!   has the store open. An eviction takes it exclusively without waiting,
//!   so it evicts nothing while another process has the store; having it,
//!   it removes the claims' files, which no process then holds
//!   ([Claims](crate::claims#claims)). A store its ledger says is over its
//!   limit while another process has it is an error, which a build warns
//!   of, rather than nothing evicted silently, unless it can be evicted
//!   beside them, as follows.
//!
//! # Shared eviction
//!
//! While other processes have the store open, a build evicts beside them
//! ([`evict_shared`]), if every process that holds the store publishes what
//! it uses ([Publishing what is used](crate::using#publishing-what-is-used))
//! and no other evicts so: one at a time, by the `evicting` lock.
//!
//! 1. It takes what each live process published, and evicts as an eviction
//!    alone does, keeping all of it, but moves each blob it evicts aside,
//!    into `tmp/evicting-<token>/`, rather than deleting it.
//! 2. It reads what each live process published again, and moves back each
//!    blob one published meanwhile, directly or through a record or tree,
//!    unless a process put a blob of its name meanwhile: then the one aside
//!    goes, of the same contents.
//! 3. It deletes what is aside.
//!
//! A process publishes what it uses before it looks for it: published
//! before step 2, it is moved back, though the process may have found it
//! gone meanwhile and put it again; published after, the process finds it
//! gone, and puts or builds it again. So none relies on what is deleted. It
//! leaves the materialised trees, which a running step may have mounted,
//! and the exposures of what it evicts, which a build may be writing,
//! for an eviction alone; it removes every mark that a tree is complete,
//! as every eviction does. What an evictor that died left aside is deleted
//! by the next.

use std::cmp::Ordering;
use std::collections::{BTreeMap, BinaryHeap, HashMap, HashSet};
use std::fs::{self, File, TryLockError};
use std::io::{self, Write as _};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::layers::remove_tree;
use crate::store::{Digest, Store, TreeEntry, Used};

/// The limit a store has when `PEKIT_CACHE_LIMIT` says none: 100 GiB.
pub const DEFAULT_LIMIT: u64 = 100 << 30;

/// The limit `PEKIT_CACHE_LIMIT`, `setting`, gives the store: its default
/// if it is unset or empty, none if it is `0`.
///
/// # Errors
///
/// That it is no size: a number of bytes, with a unit or none.
pub fn limit_of(setting: Option<&str>) -> Result<Option<u64>, String> {
    limit_named(setting, "`PEKIT_CACHE_LIMIT`")
}

/// The limit `setting`, what `name` sets, gives the store: its default if
/// it is unset or empty, none if it is `0`.
///
/// # Errors
///
/// That it is no size: a number of bytes, with a unit or none.
pub fn limit_named(setting: Option<&str>, name: &str) -> Result<Option<u64>, String> {
    let Some(text) = setting.map(str::trim).filter(|text| !text.is_empty()) else {
        return Ok(Some(DEFAULT_LIMIT));
    };
    match size_of(text) {
        Some(0) => Ok(None),
        Some(size) => Ok(Some(size)),
        None => Err(format!(
            "{name} is `{text}`, which is no size: write a number of bytes, such as `50GiB`, \
             `500MB` or `0` for no limit"
        )),
    }
}

/// The number of bytes `text` says: digits, then a unit or none.
pub(crate) fn size_of(text: &str) -> Option<u64> {
    let text = text.trim();
    let digits = text
        .find(|c: char| !c.is_ascii_digit())
        .unwrap_or(text.len());
    let number: u64 = text[..digits].parse().ok()?;
    let scale: u64 = match text[digits..].trim() {
        "" | "B" => 1,
        "K" | "KiB" => 1 << 10,
        "M" | "MiB" => 1 << 20,
        "G" | "GiB" => 1 << 30,
        "T" | "TiB" => 1 << 40,
        "KB" => 1_000,
        "MB" => 1_000_000,
        "GB" => 1_000_000_000,
        "TB" => 1_000_000_000_000,
        _ => return None,
    };
    number.checked_mul(scale)
}

/// What an eviction kept in the store between evictions.
#[derive(Debug, Default, Serialize, Deserialize)]
struct Priorities {
    /// `L`: the priority of the last evicted.
    inflation: u64,
    /// Each record's priority, by its key's hex digits.
    records: BTreeMap<String, u64>,
    /// Each scratch directory's priority, by its name.
    scratch: BTreeMap<String, u64>,
    /// The records whose contents were evicted, until each is used again.
    evicted: HashSet<String>,
}

/// What an eviction during a build keeps, whatever it costs: what the
/// build has used so far (see [Room on the disk](crate::engine#room-on-the-disk)).
#[derive(Clone, Debug, Default)]
pub struct Keep {
    /// Each blob the build used.
    pub blobs: HashSet<Digest>,
    /// Each record it found or wrote, by its key.
    pub records: HashSet<Digest>,
    /// Each scratch directory it held, by its name.
    pub scratch: HashSet<String>,
    /// Each tree it took as complete: every blob it names, at any depth.
    pub trees: HashSet<Digest>,
}

/// How far an eviction goes.
#[derive(Clone, Copy, Debug)]
enum Aim {
    /// To below nine tenths of a limit, in bytes, if the store is over it.
    Limit(u64),
    /// Until it has freed so many bytes, or has nothing more it may evict.
    Free(u64),
}

/// What [`make_room`] did.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Room {
    /// It evicted, the store from one size to another.
    Made(Evicted),
    /// It had nothing to evict.
    Nothing,
    /// Another process has the store open, so it evicted nothing.
    NotAlone,
}

/// What an eviction did.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Evicted {
    /// The store's size before, in bytes.
    pub before: u64,
    /// Its size after.
    pub after: u64,
    /// The bytes held by exposures of evicted blobs it removed (see
    /// [Exposures of evicted blobs](crate::outputs#exposures-of-evicted-blobs)).
    pub released: u64,
}

/// Something that can be evicted, by its priority.
#[derive(Debug)]
struct Candidate {
    /// Its priority.
    priority: u64,
    /// What it is.
    what: Evictable,
}

/// What a [`Candidate`] is.
#[derive(Debug)]
enum Evictable {
    /// A record's contents, by its key's hex digits.
    Record(String),
    /// A scratch directory, by its name.
    Scratch(String),
}

impl PartialEq for Candidate {
    fn eq(&self, other: &Self) -> bool {
        self.cmp(other) == Ordering::Equal
    }
}

impl Eq for Candidate {}

impl PartialOrd for Candidate {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

impl Ord for Candidate {
    /// The lowest priority first, from a heap that gives the greatest.
    fn cmp(&self, other: &Self) -> Ordering {
        other.priority.cmp(&self.priority)
    }
}

/// What an eviction saw of a store.
struct StoreSeen<'a> {
    /// The store.
    store: &'a Store,
    /// Every blob.
    blobs: &'a Blobs,
    /// The entries of every tree a record names at any depth, by its number.
    trees: &'a HashMap<u32, Box<[u32]>>,
    /// Every scratch directory, by its name, with where it is and its size.
    scratch: &'a HashMap<String, (PathBuf, u64)>,
    /// Every record, by its key's hex digits.
    records: &'a HashMap<String, RecordSeen>,
    /// What removes the file of a blob or a copy: deletes it, or moves it
    /// aside (see [Shared eviction](#shared-eviction)).
    discard: &'a dyn Fn(&Path),
}

/// A record as an eviction sees it.
#[derive(Debug)]
struct RecordSeen {
    /// The time it took to make, in milliseconds.
    cost: u64,
    /// The blobs it names itself, each once, by their numbers in [`Blobs`]:
    /// a tree's entries are its tree's.
    roots: Box<[u32]>,
    /// The size of what it names at any depth, in bytes, a file two of its
    /// trees hold counted for each.
    size: u64,
}

/// Every blob of a store, each by a number of its own, which is what an
/// eviction names it by: four bytes, where its digest's hex digits are 64,
/// so that what a store's records name at any depth, which can be millions
/// of blobs over and over, costs little to remember.
#[derive(Debug, Default)]
struct Blobs {
    /// Each blob, by its number.
    seen: Vec<BlobSeen>,
    /// Each blob's number, by its digest.
    numbers: HashMap<Digest, u32>,
    /// The other files of `cas/`, each with its size: a blob's numbered
    /// copies, `DIGEST.r1` and on, which no record names, so evicted first.
    others: Vec<(PathBuf, u64)>,
}

/// A blob as an eviction sees it: which of its files, plain and executable,
/// are there, and their size.
#[derive(Debug)]
struct BlobSeen {
    /// Its digest.
    digest: Digest,
    /// Whether its plain file is there.
    plain: bool,
    /// Whether its executable file is there.
    executable: bool,
    /// Their size, in bytes.
    size: u64,
}

impl Blobs {
    /// The number of the blob `digest`, if the store has it.
    fn number(&self, digest: &Digest) -> Option<u32> {
        self.numbers.get(digest).copied()
    }

    /// The blob numbered `number`.
    fn get(&self, number: u32) -> &BlobSeen {
        &self.seen[index(number)]
    }

    /// Every file's size, in bytes.
    fn total(&self) -> u64 {
        let blobs: u64 = self.seen.iter().map(|blob| blob.size).sum();
        blobs + self.others.iter().map(|(_, size)| size).sum::<u64>()
    }

    /// Notes the file `name` in `cas/`, at `path`, of `len` bytes.
    fn note(&mut self, name: &str, path: PathBuf, len: u64) -> io::Result<()> {
        let (hex, executable) = name
            .strip_suffix(".x")
            .map_or((name, false), |hex| (hex, true));
        // A file named for no digest, a copy, is no blob's own.
        let Some(digest) = Digest::from_hex(hex) else {
            self.others.push((path, len));
            return Ok(());
        };
        let number = if let Some(number) = self.numbers.get(&digest) {
            *number
        } else {
            let number = u32::try_from(self.seen.len())
                .map_err(|_| io::Error::other("more blobs than an eviction can number"))?;
            self.seen.push(BlobSeen {
                digest,
                plain: false,
                executable: false,
                size: 0,
            });
            self.numbers.insert(digest, number);
            number
        };
        let blob = &mut self.seen[index(number)];
        if executable {
            blob.executable = true;
        } else {
            blob.plain = true;
        }
        blob.size += len;
        Ok(())
    }
}

/// A blob's number as an index of [`Blobs`]' own.
fn index(number: u32) -> usize {
    usize::try_from(number).unwrap_or(usize::MAX)
}

/// Journals what `store` was used for, `used`, and evicts until the store is
/// below nine tenths of `limit`, if it is larger than it and no other process
/// has it (see [Eviction](#eviction)): what it did, if it did anything.
///
/// # Errors
///
/// That the store cannot be read or written, or that its ledger says it is
/// over `limit` while another process has it, so that nothing is evicted.
pub fn evict(store: &Store, used: &Used, limit: u64) -> io::Result<Option<Evicted>> {
    journal(store.root(), used)?;
    Ledger::add(store.root(), used.added)?;
    match store.held().try_lock() {
        Ok(()) => {}
        Err(TryLockError::WouldBlock) => {
            // A lock that failed to change has been let go of: held shared
            // again, as every process that has the store holds it.
            store.held().lock_shared()?;
            // Evicted beside the other processes, if each publishes what it
            // uses (see [Shared eviction](#shared-eviction)).
            match evict_shared(store, Aim::Limit(limit), &Keep::default())? {
                Shared::Made(evicted) => return Ok(Some(evicted)),
                Shared::Nothing => return Ok(None),
                Shared::Busy | Shared::Unpublished => {}
            }
            return match Ledger::read(store.root()) {
                Some(blobs) if blobs > limit => Err(io::Error::other(format!(
                    "it holds {}, over its limit of {}, and nothing was evicted, since \
                     another pekit has it open: a build evicts as it ends only if no other \
                     has the store",
                    gib(blobs),
                    gib(limit)
                ))),
                _ => Ok(None),
            };
        }
        Err(TryLockError::Error(error)) => {
            store.held().lock_shared()?;
            return Err(error);
        }
    }
    // No process holds a claim on a step, since none has the store.
    let _ = remove_tree(&store.root().join("claims"));
    let evicted = evict_held(store, Aim::Limit(limit), &Keep::default(), None);
    // Held shared again, as every process that has the store holds it.
    store.held().lock_shared()?;
    evicted
}

/// Evicts the store at `root`, its scratch directory `scratch`, until it is
/// below nine tenths of `limit`, if it is larger, as `pekit store evict`
/// does: once no other process has it, waiting, `waiting` told once if it
/// waits, while it holds the store's gate, so that a build that would open
/// the store meanwhile waits for it rather than keeping it from it; then
/// removing what killed builds left, as a build that ends alone does.
///
/// # Errors
///
/// That the store's locks cannot be taken, or the store cannot be read or
/// written.
pub fn evict_alone(
    root: &Path,
    scratch: &Path,
    limit: u64,
    waiting: &mut dyn FnMut(),
) -> io::Result<Option<Evicted>> {
    alone(root, scratch, waiting, |store| {
        let evicted = evict_held(store, Aim::Limit(limit), &Keep::default(), None)?;
        // Exposures of what earlier evictions removed go even if nothing
        // more is evicted now.
        Ok(evicted.or_else(|| {
            let released = release_orphans(store);
            (released > 0).then_some(Evicted {
                released,
                ..Evicted::default()
            })
        }))
    })
}

/// `work` done on the store at `root`, its scratch directory `scratch`,
/// once no other process has it, waiting, `waiting` told once if it waits,
/// while it holds the store's gate, so that a build that would open the
/// store meanwhile waits for it rather than keeping it from it; what killed
/// builds left removed first, as a build that ends alone does
/// ([`evict_alone`]).
///
/// # Errors
///
/// That the store's locks cannot be taken, or what `work` fails with.
pub fn alone<T>(
    root: &Path,
    scratch: &Path,
    waiting: &mut dyn FnMut(),
    work: impl FnOnce(&Store) -> io::Result<T>,
) -> io::Result<T> {
    fs::create_dir_all(root)?;
    let mut told = false;
    let mut take = |file: &File| match file.try_lock() {
        Ok(()) => Ok(()),
        Err(TryLockError::WouldBlock) => {
            if !told {
                told = true;
                waiting();
            }
            file.lock()
        }
        Err(TryLockError::Error(error)) => Err(error),
    };
    let gate = Store::lock_file(root, "gate")?;
    take(&gate)?;
    let held = Store::lock_file(root, "lock")?;
    take(&held)?;
    let store = Store::with_held(root, held)?;
    // No process holds a claim, or is using what one left.
    let _ = remove_tree(&root.join("claims"));
    let _ = crate::sweep::sweep_held(&root.join("tmp"), scratch);
    let done = work(&store);
    drop(gate);
    done
}

/// Adds the bytes of the blobs `store` wrote since this was last asked to
/// the store's ledger, if it has one: as a build goes, so that one killed
/// before it ends is counted for what it wrote.
///
/// # Errors
///
/// That the ledger cannot be read or written.
pub fn ledger_written(store: &Store) -> io::Result<()> {
    Ledger::add(store.root(), store.take_added())
}

/// What the ledger of the store at `root` last said its blobs came to, and
/// when it was measured, in seconds since the Unix epoch, if it has one:
/// at most a build's writes since an eviction walked them too few, or too
/// many by what killed builds wrote.
#[must_use]
pub fn ledgered(root: &Path) -> Option<(u64, u64)> {
    let text = fs::read_to_string(root.join("size")).ok()?;
    let (bytes, walked) = text.trim().split_once(' ')?;
    Some((bytes.parse().ok()?, walked.parse().ok()?))
}

/// Whether another process has the store at `root` open.
///
/// # Errors
///
/// That its lock cannot be opened or tried.
pub fn in_use(root: &Path) -> io::Result<bool> {
    if !root.join("lock").exists() {
        return Ok(false);
    }
    let lock = Store::lock_file(root, "lock")?;
    match lock.try_lock() {
        Ok(()) => Ok(false),
        Err(TryLockError::WouldBlock) => Ok(true),
        Err(TryLockError::Error(error)) => Err(error),
    }
}

/// The size of a store's blobs as its file `size` keeps it (see
/// [Eviction](#eviction)): `BYTES WALKED`, the bytes, and when an eviction
/// last measured them, in seconds since the Unix epoch.
struct Ledger;

impl Ledger {
    /// How long a measurement is trusted, in seconds: a day.
    const TRUSTED: u64 = 24 * 60 * 60;

    /// The size of the blobs of the store at `root` by its ledger, if it
    /// has one it trusts.
    fn read(root: &Path) -> Option<u64> {
        let text = fs::read_to_string(root.join("size")).ok()?;
        let (bytes, walked) = text.trim().split_once(' ')?;
        let (bytes, walked): (u64, u64) = (bytes.parse().ok()?, walked.parse().ok()?);
        (now_seconds().saturating_sub(walked) <= Self::TRUSTED).then_some(bytes)
    }

    /// Notes in the ledger of the store at `root` that its blobs are
    /// `bytes`, measured now.
    fn write(root: &Path, bytes: u64) -> io::Result<()> {
        let _held = Self::held(root)?;
        Self::put(root, bytes, now_seconds())
    }

    /// Adds `bytes` to the ledger of the store at `root`, if it has one.
    fn add(root: &Path, bytes: u64) -> io::Result<()> {
        if bytes == 0 {
            return Ok(());
        }
        let _held = Self::held(root)?;
        let Ok(text) = fs::read_to_string(root.join("size")) else {
            return Ok(());
        };
        let parsed = text
            .trim()
            .split_once(' ')
            .and_then(|(was, walked)| Some((was.parse::<u64>().ok()?, walked.parse().ok()?)));
        match parsed {
            Some((was, walked)) => Self::put(root, was.saturating_add(bytes), walked),
            None => Ok(()),
        }
    }

    /// The ledger's own lock, held exclusively, which every writer takes.
    fn held(root: &Path) -> io::Result<File> {
        let lock = File::options()
            .create(true)
            .truncate(false)
            .write(true)
            .open(root.join("size.lock"))?;
        lock.lock()?;
        Ok(lock)
    }

    /// Writes the ledger, `bytes` measured at `walked`, in place at once.
    fn put(root: &Path, bytes: u64, walked: u64) -> io::Result<()> {
        let temp = root
            .join("tmp")
            .join(format!("size-{}", crate::sweep::token()));
        fs::write(&temp, format!("{bytes} {walked}\n"))?;
        fs::rename(&temp, root.join("size"))
    }
}

/// `bytes` in GiB, to a tenth, as a warning shows a size.
pub(crate) fn gib(bytes: u64) -> String {
    #[expect(
        clippy::cast_precision_loss,
        reason = "a size shown to a tenth of a GiB"
    )]
    let gib = bytes as f64 / f64::from(1u32 << 30);
    format!("{gib:.1} GiB")
}

/// The seconds since the Unix epoch.
fn now_seconds() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |since| since.as_secs())
}

/// Appends `used` to a journal of its own in the store at `root`.
fn journal(root: &Path, used: &Used) -> io::Result<()> {
    if used.records.is_empty() && used.scratch.is_empty() {
        return Ok(());
    }
    let dir = root.join("usage");
    fs::create_dir_all(&dir)?;
    let stamp = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |since| since.as_nanos());
    let mut text = String::new();
    for record in &used.records {
        text.push_str("record ");
        text.push_str(&record.hex());
        text.push('\n');
    }
    for name in &used.scratch {
        text.push_str("scratch ");
        text.push_str(name);
        text.push('\n');
    }
    let temp = dir.join(format!(".{}-{stamp}", crate::sweep::token()));
    let mut out = File::create(&temp)?;
    out.write_all(text.as_bytes())?;
    out.sync_all()?;
    fs::rename(
        &temp,
        dir.join(format!("{}-{stamp}", crate::sweep::token())),
    )
}

/// Journals what `store` was used for since it was opened, and adds what it
/// wrote to its ledger, as [`evict`] does first: for a build that ends with
/// no limit to evict to.
///
/// # Errors
///
/// That the journal or the ledger cannot be written.
pub fn journal_used(store: &Store) -> io::Result<()> {
    let used = store.used();
    journal(store.root(), &used)?;
    Ledger::add(store.root(), used.added)
}

/// Removes each exposure of an evicted blob in the output directories noted
/// in `store`, which this process has to itself, and keeps what it found
/// for `pekit store stats`: the bytes they held (see [Exposures of evicted
/// blobs](crate::outputs#exposures-of-evicted-blobs)).
fn release_orphans(store: &Store) -> u64 {
    let Ok(orphans) = crate::outputs::find(store.root()) else {
        return 0;
    };
    let released = crate::outputs::release(&orphans).map_or(0, |()| orphans.bytes);
    let none = crate::outputs::Orphans::default();
    let _ = crate::outputs::keep_last(store.root(), if released > 0 { &none } else { &orphans });
    released
}

/// Evicts from `store`, which this process has open, during its build,
/// until `bytes` more are free, keeping all `keep` says the build has used
/// (see [Room on the disk](crate::engine#room-on-the-disk)): only if no other
/// process has the store, and none of this process's steps runs.
///
/// # Errors
///
/// That the store's lock cannot be taken, or the store cannot be read or
/// written.
pub fn make_room(store: &Store, bytes: u64, keep: &Keep) -> io::Result<Room> {
    match store.held().try_lock() {
        Ok(()) => {}
        Err(TryLockError::WouldBlock) => {
            store.held().lock_shared()?;
            return Ok(match evict_shared(store, Aim::Free(bytes), keep)? {
                Shared::Made(evicted) if evicted.after < evicted.before => Room::Made(evicted),
                Shared::Made(_) | Shared::Nothing => Room::Nothing,
                Shared::Busy | Shared::Unpublished => Room::NotAlone,
            });
        }
        Err(TryLockError::Error(error)) => {
            store.held().lock_shared()?;
            return Err(error);
        }
    }
    let evicted = evict_held(store, Aim::Free(bytes), keep, None);
    // Held shared again, as every process that has the store holds it.
    store.held().lock_shared()?;
    Ok(match evicted? {
        Some(evicted) if evicted.after < evicted.before => Room::Made(evicted),
        _ => Room::Nothing,
    })
}

thread_local! {
    /// What runs between moving aside and putting back, in the next shared
    /// eviction on this thread: a test's way to put a blob meanwhile.
    static BETWEEN: std::cell::RefCell<Option<Box<dyn FnOnce()>>> =
        const { std::cell::RefCell::new(None) };
}

/// Runs `between` in the next shared eviction on this thread, between
/// moving what it evicts aside and putting back what was published
/// meanwhile: for a test of what happens there.
#[doc(hidden)]
pub fn between_in_next_shared(between: impl FnOnce() + 'static) {
    BETWEEN.with(|slot| *slot.borrow_mut() = Some(Box::new(between)));
}

/// What [`evict_shared`] did.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Shared {
    /// It evicted.
    Made(Evicted),
    /// It had nothing to evict.
    Nothing,
    /// Another process is evicting beside the rest.
    Busy,
    /// A process that holds the store does not publish what it uses, so
    /// nothing may be evicted beside it.
    Unpublished,
}

/// Evicts `store`, which other processes have open too, as far as `aim`
/// says, keeping what `keep` says and all any live process published it
/// uses (see [Shared eviction](#shared-eviction)): only if every process
/// that holds the store publishes, and no other evicts so meanwhile.
///
/// # Errors
///
/// That the store cannot be read or written.
fn evict_shared(store: &Store, aim: Aim, keep: &Keep) -> io::Result<Shared> {
    let root = store.root();
    let evicting = Store::lock_file(root, "evicting")?;
    match evicting.try_lock() {
        Ok(()) => {}
        Err(TryLockError::WouldBlock) => return Ok(Shared::Busy),
        Err(TryLockError::Error(error)) => return Err(error),
    }
    let live = crate::using::live(root)?;
    if !crate::using::all_publish(root, &live) {
        return Ok(Shared::Unpublished);
    }
    // What an evictor that died left aside: none live but this one, which
    // holds the lock, so none is being put back.
    let tmp = root.join("tmp");
    if let Ok(entries) = fs::read_dir(&tmp) {
        for entry in entries.flatten() {
            if entry.file_name().to_string_lossy().starts_with("evicting-") {
                let _ = fs::remove_dir_all(entry.path());
            }
        }
    }
    let mut kept = keep.clone();
    for used in live {
        kept.blobs.extend(used.blobs);
        kept.records.extend(used.records);
        kept.trees.extend(used.trees);
    }
    let aside = tmp.join(format!("evicting-{}", crate::sweep::token()));
    fs::create_dir_all(&aside)?;
    let evicted = evict_held(store, aim, &kept, Some(&aside));
    let _ = fs::remove_dir_all(&aside);
    BETWEEN.with(|between| between.borrow_mut().take());
    Ok(match evicted? {
        Some(evicted) => Shared::Made(evicted),
        None => Shared::Nothing,
    })
}

/// [`evict`] or [`make_room`], its lock held exclusively, as far as `aim`
/// says, keeping what `keep` says; or, with `aside`, held shared, each blob
/// it evicts moved into `aside`, and put back if a live process published
/// it meanwhile (see [Shared eviction](#shared-eviction)).
fn evict_held(
    store: &Store,
    aim: Aim,
    keep: &Keep,
    aside: Option<&Path>,
) -> io::Result<Option<Evicted>> {
    let root = store.root();
    let scratch = scratch_of(root)?;
    let scratch_size = |scratch: &HashMap<String, (PathBuf, u64)>| -> u64 {
        scratch.values().map(|(_, size)| size).sum()
    };
    // Within its limit by what the ledger says, nothing more is read.
    if let Aim::Limit(limit) = aim
        && Ledger::read(root).is_some_and(|blobs| blobs + scratch_size(&scratch) <= limit)
    {
        return Ok(None);
    }
    let blobs = blobs_of(root)?;
    let measured = blobs.total();
    let total = measured + scratch_size(&scratch);
    let low = match aim {
        // Within its limit, nothing more is read: the journals wait for an
        // eviction that does something.
        Aim::Limit(limit) if total <= limit => {
            Ledger::write(root, measured)?;
            return Ok(None);
        }
        Aim::Limit(limit) => limit / 10 * 9,
        Aim::Free(bytes) => total.saturating_sub(bytes),
    };
    // No tree is known to be complete once a blob may be gone, and none
    // materialised holds a link to one.
    store.forget_complete()?;
    // A materialised tree is left while other processes have the store,
    // since a running step of one may have it mounted.
    if aside.is_none() {
        store.forget_materialised()?;
    }
    let state_path = root.join("eviction.json");
    let mut state: Priorities = fs::read(&state_path)
        .ok()
        .and_then(|bytes| serde_json::from_slice(&bytes).ok())
        .unwrap_or_default();
    let (records, trees) = records_of(store, &blobs)?;
    let (journals, used) = journals_of(root)?;
    // What was used is live again, and weighed as now.
    for key in &used.records {
        state.evicted.remove(&key.hex());
    }
    let weigh = |record: &RecordSeen| cost_per_byte(record.cost, record.size);
    for key in &used.records {
        let hex = key.hex();
        if let Some(record) = records.get(&hex) {
            state
                .records
                .insert(hex, state.inflation.saturating_add(weigh(record)));
        }
    }
    for name in &used.scratch {
        state.scratch.insert(name.clone(), state.inflation);
    }
    let discard = |path: &Path| match (aside, path.file_name()) {
        (Some(aside), Some(name)) => {
            let _ = fs::rename(path, aside.join(name));
        }
        _ => {
            let _ = fs::remove_file(path);
        }
    };
    let seen = StoreSeen {
        store,
        blobs: &blobs,
        trees: &trees,
        scratch: &scratch,
        records: &records,
        discard: &discard,
    };
    let after = evict_to(&seen, &mut state, (total, low), keep);
    if let Some(aside) = aside {
        if let Some(between) = BETWEEN.with(|between| between.borrow_mut().take()) {
            between();
        }
        put_back(&seen, aside)?;
    }
    // Only what is still there is remembered.
    state.records.retain(|hex, _| records.contains_key(hex));
    state.scratch.retain(|name, _| scratch.contains_key(name));
    state.evicted.retain(|hex| records.contains_key(hex));
    let temp = root
        .join("tmp")
        .join(format!("eviction-{}", crate::sweep::token()));
    fs::write(&temp, serde_json::to_vec(&state).map_err(io::Error::other)?)?;
    fs::rename(&temp, &state_path)?;
    for journal in journals {
        let _ = fs::remove_file(journal);
    }
    Ledger::write(root, after.saturating_sub(scratch_size(&scratch_of(root)?)))?;
    // What output directories still held of what was evicted goes too, with
    // the store to itself, so that no build writes them meanwhile.
    let released = if aside.is_none() {
        release_orphans(store)
    } else {
        0
    };
    Ok(Some(Evicted {
        before: total,
        after,
        released,
    }))
}

/// A priority's `cost / size`, of a cost in milliseconds and a size in
/// bytes, a size of nothing as one byte: in 2^-20 milliseconds per byte, so
/// that what took a millisecond to make a megabyte of still weighs.
fn cost_per_byte(cost: u64, size: u64) -> u64 {
    cost.saturating_mul(1 << 20) / size.max(1)
}

/// Evicts the blobs, scratch directories and records' contents given, by
/// `state`'s priorities, until the store, `total` bytes, is at most `low`:
/// its size after.
fn evict_to(
    seen: &StoreSeen<'_>,
    state: &mut Priorities,
    (mut total, low): (u64, u64),
    keep: &Keep,
) -> u64 {
    let StoreSeen {
        store,
        blobs,
        trees,
        scratch,
        records,
        ..
    } = seen;
    let live: Vec<(&String, &RecordSeen)> = records
        .iter()
        .filter(|(hex, _)| !state.evicted.contains(*hex))
        .collect();
    let mut named = named_of(&live, blobs, trees);
    let kept_records: HashSet<String> = keep.records.iter().map(Digest::hex).collect();
    let held = kept_blobs(seen, keep);
    let kept = |blob: u32| held.contains(&blob);
    let discard = seen.discard;
    let remove = |blob: u32, total: &mut u64| {
        let seen = blobs.get(blob);
        for (there, executable) in [(seen.plain, false), (seen.executable, true)] {
            if there {
                discard(&store.cas_path(&seen.digest, executable));
            }
        }
        *total = total.saturating_sub(seen.size);
    };
    // What no live record names costs nothing to make again: a blob no
    // live record names, and a copy, the largest first.
    let unnamed = (0..blobs.seen.len())
        .filter_map(|at| u32::try_from(at).ok())
        .filter(|blob| named[index(*blob)] == 0 && !kept(*blob));
    total = remove_unnamed(blobs, unnamed, (total, low), (&remove, discard));
    if total <= low {
        return total;
    }
    let mut heap: BinaryHeap<Candidate> = BinaryHeap::new();
    for (hex, record) in live.iter().filter(|(hex, _)| !kept_records.contains(*hex)) {
        let priority = state.records.get(*hex).copied().unwrap_or_else(|| {
            state
                .inflation
                .saturating_add(cost_per_byte(record.cost, record.size))
        });
        heap.push(Candidate {
            priority,
            what: Evictable::Record((*hex).clone()),
        });
    }
    for name in scratch.keys().filter(|name| !keep.scratch.contains(*name)) {
        let priority = state.scratch.get(name).copied().unwrap_or(state.inflation);
        heap.push(Candidate {
            priority,
            what: Evictable::Scratch(name.clone()),
        });
    }
    while total > low {
        let Some(candidate) = heap.pop() else {
            break;
        };
        state.inflation = state.inflation.max(candidate.priority);
        match candidate.what {
            Evictable::Record(hex) => {
                // Each blob let go of by everything that named it is
                // removed, and a tree lets go of its entries in turn.
                let mut letting: Vec<u32> = records[&hex].roots.to_vec();
                while let Some(blob) = letting.pop() {
                    let count = &mut named[index(blob)];
                    *count = count.saturating_sub(1);
                    // A blob the build uses stays, and so does all a tree of
                    // it names.
                    if *count == 0 && !kept(blob) {
                        remove(blob, &mut total);
                        letting.extend(
                            trees
                                .get(&blob)
                                .into_iter()
                                .flat_map(|entries| entries.iter()),
                        );
                    }
                }
                state.records.remove(&hex);
                state.evicted.insert(hex);
            }
            Evictable::Scratch(name) => {
                let (dir, size) = &scratch[&name];
                if remove_scratch(dir).unwrap_or(false) {
                    total = total.saturating_sub(*size);
                    state.scratch.remove(&name);
                }
            }
        }
    }
    total
}

/// Each blob of `seen` that `keep` keeps, by its number: each it names,
/// each a record it names names, and each a tree it names, or that one of
/// those names, holds at any depth; a tree that no record names read from
/// the store, or from `aside`, where it may have been moved.
fn kept_blobs(seen: &StoreSeen<'_>, keep: &Keep) -> HashSet<u32> {
    kept_blobs_in(seen, keep, None)
}

/// [`kept_blobs`], a tree that no record names read from `aside` too.
fn kept_blobs_in(seen: &StoreSeen<'_>, keep: &Keep, aside: Option<&Path>) -> HashSet<u32> {
    let mut held: HashSet<u32> = HashSet::new();
    let mut pending: Vec<u32> = keep
        .blobs
        .iter()
        .chain(&keep.trees)
        .filter_map(|digest| seen.blobs.number(digest))
        .collect();
    for key in &keep.records {
        if let Some(record) = seen.records.get(&key.hex()) {
            pending.extend(record.roots.iter().copied());
        }
    }
    while let Some(blob) = pending.pop() {
        if !held.insert(blob) {
            continue;
        }
        if let Some(entries) = seen.trees.get(&blob) {
            pending.extend(entries.iter().copied());
            continue;
        }
        // A tree no record names, read as its blob has it, if it is one.
        let digest = seen.blobs.get(blob).digest;
        let Some(tree) = read_tree_at(seen.store, &digest, aside) else {
            continue;
        };
        for entry in tree.entries {
            let named = match entry {
                crate::store::TreeEntry::File { file, .. } => file.digest,
                crate::store::TreeEntry::Dir { tree, .. } => tree,
                crate::store::TreeEntry::Link { .. } => continue,
            };
            pending.extend(seen.blobs.number(&named));
        }
    }
    held
}

/// The tree `digest`, if its blob is one, read from `store`, or from
/// `aside`, where an eviction may have moved it.
fn read_tree_at(
    store: &Store,
    digest: &Digest,
    aside: Option<&Path>,
) -> Option<crate::store::Tree> {
    if store.is_tree_blob(digest) {
        return store.read_tree_once(digest).ok();
    }
    let bytes = fs::read(aside?.join(digest.hex())).ok()?;
    bytes
        .starts_with(b"{\"entries\":")
        .then(|| serde_json::from_slice(&bytes).ok())
        .flatten()
}

/// Puts back into the store each blob, or copy, an eviction moved into
/// `aside` that a live process published it uses since the eviction began,
/// directly or through a record or tree (see [Shared
/// eviction](#shared-eviction)): unless one of its name is there again,
/// which a process put meanwhile, of the same contents.
fn put_back(seen: &StoreSeen<'_>, aside: &Path) -> io::Result<()> {
    let root = seen.store.root();
    let mut published = Keep::default();
    for used in crate::using::live(root)? {
        published.blobs.extend(used.blobs);
        published.records.extend(used.records);
        published.trees.extend(used.trees);
    }
    let held = kept_blobs_in(seen, &published, Some(aside));
    for entry in fs::read_dir(aside)? {
        let entry = entry?;
        let name = entry.file_name().to_string_lossy().into_owned();
        let Some(digest) = name.get(..64).and_then(Digest::from_hex) else {
            continue;
        };
        let wanted = published.blobs.contains(&digest)
            || seen
                .blobs
                .number(&digest)
                .is_some_and(|blob| held.contains(&blob));
        if !wanted {
            continue;
        }
        let target = root.join("cas").join(&name[..2]).join(&name);
        let moved = rustix::fs::renameat_with(
            rustix::fs::CWD,
            entry.path(),
            rustix::fs::CWD,
            &target,
            rustix::fs::RenameFlags::NOREPLACE,
        );
        // Put meanwhile by another process, of the same contents: dropped.
        if moved.is_err() {
            let _ = fs::remove_file(entry.path());
        }
    }
    Ok(())
}

/// What removes a blob, by its number, from the store's size, `total`.
type Remove<'a> = dyn Fn(u32, &mut u64) + 'a;

/// Removes from `blobs` each of `unnamed`, blobs no live record names, and
/// each copy of a blob, the largest first, by `remove`, until the store,
/// `total` bytes, is at `low` or below: its size after.
fn remove_unnamed(
    blobs: &Blobs,
    unnamed: impl Iterator<Item = u32>,
    (mut total, low): (u64, u64),
    (remove, discard): (&Remove<'_>, &dyn Fn(&Path)),
) -> u64 {
    let mut found: Vec<(u64, Result<u32, &Path>)> = unnamed
        .map(|blob| (blobs.get(blob).size, Ok(blob)))
        .chain(
            blobs
                .others
                .iter()
                .map(|(path, size)| (*size, Err(path.as_path()))),
        )
        .collect();
    found.sort_by_key(|(size, _)| std::cmp::Reverse(*size));
    for (size, what) in found {
        if total <= low {
            break;
        }
        match what {
            Ok(blob) => remove(blob, &mut total),
            Err(path) => {
                discard(path);
                total = total.saturating_sub(size);
            }
        }
    }
    total
}

/// How many name each blob of `blobs`, by its number: each record of
/// `live` that names it itself, and each tree such a record names at any
/// depth, by `trees`, that holds it.
fn named_of(
    live: &[(&String, &RecordSeen)],
    blobs: &Blobs,
    trees: &HashMap<u32, Box<[u32]>>,
) -> Vec<u32> {
    let mut named: Vec<u32> = vec![0; blobs.seen.len()];
    let mut reached: Vec<bool> = vec![false; blobs.seen.len()];
    let mut next: Vec<u32> = Vec::new();
    for (_, record) in live {
        for root in &record.roots {
            named[index(*root)] += 1;
            next.push(*root);
        }
    }
    // Each tree's entries are counted once, as the tree is first reached.
    while let Some(blob) = next.pop() {
        if std::mem::replace(&mut reached[index(blob)], true) {
            continue;
        }
        for entry in trees
            .get(&blob)
            .into_iter()
            .flat_map(|entries| entries.iter())
        {
            named[index(*entry)] += 1;
            next.push(*entry);
        }
    }
    named
}

/// Removes the scratch directory at `dir`, unless a run holds it: whether
/// it did.
fn remove_scratch(dir: &Path) -> io::Result<bool> {
    let lock = File::options()
        .write(true)
        .open(dir.with_extension("lock"))?;
    match lock.try_lock() {
        Ok(()) => {}
        Err(TryLockError::WouldBlock) => return Ok(false),
        Err(TryLockError::Error(error)) => return Err(error),
    }
    remove_tree(dir)?;
    let _ = fs::remove_file(dir.with_extension("size"));
    let _ = fs::remove_file(dir.with_extension("dirty"));
    let _ = fs::remove_file(dir.with_extension("lock"));
    Ok(true)
}

/// Every blob in the store at `root`.
fn blobs_of(root: &Path) -> io::Result<Blobs> {
    let mut blobs = Blobs::default();
    for shard in fs::read_dir(root.join("cas"))? {
        let shard = shard?;
        if !shard.file_type()?.is_dir() {
            continue;
        }
        for entry in fs::read_dir(shard.path())? {
            let entry = entry?;
            let name = entry.file_name().to_string_lossy().into_owned();
            blobs.note(&name, entry.path(), entry.metadata()?.len())?;
        }
    }
    Ok(blobs)
}

/// Every scratch directory of the store at `root`, by its name, with where
/// it is and its size.
fn scratch_of(root: &Path) -> io::Result<HashMap<String, (PathBuf, u64)>> {
    let mut found = HashMap::new();
    let Ok(entries) = fs::read_dir(root.join("scratch")) else {
        return Ok(found);
    };
    for entry in entries {
        let entry = entry?;
        if entry.file_type()?.is_dir() {
            let name = entry.file_name().to_string_lossy().into_owned();
            // Its size as the last run that held it left it, else as it is.
            let size = fs::read_to_string(entry.path().with_extension("size"))
                .ok()
                .and_then(|text| text.trim().parse().ok())
                .unwrap_or_else(|| size_under(&entry.path()));
            found.insert(name, (entry.path(), size));
        }
    }
    Ok(found)
}

/// The size of everything under `path`, in bytes, of what can be read.
#[must_use]
pub fn size_under(path: &Path) -> u64 {
    let Ok(meta) = fs::symlink_metadata(path) else {
        return 0;
    };
    if !meta.is_dir() {
        return meta.len();
    }
    fs::read_dir(path).map_or(0, |entries| {
        entries
            .flatten()
            .map(|entry| size_under(&entry.path()))
            .sum()
    })
}

/// Every record of `store`, by its key's hex digits, with the blobs of
/// `blobs` it names itself and the size of what it names at any depth, and
/// the entries of every tree of `blobs` they name at any depth.
fn records_of(store: &Store, blobs: &Blobs) -> io::Result<RecordsSeen> {
    let mut records = HashMap::new();
    let mut trees = TreesSeen::default();
    let Ok(shards) = fs::read_dir(store.root().join("records")) else {
        return Ok((records, trees.entries));
    };
    for shard in shards {
        let shard = shard?;
        if !shard.file_type()?.is_dir() {
            continue;
        }
        for entry in fs::read_dir(shard.path())? {
            let entry = entry?;
            let hex = entry.file_name().to_string_lossy().into_owned();
            let Ok(value) = fs::read(entry.path())
                .map_err(|error| error.to_string())
                .and_then(|bytes| {
                    serde_json::from_slice::<Value>(&bytes).map_err(|error| error.to_string())
                })
            else {
                continue;
            };
            let cost = value
                .get("duration_ms")
                .and_then(Value::as_u64)
                .unwrap_or(0);
            let mut named = Vec::new();
            digests_in(&value, &mut named);
            let mut roots: Vec<u32> = named
                .iter()
                .filter_map(|digest| Digest::from_hex(digest))
                .filter_map(|digest| blobs.number(&digest))
                .collect();
            roots.sort_unstable();
            roots.dedup();
            let size = roots
                .iter()
                .map(|root| trees.size(store, (*root, Kind::Unknown), blobs))
                .sum();
            records.insert(
                hex,
                RecordSeen {
                    cost,
                    roots: roots.into_boxed_slice(),
                    size,
                },
            );
        }
    }
    Ok((records, trees.entries))
}

/// What [`records_of`] gives: every record, and every tree's entries.
type RecordsSeen = (HashMap<String, RecordSeen>, HashMap<u32, Box<[u32]>>);

/// The trees an eviction has read, by their numbers in [`Blobs`].
#[derive(Default)]
struct TreesSeen {
    /// Each tree's entries, each once, of the blobs the store has.
    entries: HashMap<u32, Box<[u32]>>,
    /// The size of what each blob is at any depth, in bytes, by its number.
    sizes: HashMap<u32, u64>,
}

/// What a blob an eviction weighs is known to be.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Kind {
    /// A file's contents, as a tree's file entry names it.
    File,
    /// A tree, as a tree's directory entry names it.
    Tree,
    /// Either, as a record names it.
    Unknown,
}

impl TreesSeen {
    /// The size of the blob numbered `blob` of `blobs`, known to be `kind`,
    /// and, if it is a tree, of what it names at any depth, a file two of
    /// its directories hold counted for each: its entries noted as it is
    /// read. Only a blob a record names is opened to find whether it is a
    /// tree, and a tree is read without being kept by the store, so that an
    /// eviction of a large store opens no file it need not, and holds no
    /// more than each tree's entries' numbers.
    fn size(&mut self, store: &Store, (blob, kind): (u32, Kind), blobs: &Blobs) -> u64 {
        // Weighed already, but as a file when it is a tree too, one whose
        // contents a file of the store happens to have: weighed again.
        if let Some(size) = self.sizes.get(&blob)
            && (kind != Kind::Tree || self.entries.contains_key(&blob))
        {
            return *size;
        }
        let seen = blobs.get(blob);
        let tree = match kind {
            Kind::File => None,
            Kind::Tree => Some(seen.digest),
            // A tree's encoding begins so; a file's is not read to find it isn't one.
            Kind::Unknown => Some(seen.digest).filter(|digest| store.is_tree_blob(digest)),
        }
        .and_then(|digest| store.read_tree_once(&digest).ok());
        let mut size = seen.size;
        if let Some(tree) = tree {
            let mut entries: Vec<(u32, Kind)> = tree
                .entries
                .iter()
                .filter_map(|entry| match entry {
                    TreeEntry::File { file, .. } => Some((blobs.number(&file.digest)?, Kind::File)),
                    TreeEntry::Dir { tree, .. } => Some((blobs.number(tree)?, Kind::Tree)),
                    TreeEntry::Link { .. } => None,
                })
                .collect();
            entries.sort_unstable_by_key(|&(number, _)| number);
            entries.dedup_by_key(|&mut (number, _)| number);
            for &entry in &entries {
                size = size.saturating_add(self.size(store, entry, blobs));
            }
            let numbers = entries.into_iter().map(|(number, _)| number).collect();
            self.entries.insert(blob, numbers);
        }
        self.sizes.insert(blob, size);
        size
    }
}

/// Every string of 64 hex digits in `value`: what may name a blob.
pub(crate) fn digests_in(value: &Value, found: &mut Vec<String>) {
    match value {
        Value::String(text) if Digest::from_hex(text).is_some() => found.push(text.clone()),
        Value::Array(items) => items.iter().for_each(|item| digests_in(item, found)),
        Value::Object(fields) => fields.values().for_each(|item| digests_in(item, found)),
        _ => {}
    }
}

/// Every journal of the store at `root`, and what they say was used.
fn journals_of(root: &Path) -> io::Result<(Vec<PathBuf>, Used)> {
    let mut used = Used::default();
    let mut journals = Vec::new();
    let Ok(entries) = fs::read_dir(root.join("usage")) else {
        return Ok((journals, used));
    };
    for entry in entries {
        let entry = entry?;
        let name = entry.file_name().to_string_lossy().into_owned();
        // One being written is read by the next eviction.
        if name.starts_with('.') {
            continue;
        }
        let text = fs::read_to_string(entry.path()).unwrap_or_default();
        for line in text.lines() {
            match line.split_once(' ') {
                Some(("record", hex)) => {
                    if let Some(digest) = Digest::from_hex(hex) {
                        used.records.insert(digest);
                    }
                }
                Some(("scratch", name)) => {
                    used.scratch.insert(name.to_owned());
                }
                _ => {}
            }
        }
        journals.push(entry.path());
    }
    Ok((journals, used))
}
