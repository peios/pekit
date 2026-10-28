//! The pool: the threads a build's effects run on.
//!
//! # The pool
//!
//! GCSL evaluates on one thread; what a build runs in parallel is its
//! effects (see `design/engine.md`, E1). The pool has a fixed number of
//! threads, `--jobs`, each taking the next [`Work`] from one queue: an
//! effect of a unit to run under its effect context, until its deadline, or
//! a `fetch` to answer from the store or the network ([Fetching](crate::fetch#fetching)),
//! or a target's read of an output to answer from the store
//! ([Exposure](crate::exposure#exposure)), or the engine's own work for a
//! step: staging its sandbox before it runs, and keeping the files of its
//! result in the store once it has given one, and finding records in, and
//! writing them to, the remote cache ([`crate::remote`]). That work places and hashes
//! whole trees, and so is done here, beside the programs, never on the
//! thread that evaluates.
//! Each reports what it came to, a [`Ran`], with the thread that ran it and
//! when, for the build's events, and the files it kept in the store. A
//! step's program that may be found in the store is found or run there too,
//! with all the hashing and placing of trees that takes
//! ([`crate::programs`]).

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::mpsc::{Receiver, Sender, channel};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Instant;

use gcsl::eval::Effects;
use gcsl::eval::graph::{EffectJob, NodeId};
use gcsl::eval::plain::Plain;
use gcsl::{Diagnostic, Span};

use crate::exposure::{OutputRead, read_output};
use crate::fetch::{fetch, fetched_path};
use crate::git_fetch::{fetch_git, git_path};
use crate::programs::{ProgramRan, ProgramWork, perform_program};
use crate::sandbox::StepSandbox;
use crate::layers::remove_tree;
use crate::record::{Kept, StoreError, keep_files, merge_remote_manifest};
use crate::remote::Remote;
use crate::store::{Digest, FileEntry, Store};
use crate::unpack::{unpack, unpack_key, unpacked_path};

/// What a thread of the pool does (see [The pool](#the-pool)).
pub enum Work {
    /// Runs an effect under its effect context.
    Effect(EffectJob, Arc<dyn Effects + Send + Sync>),
    /// Finds or runs a program of a step in its sandbox (see [A program's
    /// work](crate::programs#a-programs-work)).
    Program(EffectJob, Arc<StepSandbox>, ProgramWork),
    /// Answers a `fetch` of `url` pinned by `sha256` from `store`, its call
    /// at `span`.
    Fetch {
        /// The URL.
        url: String,
        /// Its pin.
        sha256: String,
        /// The store.
        store: Store,
        /// Where its call is.
        span: Span,
    },
    /// Answers a `fetch_git` of `url` at `commit` from `store` (see
    /// [Fetching from git](crate::git_fetch#fetching-from-git)), its call at
    /// `span`.
    FetchGit {
        /// The repository's URL.
        url: String,
        /// The commit, by its whole name.
        commit: String,
        /// The store.
        store: Store,
        /// Where to check it out before it is stored.
        scratch: PathBuf,
        /// Where its call is.
        span: Span,
    },
    /// Unpacks an archive into the store (see
    /// [Unpacking](crate::unpack#unpacking)).
    Unpack {
        /// The archive, as the store keeps it.
        archive: FileEntry,
        /// It, as GCSL code names it.
        shown: String,
        /// How many components of each path to leave off.
        strip: u32,
        /// The store.
        store: Store,
        /// Where to unpack it before it is stored.
        scratch: PathBuf,
        /// Where its call is.
        span: Span,
    },
    /// Stages what a step is given, each by its logical path, in its
    /// sandbox, from `store`; its staging directory removed if it cannot
    /// be.
    Stage {
        /// The sandbox.
        sandbox: Arc<StepSandbox>,
        /// What it is given.
        given: Vec<(String, Kept)>,
        /// The store.
        store: Store,
    },
    /// Keeps in `store` each file and directory `plain`, a step's result,
    /// names, from the step's sandbox.
    Keep {
        /// The result.
        plain: Plain,
        /// The step's sandbox.
        sandbox: Arc<StepSandbox>,
        /// The store.
        store: Store,
    },
    /// Finds the first of the records `keys` in the remote cache, putting
    /// it into `store` with all it names (see [The remote
    /// cache](crate::remote#the-remote-cache)); or, if `manifest`, merges
    /// the manifest `keys` names into `store`'s.
    Remote {
        /// The remote cache.
        remote: Arc<Remote>,
        /// The records' keys, in the order they are asked for.
        keys: Vec<Digest>,
        /// Whether the key is a manifest's, merged rather than replaced.
        manifest: bool,
        /// The store.
        store: Store,
    },
    /// Writes the records `keys` of `store`, and all they name, to the
    /// remote cache, and the manifest `manifest`, merged with the cache's
    /// first.
    Upload {
        /// The remote cache.
        remote: Arc<Remote>,
        /// The records' keys.
        keys: Vec<Digest>,
        /// The manifest's key.
        manifest: Option<Digest>,
        /// The store.
        store: Store,
    },
    /// Answers a target's read of an output from the store (see
    /// [Exposure](crate::exposure#exposure)).
    Read {
        /// What it reads.
        read: OutputRead,
        /// The output, as GCSL code names it.
        path: String,
        /// The store.
        store: Store,
        /// Where its call is.
        span: Span,
    },
}

/// What a thread of the pool did.
pub struct Ran {
    /// Its node.
    pub node: NodeId,
    /// The thread that did it, from 0.
    pub worker: usize,
    /// When it began.
    pub began: Instant,
    /// When it ended.
    pub ended: Instant,
    /// What it came to.
    pub outcome: Result<Plain, Diagnostic>,
    /// The files it kept in the store, each by its path as GCSL code names
    /// it.
    pub kept: Vec<(String, Kept)>,
    /// What else it came to.
    pub done: Done,
}

/// What a job came to beside its outcome.
pub enum Done {
    /// Nothing more: it was an effect, or answered one.
    Effect,
    /// What became of a program sent to be found or run.
    Program(ProgramRan),
    /// A step's sandbox staged ([`Work::Stage`]), or why it could not be.
    Staged(Result<(), String>),
    /// The files of a step's result kept ([`Work::Keep`]), by logical
    /// path, or why they could not be.
    Kept(Result<HashMap<String, Kept>, StoreError>),
    /// Whether one of the records asked for was found in the remote cache
    /// ([`Work::Remote`]), or why the cache could not be asked.
    Remote(Result<bool, String>),
    /// The records were written to the remote cache ([`Work::Upload`]), or
    /// why they could not be.
    Uploaded(Result<(), String>),
}

/// One job: a node's work, until its deadline.
type Job = (NodeId, Work, Option<Instant>);

/// The pool (see [The pool](#the-pool)).
pub struct Pool {
    /// Where jobs go.
    jobs: Sender<Job>,
    /// Where what they came to comes back.
    done: Receiver<Ran>,
}

/// What a job came to: its outcome, what it kept, and what else.
type Performed = (Result<Plain, Diagnostic>, Vec<(String, Kept)>, Done);

/// Does `work`, until `deadline`: what it came to.
fn perform(work: Work, deadline: Option<Instant>) -> Performed {
    match work {
        Work::Effect(job, effects) => (job.run(&*effects, deadline), Vec::new(), Done::Effect),
        Work::Program(job, sandbox, work) => {
            let (outcome, ran) = perform_program(&job, &sandbox, &work, deadline);
            (outcome, Vec::new(), Done::Program(ran))
        }
        Work::Fetch {
            url,
            sha256,
            store,
            span,
        } => fetched(&store, (&url, &sha256), span),
        Work::FetchGit {
            url,
            commit,
            store,
            scratch,
            span,
        } => fetched_git(&store, (&url, &commit), &scratch, span),
        Work::Unpack {
            archive,
            shown,
            strip,
            store,
            scratch,
            span,
        } => match unpack(&store, (&archive, &shown), strip, &scratch) {
            Ok(tree) => {
                let path = unpacked_path(&unpack_key(&archive.digest, strip));
                let dir = Plain::Dir {
                    path: path.clone(),
                    identity: None,
                };
                (Ok(dir), vec![(path, Kept::Dir(tree))], Done::Effect)
            }
            Err(message) => (
                Err(Diagnostic::error(message, span)),
                Vec::new(),
                Done::Effect,
            ),
        },
        Work::Read {
            read,
            path,
            store,
            span,
        } => {
            let outcome = read_output(&store, read, &path).map_err(|(message, note)| {
                let error = Diagnostic::error(message, span);
                match note {
                    Some(note) => error.with_note(note),
                    None => error,
                }
            });
            (outcome, Vec::new(), Done::Effect)
        }
        Work::Remote {
            remote,
            keys,
            manifest,
            store,
        } => looked_for(&remote, &keys, manifest, &store),
        Work::Upload {
            remote,
            keys,
            manifest,
            store,
        } => uploaded(&remote, &keys, manifest.as_ref(), &store),
        Work::Stage {
            sandbox,
            given,
            store,
        } => stage(&sandbox, &given, &store),
        Work::Keep {
            plain,
            sandbox,
            store,
        } => keep(plain, &sandbox, &store),
    }
}

/// Answers a `fetch` of `url` pinned by `sha256` from `store`, its call at
/// `span` ([`Work::Fetch`]).
fn fetched(store: &Store, (url, sha256): (&str, &str), span: Span) -> Performed {
    match fetch(store, url, sha256) {
        Ok(entry) => {
            let path = fetched_path(url, &entry.digest);
            let file = Plain::File {
                path: path.clone(),
                identity: None,
            };
            (Ok(file), vec![(path, Kept::File(entry))], Done::Effect)
        }
        Err((message, note)) => {
            let mut error = Diagnostic::error(message, span);
            if let Some(note) = note {
                error = error.with_note(note);
            }
            (Err(error), Vec::new(), Done::Effect)
        }
    }
}

/// Answers a `fetch_git` of `url` at `commit` from `store`, checked out at
/// `scratch` first, its call at `span` ([`Work::FetchGit`]).
fn fetched_git(
    store: &Store,
    (url, commit): (&str, &str),
    scratch: &std::path::Path,
    span: Span,
) -> Performed {
    match fetch_git(store, (url, commit), scratch) {
        Ok(tree) => {
            let path = git_path(url, &commit.trim().to_ascii_lowercase());
            let dir = Plain::Dir {
                path: path.clone(),
                identity: None,
            };
            (Ok(dir), vec![(path, Kept::Dir(tree))], Done::Effect)
        }
        Err(message) => (
            Err(Diagnostic::error(message, span)),
            Vec::new(),
            Done::Effect,
        ),
    }
}

/// Looks for the first of the records `keys` in the remote cache `remote`,
/// putting it into `store`; or, if `manifest`, merges the manifest they
/// name into `store`'s ([`Work::Remote`]).
fn looked_for(remote: &Remote, keys: &[Digest], manifest: bool, store: &Store) -> Performed {
    let mut found = Ok(false);
    for key in keys {
        let asked = if manifest {
            merge_remote_manifest(remote, store, key).map(|()| false)
        } else {
            remote.find(store, key)
        };
        match asked {
            Ok(false) => {}
            Ok(true) => {
                found = Ok(true);
                break;
            }
            Err(error) => {
                found = Err(error.to_string());
                break;
            }
        }
    }
    (Ok(Plain::Empty), Vec::new(), Done::Remote(found))
}

/// Writes the records `keys` of `store` to the remote cache `remote`, and
/// the manifest `manifest`, merged with the cache's first ([`Work::Upload`]).
fn uploaded(
    remote: &Remote,
    keys: &[Digest],
    manifest: Option<&Digest>,
    store: &Store,
) -> Performed {
    let uploaded = manifest
        .iter()
        .try_for_each(|key| merge_remote_manifest(remote, store, key))
        .and_then(|()| {
            keys.iter()
                .chain(manifest)
                .try_for_each(|key| remote.upload(store, key))
        })
        .map_err(|error| error.to_string());
    (Ok(Plain::Empty), Vec::new(), Done::Uploaded(uploaded))
}

/// Stages `given` in the step's sandbox `sandbox` from `store`
/// ([`Work::Stage`]), its staging directory removed if it cannot be.
fn stage(sandbox: &StepSandbox, given: &[(String, Kept)], store: &Store) -> Performed {
    let staged = sandbox.stage_given(store, given);
    if staged.is_err() {
        let _ = remove_tree(sandbox.stage_dir());
    }
    (Ok(Plain::Empty), Vec::new(), Done::Staged(staged))
}

/// Keeps in `store` the files of `plain`, the result of the step whose
/// sandbox is `sandbox` ([`Work::Keep`]): its outcome `plain` again.
fn keep(plain: Plain, sandbox: &StepSandbox, store: &Store) -> Performed {
    let host = |path: &str| sandbox.host_path(path).unwrap_or_default();
    let kept = keep_files(&plain, store, &host, &sandbox.mounted());
    (Ok(plain), Vec::new(), Done::Kept(kept))
}

impl Pool {
    /// A pool of `size` threads.
    #[must_use]
    pub fn new(size: usize) -> Self {
        let (jobs, queue) = channel::<Job>();
        let (report, done) = channel();
        let queue = Arc::new(Mutex::new(queue));
        for worker in 0..size {
            let queue = Arc::clone(&queue);
            let report = report.clone();
            thread::spawn(move || {
                loop {
                    let next = queue.lock().ok().and_then(|queue| queue.recv().ok());
                    let Some((node, work, deadline)) = next else {
                        return;
                    };
                    let began = Instant::now();
                    let (outcome, kept, done) = perform(work, deadline);
                    let ran = Ran {
                        node,
                        worker,
                        began,
                        ended: Instant::now(),
                        outcome,
                        kept,
                        done,
                    };
                    if report.send(ran).is_err() {
                        return;
                    }
                }
            });
        }
        Self { jobs, done }
    }

    /// Sends the node `node`'s `work`, to do until `deadline`.
    pub fn send(&self, node: NodeId, work: Work, deadline: Option<Instant>) {
        let _ = self.jobs.send((node, work, deadline));
    }

    /// The next job's outcome, waiting for one: none if every thread of the
    /// pool has stopped.
    #[must_use]
    pub fn next(&self) -> Option<Ran> {
        self.done.recv().ok()
    }

    /// A job's outcome that is ready now, without waiting for one.
    #[must_use]
    pub fn ready(&self) -> Option<Ran> {
        self.done.try_recv().ok()
    }
}
