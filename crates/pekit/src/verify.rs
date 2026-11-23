//! Verifying the store: each blob read and hashed again, and its mode
//! looked at, as `pekit store verify` does.
//!
//! # Verifying
//!
//! A blob is named by the SHA-256 of its contents, `.x` after it for an
//! executable one and `.rN` after that for a numbered copy, and is
//! read-only: `0444`, or `0555` if it is executable ([Blobs and
//! trees](crate::store#blobs-and-trees)). A build trusts each blob it finds
//! as its name says. One that is not, whose contents were changed in place
//! through a link to it, say, or by a disk's fault, is *damaged*; one whose
//! contents are as its name says but whose mode is not its own is
//! *exposed*, since it can be changed through any link to it, such as an
//! output a build copied out. A verification reads every blob, on several
//! threads, and reports each that is either; files in `cas/` that are no
//! blob's are reported and left alone.
//!
//! # Repairing
//!
//! A repair makes each exposed blob read-only again, and removes each
//! damaged blob, so that what needs it is fetched or built again. A damaged
//! blob may be linked from materialised trees, and named by trees marked
//! complete, so if one is removed, so is every mark and every materialised
//! tree, as an eviction removes them. It runs with the store to itself, as
//! `pekit store evict` does ([`crate::evict::evict_alone`]).

use std::fs::{self, File};
use std::io::{self, Read as _};
use std::os::unix::fs::PermissionsExt as _;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};

use sha2::{Digest as _, Sha256};

use crate::store::Store;

/// The most threads a verification reads blobs on.
const MOST_THREADS: usize = 8;

/// How often a verification tells how far it is.
pub const PROGRESS_EVERY: std::time::Duration = std::time::Duration::from_secs(5);

/// What a verification found (see [Verifying](#verifying)).
#[derive(Debug, Default)]
pub struct Verified {
    /// How many blobs it read.
    pub blobs: u64,
    /// How many bytes they hold.
    pub bytes: u64,
    /// Each damaged blob.
    pub damaged: Vec<PathBuf>,
    /// Each exposed blob.
    pub exposed: Vec<PathBuf>,
    /// Each file in `cas/` that is no blob's, or could not be read.
    pub unknown: Vec<PathBuf>,
}

impl Verified {
    /// Whether every blob is whole and read-only, and nothing else is in
    /// `cas/`.
    #[must_use]
    pub fn sound(&self) -> bool {
        self.damaged.is_empty() && self.exposed.is_empty() && self.unknown.is_empty()
    }
}

/// What the blob named `name` holds, by its name: its contents' SHA-256 in
/// hexadecimal, and whether it is executable; `None` for a name that is no
/// blob's.
fn named(name: &str) -> Option<(&str, bool)> {
    let hex = name.get(..64)?;
    if !hex
        .bytes()
        .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
    {
        return None;
    }
    let rest = &name[64..];
    let (executable, rest) = match rest.strip_prefix(".x") {
        Some(rest) => (true, rest),
        None => (false, rest),
    };
    let copy = rest.strip_prefix(".r").map(str::parse::<u32>);
    match (rest.is_empty(), copy) {
        (true, _) | (false, Some(Ok(_))) => Some((hex, executable)),
        _ => None,
    }
}

/// The SHA-256 of the file at `path`, in hexadecimal, and its size.
fn hashed(path: &Path) -> io::Result<(String, u64)> {
    let mut hasher = Sha256::new();
    let mut file = File::open(path)?;
    let mut buffer = vec![0u8; 1 << 20];
    let mut size = 0u64;
    loop {
        let read = file.read(&mut buffer)?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
        size += read as u64;
    }
    let hex = hasher
        .finalize()
        .iter()
        .fold(String::new(), |mut hex, byte| {
            use std::fmt::Write as _;
            let _ = write!(hex, "{byte:02x}");
            hex
        });
    Ok((hex, size))
}

/// Every file in the store at `root`'s `cas/`.
fn blobs(root: &Path) -> io::Result<Vec<PathBuf>> {
    let mut found = Vec::new();
    let cas = root.join("cas");
    let shards = match fs::read_dir(&cas) {
        Ok(shards) => shards,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(found),
        Err(error) => return Err(error),
    };
    for shard in shards {
        let shard = shard?;
        if !shard.file_type()?.is_dir() {
            found.push(shard.path());
            continue;
        }
        for entry in fs::read_dir(shard.path())? {
            found.push(entry?.path());
        }
    }
    found.sort();
    Ok(found)
}

/// What became of one file of `cas/` verified.
enum Found {
    /// A blob whole and read-only, of its size.
    Sound(u64),
    /// A blob whole but not read-only, of its size.
    Exposed(u64),
    /// A blob not as its name says, of its size.
    Damaged(u64),
    /// No blob's, or not to be read.
    Unknown,
}

/// What the file at `path` of `cas/` is (see [Verifying](#verifying)).
fn verified(path: &Path) -> Found {
    let Some(name) = path.file_name().and_then(|name| name.to_str()) else {
        return Found::Unknown;
    };
    let Some((hex, executable)) = named(name) else {
        return Found::Unknown;
    };
    let Ok(meta) = fs::symlink_metadata(path) else {
        return Found::Unknown;
    };
    if !meta.is_file() {
        return Found::Unknown;
    }
    let Ok((digest, size)) = hashed(path) else {
        return Found::Unknown;
    };
    if digest != hex {
        return Found::Damaged(size);
    }
    let mode = if executable { 0o555 } else { 0o444 };
    if meta.permissions().mode() & 0o7777 == mode {
        Found::Sound(size)
    } else {
        Found::Exposed(size)
    }
}

/// Verifies every blob of the store at `root` (see
/// [Verifying](#verifying)), on several threads, `progress` told every
/// [`PROGRESS_EVERY`] how many blobs, and bytes, are verified of how many:
/// as `pekit store verify` does, with the store open beside builds.
///
/// # Errors
///
/// That the store's `cas/` cannot be listed.
pub fn verify(root: &Path, progress: &mut dyn FnMut(u64, u64, usize)) -> io::Result<Verified> {
    let paths = blobs(root)?;
    let next = AtomicUsize::new(0);
    let (done, bytes) = (AtomicU64::new(0), AtomicU64::new(0));
    let found: Mutex<Vec<(usize, Found)>> = Mutex::new(Vec::with_capacity(paths.len()));
    let threads = std::thread::available_parallelism()
        .map_or(1, usize::from)
        .min(MOST_THREADS);
    let running = AtomicUsize::new(threads);
    std::thread::scope(|scope| {
        for _ in 0..threads {
            scope.spawn(|| {
                let mut own = Vec::new();
                loop {
                    let at = next.fetch_add(1, Ordering::Relaxed);
                    let Some(path) = paths.get(at) else {
                        break;
                    };
                    let one = verified(path);
                    if let Found::Sound(size) | Found::Exposed(size) | Found::Damaged(size) = one {
                        bytes.fetch_add(size, Ordering::Relaxed);
                    }
                    done.fetch_add(1, Ordering::Relaxed);
                    own.push((at, one));
                }
                if let Ok(mut found) = found.lock() {
                    found.extend(own);
                }
                running.fetch_sub(1, Ordering::Release);
            });
        }
        let mut told = std::time::Instant::now();
        while running.load(Ordering::Acquire) > 0 {
            std::thread::sleep(std::time::Duration::from_millis(50));
            if told.elapsed() >= PROGRESS_EVERY {
                told = std::time::Instant::now();
                progress(
                    done.load(Ordering::Relaxed),
                    bytes.load(Ordering::Relaxed),
                    paths.len(),
                );
            }
        }
    });
    let mut found = found.into_inner().unwrap_or_default();
    found.sort_by_key(|(at, _)| *at);
    let mut verified = Verified::default();
    for (at, one) in found {
        let path = paths[at].clone();
        match one {
            Found::Sound(size) => {
                verified.blobs += 1;
                verified.bytes += size;
            }
            Found::Exposed(size) => {
                verified.blobs += 1;
                verified.bytes += size;
                verified.exposed.push(path);
            }
            Found::Damaged(size) => {
                verified.blobs += 1;
                verified.bytes += size;
                verified.damaged.push(path);
            }
            Found::Unknown => verified.unknown.push(path),
        }
    }
    Ok(verified)
}

/// Repairs what `verified` found of `store` (see
/// [Repairing](#repairing)): each exposed blob made read-only, and each
/// damaged one removed, with every mark that a tree is complete and every
/// materialised tree if any is; the store's lock held exclusively.
///
/// # Errors
///
/// That a blob's mode cannot be set, or a blob, mark or materialised tree
/// cannot be removed.
pub fn repair(store: &Store, verified: &Verified) -> io::Result<()> {
    for path in &verified.exposed {
        let name = path
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or_default();
        let executable = named(name).is_some_and(|(_, executable)| executable);
        let mode = if executable { 0o555 } else { 0o444 };
        fs::set_permissions(path, fs::Permissions::from_mode(mode))?;
    }
    if verified.damaged.is_empty() {
        return Ok(());
    }
    store.forget_complete()?;
    store.forget_materialised()?;
    for path in &verified.damaged {
        match fs::remove_file(path) {
            Err(error) if error.kind() != io::ErrorKind::NotFound => return Err(error),
            _ => {}
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::named;

    #[test]
    fn a_blob_is_named_by_its_digest_its_mode_and_its_copy() {
        let hex = "ab".repeat(32);
        assert_eq!(named(&hex), Some((hex.as_str(), false)));
        assert_eq!(named(&format!("{hex}.x")), Some((hex.as_str(), true)));
        assert_eq!(named(&format!("{hex}.r2")), Some((hex.as_str(), false)));
        assert_eq!(named(&format!("{hex}.x.r3")), Some((hex.as_str(), true)));
        assert_eq!(named(&format!("{hex}.y")), None);
        assert_eq!(named(&format!("{hex}.r")), None);
        assert_eq!(named(&"AB".repeat(32)), None);
        assert_eq!(named("short"), None);
    }
}
