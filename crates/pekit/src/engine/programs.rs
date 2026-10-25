//! Per-program caching, as the engine keeps it: what each step's programs'
//! keys are made of (see `design/additive.md`, F1).
//!
//! # Programs
//!
//! A step's [`Programs`] are its *view*, a chain folding in each effect it
//! had run, and the digests of the trees its programs can write, its own
//! output directory and its `/tmp`, while they are known: from a program's
//! end until the next effect that may write them. Before an effect of a
//! step is sent to run, [`Run::program_before`] says what it is: a program
//! to find or run as a thread of the pool does it ([`crate::programs`]), an
//! effect to fold into the view once it has run ([`Run::program_folded`]),
//! or neither, an effect that only reads. A step whose sandbox binds a
//! secret, or whose writable trees could not be kept, has its programs
//! neither found nor recorded from then on.

use gcsl::check::Builtin;
use gcsl::eval::graph::{EffectJob, NodeId};
use gcsl::eval::plain::Plain;
use gcsl::{Diagnostic, Span};

use crate::programs::{ProgramRan, ProgramWork, digest_of};
use crate::receipt::folded;
use crate::record::Kept;
use crate::store::Digest;

use super::Run;

/// What a step's programs' keys are made of (see [Programs](#programs)).
#[derive(Clone, Debug)]
pub(super) struct Programs {
    /// Whether its programs may be found and recorded.
    on: bool,
    /// Its view: the chain of the effects it had run.
    view: Digest,
    /// The digests of the trees its programs can write, its own output
    /// directory's and its `/tmp`'s, while they are known.
    writable: Option<(Digest, Digest)>,
}

impl Programs {
    /// A step's, as an attempt of it starts: its view the platform
    /// `platform`, its own output directory `own`, where its programs run
    /// and which a compiler names in what it writes, and each input `given`
    /// staged at its start.
    pub(super) fn starting((platform, own): (&str, &str), given: &[(String, Kept)]) -> Self {
        let start = Digest::of(format!("pekit-programs-1\0{platform}\0{own}").as_bytes());
        Self {
            on: true,
            view: with_staged(&start, given),
            writable: None,
        }
    }
}

/// What [`Run::program_before`] says an effect of a step is.
pub(super) enum Before {
    /// A program, to find or run as this says.
    Program(ProgramWork),
    /// An effect to fold into the step's view once it has run: its built-in
    /// and its arguments.
    Fold(Builtin, Vec<Plain>),
    /// Neither.
    Plain,
}

/// `view` with each of `staged`, by its path and contents, folded in, in
/// the order of their paths.
fn with_staged(view: &Digest, staged: &[(String, Kept)]) -> Digest {
    if staged.is_empty() {
        return *view;
    }
    let mut sorted: Vec<(&str, Digest)> = staged
        .iter()
        .map(|(path, kept)| (path.as_str(), digest_of(*kept)))
        .collect();
    sorted.sort_unstable();
    let mut text = format!("{}\0", view.hex());
    for (path, digest) in sorted {
        text.push_str(path);
        text.push('\0');
        text.push_str(&digest.hex());
        text.push('\0');
    }
    Digest::of(text.as_bytes())
}

/// Whether `builtin` only reads, so changes nothing its step's programs
/// can see.
fn only_reads(builtin: Builtin) -> bool {
    matches!(
        builtin,
        Builtin::FsExists
            | Builtin::FsRead
            | Builtin::FileRead
            | Builtin::DirFiles
            | Builtin::Which
    )
}

/// Whether `builtin` runs a program that may be found and recorded.
fn is_program(builtin: Builtin) -> bool {
    matches!(
        builtin,
        Builtin::Exec | Builtin::ExecLive | Builtin::SandboxExec
    )
}

impl Run<'_> {
    /// What the effect `job`, its call at `span`, of the step `step` is,
    /// before it is sent to run (see [Programs](#programs)).
    pub(super) fn program_before(&mut self, step: NodeId, job: &EffectJob, span: Span) -> Before {
        let builtin = job.builtin();
        if only_reads(builtin) {
            return Before::Plain;
        }
        let reuse = self.settings.reuse;
        let store = self.store.clone();
        let Some(run) = self.steps.get_mut(&step) else {
            return Before::Plain;
        };
        if builtin == Builtin::ExecSecret || run.sandbox.binds_secrets() {
            run.programs.on = false;
        }
        if !run.programs.on {
            return Before::Plain;
        }
        // Whatever comes of it, what its step's programs can write is known
        // again only once it has run.
        let writable = run.programs.writable.take();
        if !is_program(builtin) {
            // It can write into a sealed composition.
            if let Some(mut seen) = run.sandbox.seen() {
                seen.changed();
            }
            return Before::Fold(builtin, job.args().to_vec());
        }
        Before::Program(ProgramWork {
            store,
            view: run.programs.view,
            writable,
            reuse,
            span,
        })
    }

    /// Once the effect `builtin` on `args` of the step `step` has run, folds
    /// it into the step's view, with `kept`, what an unpacking of it staged.
    pub(super) fn program_folded(
        &mut self,
        step: NodeId,
        (builtin, args): (Builtin, &[Plain]),
        kept: &[(String, Kept)],
    ) {
        let Some(run) = self.steps.get_mut(&step) else {
            return;
        };
        match folded(&run.programs.view, builtin.name(), args) {
            Ok(view) => run.programs.view = with_staged(&view, kept),
            Err(_) => run.programs.on = false,
        }
    }

    /// Once a program of the step `step` has been found or run, as `ran`
    /// says, with `outcome`: what its step's programs can write is known, or
    /// they can no longer be found or recorded.
    pub(super) fn program_ran(
        &mut self,
        step: NodeId,
        ran: ProgramRan,
        outcome: &Result<Plain, Diagnostic>,
    ) {
        if ran.found {
            self.programs_found += 1;
        }
        let Some(run) = self.steps.get_mut(&step) else {
            return;
        };
        run.programs.writable = ran.writable;
        // A program that failed ends its step's attempt; one whose step's
        // trees could not be kept ends their programs' caching.
        if ran.writable.is_none() && outcome.is_ok() {
            run.programs.on = false;
        }
    }
}
