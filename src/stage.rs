//! Choosing a package's files from its build's install tree.
//!
//! Each entry of a package's `files` maps a path in /usr to the path in the
//! install tree it comes from. A path is relative, `/` separated, with no
//! `.` or `..`; its last segment may be `*`, on both sides or neither, to
//! take every entry of a directory. A directory is taken whole, and a
//! symlink as it is, never followed.

use std::fs;
use std::os::unix::fs::symlink;
use std::path::Path;

use anyhow::{Context, Result, bail};

/// Copies the files `files` chooses from the install tree `tree` into
/// `stage`, which exists and is empty.
pub fn stage(files: &[(String, String)], tree: &Path, stage: &Path) -> Result<()> {
    for (to, from) in files {
        let entry = || format!("files: \"{to}\" = \"{from}\"");
        let to_segments = segments(to).with_context(entry)?;
        let from_segments = segments(from).with_context(entry)?;
        match (to_segments.split_last(), from_segments.split_last()) {
            (Some((&"*", to_dir)), Some((&"*", from_dir))) => {
                let from_dir = tree.join(from_dir.join("/"));
                let to_dir = stage.join(to_dir.join("/"));
                let mut names = fs::read_dir(&from_dir)
                    .with_context(|| format!("{}: not in the install tree", from_dir.display()))
                    .with_context(entry)?
                    .map(|found| found.map(|found| found.file_name()))
                    .collect::<Result<Vec<_>, _>>()?;
                names.sort();
                for name in names {
                    copy(&from_dir.join(&name), &to_dir.join(&name)).with_context(entry)?;
                }
            }
            (Some((&"*", _)), _) | (_, Some((&"*", _))) => {
                bail!("{}: `*` must end both paths or neither", entry())
            }
            _ => copy(&tree.join(from), &stage.join(to)).with_context(entry)?,
        }
    }
    Ok(())
}

/// The segments of the relative path `path`, checked.
fn segments(path: &str) -> Result<Vec<&str>> {
    if path.is_empty() || path.starts_with('/') {
        bail!("{path:?} is not a relative path");
    }
    let segments: Vec<&str> = path.split('/').collect();
    for (at, segment) in segments.iter().enumerate() {
        match *segment {
            "" | "." | ".." => bail!("{path:?} has an empty, `.` or `..` segment"),
            "*" if at + 1 == segments.len() => {}
            segment if segment.contains('*') => {
                bail!("{path:?}: only a whole last segment can be `*`")
            }
            _ => {}
        }
    }
    Ok(segments)
}

/// Copies the file, symlink or directory at `from` to `to`, which must not
/// exist unless both are directories.
fn copy(from: &Path, to: &Path) -> Result<()> {
    let meta = fs::symlink_metadata(from)
        .with_context(|| format!("{}: not in the install tree", from.display()))?;
    if let Some(parent) = to.parent() {
        fs::create_dir_all(parent).with_context(|| parent.display().to_string())?;
    }
    if let Ok(existing) = fs::symlink_metadata(to)
        && !(existing.is_dir() && meta.is_dir())
    {
        bail!("{}: chosen twice", to.display());
    }
    let kind = meta.file_type();
    if kind.is_symlink() {
        let target = fs::read_link(from).with_context(|| from.display().to_string())?;
        symlink(&target, to).with_context(|| to.display().to_string())?;
    } else if kind.is_dir() {
        fs::create_dir_all(to).with_context(|| to.display().to_string())?;
        let mut names = fs::read_dir(from)
            .with_context(|| from.display().to_string())?
            .map(|found| found.map(|found| found.file_name()))
            .collect::<Result<Vec<_>, _>>()?;
        names.sort();
        for name in names {
            copy(&from.join(&name), &to.join(&name))?;
        }
    } else if kind.is_file() {
        // Permissions are copied too, which keeps the execute bit, the only
        // part of the mode a package records.
        fs::copy(from, to).with_context(|| format!("{} to {}", from.display(), to.display()))?;
    } else {
        bail!("{}: not a file, directory or symlink", from.display());
    }
    Ok(())
}
