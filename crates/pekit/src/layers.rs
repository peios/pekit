//! Layers: a step's compositions and overlay sandboxes, on the host.
//!
//! # Compositions and sandboxes
//!
//! A step composes directories (`fs.compose_dir`) and runs commands in
//! sandboxes of them (`Dir.sandbox`) as plain GCSL does, but without copying
//! (see `design/libraries.md`, L6a). Its sandbox ([`crate::sandbox::StepSandbox`])
//! answers both itself:
//!
//! - **A composition** hard-links each file it places that nothing can
//!   change, a staged input's or another composition's, and copies any other
//!   ([`Layers::place_file`]). Once it is done it is *sealed*: its files made
//!   read-only on the host, and it bound read-only to every command, so that
//!   no program can change it, and nothing a store's blob through it. Its
//!   directories keep their modes, since a sandbox's merged directories show
//!   their lower's mode, and a read-only one would refuse every write, even
//!   into the sandbox's own upper layer: a build in a sandbox of the
//!   composition writes beside its source.
//! - **A sandbox** of a directory that nothing can change is an overlay
//!   ([`Layers::make_sandbox`]): its commands see the directory as their
//!   `/`, their writes kept in the sandbox's own directory, the *upper*
//!   layer, never in the directory, the *lower* one. A sandbox of any other
//!   directory is copied, as plain GCSL does.
//! - **A path in an overlay sandbox** is read upper over lower
//!   ([`Layers::resolve`]), as the overlay shows it to its commands: what the
//!   upper layer has, unless it is a *whiteout*, a character device of number
//!   0, which hides what the lower has; else what the lower has, unless an
//!   upper directory on the way is *opaque* (`user.overlay.opaque`), which
//!   hides all the lower has below it. A directory both layers have is merged
//!   into a new tree of hard links, a snapshot of it as it is then. The
//!   sandbox's own root is its upper layer, which is where GCSL itself writes
//!   into it, such as the working directory it makes.

use std::collections::HashSet;
use std::fs;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

/// An overlay sandbox (see [Compositions and sandboxes](#compositions-and-sandboxes)).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Overlay {
    /// Its root, as GCSL code names it.
    pub root: String,
    /// The directory it is of, on the host: its lower layer.
    pub lower: PathBuf,
    /// Its writes, on the host: its upper layer, the root's own directory.
    pub upper: PathBuf,
    /// The overlay's work directory, on the host's file system of `upper`.
    pub work: PathBuf,
}

/// What a step's sandbox knows of its compositions and sandboxes.
#[derive(Debug, Default)]
struct Known {
    /// The sealed compositions, each its path as GCSL code names it and on
    /// the host.
    sealed: Vec<(String, PathBuf)>,
    /// The overlay sandboxes.
    overlays: Vec<Overlay>,
    /// The number of the next overlay's work directory, and of the next
    /// merged directory.
    next: u32,
    /// The files, by device and inode, that the composition being made has
    /// a link to: GCSL makes one composition at a time, and ends each by
    /// sealing it.
    placing: HashSet<(u64, u64)>,
    /// The files placed since the last composition was sealed that a
    /// program could write, each with its mode: what sealing the next makes
    /// read-only.
    writable: Vec<(PathBuf, u32)>,
}

/// A step's compositions and sandboxes (see [Compositions and
/// sandboxes](#compositions-and-sandboxes)), kept under its staging
/// directory.
#[derive(Debug)]
pub struct Layers {
    /// The directories on the host whose files nothing can change: the step's
    /// staged inputs.
    unchanging: Vec<PathBuf>,
    /// The directory under them that the step changes: its own outputs.
    changing: PathBuf,
    /// Where work directories and merged directories are made.
    scratch: PathBuf,
    /// What it knows.
    known: Mutex<Known>,
}

/// Whether `path` is `dir` or under it.
fn within(path: &Path, dir: &Path) -> bool {
    path.starts_with(dir)
}

/// Whether the entry at `path` is an overlay's whiteout: a character device
/// of number 0.
fn whiteout(path: &Path) -> bool {
    fs::symlink_metadata(path)
        .is_ok_and(|meta| meta.file_type().is_char_device() && meta.rdev() == 0)
}

/// Whether the directory at `path` is an overlay's opaque one.
fn opaque(path: &Path) -> bool {
    ["user.overlay.opaque", "trusted.overlay.opaque"]
        .iter()
        .any(|name| matches!(xattr::get(path, name), Ok(Some(value)) if value == b"y"))
}

/// Whether the entry at `path`, of metadata `meta`, is what a store's tree
/// can't keep, or a copy of it would lose: neither a file, a directory nor
/// a symbolic link, as an overlay's whiteout is, or marked by an overlay's
/// attributes, as an opaque directory is, but for those that say only where
/// it came from or which layer it is.
#[must_use]
pub fn unkeepable(path: &Path, meta: &fs::Metadata) -> bool {
    let kind = meta.file_type();
    if !(kind.is_file() || kind.is_dir() || kind.is_symlink()) {
        return true;
    }
    xattr::list(path).is_ok_and(|mut names| {
        names.any(|name| {
            let name = name.to_string_lossy();
            let own = name
                .strip_prefix("user.overlay.")
                .or_else(|| name.strip_prefix("trusted.overlay."));
            own.is_some_and(|own| !matches!(own, "origin" | "impure" | "uuid"))
        })
    })
}

/// Whether there is an entry at `path`, a whiteout as much as a file.
fn present(path: &Path) -> bool {
    fs::symlink_metadata(path).is_ok()
}

/// Whether there is a directory at `path`, not following a link.
fn is_dir(path: &Path) -> bool {
    fs::symlink_metadata(path).is_ok_and(|meta| meta.is_dir())
}

/// Makes every file under `path`, or it if it is one, read-only. Its
/// directories keep their modes, which a sandbox of it shows its commands:
/// read-only, a command could write nothing in it, even into the sandbox's
/// own upper layer.
fn seal(path: &Path) -> io::Result<()> {
    let meta = fs::symlink_metadata(path)?;
    if meta.file_type().is_symlink() {
        return Ok(());
    }
    if meta.is_dir() {
        for entry in fs::read_dir(path)? {
            seal(&entry?.path())?;
        }
        return Ok(());
    }
    let mode = meta.permissions().mode();
    fs::set_permissions(path, fs::Permissions::from_mode(mode & !0o222))
}

/// Removes the tree at `path`, if there is one, its directories made
/// writable first, since a sealed composition's are not.
///
/// # Errors
///
/// That it could not be removed.
pub fn remove_tree(path: &Path) -> io::Result<()> {
    if !present(path) {
        return Ok(());
    }
    unseal(path)?;
    fs::remove_dir_all(path)
}

/// Makes every directory under `path`, and it, writable by its owner.
fn unseal(path: &Path) -> io::Result<()> {
    let meta = fs::symlink_metadata(path)?;
    if !meta.is_dir() {
        return Ok(());
    }
    let mode = meta.permissions().mode();
    fs::set_permissions(path, fs::Permissions::from_mode(mode | 0o700))?;
    for entry in fs::read_dir(path)? {
        unseal(&entry?.path())?;
    }
    Ok(())
}

impl Layers {
    /// The layers of a step whose staged inputs, which nothing changes, are
    /// under `unchanging`, but its own outputs, `changing`, made under
    /// `scratch`.
    #[must_use]
    pub fn new(unchanging: Vec<PathBuf>, changing: &Path, scratch: &Path) -> Self {
        Self {
            unchanging,
            changing: changing.to_path_buf(),
            scratch: scratch.to_path_buf(),
            known: Mutex::new(Known::default()),
        }
    }

    /// What it knows, even if a thread panicked holding it.
    fn known(&self) -> std::sync::MutexGuard<'_, Known> {
        self.known
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Whether nothing can change the file or directory at `path` on the
    /// host: a staged input's, or a sealed composition's.
    fn is_unchanging(&self, path: &Path) -> bool {
        let staged =
            self.unchanging.iter().any(|dir| within(path, dir)) && !within(path, &self.changing);
        staged || self.known().sealed.iter().any(|(_, dir)| within(path, dir))
    }

    /// Places the file at `source` at `target`, both on the host, for a
    /// composition: a hard link if nothing can change it, it is on the same
    /// file system and the composition has no other link to it, else a copy
    /// of its own. So two files of a composition are never one, whatever
    /// the store shares between them, as two of a tree it stages are not
    /// ([`crate::store::Store::materialise_tree`]).
    ///
    /// # Errors
    ///
    /// That it could not be placed.
    pub fn place_file(&self, source: &Path, target: &Path) -> io::Result<()> {
        let meta = fs::metadata(source)?;
        if self.is_unchanging(source) {
            let first = self.known().placing.insert((meta.dev(), meta.ino()));
            if first && fs::hard_link(source, target).is_ok() {
                self.note_mode(target, meta.permissions().mode());
                return Ok(());
            }
        }
        fs::copy(source, target)?;
        // A copy has its source's mode.
        self.note_mode(target, meta.permissions().mode());
        Ok(())
    }

    /// Notes the file at `target`, of mode `mode`, for sealing, if a
    /// program could write it.
    fn note_mode(&self, target: &Path, mode: u32) {
        if mode & 0o222 != 0 {
            self.known().writable.push((target.to_path_buf(), mode));
        }
    }

    /// Seals the finished composition at `root`, as GCSL code names it, at
    /// `host` on the host: makes each file placed in it that a program
    /// could write read-only. Every file of it was placed
    /// ([`Layers::place_file`]), but its symbolic links.
    ///
    /// # Errors
    ///
    /// That one could not be made read-only.
    pub fn composed(&self, root: &str, host: &Path) -> io::Result<()> {
        let writable = {
            let mut known = self.known();
            // The next composition links each file anew.
            known.placing.clear();
            std::mem::take(&mut known.writable)
        };
        // What was placed elsewhere, as a sandbox's copy is, is no part of
        // it.
        for (path, mode) in writable {
            if within(&path, host) {
                fs::set_permissions(&path, fs::Permissions::from_mode(mode & !0o222))?;
            }
        }
        self.known()
            .sealed
            .push((root.to_owned(), host.to_path_buf()));
        Ok(())
    }

    /// Seals each sealed composition again, as it was placed again from the
    /// store as copies, which are writable (see `design/additive.md`, F1).
    ///
    /// # Errors
    ///
    /// That one could not be made read-only.
    pub fn reseal(&self) -> io::Result<()> {
        for (_, host) in self.sealed() {
            if present(&host) {
                seal(&host)?;
            }
        }
        Ok(())
    }

    /// The sealed compositions, each as GCSL code names it and on the host:
    /// what every command binds read-only.
    #[must_use]
    pub fn sealed(&self) -> Vec<(String, PathBuf)> {
        self.known().sealed.clone()
    }

    /// Makes the sandbox of the directory at `lower` on the host whose root
    /// is `root`, as GCSL code names it, at `upper` on the host, as an
    /// overlay if nothing can change `lower`: whether it did.
    ///
    /// # Errors
    ///
    /// That its work directory could not be made.
    pub fn make_sandbox(&self, root: &str, lower: &Path, upper: &Path) -> io::Result<bool> {
        if !self.is_unchanging(lower) {
            return Ok(false);
        }
        let mut known = self.known();
        let work = self.scratch.join(format!("overlay-work-{}", known.next));
        known.next += 1;
        fs::create_dir_all(&work)?;
        known.overlays.push(Overlay {
            root: root.to_owned(),
            lower: lower.to_path_buf(),
            upper: upper.to_path_buf(),
            work,
        });
        Ok(true)
    }

    /// The overlay sandbox whose root is `root`, as GCSL code names it.
    #[must_use]
    pub fn overlay(&self, root: &str) -> Option<Overlay> {
        self.known()
            .overlays
            .iter()
            .find(|overlay| overlay.root == root)
            .cloned()
    }

    /// Where `path`, as GCSL code names it, is on the host if it is in an
    /// overlay sandbox (see [Compositions and
    /// sandboxes](#compositions-and-sandboxes)); `None` if it is in none.
    ///
    /// # Errors
    ///
    /// That a directory both layers have could not be merged.
    pub fn resolve(&self, path: &str) -> Option<io::Result<PathBuf>> {
        let overlay = self.known().overlays.iter().find_map(|overlay| {
            let rest = path.strip_prefix(&overlay.root)?;
            if rest.is_empty() || rest.starts_with('/') {
                Some((overlay.clone(), rest.trim_start_matches('/').to_owned()))
            } else {
                None
            }
        })?;
        let (overlay, rest) = overlay;
        if rest.is_empty() {
            return Some(Ok(overlay.upper));
        }
        Some(self.merged(&overlay, &rest))
    }

    /// Where `rest`, a path relative to the root of `overlay`, is on the host,
    /// upper over lower.
    fn merged(&self, overlay: &Overlay, rest: &str) -> io::Result<PathBuf> {
        let mut upper = overlay.upper.clone();
        let mut lower = Some(overlay.lower.clone());
        let parts: Vec<&str> = rest.split('/').filter(|part| !part.is_empty()).collect();
        for (at, part) in parts.iter().enumerate() {
            upper.push(part);
            lower = lower.map(|lower| lower.join(part));
            let last = at + 1 == parts.len();
            if whiteout(&upper) {
                // Hidden in both: a path that is nowhere.
                return Ok(self.scratch.join("absent").join(rest));
            }
            if is_dir(&upper) && opaque(&upper) {
                lower = None;
            }
            if !last && present(&upper) && !is_dir(&upper) {
                lower = None;
            }
        }
        let lower = lower.filter(|lower| present(lower));
        match lower {
            Some(lower) if !present(&upper) => Ok(lower),
            Some(lower) if is_dir(&upper) && is_dir(&lower) => self.merge_dirs(&upper, &lower),
            _ => Ok(upper),
        }
    }

    /// A new tree of hard links of the directory `upper` merged over `lower`.
    fn merge_dirs(&self, upper: &Path, lower: &Path) -> io::Result<PathBuf> {
        let at = {
            let mut known = self.known();
            let at = self.scratch.join(format!("merged-{}", known.next));
            known.next += 1;
            at
        };
        merge_into(upper, Some(lower), &at)?;
        Ok(at)
    }
}

/// Makes at `at` the tree of `upper` over `lower`, if there is one, of hard
/// links, each whiteout hiding what is below it.
fn merge_into(upper: &Path, lower: Option<&Path>, at: &Path) -> io::Result<()> {
    fs::create_dir_all(at)?;
    let lower = lower.filter(|_| !opaque(upper));
    let mut names: Vec<std::ffi::OsString> = Vec::new();
    for dir in std::iter::once(upper).chain(lower) {
        if let Ok(entries) = fs::read_dir(dir) {
            for entry in entries {
                let name = entry?.file_name();
                if !names.contains(&name) {
                    names.push(name);
                }
            }
        }
    }
    names.sort();
    for name in names {
        let top = upper.join(&name);
        let below = lower.map(|lower| lower.join(&name));
        let target = at.join(&name);
        if present(&top) {
            if whiteout(&top) {
                continue;
            }
            if is_dir(&top) {
                let below = below.filter(|below| is_dir(below));
                merge_into(&top, below.as_deref(), &target)?;
            } else {
                link_entry(&top, &target)?;
            }
        } else if let Some(below) = below {
            if is_dir(&below) {
                merge_into(&below, None, &target)?;
            } else {
                link_entry(&below, &target)?;
            }
        }
    }
    Ok(())
}

/// Places the file or link at `source` at `target`: a link as a link, a
/// file as a hard link, or a copy if it cannot be linked.
fn link_entry(source: &Path, target: &Path) -> io::Result<()> {
    let meta = fs::symlink_metadata(source)?;
    if meta.file_type().is_symlink() {
        return std::os::unix::fs::symlink(fs::read_link(source)?, target);
    }
    if fs::hard_link(source, target).is_ok() {
        return Ok(());
    }
    fs::copy(source, target).map(|_| ())
}
