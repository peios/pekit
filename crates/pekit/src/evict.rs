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
//!   one whose contents are all there, names it; a blob no record names
//!   goes first, as costing nothing to make again.
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
use crate::store::{Digest, Store, Tree, TreeEntry, Used};

/// The limit a store has when `PEKIT_CACHE_LIMIT` says none: 50 GiB.
pub const DEFAULT_LIMIT: u64 = 50 << 30;

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
    /// Every blob, by its digest's hex digits.
    blobs: &'a HashMap<String, BlobSeen>,
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
    /// The blobs it names, at any depth, by their digests' hex digits.
    blobs: Vec<String>,
}

/// A blob as an eviction sees it: its files, plain and executable, and their
/// size.
#[derive(Debug, Default)]
struct BlobSeen {
    /// Its files.
    files: Vec<PathBuf>,
    /// Their size, in bytes.
    size: u64,
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
    let measured: u64 = blobs.values().map(|blob| blob.size).sum();
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
    let records = records_of(store, &blobs)?;
    let (journals, used) = journals_of(root)?;
    // What was used is live again, and weighed as now.
    for key in &used.records {
        state.evicted.remove(&key.hex());
    }
    let weigh = |record: &RecordSeen| {
        let size: u64 = record
            .blobs
            .iter()
            .filter_map(|blob| blobs.get(blob))
            .map(|blob| blob.size)
            .sum();
        cost_per_byte(record.cost, size)
    };
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
            blobs: &blobs,
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
        blobs,
        scratch,
        records,
    } = seen;
    let live: Vec<(&String, &RecordSeen)> = records
        .iter()
        .filter(|(hex, record)| {
            !state.evicted.contains(*hex)
                && record.blobs.iter().all(|blob| blobs.contains_key(blob))
        })
        .collect();
    let mut named: HashMap<&str, u32> = HashMap::new();
    for (_, record) in &live {
        for blob in &record.blobs {
            *named.entry(blob).or_default() += 1;
        }
    }
    let remove = |blob: &str, total: &mut u64| {
        if let Some(seen) = blobs.get(blob) {
            for file in &seen.files {
                let _ = fs::remove_file(file);
            }
            *total = total.saturating_sub(seen.size);
        }
    };
    // What no live record names costs nothing to make again.
    let mut unnamed: Vec<&String> = blobs
        .keys()
        .filter(|blob| !named.contains_key(blob.as_str()))
        .collect();
    unnamed.sort_by_key(|blob| std::cmp::Reverse(blobs[*blob].size));
    for blob in unnamed {
        if total <= low {
            return total;
        }
        remove(blob, &mut total);
    }
    let mut heap: BinaryHeap<Candidate> = BinaryHeap::new();
    for (hex, record) in &live {
        let size: u64 = record.blobs.iter().map(|blob| blobs[blob].size).sum();
        let priority = state.records.get(*hex).copied().unwrap_or_else(|| {
            state
                .inflation
                .saturating_add(cost_per_byte(record.cost, size))
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
        let Some(next) = heap.pop() else {
            break;
        };
        state.inflation = state.inflation.max(next.priority);
        match next.what {
            Evictable::Record(hex) => {
                for blob in &records[&hex].blobs {
                    let count = named.entry(blob).or_default();
                    *count = count.saturating_sub(1);
                    if *count == 0 {
                        remove(blob, &mut total);
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

/// Every blob in the store at `root`, by its digest's hex digits.
fn blobs_of(root: &Path) -> io::Result<HashMap<String, BlobSeen>> {
    let mut blobs: HashMap<String, BlobSeen> = HashMap::new();
    for shard in fs::read_dir(root.join("cas"))? {
        let shard = shard?;
        if !shard.file_type()?.is_dir() {
            continue;
        }
        for entry in fs::read_dir(shard.path())? {
            let entry = entry?;
            let name = entry.file_name().to_string_lossy().into_owned();
            let hex = name.trim_end_matches(".x").to_owned();
            let size = entry.metadata()?.len();
            let blob = blobs.entry(hex).or_default();
            blob.files.push(entry.path());
            blob.size += size;
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

/// Every record of `store`, by its key's hex digits, with the blobs it
/// names of `blobs`, at any depth.
fn records_of(
    store: &Store,
    blobs: &HashMap<String, BlobSeen>,
) -> io::Result<HashMap<String, RecordSeen>> {
    let mut records = HashMap::new();
    let mut trees: HashMap<String, Vec<String>> = HashMap::new();
    let Ok(shards) = fs::read_dir(store.root().join("records")) else {
        return Ok(records);
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
            let mut found: Vec<String> = Vec::new();
            for digest in named {
                named_blobs(store, &digest, (blobs, &mut trees), &mut found);
            }
            found.sort_unstable();
            found.dedup();
            records.insert(hex, RecordSeen { cost, blobs: found });
        }
    }
    Ok(records)
}

/// Every string of 64 hex digits in `value`: what may name a blob.
fn digests_in(value: &Value, found: &mut Vec<String>) {
    match value {
        Value::String(text) if Digest::from_hex(text).is_some() => found.push(text.clone()),
        Value::Array(items) => items.iter().for_each(|item| digests_in(item, found)),
        Value::Object(fields) => fields.values().for_each(|item| digests_in(item, found)),
        _ => {}
    }
}

/// Adds to `found` the blob `digest`, if `blobs` has it, and, if it is a
/// tree, every blob it names at any depth, each tree's remembered in
/// `trees`.
fn named_blobs(
    store: &Store,
    digest: &str,
    (blobs, trees): (
        &HashMap<String, BlobSeen>,
        &mut HashMap<String, Vec<String>>,
    ),
    found: &mut Vec<String>,
) {
    if !blobs.contains_key(digest) {
        return;
    }
    found.push(digest.to_owned());
    if let Some(known) = trees.get(digest) {
        found.extend(known.iter().cloned());
        return;
    }
    // A tree's encoding begins so; a file's is not read to find it isn't one.
    let Some(tree) = Digest::from_hex(digest)
        .filter(|digest| looks_like_tree(store, digest))
        .and_then(|digest| store.read_tree(&digest).ok())
    else {
        trees.insert(digest.to_owned(), Vec::new());
        return;
    };
    let mut within = Vec::new();
    for entry in tree_entries(&tree) {
        named_blobs(store, &entry, (blobs, trees), &mut within);
    }
    found.extend(within.iter().cloned());
    trees.insert(digest.to_owned(), within);
}

/// Whether the blob `digest` of `store` begins as a tree's encoding does,
/// read no further.
fn looks_like_tree(store: &Store, digest: &Digest) -> bool {
    const BEGINNING: &[u8] = b"{\"entries\":";
    let mut start = [0u8; BEGINNING.len()];
    File::open(store.blob_path(digest, false))
        .and_then(|mut file| io::Read::read_exact(&mut file, &mut start))
        .is_ok_and(|()| start == BEGINNING)
}

/// The digests a tree's entries name.
fn tree_entries(tree: &Tree) -> Vec<String> {
    tree.entries
        .iter()
        .filter_map(|entry| match entry {
            TreeEntry::File { file, .. } => Some(file.digest.hex()),
            TreeEntry::Dir { tree, .. } => Some(tree.hex()),
            TreeEntry::Link { .. } => None,
        })
        .collect()
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
