//! Unpacking: pekit's answer to GCSL's `archive.unpack`.
//!
//! # Unpacking
//!
//! Pekit unpacks a tar archive itself (see `design/libraries.md`, L3), since
//! the build environment that would give a step `tar` is itself an archive.
//! An unpacking is keyed by the archive's digest and `strip`, and recorded in
//! the store as the tree it made, so the same archive is unpacked once, on
//! any machine that shares the store. Its directory is
//! `/pekit-out/.unpack/<key>`, fixed by what it holds.
//!
//! The archive is read as a zip by its first bytes, as a wheel or a jar is,
//! else as gzip, xz, bzip2 or zstd, or as a plain tar. Each entry's path has its first `strip` components left off,
//! an entry with no more left out, as `tar --strip-components` does. An
//! entry whose path would leave the directory, being absolute, holding `..`,
//! or passing through a symbolic link the archive made, fails the unpacking:
//! nothing is ever written outside it. A symbolic link is kept as it is,
//! which a sandbox resolves inside itself; a hard link becomes a copy; a
//! device or a pipe is left out. Of each file's mode only whether it is
//! executable is kept, as the store keeps files.

use std::fs;
use std::io::{self, BufRead, BufReader, Read};
use std::os::unix::fs::PermissionsExt;
use std::path::{Component, Path, PathBuf};
use std::sync::atomic::{AtomicU32, Ordering};

use serde::{Deserialize, Serialize};
use sha2::{Digest as _, Sha256};
use tar::EntryType;

use crate::layers::remove_tree;
use crate::sandbox::OUTPUTS;
use crate::store::{Digest, FileEntry, Store};

/// The record of an unpacking: the tree it made.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
struct Unpacked {
    /// Its tree.
    tree: Digest,
    /// How long it took, in milliseconds: what finding it saves.
    #[serde(default)]
    duration_ms: u64,
}

/// The number of the next directory unpacked into, in this process.
static NEXT: AtomicU32 = AtomicU32::new(0);

/// The key of the unpacking of the archive `archive` with `strip`.
#[must_use]
pub fn unpack_key(archive: &Digest, strip: u32) -> Digest {
    let mut hasher = Sha256::new();
    hasher.update(b"pekit-unpack-1\0");
    hasher.update(archive.bytes());
    hasher.update(strip.to_le_bytes());
    Digest::from_hasher(hasher)
}

/// The directory, as GCSL code names it, of the unpacking of key `key`.
#[must_use]
pub fn unpacked_path(key: &Digest) -> String {
    format!("{OUTPUTS}/.unpack/{}", key.hex())
}

/// The tree of what the archive `archive`, named `shown` in errors, holds,
/// the first `strip` components of each path left off, from `store`'s
/// record of it, else unpacked in a new directory under `scratch` and put
/// into the store (see [Unpacking](#unpacking)).
///
/// # Errors
///
/// That the archive cannot be read, or holds an entry that would leave the
/// directory.
pub fn unpack(
    store: &Store,
    (archive, shown): (&FileEntry, &str),
    strip: u32,
    scratch: &Path,
) -> Result<Digest, String> {
    let key = unpack_key(&archive.digest, strip);
    // An unpacking whose contents were evicted is done again.
    if let Ok(Some(Unpacked { tree, .. })) = store.record::<Unpacked>(&key)
        && store.holds_tree(&tree)
    {
        return Ok(tree);
    }
    let began = std::time::Instant::now();
    let fail = |reason: String| format!("`archive.unpack` could not unpack `{shown}`: {reason}");
    let number = NEXT.fetch_add(1, Ordering::Relaxed);
    let dir = scratch.join(format!("unpack-{}-{number}", crate::sweep::token()));
    remove_tree(&dir).map_err(|error| fail(error.to_string()))?;
    fs::create_dir_all(&dir).map_err(|error| fail(error.to_string()))?;
    let unpacked = extract(store, archive, strip, &dir)
        .and_then(|()| store.put_tree(&dir).map_err(|error| error.to_string()));
    let _ = remove_tree(&dir);
    let tree = unpacked.map_err(fail)?;
    store
        .put_record(
            &key,
            &Unpacked {
                tree,
                duration_ms: u64::try_from(began.elapsed().as_millis()).unwrap_or(u64::MAX),
            },
        )
        .map_err(|error| fail(error.to_string()))?;
    Ok(tree)
}

/// The reader of the blob of `archive`, decompressed as its first bytes say.
fn decompressed(store: &Store, archive: &FileEntry) -> io::Result<Box<dyn Read>> {
    let file = fs::File::open(store.blob_path(&archive.digest, archive.executable))?;
    let mut buffered = BufReader::new(file);
    let head = buffered.fill_buf()?.to_vec();
    Ok(match head.as_slice() {
        [0x1f, 0x8b, ..] => Box::new(flate2::read::MultiGzDecoder::new(buffered)),
        [0xfd, b'7', b'z', b'X', b'Z', 0x00, ..] => {
            Box::new(liblzma::read::XzDecoder::new_multi_decoder(buffered))
        }
        [b'B', b'Z', b'h', ..] => Box::new(bzip2::read::MultiBzDecoder::new(buffered)),
        [0x28, 0xb5, 0x2f, 0xfd, ..] => {
            Box::new(zstd::stream::read::Decoder::with_buffer(buffered)?)
        }
        _ => Box::new(buffered),
    })
}

/// `path` with its first `strip` components left off, if it has more, and
/// if it stays inside the directory: relative, with no `..`.
///
/// # Errors
///
/// That it would leave the directory.
fn stripped(path: &Path, strip: u32) -> Result<Option<PathBuf>, String> {
    let mut kept = PathBuf::new();
    let mut seen = 0u32;
    for component in path.components() {
        match component {
            Component::Normal(part) => {
                if seen >= strip {
                    kept.push(part);
                }
                seen = seen.saturating_add(1);
            }
            Component::CurDir => {}
            Component::RootDir | Component::Prefix(_) | Component::ParentDir => {
                return Err(format!(
                    "it holds `{}`, which leaves its directory",
                    path.display()
                ));
            }
        }
    }
    Ok((!kept.as_os_str().is_empty()).then_some(kept))
}

/// Whether a component of `path` under `dir`, but its last, is a symbolic
/// link: what an entry must never be written through.
fn through_link(dir: &Path, path: &Path) -> bool {
    let mut at = dir.to_path_buf();
    let parts: Vec<_> = path.components().collect();
    for part in parts.iter().take(parts.len().saturating_sub(1)) {
        at.push(part);
        if fs::symlink_metadata(&at).is_ok_and(|meta| meta.file_type().is_symlink()) {
            return true;
        }
    }
    false
}

/// Unpacks the archive `archive` from `store` into `dir`, the first `strip`
/// components of each path left off (see [Unpacking](#unpacking)).
/// Whether the file at `path` begins as a zip archive does: a local file's
/// header, or the end of an empty archive's directory.
fn is_zip(path: &Path) -> bool {
    let mut start = [0u8; 4];
    fs::File::open(path)
        .and_then(|mut file| file.read_exact(&mut start))
        .is_ok_and(|()| matches!(&start, b"PK\x03\x04" | b"PK\x05\x06"))
}

/// Unpacks the zip archive at `path` into `dir`, as [`extract`] unpacks a
/// tar: the first `strip` components of each path left off, nothing written
/// outside `dir`, a symbolic link kept, and of each file's mode only whether
/// it is executable.
fn extract_zip(path: &Path, strip: u32, dir: &Path) -> Result<(), String> {
    const KIND: u32 = 0o170_000;
    const LINK: u32 = 0o120_000;
    let file = fs::File::open(path).map_err(|error| error.to_string())?;
    let mut zip = zip::ZipArchive::new(file).map_err(|error| error.to_string())?;
    for index in 0..zip.len() {
        let mut entry = zip.by_index(index).map_err(|error| error.to_string())?;
        let name = PathBuf::from(entry.name().map_err(|error| error.to_string())?.as_ref());
        let Some(relative) = stripped(&name, strip)? else {
            continue;
        };
        if through_link(dir, &relative) {
            return Err(format!(
                "it holds `{}`, which passes through a link it made",
                name.display()
            ));
        }
        let target = dir.join(&relative);
        let io = |error: io::Error| format!("`{}`: {error}", name.display());
        fs::create_dir_all(target.parent().unwrap_or(dir)).map_err(io)?;
        let mode = entry.unix_mode().unwrap_or(0o644);
        if entry.is_dir() {
            fs::create_dir_all(&target).map_err(io)?;
        } else if mode & KIND == LINK {
            let mut link = String::new();
            entry.read_to_string(&mut link).map_err(io)?;
            std::os::unix::fs::symlink(link, &target).map_err(io)?;
        } else {
            let mut out = fs::File::create(&target).map_err(io)?;
            io::copy(&mut entry, &mut out).map_err(io)?;
            let mode = if mode & 0o111 == 0 { 0o644 } else { 0o755 };
            fs::set_permissions(&target, fs::Permissions::from_mode(mode)).map_err(io)?;
        }
    }
    Ok(())
}

fn extract(store: &Store, archive: &FileEntry, strip: u32, dir: &Path) -> Result<(), String> {
    let blob = store.blob_path(&archive.digest, archive.executable);
    if is_zip(&blob) {
        return extract_zip(&blob, strip, dir);
    }
    let reader = decompressed(store, archive).map_err(|error| error.to_string())?;
    let mut tar = tar::Archive::new(reader);
    for entry in tar.entries().map_err(|error| error.to_string())? {
        let mut entry = entry.map_err(|error| error.to_string())?;
        let path = entry
            .path()
            .map_err(|error| error.to_string())?
            .into_owned();
        let Some(relative) = stripped(&path, strip)? else {
            continue;
        };
        if through_link(dir, &relative) {
            return Err(format!(
                "it holds `{}`, which passes through a link it made",
                path.display()
            ));
        }
        let target = dir.join(&relative);
        let parent = target.parent().unwrap_or(dir);
        let io = |error: io::Error| format!("`{}`: {error}", path.display());
        fs::create_dir_all(parent).map_err(io)?;
        match entry.header().entry_type() {
            EntryType::Directory => fs::create_dir_all(&target).map_err(io)?,
            EntryType::Regular | EntryType::Continuous | EntryType::GNUSparse => {
                let mode = entry.header().mode().unwrap_or(0o644);
                let mut file = fs::File::create(&target).map_err(io)?;
                io::copy(&mut entry, &mut file).map_err(io)?;
                let mode = if mode & 0o111 == 0 { 0o644 } else { 0o755 };
                fs::set_permissions(&target, fs::Permissions::from_mode(mode)).map_err(io)?;
            }
            EntryType::Symlink => {
                let link = entry
                    .link_name()
                    .map_err(|error| error.to_string())?
                    .ok_or_else(|| format!("`{}` is a link to nothing", path.display()))?;
                std::os::unix::fs::symlink(link.as_ref(), &target).map_err(io)?;
            }
            EntryType::Link => {
                let link = entry
                    .link_name()
                    .map_err(|error| error.to_string())?
                    .ok_or_else(|| format!("`{}` is a link to nothing", path.display()))?;
                let Some(source) = stripped(&link, strip)? else {
                    continue;
                };
                fs::copy(dir.join(source), &target).map_err(io)?;
            }
            // A device, a pipe or what else a tar may hold is no file.
            _ => {}
        }
    }
    Ok(())
}
