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
//!    record's, whole, and gives the record's result, without running it;
//! 4. else it runs it, hashes its writable trees again, and, where records
//!    are kept, records what it gave and them.
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
use std::time::Instant;

use gcsl::eval::graph::EffectJob;
use gcsl::eval::plain::Plain;
use gcsl::{Diagnostic, Span};

use crate::engine::Reuse;
use crate::layers::{remove_tree, unkeepable};
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
    let plainly = |sandbox: &StepSandbox| {
        let outcome = job.run(sandbox, deadline);
        let ran = ProgramRan {
            found: false,
            writable: None,
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
                };
                return (Ok(result), ran);
            }
            Ok(false) => {}
            Err(message) => {
                let ran = ProgramRan {
                    found: false,
                    writable: None,
                };
                return (Err(Diagnostic::error(message, work.span)), ran);
            }
        }
    }
    let began = Instant::now();
    let outcome = job.run(sandbox, deadline);
    let duration_ms = u64::try_from(began.elapsed().as_millis()).unwrap_or(u64::MAX);
    // What it wrote is hashed again whether it ran well or not.
    let after = captured(sandbox, &work.store);
    if let (Ok(plain), Some((own, tmp)), false) = (&outcome, after, work.reuse == Reuse::Compare)
        && let Ok(result) = stored(plain, &HashMap::new(), &work.store)
    {
        // A record that cannot be written is a program run again.
        let _ = work.store.put_record(
            &key,
            &ExecRecord {
                result,
                own,
                tmp,
                duration_ms,
            },
        );
    }
    let ran = ProgramRan {
        found: false,
        writable: after,
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
