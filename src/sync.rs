//! Keeping a build's copy of its source up to date.
//!
//! A build's work directory keeps its copy of the source between builds, so
//! that what a build leaves there, such as object files, lets the next one
//! redo only what changed. Syncing rewrites only the files whose content or
//! execute bit changed, or that the build changed itself, so that every other
//! file keeps its modification time. It deletes only what the last sync
//! placed and the source no longer has, never what a build made. A record
//! beside the copy says what each sync placed.
//!
//! The record also keeps each source file's size and modification time when
//! it was hashed, so that a file whose size and time are unchanged is not
//! read again, as Git's index does.

use std::collections::{HashMap, HashSet};
use std::ffi::OsStr;
use std::fs::{self, Metadata};
use std::io;
use std::os::unix::ffi::{OsStrExt, OsStringExt};
use std::os::unix::fs::{MetadataExt, PermissionsExt, symlink};
use std::path::{Path, PathBuf};

use anyhow::{Context, Result, bail};
use sha2::{Digest, Sha256};

use crate::source::hash_file;

/// What a sync can place.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Kind {
    File,
    /// A file with an execute bit set.
    Exec,
    Symlink,
}

impl Kind {
    fn code(self) -> u8 {
        match self {
            Kind::File => b'f',
            Kind::Exec => b'x',
            Kind::Symlink => b'l',
        }
    }
}

/// A file's size and modification time: as much as can tell, without
/// reading it, that it has not changed.
#[derive(Clone, Copy, PartialEq, Eq, Default)]
struct Stamp {
    size: u64,
    seconds: i64,
    nanoseconds: i64,
}

impl Stamp {
    fn of(meta: &Metadata) -> Self {
        Stamp {
            size: meta.size(),
            seconds: meta.mtime(),
            nanoseconds: meta.mtime_nsec(),
        }
    }
}

/// A file or symlink of a source tree.
#[derive(Clone)]
struct Entry {
    kind: Kind,
    /// A file's SHA-256 in lower-case hex, or a symlink's target.
    content: Vec<u8>,
    /// The source's file when it was hashed.
    origin: Stamp,
    /// The copy as the sync left it.
    copy: Stamp,
}

/// What the last sync into a work directory placed, from which source.
#[derive(Default)]
pub struct Record {
    /// The source synced from, as [`Tree::scan`] was told it.
    source: String,
    entries: HashMap<PathBuf, Entry>,
}

/// The first field of a record, which says its format.
const FORMAT: &[u8] = b"pekit-sync 1";

impl Record {
    /// The record at `path`, or an empty one if there is none.
    pub fn read(path: &Path) -> Result<Self> {
        let bytes = match fs::read(path) {
            Ok(bytes) => bytes,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(Self::default()),
            Err(error) => return Err(error).with_context(|| path.display().to_string()),
        };
        Self::parse(&bytes).with_context(|| {
            format!(
                "{}: not a record pekit can read; run with --rebuild",
                path.display()
            )
        })
    }

    /// A record's fields are each ended by a NUL, which neither a path nor
    /// a symlink's target can hold: the format and source, then nine for
    /// each entry.
    fn parse(bytes: &[u8]) -> Result<Self> {
        let mut fields = bytes.split(|&byte| byte == 0);
        if fields.next() != Some(FORMAT) {
            bail!("unknown format");
        }
        let source = text(fields.next())?.to_owned();
        let fields: Vec<&[u8]> = fields.collect();
        // The last field's NUL leaves an empty field after it.
        let Some((&[], fields)) = fields.split_last() else {
            bail!("truncated");
        };
        if fields.len() % 9 != 0 {
            bail!("truncated");
        }
        let mut entries = HashMap::new();
        for entry in fields.chunks(9) {
            let kind = match entry[1] {
                b"f" => Kind::File,
                b"x" => Kind::Exec,
                b"l" => Kind::Symlink,
                _ => bail!("unknown kind"),
            };
            let stamp = |at: usize| -> Result<Stamp> {
                Ok(Stamp {
                    size: text(Some(entry[at]))?.parse()?,
                    seconds: text(Some(entry[at + 1]))?.parse()?,
                    nanoseconds: text(Some(entry[at + 2]))?.parse()?,
                })
            };
            entries.insert(
                PathBuf::from(OsStr::from_bytes(entry[0])),
                Entry {
                    kind,
                    content: entry[2].to_vec(),
                    origin: stamp(3)?,
                    copy: stamp(6)?,
                },
            );
        }
        Ok(Record { source, entries })
    }

    /// Writes the record to `path`, replacing it whole.
    pub fn write(&self, path: &Path) -> Result<()> {
        let mut bytes = Vec::new();
        let mut field = |value: &[u8]| {
            bytes.extend_from_slice(value);
            bytes.push(0);
        };
        field(FORMAT);
        field(self.source.as_bytes());
        let mut paths: Vec<&PathBuf> = self.entries.keys().collect();
        paths.sort();
        for path in paths {
            let entry = &self.entries[path];
            field(path.as_os_str().as_bytes());
            field(&[entry.kind.code()]);
            field(&entry.content);
            for stamp in [entry.origin, entry.copy] {
                field(stamp.size.to_string().as_bytes());
                field(stamp.seconds.to_string().as_bytes());
                field(stamp.nanoseconds.to_string().as_bytes());
            }
        }
        let dir = path.parent().unwrap_or(Path::new("."));
        let pending = tempfile::NamedTempFile::new_in(dir)?;
        fs::write(pending.path(), bytes).with_context(|| pending.path().display().to_string())?;
        pending
            .persist(path)
            .with_context(|| path.display().to_string())?;
        Ok(())
    }
}

fn text(field: Option<&[u8]>) -> Result<&str> {
    match field {
        Some(field) => Ok(std::str::from_utf8(field)?),
        None => bail!("truncated"),
    }
}

/// A source tree, read: each file and symlink, with its content.
pub struct Tree {
    dir: PathBuf,
    source: String,
    /// In path order.
    entries: Vec<(PathBuf, Entry)>,
}

impl Tree {
    /// Reads `files`, paths relative to the source tree at `dir`, as
    /// `source`, which names where the tree came from. A file whose size and
    /// modification time are those `previous` recorded, from the same
    /// source, is not read again. A file that is not there is left out.
    pub fn scan(
        dir: &Path,
        mut files: Vec<PathBuf>,
        source: &str,
        previous: &Record,
    ) -> Result<Self> {
        files.sort();
        files.dedup();
        let mut entries = Vec::new();
        for path in files {
            let from = dir.join(&path);
            let meta = match fs::symlink_metadata(&from) {
                Ok(meta) => meta,
                // Tracked by Git, but deleted and not yet committed.
                Err(error) if error.kind() == io::ErrorKind::NotFound => continue,
                Err(error) => return Err(error).with_context(|| from.display().to_string()),
            };
            let origin = Stamp::of(&meta);
            let (kind, content) = if meta.file_type().is_symlink() {
                let target = fs::read_link(&from).with_context(|| from.display().to_string())?;
                (Kind::Symlink, target.into_os_string().into_vec())
            } else if meta.is_file() {
                let kind = match meta.permissions().mode() & 0o111 {
                    0 => Kind::File,
                    _ => Kind::Exec,
                };
                let content = match previous.entries.get(&path) {
                    Some(old)
                        if previous.source == source
                            && old.kind != Kind::Symlink
                            && old.origin == origin =>
                    {
                        old.content.clone()
                    }
                    _ => hash_file(&from)?.into_bytes(),
                };
                (kind, content)
            } else {
                bail!("{}: not a file or symlink", from.display());
            };
            let entry = Entry {
                kind,
                content,
                origin,
                copy: Stamp::default(),
            };
            entries.push((path, entry));
        }
        Ok(Tree {
            dir: dir.to_path_buf(),
            source: source.to_owned(),
            entries,
        })
    }

    /// The SHA-256 of the tree's paths, kinds and contents, in lower-case
    /// hex: the same for the same files, whenever and wherever they are.
    pub fn digest(&self) -> String {
        let mut hasher = Sha256::new();
        for (path, entry) in &self.entries {
            hasher.update(path.as_os_str().as_bytes());
            hasher.update([0, entry.kind.code(), 0]);
            hasher.update(&entry.content);
            hasher.update([0]);
        }
        hex(&hasher.finalize())
    }

    /// The newest modification time of the tree's files and symlinks, in
    /// seconds since the epoch.
    pub fn newest(&self) -> u64 {
        self.entries
            .iter()
            .map(|(_, entry)| u64::try_from(entry.origin.seconds).unwrap_or(0))
            .max()
            .unwrap_or(0)
    }

    /// Makes the copy at `into`, which `previous` says the last sync left,
    /// the same as the tree, and returns what this sync placed.
    pub fn sync(&self, into: &Path, previous: &Record) -> Result<Record> {
        fs::create_dir_all(into).with_context(|| into.display().to_string())?;
        let wanted: HashSet<&PathBuf> = self.entries.iter().map(|(path, _)| path).collect();
        for path in previous.entries.keys() {
            if !wanted.contains(path) {
                remove(&into.join(path))?;
            }
        }
        let mut entries = HashMap::new();
        for (path, entry) in &self.entries {
            let to = into.join(path);
            let copy = match previous.entries.get(path) {
                Some(old)
                    if old.kind == entry.kind
                        && old.content == entry.content
                        && unchanged(&to, old)? =>
                {
                    old.copy
                }
                _ => place(&self.dir.join(path), &to, entry)?,
            };
            entries.insert(
                path.clone(),
                Entry {
                    copy,
                    ..entry.clone()
                },
            );
        }
        Ok(Record {
            source: self.source.clone(),
            entries,
        })
    }
}

/// Whether the copy at `to` is as the sync that recorded `old` left it.
fn unchanged(to: &Path, old: &Entry) -> Result<bool> {
    let meta = match fs::symlink_metadata(to) {
        Ok(meta) => meta,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(false),
        Err(error) => return Err(error).with_context(|| to.display().to_string()),
    };
    if old.kind == Kind::Symlink {
        return Ok(meta.file_type().is_symlink()
            && fs::read_link(to)?.as_os_str().as_bytes() == old.content);
    }
    Ok(meta.is_file() && Stamp::of(&meta) == old.copy)
}

/// Copies `entry`, the file or symlink at `from`, to `to`, replacing
/// whatever is there, and returns the copy's stamp.
fn place(from: &Path, to: &Path, entry: &Entry) -> Result<Stamp> {
    remove(to)?;
    if let Some(parent) = to.parent() {
        fs::create_dir_all(parent).with_context(|| {
            format!(
                "{}: cannot make it a directory, perhaps because the build made a file there; run with --rebuild",
                parent.display()
            )
        })?;
    }
    if entry.kind == Kind::Symlink {
        symlink(OsStr::from_bytes(&entry.content), to).with_context(|| to.display().to_string())?;
    } else {
        // Permissions are copied too, which keeps the execute bit.
        fs::copy(from, to).with_context(|| format!("{} to {}", from.display(), to.display()))?;
    }
    let meta = fs::symlink_metadata(to).with_context(|| to.display().to_string())?;
    Ok(Stamp::of(&meta))
}

/// Removes the file, symlink or directory at `path`, if there is one.
fn remove(path: &Path) -> Result<()> {
    let removed = match fs::symlink_metadata(path) {
        Ok(meta) if meta.is_dir() => fs::remove_dir_all(path),
        Ok(_) => fs::remove_file(path),
        Err(error) => Err(error),
    };
    match removed {
        Err(error) if error.kind() != io::ErrorKind::NotFound => {
            Err(error).with_context(|| path.display().to_string())
        }
        _ => Ok(()),
    }
}

/// `bytes` in lower-case hex.
pub fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}
