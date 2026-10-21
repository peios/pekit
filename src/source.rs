//! Fetching, checking and unpacking a build's source.
//!
//! Downloads are kept by their SHA-256 in the cache, so a source is fetched
//! once however many builds use it. curl and tar do the work for now; pekit
//! can take them over when it needs to run without them.

use std::fs::{self, File};
use std::io;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::UNIX_EPOCH;

use anyhow::{Context, Result, bail};
use sha2::{Digest, Sha256};

use crate::recipes::Source;

/// The path of `source` in the cache at `cache`, downloading it first unless
/// it is there already. Either way its SHA-256 is the recipe's.
pub fn fetch(source: &Source, cache: &Path) -> Result<PathBuf> {
    let dir = cache.join("sources").join("sha256");
    fs::create_dir_all(&dir).with_context(|| dir.display().to_string())?;
    let path = dir.join(&source.sha256);
    if path.exists() && hash_file(&path)? == source.sha256 {
        return Ok(path);
    }
    // Downloaded beside where it goes, so that only a checked download is
    // ever at that path.
    let partial = tempfile::NamedTempFile::new_in(&dir)?;
    eprintln!("pekit: fetching {}", source.url);
    let status = Command::new("curl")
        .args([
            "--fail",
            "--silent",
            "--show-error",
            "--location",
            "--output",
        ])
        .arg(partial.path())
        .arg(&source.url)
        .status()
        .context("cannot run curl")?;
    if !status.success() {
        bail!("{}: download failed", source.url);
    }
    let got = hash_file(partial.path())?;
    if got != source.sha256 {
        bail!(
            "{}: sha256 is {got}, but the recipe says {}",
            source.url,
            source.sha256
        );
    }
    partial
        .persist(&path)
        .with_context(|| path.display().to_string())?;
    Ok(path)
}

/// Unpacks the archive `archive` into `into`, a directory it creates, and
/// returns the source's top directory: the one directory the archive holds,
/// as most do, or `into` itself.
pub fn unpack(archive: &Path, into: &Path) -> Result<PathBuf> {
    fs::create_dir(into).with_context(|| into.display().to_string())?;
    let status = Command::new("tar")
        .arg("-xf")
        .arg(archive)
        .arg("-C")
        .arg(into)
        .status()
        .context("cannot run tar")?;
    if !status.success() {
        bail!("{}: cannot unpack", archive.display());
    }
    let entries = fs::read_dir(into)
        .with_context(|| into.display().to_string())?
        .collect::<io::Result<Vec<_>>>()?;
    if let [entry] = entries.as_slice()
        && entry.file_type()?.is_dir()
    {
        return Ok(entry.path());
    }
    Ok(into.to_path_buf())
}

/// The newest modification time, in seconds since the epoch, of anything
/// under the directory `dir`, whose own time does not count: for an unpacked
/// source, the time of its newest file, which the archive gives it.
pub fn newest_mtime(dir: &Path) -> Result<u64> {
    let mut newest = 0;
    for entry in fs::read_dir(dir).with_context(|| dir.display().to_string())? {
        let path = entry?.path();
        let meta = fs::symlink_metadata(&path).with_context(|| path.display().to_string())?;
        let modified = meta
            .modified()
            .with_context(|| path.display().to_string())?
            .duration_since(UNIX_EPOCH)
            .map_or(0, |since| since.as_secs());
        newest = newest.max(modified);
        if meta.is_dir() {
            newest = newest.max(newest_mtime(&path)?);
        }
    }
    Ok(newest)
}

/// The SHA-256 of the file at `path`, in lower-case hex.
fn hash_file(path: &Path) -> Result<String> {
    let mut file = File::open(path).with_context(|| path.display().to_string())?;
    let mut hasher = Sha256::new();
    io::copy(&mut file, &mut hasher).with_context(|| path.display().to_string())?;
    Ok(hasher
        .finalize()
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect())
}
