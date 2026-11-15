//! Per-program caching, as a thread of the pool does it: each program a
//! step runs, found or recorded by what it can see (see
//! `design/additive.md`, F1).
//!
//! # A program's work
//!
//! The engine keeps each step's *view*, the chain of the effects it had run
//! (see `engine/programs.rs`), and sends each program of a step whose
//! programs may be cached as a [`ProgramWork`]. The thread that takes it
//! does the rest, so that no tree is hashed or placed on the engine's own
//! thread:
//!
//! 1. it hashes the trees the program can write, the step's own output
//!    directory and its `/tmp`, unless the engine knows their digests;
//! 2. it makes the program's key ([`exec_key`]) of the view, every input
//!    the step has staged, those digests, the built-in and its arguments;
//! 3. where the store's records are reused, it finds the program's record,
//!    places again each writable tree whose digest differs from the
//!    record's, whole, gives both the times and modes the run left, and
//!    gives the record's result, without running it;
//! 4. else it runs it, hashes its writable trees again, and, where records
//!    are kept, records what it gave, them, and their times and modes.
//!
//! What it tells the engine, a [`ProgramRan`], is whether the program was
//! found and its writable trees' digests after it, or that they could not
//! be hashed, or held what the store can't keep, so that the step's
//! programs are neither found nor recorded from then on. Nothing here fails
//! a program but a replay that changed the step's world and could not
//! finish: what cannot be hashed, read or recorded is a program run, not
//! found.

use std::collections::HashMap;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use gcsl::eval::graph::EffectJob;
use gcsl::eval::plain::Plain;
use gcsl::{Diagnostic, Span};

use crate::budget::Held;
use crate::engine::Reuse;
use crate::layers::{remove_tree, unkeepable};
use crate::pool::admit;
use crate::receipt::{ExecOf, exec_key};
use crate::record::{ExecRecord, Kept, stored};
use crate::sandbox::StepSandbox;
use crate::store::{Digest, Store};

/// A program of a step, as the engine sends it to be found or run (see [A
/// program's work](#a-programs-work)).
#[derive(Debug)]
pub struct ProgramWork {
    /// The store.
    pub store: Store,
    /// The step's view before it.
    pub view: Digest,
    /// The digests of its writable trees, if the engine knows them.
    pub writable: Option<(Digest, Digest)>,
    /// Whether its record is looked for, kept, or neither.
    pub reuse: Reuse,
    /// Where its call is.
    pub span: Span,
}

/// What became of a program sent as a [`ProgramWork`].
#[derive(Clone, Copy, Debug)]
pub struct ProgramRan {
    /// Whether it was found in the store, and not run.
    pub found: bool,
    /// The digests of its step's writable trees after it, or `None` if its
    /// step's programs can no longer be found or recorded.
    pub writable: Option<(Digest, Digest)>,
    /// How long it waited for the machine's budget before it ran.
    pub queued: Duration,
}

/// The digest a store's file or directory is kept by.
#[must_use]
pub fn digest_of(kept: Kept) -> Digest {
    match kept {
        Kept::File(entry) => entry.digest,
        Kept::Dir(tree) => tree,
    }
}

/// Finds or runs the program `job` of the step whose sandbox is `sandbox`,
/// as `work` says, until `deadline` (see [A program's
/// work](#a-programs-work)): what it gave, and what became of it.
pub fn perform_program(
    job: &EffectJob,
    sandbox: &StepSandbox,
    work: &ProgramWork,
    deadline: Option<Instant>,
) -> (Result<Plain, Diagnostic>, ProgramRan) {
    // A program that runs waits for the machine's budget first, its
    // deadline put off by as long.
    let run = |sandbox: &StepSandbox| {
        let held = admit(job, sandbox.metered());
        let queued = held.as_ref().map_or(Duration::ZERO, Held::waited);
        let outcome = job.run(sandbox, deadline.map(|deadline| deadline + queued));
        (outcome, queued)
    };
    let plainly = |sandbox: &StepSandbox| {
        let (outcome, queued) = run(sandbox);
        let ran = ProgramRan {
            found: false,
            writable: None,
            queued,
        };
        (outcome, ran)
    };
    let Some(before) = work.writable.or_else(|| captured(sandbox, &work.store)) else {
        return plainly(sandbox);
    };
    let mut staged: Vec<(String, Digest)> = sandbox
        .staged()
        .into_iter()
        .map(|(path, kept)| (path, digest_of(kept)))
        .collect();
    staged.sort_unstable();
    let key = exec_key(ExecOf {
        view: &work.view,
        staged: &staged,
        writable: (&before.0, &before.1),
        builtin: job.builtin().name(),
        args: job.args(),
    });
    let Ok(key) = key else {
        return plainly(sandbox);
    };
    // A record whose long strings the store no longer holds is a program
    // run again.
    if work.reuse == Reuse::Store
        && let Ok(Some(found)) = work.store.record::<ExecRecord>(&key)
        && let Ok(result) = found.result.plain(&work.store)
    {
        match replay(sandbox, &work.store, &found, before) {
            Ok(true) => {
                let ran = ProgramRan {
                    found: true,
                    writable: Some((found.own, found.tmp)),
                    queued: Duration::ZERO,
                };
                return (Ok(result), ran);
            }
            Ok(false) => {}
            Err(message) => {
                let ran = ProgramRan {
                    found: false,
                    writable: None,
                    queued: Duration::ZERO,
                };
                return (Err(Diagnostic::error(message, work.span)), ran);
            }
        }
    }
    let began = Instant::now();
    let (outcome, queued) = run(sandbox);
    // Its cost is its run's, not its wait's.
    let duration = began.elapsed().saturating_sub(queued);
    let duration_ms = u64::try_from(duration.as_millis()).unwrap_or(u64::MAX);
    // What it wrote is hashed again whether it ran well or not.
    let after = captured(sandbox, &work.store);
    if let (Ok(plain), Some((own, tmp)), false) = (&outcome, after, work.reuse == Reuse::Compare)
        && let Ok(result) = stored(plain, &HashMap::new(), &work.store)
        && let Some(times) = times_of(sandbox, &work.store)
    {
        // A record that cannot be written is a program run again.
        let _ = work.store.put_record(
            &key,
            &ExecRecord {
                result,
                own,
                tmp,
                duration_ms,
                times: Some(times),
            },
        );
    }
    let ran = ProgramRan {
        found: false,
        writable: after,
        queued,
    };
    (outcome, ran)
}

/// The digests of the writable trees of the step whose sandbox is
/// `sandbox`, its own output directory's and its `/tmp`'s, put into
/// `store`, reading only what the sandbox does not know unchanged, or
/// `None` if one cannot be read or holds what the store can't keep.
fn captured(sandbox: &StepSandbox, store: &Store) -> Option<(Digest, Digest)> {
    let (own, tmp) = sandbox.writable().ok()?;
    let sealed = sandbox.sealed();
    let mounted = sandbox.mounted();
    let mut seen = sandbox.seen()?;
    seen.fix(sealed);
    seen.mount(mounted);
    let own = store.put_tree_seen(&own, &mut seen, &unkeepable).ok()??;
    let tmp = store.put_tree_seen(&tmp, &mut seen, &unkeepable).ok()??;
    Some((own, tmp))
}

/// Places again each writable tree of the step whose sandbox is `sandbox`
/// whose digest, `now`, differs from `found`'s, whole, from `store`:
/// whether it could, every content there. Nothing is changed if one is
/// missing.
///
/// # Errors
///
/// That the trees were being placed and could not all be.
fn replay(
    sandbox: &StepSandbox,
    store: &Store,
    found: &ExecRecord,
    now: (Digest, Digest),
) -> Result<bool, String> {
    let Ok((own, tmp)) = sandbox.writable() else {
        return Ok(false);
    };
    // A record that kept no times, or whose times the store no longer
    // holds, is a program run again: placed without them, what it wrote
    // would look as new as the copy, to make and the like.
    let Some(times) = found
        .times
        .and_then(|times| store.read_blob(&times).ok())
        .and_then(|blob| Times::decode(&blob))
    else {
        return Ok(false);
    };
    let mut placing = Vec::new();
    let mounted = sandbox.mounted();
    for (path, before, after) in [(&own, now.0, found.own), (&tmp, now.1, found.tmp)] {
        if before == after {
            continue;
        }
        if let Ok(beside) = placed_beside(store, path, (&after, &mounted)) {
            placing.push((path.clone(), beside));
        } else {
            for (_, beside) in &placing {
                let _ = remove_tree(beside);
            }
            return Ok(false);
        }
    }
    // Its sealed compositions, if they are placed again, are copies.
    if let Some(mut seen) = sandbox.seen() {
        seen.changed();
    }
    for (path, beside) in placing {
        remove_tree(&path)
            .and_then(|()| fs::rename(&beside, &path))
            .map_err(|error| {
                format!(
                    "cannot place what a program found in the store wrote at `{}`: {error}",
                    path.display()
                )
            })?;
    }
    // Every tree, placed again or not, is given the times and modes the run
    // left: a program that only touched a file changed no digest.
    times
        .apply([&own, &tmp], &skipped(sandbox))
        .map_err(|error| {
            format!("cannot give what a program found in the store wrote its times: {error}")
        })?;
    // Its sealed compositions, if they were placed again, are read-only
    // again, as a run would have left them.
    sandbox.reseal().map_err(|error| {
        format!("cannot seal again what a program found in the store left: {error}")
    })?;
    Ok(true)
}

/// The tree `tree` from `store`, copied beside `path`, under a name of its
/// own, but each mount point of `mounted` under `path`, by where it is, of
/// its tree, left empty for it to be mounted on again: where it is.
///
/// # Errors
///
/// That the store lacks something of it, or it cannot be written.
fn placed_beside(
    store: &Store,
    path: &Path,
    (tree, mounted): (&Digest, &HashMap<PathBuf, Digest>),
) -> io::Result<PathBuf> {
    let name = path
        .file_name()
        .map_or_else(String::new, |name| name.to_string_lossy().into_owned());
    let beside = path.with_file_name(format!(".{name}.replay-{}", crate::sweep::token()));
    remove_tree(&beside)?;
    let mounted: HashMap<PathBuf, Digest> = mounted
        .iter()
        .filter_map(|(point, digest)| Some((beside.join(point.strip_prefix(path).ok()?), *digest)))
        .collect();
    if let Err(error) = store.copy_tree_mounted(tree, &beside, &mounted) {
        let _ = remove_tree(&beside);
        return Err(error);
    }
    Ok(beside)
}

/// What the trees' digests leave out of what a program left: each file's,
/// directory's and link's time of change, and each file's and directory's
/// mode, by its tree, its own output directory (0) or `/tmp` (1), and its
/// path in it. A program found is given them, so that what it wrote is
/// newer than what it read, as its run left it, and a mode it set is kept.
#[derive(Debug, Default, PartialEq, Eq)]
struct Times {
    /// Each entry, a directory before what it holds.
    entries: Vec<Stamped>,
}

/// One entry of [`Times`].
#[derive(Debug, PartialEq, Eq)]
struct Stamped {
    /// Its tree: 0 for the own output directory, 1 for `/tmp`.
    tree: u8,
    /// Its path in it, empty for the tree itself.
    path: Vec<u8>,
    /// Its mode's permission bits; none of a link's.
    mode: u32,
    /// Its time of change: seconds and nanoseconds.
    modified: (i64, u32),
}

impl Times {
    /// The times of what the trees `trees` hold, but what is at or under
    /// one of `skipped`.
    ///
    /// # Errors
    ///
    /// That something in them cannot be read.
    fn of(trees: [&Path; 2], skipped: &[PathBuf]) -> io::Result<Self> {
        let mut times = Self::default();
        for (tree, root) in (0u8..).zip(trees) {
            times.walk(tree, root, root, skipped)?;
        }
        Ok(times)
    }

    /// Adds `path`, in the tree `tree` at `root`, and, if it is a
    /// directory, what it holds, in the order of their names.
    fn walk(&mut self, tree: u8, root: &Path, path: &Path, skipped: &[PathBuf]) -> io::Result<()> {
        use std::os::unix::ffi::OsStrExt as _;
        use std::os::unix::fs::MetadataExt as _;
        if skipped.iter().any(|skip| path.starts_with(skip)) {
            return Ok(());
        }
        let meta = match fs::symlink_metadata(path) {
            Ok(meta) => meta,
            Err(error) if error.kind() == io::ErrorKind::NotFound && path != root => return Ok(()),
            Err(error) => return Err(error),
        };
        let kind = meta.file_type();
        if !(kind.is_file() || kind.is_dir() || kind.is_symlink()) {
            return Ok(());
        }
        let relative = path.strip_prefix(root).unwrap_or(path);
        self.entries.push(Stamped {
            tree,
            path: relative.as_os_str().as_bytes().to_vec(),
            mode: if kind.is_symlink() {
                0
            } else {
                meta.mode() & 0o7777
            },
            modified: (meta.mtime(), u32::try_from(meta.mtime_nsec()).unwrap_or(0)),
        });
        if kind.is_dir() {
            let mut names = fs::read_dir(path)?
                .map(|entry| entry.map(|entry| entry.file_name()))
                .collect::<io::Result<Vec<_>>>()?;
            names.sort_unstable();
            for name in names {
                self.walk(tree, root, &path.join(name), skipped)?;
            }
        }
        Ok(())
    }

    /// Gives each entry still in the trees `trees` its mode and time of
    /// change, what a directory holds before it, so that writing to it
    /// leaves its own time; but nothing at or under one of `skipped`, and
    /// no file of more than one link, which may be another's too.
    ///
    /// # Errors
    ///
    /// That one could not be given them.
    fn apply(&self, trees: [&Path; 2], skipped: &[PathBuf]) -> io::Result<()> {
        use rustix::fs::{AtFlags, CWD, Timespec, Timestamps, utimensat};
        use std::os::unix::ffi::OsStrExt as _;
        use std::os::unix::fs::{MetadataExt as _, PermissionsExt as _};
        for entry in self.entries.iter().rev() {
            let Some(root) = trees.get(usize::from(entry.tree)) else {
                continue;
            };
            let path = if entry.path.is_empty() {
                root.to_path_buf()
            } else {
                root.join(std::ffi::OsStr::from_bytes(&entry.path))
            };
            if skipped.iter().any(|skip| path.starts_with(skip)) {
                continue;
            }
            let Ok(meta) = fs::symlink_metadata(&path) else {
                continue;
            };
            let kind = meta.file_type();
            if kind.is_file() && meta.nlink() > 1 {
                continue;
            }
            if !kind.is_symlink() && meta.mode() & 0o7777 != entry.mode {
                fs::set_permissions(&path, fs::Permissions::from_mode(entry.mode))?;
            }
            let modified = Timespec {
                tv_sec: entry.modified.0,
                tv_nsec: entry.modified.1.into(),
            };
            let times = Timestamps {
                last_access: modified,
                last_modification: modified,
            };
            utimensat(CWD, &path, &times, AtFlags::SYMLINK_NOFOLLOW)?;
        }
        Ok(())
    }

    /// Its bytes, as the store keeps them: each entry's tree, mode, time
    /// and path, compressed.
    fn encode(&self) -> io::Result<Vec<u8>> {
        let mut bytes = Vec::new();
        for entry in &self.entries {
            bytes.push(entry.tree);
            bytes.extend_from_slice(&entry.mode.to_le_bytes());
            bytes.extend_from_slice(&entry.modified.0.to_le_bytes());
            bytes.extend_from_slice(&entry.modified.1.to_le_bytes());
            let length = u32::try_from(entry.path.len()).map_err(io::Error::other)?;
            bytes.extend_from_slice(&length.to_le_bytes());
            bytes.extend_from_slice(&entry.path);
        }
        zstd::encode_all(&bytes[..], 3)
    }

    /// The times whose bytes are `blob`, or `None` if they are not.
    fn decode(blob: &[u8]) -> Option<Self> {
        let bytes = zstd::decode_all(blob).ok()?;
        let mut rest = &bytes[..];
        let mut take = |count: usize| -> Option<&[u8]> {
            let (taken, after) = rest.split_at_checked(count)?;
            rest = after;
            Some(taken)
        };
        let mut entries = Vec::new();
        while let Some(tree) = take(1) {
            let tree = tree[0];
            let mode = u32::from_le_bytes(take(4)?.try_into().ok()?);
            let seconds = i64::from_le_bytes(take(8)?.try_into().ok()?);
            let nanoseconds = u32::from_le_bytes(take(4)?.try_into().ok()?);
            let length = u32::from_le_bytes(take(4)?.try_into().ok()?);
            let path = take(usize::try_from(length).ok()?)?.to_vec();
            entries.push(Stamped {
                tree,
                path,
                mode,
                modified: (seconds, nanoseconds),
            });
        }
        Some(Self { entries })
    }
}

/// What a program's times leave alone in the step whose sandbox is
/// `sandbox`: its sealed compositions, whose files may be the store's own,
/// and its mount points, on the host.
fn skipped(sandbox: &StepSandbox) -> Vec<PathBuf> {
    let mut skipped = sandbox.sealed();
    skipped.extend(sandbox.mounted().into_keys());
    skipped
}

/// The blob of the times of what the writable trees of the step whose
/// sandbox is `sandbox` hold, put into `store`, or `None` if they cannot be
/// read or kept.
fn times_of(sandbox: &StepSandbox, store: &Store) -> Option<Digest> {
    let (own, tmp) = sandbox.writable().ok()?;
    let times = Times::of([&own, &tmp], &skipped(sandbox)).ok()?;
    store.put_bytes(&times.encode().ok()?).ok()
}
