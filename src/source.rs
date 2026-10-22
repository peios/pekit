//! Fetching, checking and unpacking a build's source, and listing the files
//! of a source tree.
//!
//! Downloads are kept by their SHA-256 in the cache, so a source is fetched
//! once however many builds use it. curl, tar and git do the work for now;
//! pekit can take them over when it needs to run without them.

use std::ffi::OsStr;
use std::fs::{self, File, Metadata};
use std::io;
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::UNIX_EPOCH;

use anyhow::{Context, Result, bail};
use sha2::{Digest, Sha256};

use crate::sync::hex;

/// The path of the archive at `url` in the cache at `cache`, downloading it
/// first unless it is there already. Either way its SHA-256 is `sha256`.
pub fn fetch(url: &str, sha256: &str, cache: &Path) -> Result<PathBuf> {
    let dir = cache.join("sources").join("sha256");
    fs::create_dir_all(&dir).with_context(|| dir.display().to_string())?;
    let path = dir.join(sha256);
    if path.exists() && hash_file(&path)? == sha256 {
        return Ok(path);
    }
    // Downloaded beside where it goes, so that only a checked download is
    // ever at that path.
    let partial = tempfile::NamedTempFile::new_in(&dir)?;
    eprintln!("pekit: fetching {url}");
    let status = Command::new("curl")
        .args([
            "--fail",
            "--silent",
            "--show-error",
            "--location",
            "--output",
        ])
        .arg(partial.path())
        .arg(url)
        .status()
        .context("cannot run curl")?;
    if !status.success() {
        bail!("{url}: download failed");
    }
    let got = hash_file(partial.path())?;
    if got != sha256 {
        bail!("{url}: sha256 is {got}, but the recipe says {sha256}");
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
        newest = newest.max(mtime(&meta, &path)?);
        if meta.is_dir() {
            newest = newest.max(newest_mtime(&path)?);
        }
    }
    Ok(newest)
}

/// The files and symlinks of the unpacked source at `dir`, relative to it.
pub fn files(dir: &Path) -> Result<Vec<PathBuf>> {
    let mut files = Vec::new();
    walk(dir, Path::new(""), &mut files)?;
    Ok(files)
}

/// The files and symlinks of the local source tree at `dir` that a build
/// is given, relative to it.
///
/// In a Git work tree, they are the files Git would commit: those tracked
/// and those untracked but not ignored, as they are on disk. Uncommitted
/// changes are built, and what Git ignores, such as earlier build output,
/// is left behind. A Git submodule, a work tree of its own, is read the
/// same way. Anywhere else, they are every file.
pub fn local_files(dir: &Path) -> Result<Vec<PathBuf>> {
    let meta = fs::metadata(dir).with_context(|| dir.display().to_string())?;
    if !meta.is_dir() {
        bail!("{}: not a directory", dir.display());
    }
    let mut files = Vec::new();
    for relative in git_files(dir)? {
        let path = dir.join(&relative);
        match fs::symlink_metadata(&path) {
            Ok(meta) if meta.is_dir() => files.extend(
                local_files(&path)?
                    .into_iter()
                    .map(|inner| relative.join(inner)),
            ),
            _ => files.push(relative),
        }
    }
    Ok(files)
}

/// The paths, relative to `dir`, Git would commit, or every file and
/// symlink outside a Git work tree.
fn git_files(dir: &Path) -> Result<Vec<PathBuf>> {
    let inside = Command::new("git")
        .arg("-C")
        .arg(dir)
        .args(["rev-parse", "--is-inside-work-tree"])
        .output();
    if !inside.is_ok_and(|output| output.status.success() && output.stdout == b"true\n") {
        return files(dir);
    }
    let output = Command::new("git")
        .arg("-C")
        .arg(dir)
        .args([
            "ls-files",
            "-z",
            "--cached",
            "--others",
            "--exclude-standard",
        ])
        .output()
        .context("cannot run git")?;
    if !output.status.success() {
        bail!(
            "{}: cannot list its files: {}",
            dir.display(),
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }
    let mut files: Vec<PathBuf> = output
        .stdout
        .split(|&byte| byte == 0)
        .filter(|path| !path.is_empty())
        .map(|path| PathBuf::from(OsStr::from_bytes(path)))
        .collect();
    // A file with unmerged changes is listed once for each side.
    files.dedup();
    Ok(files)
}

/// Adds every file and symlink under `base`'s subdirectory `relative`, as a
/// path relative to `base`, to `files`.
fn walk(base: &Path, relative: &Path, files: &mut Vec<PathBuf>) -> Result<()> {
    let dir = base.join(relative);
    for entry in fs::read_dir(&dir).with_context(|| dir.display().to_string())? {
        let entry = entry.with_context(|| dir.display().to_string())?;
        let path = relative.join(entry.file_name());
        if entry.file_type()?.is_dir() {
            walk(base, &path, files)?;
        } else {
            files.push(path);
        }
    }
    Ok(())
}

/// The modification time of `meta`, the metadata of `path`, in seconds
/// since the epoch.
fn mtime(meta: &Metadata, path: &Path) -> Result<u64> {
    Ok(meta
        .modified()
        .with_context(|| path.display().to_string())?
        .duration_since(UNIX_EPOCH)
        .map_or(0, |since| since.as_secs()))
}

/// The SHA-256 of the file at `path`, in lower-case hex.
pub fn hash_file(path: &Path) -> Result<String> {
    let mut file = File::open(path).with_context(|| path.display().to_string())?;
    let mut hasher = Sha256::new();
    io::copy(&mut file, &mut hasher).with_context(|| path.display().to_string())?;
    Ok(hex(&hasher.finalize()))
}
