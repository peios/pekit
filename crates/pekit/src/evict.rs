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
//!   build wrote since, added as it ends. The blobs are walked only when
//!   that, which can only be too large, is above the limit, or is older, or
//!   is not there; a build that is ended before it adds what it wrote adds
//!   nothing, which the next walk sets right.
//! - **Concurrency:** every process holds the store's lock shared while it
//!   has the store open. An eviction takes it exclusively without waiting,
//!   so it evicts nothing while another process has the store.

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
/// if it is unset, none if it is `0`.
///
/// # Errors
///
/// That it is no size: a number of bytes, with a unit or none.
pub fn limit_of(setting: Option<&str>) -> Result<Option<u64>, String> {
    let Some(text) = setting else {
        return Ok(Some(DEFAULT_LIMIT));
    };
    match size_of(text) {
        Some(0) => Ok(None),
        Some(size) => Ok(Some(size)),
        None => Err(format!(
            "`PEKIT_CACHE_LIMIT` is `{text}`, which is no size: write a number of bytes, \
             such as `50GiB`, `500MB` or `0` for no limit"
        )),
    }
}

/// The number of bytes `text` says: digits, then a unit or none.
fn size_of(text: &str) -> Option<u64> {
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

/// What an eviction did.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Evicted {
    /// The store's size before, in bytes.
    pub before: u64,
    /// Its size after.
    pub after: u64,
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
/// That the store cannot be read or written.
pub fn evict(store: &Store, used: &Used, limit: u64) -> io::Result<Option<Evicted>> {
    journal(store.root(), used)?;
    Ledger::add(store.root(), used.added)?;
    match store.held().try_lock() {
        Ok(()) => {}
        Err(TryLockError::WouldBlock) => {
            // A lock that failed to change has been let go of: held shared
            // again, as every process that has the store holds it.
            store.held().lock_shared()?;
            return Ok(None);
        }
        Err(TryLockError::Error(error)) => {
            store.held().lock_shared()?;
            return Err(error);
        }
    }
    let evicted = evict_held(store, limit);
    // Held shared again, as every process that has the store holds it.
    store.held().lock_shared()?;
    evicted
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

/// [`evict`], its lock held exclusively.
fn evict_held(store: &Store, limit: u64) -> io::Result<Option<Evicted>> {
    let root = store.root();
    let scratch = scratch_of(root)?;
    let scratch_size = |scratch: &HashMap<String, (PathBuf, u64)>| -> u64 {
        scratch.values().map(|(_, size)| size).sum()
    };
    // Within its limit by what the ledger says, nothing more is read.
    if Ledger::read(root).is_some_and(|blobs| blobs + scratch_size(&scratch) <= limit) {
        return Ok(None);
    }
    let blobs = blobs_of(root)?;
    let measured = blobs.total();
    let total = measured + scratch_size(&scratch);
    // Within its limit, nothing more is read: the journals wait for an
    // eviction that does something.
    if total <= limit {
        Ledger::write(root, measured)?;
        return Ok(None);
    }
    // No tree is known to be complete once a blob may be gone, and none
    // materialised holds a link to one.
    store.forget_complete()?;
    store.forget_materialised()?;
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
    let low = limit / 10 * 9;
    let after = evict_to(
        &StoreSeen {
            store,
            blobs: &blobs,
            trees: &trees,
            scratch: &scratch,
            records: &records,
        },
        &mut state,
        (total, low),
    );
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
    Ok(Some(Evicted {
        before: total,
        after,
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
fn evict_to(seen: &StoreSeen<'_>, state: &mut Priorities, (mut total, low): (u64, u64)) -> u64 {
    let StoreSeen {
        store,
        blobs,
        trees,
        scratch,
        records,
    } = seen;
    let live: Vec<(&String, &RecordSeen)> = records
        .iter()
        .filter(|(hex, _)| !state.evicted.contains(*hex))
        .collect();
    let mut named = named_of(&live, blobs, trees);
    let remove = |blob: u32, total: &mut u64| {
        let seen = blobs.get(blob);
        for (there, executable) in [(seen.plain, false), (seen.executable, true)] {
            if there {
                let _ = fs::remove_file(store.blob_path(&seen.digest, executable));
            }
        }
        *total = total.saturating_sub(seen.size);
    };
    // What no live record names costs nothing to make again: a blob no
    // live record names, and a copy, the largest first.
    let mut unnamed: Vec<(u64, Result<u32, &Path>)> = (0..blobs.seen.len())
        .filter(|at| named[*at] == 0)
        .filter_map(|at| u32::try_from(at).ok())
        .map(|blob| (blobs.get(blob).size, Ok(blob)))
        .chain(
            blobs
                .others
                .iter()
                .map(|(path, size)| (*size, Err(path.as_path()))),
        )
        .collect();
    unnamed.sort_by_key(|(size, _)| std::cmp::Reverse(*size));
    for (size, what) in unnamed {
        if total <= low {
            return total;
        }
        match what {
            Ok(blob) => remove(blob, &mut total),
            Err(path) => {
                let _ = fs::remove_file(path);
                total = total.saturating_sub(size);
            }
        }
    }
    let mut heap: BinaryHeap<Candidate> = BinaryHeap::new();
    for (hex, record) in &live {
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
    for name in scratch.keys() {
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
                    if *count == 0 {
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
                .map(|root| trees.size(store, *root, blobs))
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

impl TreesSeen {
    /// The size of the blob numbered `blob` of `blobs` and, if it is a
    /// tree, of what it names at any depth, a file two of its directories
    /// hold counted for each: its entries noted as it is read.
    fn size(&mut self, store: &Store, blob: u32, blobs: &Blobs) -> u64 {
        if let Some(size) = self.sizes.get(&blob) {
            return *size;
        }
        let seen = blobs.get(blob);
        // A tree's encoding begins so; a file's is not read to find it isn't one.
        let tree = Some(seen.digest)
            .filter(|digest| store.is_tree_blob(digest))
            .and_then(|digest| store.read_tree(&digest).ok());
        let mut size = seen.size;
        if let Some(tree) = tree {
            let mut entries: Vec<u32> = tree
                .entries
                .iter()
                .filter_map(|entry| match entry {
                    TreeEntry::File { file, .. } => blobs.number(&file.digest),
                    TreeEntry::Dir { tree, .. } => blobs.number(tree),
                    TreeEntry::Link { .. } => None,
                })
                .collect();
            entries.sort_unstable();
            entries.dedup();
            for entry in &entries {
                size = size.saturating_add(self.size(store, *entry, blobs));
            }
            self.entries.insert(blob, entries.into_boxed_slice());
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
