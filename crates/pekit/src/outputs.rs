//! Exposures of evicted blobs: what output directories hold that the store
//! no longer does.
//!
//! # Exposures of evicted blobs
//!
//! A build exposes outputs under its output directory, `pekit-out/`, as
//! hard links to the store's blobs: each step's under `.steps/`, each
//! unpacking's under `.unpack/`, each git commit's under `.git/`, and each
//! fetch's under `.fetch/` ([Exposure](crate::exposure#exposure)). An
//! eviction removes a blob's name in the store, but an exposure's link keeps
//! its contents on the disk: space the store no longer counts, and that no
//! build can use, since a build exposes what it needs again from the store.
//!
//! So the store notes each workspace's output directory beside its stamps
//! ([`note`]), and an eviction with the store to itself removes each
//! exposure that holds a file no blob of the store is ([`find`],
//! [`release`]): one of the store's file system that no name in `cas/`
//! links. The copies a target gives, outside these directories, are its
//! user's, and are never touched; nor is anything on another file system,
//! which is a copy, not a link. What was found is kept in the store's
//! `orphaned` file, which `pekit store stats` shows ([`last`]).

use std::collections::HashSet;
use std::fs;
use std::io;
use std::os::unix::fs::{DirEntryExt as _, MetadataExt as _};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use crate::store::in_parallel_by;

/// The directories of an output directory that hold exposures, each entry
/// of which is one exposure, but `.steps/`, each of whose entries holds one
/// exposure for each step of its function.
const EXPOSED: [&str; 3] = [".unpack", ".git", ".fetch"];

/// What [`find`] found: each exposure that holds a file no blob of the
/// store is, and the bytes and files of those files.
#[derive(Debug, Default)]
pub struct Orphans {
    /// Each such exposure: a directory, or a file.
    pub exposures: Vec<PathBuf>,
    /// The bytes of the files no blob is, each counted once.
    pub bytes: u64,
    /// How many there are.
    pub files: u64,
}

/// Notes `outputs`, the output directory of the workspace at `root`, in the
/// store at `store`, beside its stamps: `seen/<digest>.outputs`.
///
/// # Errors
///
/// That it cannot be written.
pub fn note(store: &Path, root: &Path, outputs: &Path) -> io::Result<()> {
    let path = crate::store::stamps_path(store, root, "").with_extension("outputs");
    let bytes = outputs.as_os_str().as_encoded_bytes();
    if fs::read(&path).is_ok_and(|noted| noted == bytes) {
        return Ok(());
    }
    fs::create_dir_all(store.join("seen"))?;
    let temp = path.with_extension(format!("tmp-{}", crate::sweep::token()));
    fs::write(&temp, bytes)?;
    fs::rename(&temp, &path)
}

/// Each output directory noted in the store at `store`, that is there: by
/// its note, else, for a workspace noted by its root alone, `pekit-out`
/// under it.
#[must_use]
pub fn noted(store: &Path) -> Vec<PathBuf> {
    use std::os::unix::ffi::OsStrExt as _;
    let Ok(entries) = fs::read_dir(store.join("seen")) else {
        return Vec::new();
    };
    let mut found: Vec<PathBuf> = Vec::new();
    for entry in entries.flatten() {
        let path = entry.path();
        let as_path = |bytes: &[u8]| PathBuf::from(std::ffi::OsStr::from_bytes(bytes));
        let outputs = match path.extension().and_then(|extension| extension.to_str()) {
            Some("outputs") => fs::read(&path).ok().map(|bytes| as_path(&bytes)),
            Some("root") if !path.with_extension("outputs").exists() => fs::read(&path)
                .ok()
                .map(|bytes| as_path(&bytes).join("pekit-out")),
            _ => None,
        };
        // As where it leads, so that two names of one directory are one.
        if let Some(outputs) = outputs.and_then(|outputs| outputs.canonicalize().ok())
            && outputs.is_dir()
            && !found.contains(&outputs)
        {
            found.push(outputs);
        }
    }
    found.sort();
    found
}

/// The inodes of every blob of the store at `store`, and the device they
/// are on, read from its directories' listings.
fn blob_inodes(store: &Path) -> io::Result<(u64, HashSet<u64>)> {
    let cas = store.join("cas");
    let device = fs::metadata(&cas)?.dev();
    let mut inodes = HashSet::new();
    for shard in fs::read_dir(&cas)? {
        let shard = shard?;
        if shard.file_type()?.is_dir() {
            for entry in fs::read_dir(shard.path())? {
                inodes.insert(entry?.ino());
            }
        }
    }
    Ok((device, inodes))
}

/// Each exposure under the output directory `outputs`.
fn exposures_under(outputs: &Path) -> Vec<PathBuf> {
    let entries = |dir: &Path| -> Vec<PathBuf> {
        fs::read_dir(dir)
            .map(|entries| entries.flatten().map(|entry| entry.path()).collect())
            .unwrap_or_default()
    };
    let mut found: Vec<PathBuf> = entries(&outputs.join(".steps"))
        .iter()
        .flat_map(|function| entries(function))
        .collect();
    for dir in EXPOSED {
        found.extend(entries(&outputs.join(dir)));
    }
    found
}

/// The files at or under `path` of the store's file system `device` that no
/// blob of `blobs` is: each's inode and size.
fn unlinked(path: &Path, (device, blobs): (u64, &HashSet<u64>)) -> Vec<(u64, u64)> {
    let mut found = Vec::new();
    let mut pending = vec![path.to_path_buf()];
    while let Some(path) = pending.pop() {
        let Ok(meta) = fs::symlink_metadata(&path) else {
            continue;
        };
        if meta.is_dir() {
            if let Ok(entries) = fs::read_dir(&path) {
                pending.extend(entries.flatten().map(|entry| entry.path()));
            }
        } else if meta.is_file() && meta.dev() == device && !blobs.contains(&meta.ino()) {
            found.push((meta.ino(), meta.len()));
        }
    }
    found
}

/// Each exposure in every output directory noted in the store at `store`
/// that holds a file no blob of the store is (see [Exposures of evicted
/// blobs](#exposures-of-evicted-blobs)), looked at on several threads.
///
/// # Errors
///
/// That the store's blobs cannot be listed.
pub fn find(store: &Path) -> io::Result<Orphans> {
    let (device, blobs) = blob_inodes(store)?;
    let exposures: Vec<PathBuf> = noted(store)
        .iter()
        .flat_map(|outputs| exposures_under(outputs))
        .collect();
    let found = in_parallel_by(&exposures, 16, |exposure| {
        unlinked(exposure, (device, &blobs))
    });
    let mut orphans = Orphans::default();
    let mut counted = HashSet::new();
    for (exposure, files) in exposures.into_iter().zip(found) {
        if files.is_empty() {
            continue;
        }
        for (inode, size) in files {
            if counted.insert(inode) {
                orphans.bytes += size;
                orphans.files += 1;
            }
        }
        orphans.exposures.push(exposure);
    }
    Ok(orphans)
}

/// Removes each exposure of `orphans`, directories made writable as they
/// are emptied, their files left as they are: what a build exposes again
/// from the store if it needs it. The store's lock is held exclusively, so
/// that no build writes them meanwhile.
///
/// # Errors
///
/// That one cannot be removed.
pub fn release(orphans: &Orphans) -> io::Result<()> {
    for exposure in &orphans.exposures {
        crate::layers::remove_tree(exposure)?;
    }
    Ok(())
}

/// Keeps in the store at `store` what [`find`] found as of now, for
/// [`last`].
///
/// # Errors
///
/// That it cannot be written.
pub fn keep_last(store: &Path, orphans: &Orphans) -> io::Result<()> {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |since| since.as_secs());
    let temp = store
        .join("tmp")
        .join(format!("orphaned-{}", crate::sweep::token()));
    fs::write(
        &temp,
        format!("{} {} {now}\n", orphans.bytes, orphans.exposures.len()),
    )?;
    fs::rename(&temp, store.join("orphaned"))
}

/// What the store at `store` last found held by exposures of evicted blobs:
/// their bytes, how many exposures, and when, in seconds since the Unix
/// epoch.
#[must_use]
pub fn last(store: &Path) -> Option<(u64, u64, u64)> {
    let text = fs::read_to_string(store.join("orphaned")).ok()?;
    let mut numbers = text.split_whitespace().map(|word| word.parse().ok());
    Some((numbers.next()??, numbers.next()??, numbers.next()??))
}
