//! Events: what a build tells as it goes, for whatever shows it.
//!
//! # Events
//!
//! A build emits an [`Event`] at each change in a step's life and each
//! program it runs (see `design/engine.md`, E8): a step met, found in the
//! store, started, finished or failed, and an effect queued and run; and at
//! each run of a target's or a step's code, which GCSL's graph mode runs
//! again from its start each time it stops for a result. The
//! terminal's display and the Chrome trace (`--trace`) are made from them,
//! and so will the web view be. Each is stamped with its time since the
//! build began.
//!
//! An event never holds a secret's value. It names a step by its name and an
//! effect by its built-in and the program it runs, never the program's other
//! arguments, and a program named by a secret as `<secret>`. A failure's
//! message is not an event's: the build's error shows it, as GCSL redacts it.

use std::time::Duration;

use gcsl::check::Builtin;
use gcsl::eval::fetched_name;
use gcsl::eval::graph::EffectJob;
use gcsl::eval::plain::Plain;

/// Something a build tells (see [Events](#events)).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Event {
    /// When, since the build began.
    pub at: Duration,
    /// What.
    pub kind: EventKind,
}

/// What an [`Event`] tells.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum EventKind {
    /// The target's code met a step.
    Discovered {
        /// The step's name.
        step: String,
    },
    /// A step was found in the store, and not run.
    Cached {
        /// The step's name.
        step: String,
    },
    /// An attempt of a step began, in a fresh sandbox.
    Started {
        /// The step's name.
        step: String,
        /// Which attempt, from 0.
        attempt: u32,
    },
    /// A step succeeded.
    Finished {
        /// The step's name.
        step: String,
    },
    /// An attempt of a step failed.
    Failed {
        /// The step's name.
        step: String,
        /// Whether it will be run again.
        retrying: bool,
    },
    /// An effect was queued to run.
    EffectQueued {
        /// Its node's number.
        node: u32,
        /// The step it is of, or none for the target's own.
        step: Option<String>,
        /// What it runs ([`effect_label`]).
        effect: String,
    },
    /// A program a step runs was found in the store, and not run (see
    /// `design/additive.md`, F1).
    EffectFound {
        /// Its node's number.
        node: u32,
        /// The step it is of.
        step: String,
        /// What it runs ([`effect_label`]).
        effect: String,
    },
    /// An effect ran.
    EffectRan {
        /// Its node's number.
        node: u32,
        /// The step it is of, or none for the target's own.
        step: Option<String>,
        /// What it ran ([`effect_label`]).
        effect: String,
        /// The pool's thread that ran it, from 0.
        worker: usize,
        /// When it began, since the build began.
        began: Duration,
        /// Whether it succeeded.
        ok: bool,
    },
    /// A target's or a step's code ran once, until it ended or stopped for
    /// a result it needs.
    Evaluated {
        /// The target's name, or the step's.
        unit: String,
        /// Whether it is a step's code.
        step: bool,
        /// When it began, since the build began.
        began: Duration,
    },
}

/// What the effect `job` runs, as an event names it: its built-in, and the
/// program it runs if it runs one, `exec cc`, a program named by a secret
/// `<secret>`, or the file a fetch fetches, `fetch zlib-1.3.1.tar.gz` (see
/// [Events](#events)).
#[must_use]
pub fn effect_label(job: &EffectJob) -> String {
    let builtin = job.builtin().name();
    if job.builtin() == Builtin::Fetch
        && let Some(Plain::Str(url)) = job.args().first()
    {
        return format!("{builtin} {}", fetched_name(url));
    }
    let program = job.args().first().and_then(|argv| match argv {
        Plain::List(words) => words.first(),
        _ => None,
    });
    match program {
        Some(Plain::Str(program)) => {
            let name = program.rsplit('/').next().unwrap_or(program);
            format!("{builtin} {name}")
        }
        Some(Plain::Secret { .. }) => format!("{builtin} <secret>"),
        _ => builtin.to_owned(),
    }
}
