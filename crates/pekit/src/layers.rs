//! Layers: a step's compositions and overlay sandboxes, on the host.
//!
//! # Compositions and sandboxes
//!
//! A step composes directories (`fs.compose_dir`) and runs commands in
//! sandboxes of them (`Dir.sandbox`) as plain GCSL does, but without copying
//! (see `design/libraries.md`, L6a). Its sandbox ([`crate::sandbox::StepSandbox`])
//! answers both itself:
//!
//! - **A composition** mounts each directory it places whole that is a tree
//!   of the store of [`MOUNT_FILES`] files or more, materialised (see
//!   [Materialised trees](crate::store::trees#materialised-trees)), on an
//!   empty directory of its own, its *mount point* ([`Layers::place_tree`]):
//!   every command sees the tree there, read-only, and GCSL finds what it
//!   holds there too. It hard-links each other file it places that nothing
//!   can change, a staged input's, a materialised tree's or another
//!   composition's, and copies any other ([`Layers::place_file`]). A content
//!   it places twice, as a toolchain's runtime is placed both on its own and
//!   within the toolchain, is a link to the store's blob once and to a copy
//!   the store keeps of it each other time, and a tree it mounts that holds
//!   a content it has already is an instance of the tree whose files of it
//!   link later copies, so that no two of its files are one, and none is
//!   copied anew. Once it is done it is *sealed*: its files made
//!   read-only on the host, and it bound read-only to every command, so that
//!   no program can change it, and nothing a store's blob through it. Its
//!   directories keep their modes, since a sandbox's merged directories show
//!   their lower's mode, and a read-only one would refuse every write, even
//!   into the sandbox's own upper layer: a build in a sandbox of the
//!   composition writes beside its source.
//! - **A sandbox** of a directory that nothing can change is an overlay
//!   ([`Layers::make_sandbox`]): its commands see the directory as their
//!   `/`, their writes kept in the sandbox's own directory, the *upper*
//!   layer, never in the directory, the *lower* one. Each tree mounted in
//!   the directory is an overlay of its own, *nested* on its mount point,
//!   whose upper layer is a directory of the step's own outputs, so that a
//!   command writes there too; but it cannot move or remove the mount
//!   point itself, which a copy would let it. A sandbox of any other
//!   directory is copied, as plain GCSL does.
//! - **A path in an overlay sandbox** is read upper over lower
//!   ([`Layers::resolve`]), as the overlay shows it to its commands: what the
//!   upper layer has, unless it is a *whiteout*, a character device of number
//!   0, which hides what the lower has; else what the lower has, unless an
//!   upper directory on the way is *opaque* (`user.overlay.opaque`), which
//!   hides all the lower has below it. A directory both layers have is merged
//!   into a new tree of hard links, a snapshot of it as it is then, as is
//!   one that holds a nested overlay's mount point. A path in a nested
//!   overlay is read from its layers. The sandbox's own root is its upper
//!   layer, which is where GCSL itself writes into it, such as the working
//!   directory it makes, and GCSL writes a path of it in the upper layer of
//!   the overlay it is in ([`Layers::write_target`]).
//! - **A step's mounts** are the directories its staged inputs and its
//!   compositions mount, each a materialised tree on a mount point
//!   ([`Mount`]), which its commands are given read-only and its paths are
//!   found in ([`Layers::mounted_path`]); what walks a directory on the
//!   host finds a mount point's contents in its tree
//!   ([`Layers::contents`]), and what puts one into the store takes the
//!   tree's digest ([`Layers::mounted`]).

use std::collections::{HashMap, HashSet};
use std::fs;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use crate::store::{Digest, FileEntry, Store};

/// How a composition places a file that nothing can change
/// ([`Layers::place_file`]).
enum Placing {
    /// As a link to it.
    Link,
    /// As a link to the copy of this number of the store's blob of these
    /// contents and mode.
    Copy(FileEntry, u32),
    /// As a copy of its own.
    Fresh,
}

/// The fewest files and symbolic links a tree of the store has that a step
/// mounts, rather than placing each of them: each mount costs every
/// program the step runs a little.
pub const MOUNT_FILES: u64 = 64;

/// A materialised tree a step mounts (see [Compositions and
/// sandboxes](#compositions-and-sandboxes)).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Mount {
    /// Its path, as GCSL code names it.
    pub logical: String,
    /// Its mount point, an empty directory on the host.
    pub point: PathBuf,
    /// The materialised tree, on the host.
    pub source: PathBuf,
    /// The tree's digest.
    pub digest: Digest,
}

/// An overlay nested in an overlay sandbox, on the mount point of a tree
/// mounted in the directory the sandbox is of.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Nested {
    /// Its path, relative to the sandbox's root.
    pub at: String,
    /// The tree, on the host: its lower layer.
    pub lower: PathBuf,
    /// Its writes, on the host: its upper layer.
    pub upper: PathBuf,
    /// The overlay's work directory, on the host's file system of `upper`.
    pub work: PathBuf,
}

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
    /// The overlays nested in it, by their paths.
    pub nested: Vec<Nested>,
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
    /// Of each blob of the store the composition being made has a link to,
    /// by its contents and mode, one more than the highest of it it links,
    /// 0 the blob and any other the copy of that number: the next it may.
    copy_next: HashMap<FileEntry, u32>,
    /// The step's mounts.
    mounts: Vec<Mount>,
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
    /// Where the store's materialised trees are, which nothing changes, if
    /// the step has a store.
    trees: Option<PathBuf>,
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
            trees: None,
            known: Mutex::new(Known::default()),
        }
    }

    /// The layers of a step whose store keeps its materialised trees at
    /// `trees`.
    #[must_use]
    pub fn with_trees(mut self, trees: PathBuf) -> Self {
        self.trees = Some(trees);
        self
    }

    /// What it knows, even if a thread panicked holding it.
    fn known(&self) -> std::sync::MutexGuard<'_, Known> {
        self.known
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Whether nothing can change the file or directory at `path` on the
    /// host: a staged input's, a materialised tree's, or a sealed
    /// composition's.
    #[must_use]
    pub fn is_unchanging(&self, path: &Path) -> bool {
        let staged =
            self.unchanging.iter().any(|dir| within(path, dir)) && !within(path, &self.changing);
        staged
            || self.in_store(path)
            || self.known().sealed.iter().any(|(_, dir)| within(path, dir))
    }

    /// Whether `path`, on the host, is in a materialised tree of the store,
    /// which nothing may change.
    #[must_use]
    pub fn in_store(&self, path: &Path) -> bool {
        self.trees.as_ref().is_some_and(|trees| within(path, trees))
    }

    /// Places the file at `source` at `target`, both on the host, for a
    /// composition: a hard link if nothing can change it, it is on the same
    /// file system and the composition has no other link to it, else a copy
    /// of its own. So two files of a composition are never one, whatever
    /// the store shares between them, as two of a tree it stages are not
    /// ([`crate::store::Store::materialise_tree`]). A file that is a link
    /// to a blob of `store`, or to a copy of one, that the composition
    /// links already is a link to another copy of the blob, which the store
    /// keeps ([`Store::link_copy`]): placed again, as each composition of a
    /// toolchain places it, it is copied no more.
    ///
    /// # Errors
    ///
    /// That it could not be placed.
    pub fn place_file(
        &self,
        source: &Path,
        target: &Path,
        store: Option<&Store>,
    ) -> io::Result<()> {
        let meta = fs::metadata(source)?;
        if self.is_unchanging(source) {
            let node = (meta.dev(), meta.ino());
            let origin =
                store.and_then(|store| store.origin(node).or_else(|| store.file_in(source)));
            let placing = self.placing(node, origin);
            let linked = match (placing, store) {
                (Placing::Link, _) => fs::hard_link(source, target).is_ok(),
                (Placing::Copy(entry, copy), Some(store)) => {
                    store.link_copy(&entry, copy, target).is_ok()
                }
                (Placing::Copy(..) | Placing::Fresh, _) => false,
            };
            if linked {
                self.note_mode(target, meta.permissions().mode());
                return Ok(());
            }
        }
        fs::copy(source, target)?;
        // A copy has its source's mode.
        self.note_mode(target, meta.permissions().mode());
        Ok(())
    }

    /// How the composition being made places a file that nothing can
    /// change, of device and inode `node`, a link to the blob or copy of
    /// the store `origin` says, if it is one.
    fn placing(&self, node: (u64, u64), origin: Option<(FileEntry, u32)>) -> Placing {
        let mut known = self.known();
        let first = known.placing.insert(node);
        let Some((entry, copy)) = origin else {
            return if first { Placing::Link } else { Placing::Fresh };
        };
        let next = known.copy_next.entry(entry).or_default();
        if first && copy >= *next {
            *next = copy + 1;
            return Placing::Link;
        }
        let Some(after) = next.checked_add(1) else {
            return Placing::Fresh;
        };
        let other = std::mem::replace(next, after);
        Placing::Copy(entry, other)
    }

    /// Places the directory at `source` at `target`, both on the host, whole,
    /// for the composition being made, if it is a tree of `store` of
    /// [`MOUNT_FILES`] files or more: mounted at `target`, `logical` as GCSL
    /// code names it, as an instance of the tree whose files link no blob or
    /// copy the composition links already (see [Compositions and
    /// sandboxes](#compositions-and-sandboxes)). `None` for any other,
    /// whose entries GCSL places.
    pub fn place_tree(
        &self,
        (source, target): (&Path, &Path),
        logical: String,
        store: &Store,
    ) -> Option<io::Result<()>> {
        let digest = store.tree_in(source)?;
        match store.file_count(&digest) {
            Ok(count) if count >= MOUNT_FILES => {}
            Ok(_) => return None,
            Err(error) => return Some(Err(error)),
        }
        Some(self.mount_composed(&digest, (target, logical), store))
    }

    /// Mounts the tree `digest` of `store` at `target`, `logical` as GCSL
    /// code names it, for the composition being made, as
    /// [`Layers::place_tree`] does.
    fn mount_composed(
        &self,
        digest: &Digest,
        (target, logical): (&Path, String),
        store: &Store,
    ) -> io::Result<()> {
        let counts = store.blob_counts(digest)?;
        let mut bases = Vec::new();
        {
            let mut known = self.known();
            for (entry, count) in counts.iter() {
                let next = known.copy_next.entry(*entry).or_default();
                if *next > 0 {
                    bases.push((*entry, *next));
                }
                *next = next.saturating_add(*count);
            }
        }
        bases.sort_by_key(|(entry, _)| (entry.digest, entry.executable));
        let source = store.materialised(digest, &bases)?;
        fs::create_dir(target)?;
        self.mount(Mount {
            logical,
            point: target.to_path_buf(),
            source,
            digest: *digest,
        });
        Ok(())
    }

    /// Notes the step's mount `mount`.
    pub fn mount(&self, mount: Mount) {
        self.known().mounts.push(mount);
    }

    /// The step's mounts, and its sealed compositions, each as GCSL code
    /// names it and on the host, each after any it is in: what every
    /// command binds read-only.
    #[must_use]
    pub fn binds(&self) -> Vec<(String, PathBuf)> {
        let known = self.known();
        let mut binds = known.sealed.clone();
        binds.extend(
            known
                .mounts
                .iter()
                .map(|mount| (mount.logical.clone(), mount.source.clone())),
        );
        binds.sort();
        binds
    }

    /// The digest of the tree mounted on each of the step's mount points,
    /// by where it is on the host.
    #[must_use]
    pub fn mounted(&self) -> HashMap<PathBuf, Digest> {
        self.known()
            .mounts
            .iter()
            .map(|mount| (mount.point.clone(), mount.digest))
            .collect()
    }

    /// Where the contents of the directory at `host` are, if it is one of
    /// the step's mount points: its tree.
    #[must_use]
    pub fn contents(&self, host: &Path) -> Option<PathBuf> {
        self.known()
            .mounts
            .iter()
            .find(|mount| mount.point == host)
            .map(|mount| mount.source.clone())
    }

    /// Where `path`, as GCSL code names it, is on the host if it is in one
    /// of the step's mounts, the innermost.
    #[must_use]
    pub fn mounted_path(&self, path: &str) -> Option<PathBuf> {
        let known = self.known();
        let (mount, rest) = known
            .mounts
            .iter()
            .filter_map(|mount| Some((mount, inside(path, &mount.logical)?)))
            .max_by_key(|(mount, _)| mount.logical.len())?;
        Some(if rest.is_empty() {
            mount.source.clone()
        } else {
            mount.source.join(rest)
        })
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
            known.copy_next.clear();
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
    /// overlay if nothing can change `lower`: whether it did. Each tree
    /// mounted in `lower` is an overlay nested in it, whose upper layer
    /// `fresh` makes.
    ///
    /// # Errors
    ///
    /// That a work directory or an upper layer could not be made.
    pub fn make_sandbox(
        &self,
        root: &str,
        (lower, upper): (&Path, &Path),
        fresh: &dyn Fn() -> io::Result<PathBuf>,
    ) -> io::Result<bool> {
        if !self.is_unchanging(lower) {
            return Ok(false);
        }
        let mut mounted: Vec<(String, PathBuf)> = self
            .known()
            .mounts
            .iter()
            .filter_map(|mount| {
                let at = mount.point.strip_prefix(lower).ok()?.to_str()?;
                (!at.is_empty()).then(|| (at.to_owned(), mount.source.clone()))
            })
            .collect();
        mounted.sort();
        let mut nested = Vec::new();
        for (at, tree) in mounted {
            nested.push(Nested {
                at,
                lower: tree,
                upper: fresh()?,
                work: self.work_dir()?,
            });
        }
        let work = self.work_dir()?;
        self.known().overlays.push(Overlay {
            root: root.to_owned(),
            lower: lower.to_path_buf(),
            upper: upper.to_path_buf(),
            work,
            nested,
        });
        Ok(true)
    }

    /// A new overlay work directory, on the host's file system of the
    /// step's own outputs.
    fn work_dir(&self) -> io::Result<PathBuf> {
        let work = {
            let mut known = self.known();
            let work = self.scratch.join(format!("overlay-work-{}", known.next));
            known.next += 1;
            work
        };
        fs::create_dir_all(&work)?;
        Ok(work)
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

    /// The overlay sandbox `path`, as GCSL code names it, is in, and its
    /// path relative to the sandbox's root.
    fn overlay_of(&self, path: &str) -> Option<(Overlay, String)> {
        self.known().overlays.iter().find_map(|overlay| {
            let rest = inside(path, &overlay.root)?;
            Some((overlay.clone(), rest.to_owned()))
        })
    }

    /// Where `path`, as GCSL code names it, is on the host if it is in an
    /// overlay sandbox (see [Compositions and
    /// sandboxes](#compositions-and-sandboxes)); `None` if it is in none.
    ///
    /// # Errors
    ///
    /// That a directory both layers have could not be merged.
    pub fn resolve(&self, path: &str) -> Option<io::Result<PathBuf>> {
        let (overlay, rest) = self.overlay_of(path)?;
        if rest.is_empty() {
            return Some(Ok(overlay.upper));
        }
        Some(match nested_of(&overlay, &rest) {
            Some((nested, inner)) => self.merged((&nested.upper, &nested.lower), (inner, &[])),
            None => self.merged((&overlay.upper, &overlay.lower), (&rest, &overlay.nested)),
        })
    }

    /// Where GCSL writes `path`, as GCSL code names it, if it is in an
    /// overlay sandbox: in the upper layer of the overlay it is in, its
    /// directory made there if the sandbox shows one. `None` if it is in
    /// none.
    ///
    /// # Errors
    ///
    /// That its directory could not be made.
    pub fn write_target(&self, path: &str) -> Option<io::Result<PathBuf>> {
        let (overlay, rest) = self.overlay_of(path)?;
        if rest.is_empty() {
            return Some(Ok(overlay.upper));
        }
        let (upper, inner) = match nested_of(&overlay, &rest) {
            Some((nested, inner)) => (nested.upper.clone(), inner.to_owned()),
            None => (overlay.upper.clone(), rest.clone()),
        };
        if let Some((parent, _)) = inner.rsplit_once('/')
            && !is_dir(&upper.join(parent))
            && let Some((shown, _)) = path.rsplit_once('/')
            && self
                .resolve(shown)
                .is_some_and(|dir| dir.is_ok_and(|dir| is_dir(&dir)))
            && let Err(error) = fs::create_dir_all(upper.join(parent))
        {
            return Some(Err(error));
        }
        Some(Ok(upper.join(inner)))
    }

    /// Where `rest`, a path relative to the root of an overlay of the layers
    /// `upper` over `lower`, is on the host, upper over lower, the overlays
    /// `nested` in it (see [Compositions and
    /// sandboxes](#compositions-and-sandboxes)).
    fn merged(
        &self,
        (upper, lower): (&Path, &Path),
        (rest, nested): (&str, &[Nested]),
    ) -> io::Result<PathBuf> {
        let mut upper = upper.to_path_buf();
        let mut lower = Some(lower.to_path_buf());
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
        // A directory that holds a nested overlay's mount point is merged
        // with what that overlay shows.
        let holds = nested
            .iter()
            .any(|nested| inside(&nested.at, rest).is_some_and(|inner| !inner.is_empty()));
        let lower_dir = lower.as_deref().filter(|lower| is_dir(lower));
        if holds && (is_dir(&upper) || (!present(&upper) && lower_dir.is_some())) {
            return self.merge_dirs((&upper, lower_dir), (rest, nested));
        }
        match lower {
            Some(lower) if !present(&upper) => Ok(lower),
            Some(lower) if is_dir(&upper) && is_dir(&lower) => {
                if empty_dir(&upper) {
                    Ok(lower)
                } else {
                    self.merge_dirs((&upper, Some(&lower)), (rest, nested))
                }
            }
            _ => Ok(upper),
        }
    }

    /// A new tree of hard links of the directory `upper` merged over
    /// `lower`, at `rest` relative to its overlay's root, the overlays
    /// `nested` in it.
    fn merge_dirs(
        &self,
        (upper, lower): (&Path, Option<&Path>),
        (rest, nested): (&str, &[Nested]),
    ) -> io::Result<PathBuf> {
        let at = {
            let mut known = self.known();
            let at = self.scratch.join(format!("merged-{}", known.next));
            known.next += 1;
            at
        };
        merge_into((upper, lower), &at, (rest, nested))?;
        Ok(at)
    }
}

/// `path` under the logical root `root`, if it is it or under it: the rest,
/// relative, `""` for the root itself.
fn inside<'a>(path: &'a str, root: &str) -> Option<&'a str> {
    if root.is_empty() {
        return Some(path);
    }
    let rest = path.strip_prefix(root)?;
    if rest.is_empty() {
        return Some("");
    }
    rest.strip_prefix('/')
}

/// The overlay nested in `overlay` that `rest`, a path relative to its
/// root, is in, the innermost, and its path in it.
fn nested_of<'a>(overlay: &'a Overlay, rest: &'a str) -> Option<(&'a Nested, &'a str)> {
    overlay
        .nested
        .iter()
        .filter_map(|nested| Some((nested, inside(rest, &nested.at)?)))
        .max_by_key(|(nested, _)| nested.at.len())
}

/// Whether the directory at `path` holds nothing.
fn empty_dir(path: &Path) -> bool {
    fs::read_dir(path).is_ok_and(|mut entries| entries.next().is_none())
}

/// Makes at `at` the tree of `upper` over `lower`, if there is one, of hard
/// links, each whiteout hiding what is below it, `rest` its path relative
/// to its overlay's root, each overlay of `nested` merged at its mount
/// point.
fn merge_into(
    (upper, lower): (&Path, Option<&Path>),
    at: &Path,
    (rest, nested): (&str, &[Nested]),
) -> io::Result<()> {
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
        let inner = if rest.is_empty() {
            name.to_string_lossy().into_owned()
        } else {
            format!("{rest}/{}", name.to_string_lossy())
        };
        let top = upper.join(&name);
        let below = lower.map(|lower| lower.join(&name));
        let target = at.join(&name);
        if let Some(mounted) = nested.iter().find(|nested| nested.at == inner) {
            merge_into((&mounted.upper, Some(&mounted.lower)), &target, ("", &[]))?;
        } else if present(&top) {
            if whiteout(&top) {
                continue;
            }
            if is_dir(&top) {
                let below = below.filter(|below| is_dir(below));
                merge_into((&top, below.as_deref()), &target, (&inner, nested))?;
            } else {
                link_entry(&top, &target)?;
            }
        } else if let Some(below) = below {
            if is_dir(&below) {
                merge_into((&below, None), &target, (&inner, nested))?;
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
