//! Materialised trees: the store's trees made once as directories, for
//! steps to mount.
//!
//! # Materialised trees
//!
//! A step is given, and composes, whole trees, such as a toolchain of tens
//! of thousands of files. Rather than link each file into each step's
//! staging directory, the store makes a tree's directory once, in `trees/`,
//! of hard links to its blobs and their copies, as
//! [`Store::materialise_tree`] makes one, and a step mounts it read-only
//! (see `design/libraries.md`, L6a). Nothing changes it, so every step of
//! every build can share it. Each is made in `tmp/` and renamed into place
//! whole, so that one there is complete; a process that loses a race to
//! make one drops its own.
//!
//! A tree is materialised as an *instance*: its digest, and for some of its
//! contents a *base*, the copy its first file of those contents and mode
//! links, its next the next copy and on. A composition that mounts a tree
//! whose contents it holds already, elsewhere, mounts an instance whose
//! files link copies it does not, so that no two of its files are one (see
//! [Compositions and sandboxes](crate::layers#compositions-and-sandboxes)).
//! The instance of no bases, the tree's own, is named by the tree's digest;
//! any other by it and a digest of its bases.
//!
//! An eviction removes every materialised tree before it removes a blob,
//! since each holds links to blobs ([`Store::forget_materialised`]).

use std::collections::HashMap;
use std::fmt::Write as _;
use std::fs;
use std::io;
use std::path::{Component, Path, PathBuf};
use std::sync::{Arc, Mutex, PoisonError};

use super::{Digest, FileEntry, Store, TreeEntry};

/// An instance's bases: each content and mode, and the copy its first file
/// links, sorted.
type Bases = Arc<[(FileEntry, u32)]>;

/// Each file of an instance, by its path in it: its contents and mode, and
/// the copy it links.
type Origins = HashMap<PathBuf, (FileEntry, u32)>;

/// What a process knows of the store's materialised trees (see
/// [Materialised trees](#materialised-trees)).
#[derive(Debug, Default)]
pub(super) struct Materialised {
    /// Each tree's number of files and symbolic links, at any depth, by its
    /// digest.
    files: HashMap<Digest, u64>,
    /// Of each tree asked of, by its digest, how many of its files have each
    /// content and mode, at any depth.
    blobs: HashMap<Digest, Arc<HashMap<FileEntry, u32>>>,
    /// Each instance this process made or found, by its name: its tree and
    /// bases.
    instances: HashMap<String, (Digest, Bases)>,
    /// Of each instance whose files were asked of, by its name, each file's
    /// contents and mode and the copy it links, by its path in it.
    origins: HashMap<String, Arc<Origins>>,
    /// A lock for each instance this process makes, by its name, so that
    /// two of its threads never make one twice.
    making: HashMap<String, Arc<Mutex<()>>>,
}

/// The name of the instance of the tree `digest` of the bases `bases` (see
/// [Materialised trees](#materialised-trees)).
fn instance_name(digest: &Digest, bases: &[(FileEntry, u32)]) -> String {
    if bases.is_empty() {
        return digest.hex();
    }
    let mut encoded = String::new();
    for (entry, base) in bases {
        let mode = if entry.executable { "x" } else { "" };
        let _ = writeln!(encoded, "{}{mode} {base}", entry.digest);
    }
    let hex = Digest::of(encoded.as_bytes()).hex();
    format!("{}-{}", digest.hex(), &hex[..16])
}

impl Store {
    /// What this process knows of the materialised trees, even if a thread
    /// panicked holding it.
    fn materialised_known(&self) -> std::sync::MutexGuard<'_, Materialised> {
        self.materialised
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
    }

    /// Where the store's materialised trees are.
    #[must_use]
    pub fn trees_root(&self) -> PathBuf {
        self.root.join("trees")
    }

    /// The number of files and symbolic links of the tree `digest`, at any
    /// depth.
    ///
    /// # Errors
    ///
    /// That the store lacks a tree of it.
    pub fn file_count(&self, digest: &Digest) -> io::Result<u64> {
        if let Some(count) = self.materialised_known().files.get(digest) {
            return Ok(*count);
        }
        let mut count = 0;
        for entry in self.read_tree(digest)?.entries {
            count += match entry {
                TreeEntry::Dir { tree, .. } => self.file_count(&tree)?,
                TreeEntry::File { .. } | TreeEntry::Link { .. } => 1,
            };
        }
        self.materialised_known().files.insert(*digest, count);
        Ok(count)
    }

    /// How many files of the tree `digest`, at any depth, have each content
    /// and mode.
    ///
    /// # Errors
    ///
    /// That the store lacks a tree of it.
    pub fn blob_counts(&self, digest: &Digest) -> io::Result<Arc<HashMap<FileEntry, u32>>> {
        if let Some(counts) = self.materialised_known().blobs.get(digest) {
            return Ok(Arc::clone(counts));
        }
        let mut counts = HashMap::new();
        self.count_blobs(digest, &mut counts)?;
        let counts = Arc::new(counts);
        self.materialised_known()
            .blobs
            .insert(*digest, Arc::clone(&counts));
        Ok(counts)
    }

    /// Adds to `counts` how many files of the tree `digest` have each
    /// content and mode.
    fn count_blobs(&self, digest: &Digest, counts: &mut HashMap<FileEntry, u32>) -> io::Result<()> {
        for entry in self.read_tree(digest)?.entries {
            match entry {
                TreeEntry::File { file, .. } => *counts.entry(file).or_default() += 1,
                TreeEntry::Dir { tree, .. } => self.count_blobs(&tree, counts)?,
                TreeEntry::Link { .. } => {}
            }
        }
        Ok(())
    }

    /// The directory of the instance of the tree `digest` of the bases
    /// `bases`, each a content and mode and the copy its first file links,
    /// sorted, made if it is not there (see [Materialised
    /// trees](#materialised-trees)). Nothing may change it.
    ///
    /// # Errors
    ///
    /// That the store lacks something of the tree, or it cannot be made.
    pub fn materialised(&self, digest: &Digest, bases: &[(FileEntry, u32)]) -> io::Result<PathBuf> {
        let name = instance_name(digest, bases);
        let at = self.trees_root().join(&name);
        let making = {
            let mut known = self.materialised_known();
            known
                .instances
                .entry(name.clone())
                .or_insert_with(|| (*digest, Arc::from(bases)));
            Arc::clone(known.making.entry(name).or_default())
        };
        if at.is_dir() {
            return Ok(at);
        }
        let _making = making.lock().unwrap_or_else(PoisonError::into_inner);
        if at.is_dir() {
            return Ok(at);
        }
        fs::create_dir_all(self.trees_root())?;
        let temp = self.temp_path();
        let mut linked: HashMap<FileEntry, u32> = bases.iter().copied().collect();
        if let Err(error) = self.materialise_tree_into(digest, &temp, &mut linked) {
            let _ = fs::remove_dir_all(&temp);
            return Err(error);
        }
        match fs::rename(&temp, &at) {
            Ok(()) => Ok(at),
            // Another process made it first, which is as good.
            Err(_) if at.is_dir() => fs::remove_dir_all(&temp).map(|()| at),
            Err(error) => {
                let _ = fs::remove_dir_all(&temp);
                Err(error)
            }
        }
    }

    /// The instance a path under the materialised trees is in, by its name,
    /// and its path in it.
    fn instance_of<'a>(&self, host: &'a Path) -> Option<(String, &'a Path)> {
        let within = host.strip_prefix(self.trees_root()).ok()?;
        let mut components = within.components();
        let Some(Component::Normal(name)) = components.next() else {
            return None;
        };
        Some((name.to_str()?.to_owned(), components.as_path()))
    }

    /// The tree and bases of the instance named `name`, if this process
    /// knows them: any instance of no bases, and those it made or found.
    fn instance(&self, name: &str) -> Option<(Digest, Bases)> {
        if let Some(digest) = Digest::from_hex(name) {
            return Some((digest, Arc::from(Vec::new())));
        }
        self.materialised_known().instances.get(name).cloned()
    }

    /// The digest of the directory at `host`, if it is a materialised tree
    /// or a directory in one.
    #[must_use]
    pub fn tree_in(&self, host: &Path) -> Option<Digest> {
        let (name, rest) = self.instance_of(host)?;
        let (mut digest, _) = self.instance(&name)?;
        for part in rest.components() {
            let Component::Normal(part) = part else {
                return None;
            };
            let tree = self.read_tree(&digest).ok()?;
            digest = tree.entries.iter().find_map(|entry| match entry {
                TreeEntry::Dir { name, tree } if name.as_str() == part => Some(*tree),
                _ => None,
            })?;
        }
        Some(digest)
    }

    /// The contents and mode of the file at `host`, and which of its blob
    /// and copies it links, 0 the blob, if it is a file of a materialised
    /// tree.
    #[must_use]
    pub fn file_in(&self, host: &Path) -> Option<(FileEntry, u32)> {
        let (name, rest) = self.instance_of(host)?;
        let known = self.materialised_known().origins.get(&name).cloned();
        if let Some(origins) = known {
            return origins.get(rest).copied();
        }
        let (digest, bases) = self.instance(&name)?;
        let mut origins = HashMap::new();
        let mut linked: HashMap<FileEntry, u32> = bases.iter().copied().collect();
        self.origins_of(&digest, Path::new(""), (&mut linked, &mut origins))
            .ok()?;
        let found = origins.get(rest).copied();
        self.materialised_known()
            .origins
            .insert(name, Arc::new(origins));
        found
    }

    /// Adds to `origins` each file of the tree `digest`, at `at` in an
    /// instance, by its path, with its contents and mode and the copy it
    /// links, `linked` how many of each its instance has linked before, as
    /// [`Store::materialise_tree`] numbers them.
    fn origins_of(
        &self,
        digest: &Digest,
        at: &Path,
        (linked, origins): (&mut HashMap<FileEntry, u32>, &mut Origins),
    ) -> io::Result<()> {
        for entry in self.read_tree(digest)?.entries {
            match entry {
                TreeEntry::File { name, file } => {
                    let count = linked.entry(file).or_default();
                    origins.insert(at.join(name), (file, *count));
                    *count += 1;
                }
                TreeEntry::Dir { name, tree } => {
                    self.origins_of(&tree, &at.join(name), (&mut *linked, &mut *origins))?;
                }
                TreeEntry::Link { .. } => {}
            }
        }
        Ok(())
    }

    /// Removes every materialised tree, as an eviction does before it
    /// removes a blob.
    ///
    /// # Errors
    ///
    /// That they cannot be removed.
    pub fn forget_materialised(&self) -> io::Result<()> {
        match fs::remove_dir_all(self.trees_root()) {
            Err(error) if error.kind() != io::ErrorKind::NotFound => Err(error),
            _ => Ok(()),
        }
    }
}
