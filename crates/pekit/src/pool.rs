//! The pool: the threads a build's effects run on.
//!
//! # The pool
//!
//! GCSL evaluates on one thread; what a build runs in parallel is its
//! effects (see `design/engine.md`, E1). The pool has a fixed number of
//! threads, `--jobs`, each taking the next [`Work`] from one queue: an
//! effect of a unit to run under its effect context, until its deadline, or
//! a `fetch` to answer from the store or the network ([Fetching](crate::fetch#fetching)).
//! Each reports what it came to, a [`Ran`], with the thread that ran it and
//! when, for the build's events, and the files it kept in the store. A
//! step's program that may be found in the store is found or run there too,
//! with all the hashing and placing of trees that takes
//! ([`crate::programs`]).

use std::path::PathBuf;
use std::sync::mpsc::{Receiver, Sender, channel};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Instant;

use gcsl::eval::Effects;
use gcsl::eval::graph::{EffectJob, NodeId};
use gcsl::eval::plain::Plain;
use gcsl::{Diagnostic, Span};

use crate::fetch::{fetch, fetched_path};
use crate::git_fetch::{fetch_git, git_path};
use crate::programs::{ProgramRan, ProgramWork, perform_program};
use crate::sandbox::StepSandbox;
use crate::record::Kept;
use crate::store::{FileEntry, Store};
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
    /// What became of it, if it was a program sent to be found or run.
    pub program: Option<ProgramRan>,
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

/// What a job came to: its outcome, what it kept, and what became of it if
/// it was a program sent to be found or run.
type Performed = (
    Result<Plain, Diagnostic>,
    Vec<(String, Kept)>,
    Option<ProgramRan>,
);

/// Does `work`, until `deadline`: what it came to.
fn perform(work: Work, deadline: Option<Instant>) -> Performed {
    match work {
        Work::Effect(job, effects) => (job.run(&*effects, deadline), Vec::new(), None),
        Work::Program(job, sandbox, work) => {
            let (outcome, ran) = perform_program(&job, &sandbox, &work, deadline);
            (outcome, Vec::new(), Some(ran))
        }
        Work::Fetch {
            url,
            sha256,
            store,
            span,
        } => match fetch(&store, &url, &sha256) {
            Ok(entry) => {
                let path = fetched_path(&url, &entry.digest);
                let file = Plain::File {
                    path: path.clone(),
                    identity: None,
                };
                (Ok(file), vec![(path, Kept::File(entry))], None)
            }
            Err((message, note)) => {
                let mut error = Diagnostic::error(message, span);
                if let Some(note) = note {
                    error = error.with_note(note);
                }
                (Err(error), Vec::new(), None)
            }
        },
        Work::FetchGit {
            url,
            commit,
            store,
            scratch,
            span,
        } => match fetch_git(&store, (&url, &commit), &scratch) {
            Ok(tree) => {
                let path = git_path(&url, &commit.trim().to_ascii_lowercase());
                let dir = Plain::Dir {
                    path: path.clone(),
                    identity: None,
                };
                (Ok(dir), vec![(path, Kept::Dir(tree))], None)
            }
            Err(message) => (Err(Diagnostic::error(message, span)), Vec::new(), None),
        },
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
                (Ok(dir), vec![(path, Kept::Dir(tree))], None)
            }
            Err(message) => (Err(Diagnostic::error(message, span)), Vec::new(), None),
        },
    }
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
                    let (outcome, kept, program) = perform(work, deadline);
                    let ran = Ran {
                        node,
                        worker,
                        began,
                        ended: Instant::now(),
                        outcome,
                        kept,
                        program,
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
