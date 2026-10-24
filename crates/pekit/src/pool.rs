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
//! when, for the build's events, and the files it kept in the store.

use std::sync::mpsc::{Receiver, Sender, channel};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Instant;

use gcsl::eval::Effects;
use gcsl::eval::graph::{EffectJob, NodeId};
use gcsl::eval::plain::Plain;
use gcsl::{Diagnostic, Span};

use crate::fetch::{fetch, fetched_path};
use crate::record::Kept;
use crate::store::Store;

/// What a thread of the pool does (see [The pool](#the-pool)).
pub enum Work {
    /// Runs an effect under its effect context.
    Effect(EffectJob, Arc<dyn Effects + Send + Sync>),
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

/// Does `work`, until `deadline`: what it came to, and what it kept.
fn perform(
    work: Work,
    deadline: Option<Instant>,
) -> (Result<Plain, Diagnostic>, Vec<(String, Kept)>) {
    match work {
        Work::Effect(job, effects) => (job.run(&*effects, deadline), Vec::new()),
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
                (Ok(file), vec![(path, Kept::File(entry))])
            }
            Err((message, note)) => {
                let mut error = Diagnostic::error(message, span);
                if let Some(note) = note {
                    error = error.with_note(note);
                }
                (Err(error), Vec::new())
            }
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
                    let (outcome, kept) = perform(work, deadline);
                    let ran = Ran {
                        node,
                        worker,
                        began,
                        ended: Instant::now(),
                        outcome,
                        kept,
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
}
