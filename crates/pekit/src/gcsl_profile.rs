//! The GCSL profile: where a build's evaluation spends its time.
//!
//! # The GCSL profile
//!
//! `--gcsl-profile FILE` has GCSL profile the build's evaluation
//! ([Profiling](gcsl::eval::profile#profiling)) and writes what it found to
//! `FILE` as text. Its head counts the runs of the targets' code and of the
//! steps' code, from the build's [events](crate::events), and how long they
//! took: GCSL's graph mode stops a unit each time it waits for a result,
//! and runs it again, from where it stopped, once the result is given, so a
//! target that meets a hundred steps and effects runs a hundred times, but
//! runs no code twice. It says how much of that time was spent in
//! functions, and so how much outside any, at the top level of the files.
//! Its table has a row for each function and built-in that ran: its own
//! time, its total, how many calls of it ran, how many of those found a
//! memo entry, and what it is, by its name and where its body begins, the
//! longest own time first. The programs and other effects that steps run
//! on the pool are not GCSL's evaluation, and are not in it: the trace
//! (`--trace`) shows them. A build that `--check-reproducible` runs twice
//! is profiled across both.

use std::collections::HashMap;
use std::fmt::Write as _;
use std::time::Duration;

use gcsl::eval::profile::{Profiled, ProfileReport, ProfileRow};
use gcsl::ir::FnKind;

use crate::events::{Event, EventKind};
use crate::load::Loaded;

/// A build's GCSL profile being gathered (see [The GCSL
/// profile](#the-gcsl-profile)).
#[derive(Debug, Default)]
pub struct GcslProfile {
    /// How many times the targets' code ran, and for how long.
    targets: (u64, Duration),
    /// How many times the steps' code ran, and for how long.
    steps: (u64, Duration),
    /// The time of the calls no call was running around, in every build.
    outermost: Duration,
    /// Each function's and built-in's tally, in every build.
    rows: HashMap<Profiled, ProfileRow>,
}

impl GcslProfile {
    /// A profile of nothing yet.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Takes in `event`, counting a run of a target's or a step's code.
    pub fn add(&mut self, event: &Event) {
        if let EventKind::Evaluated { step, began, .. } = &event.kind {
            let tally = if *step {
                &mut self.steps
            } else {
                &mut self.targets
            };
            tally.0 += 1;
            tally.1 += event.at.saturating_sub(*began);
        }
    }

    /// Takes in what GCSL's profile of one build found.
    pub fn add_report(&mut self, report: ProfileReport) {
        self.outermost += report.outermost;
        for row in report.rows {
            match self.rows.get_mut(&row.site) {
                Some(had) => {
                    had.calls += row.calls;
                    had.found += row.found;
                    had.own += row.own;
                    had.total += row.total;
                }
                None => {
                    self.rows.insert(row.site.clone(), row);
                }
            }
        }
    }

    /// The profile as text, each function named where its body begins in
    /// `loaded`'s sources.
    #[must_use]
    pub fn render(&self, loaded: &Loaded) -> String {
        let evaluated = self.targets.1 + self.steps.1;
        let mut text = String::from("GCSL profile of the build\n\n");
        let _ = writeln!(
            text,
            "The targets' code ran {}, for {}; the steps' code {}, for {}.",
            times(self.targets.0),
            seconds(self.targets.1),
            times(self.steps.0),
            seconds(self.steps.1),
        );
        let _ = writeln!(
            text,
            "Of that, {} was in functions, and {} outside any.\n",
            seconds(self.outermost),
            seconds(evaluated.saturating_sub(self.outermost)),
        );
        let _ = writeln!(
            text,
            "{:>12} {:>12} {:>10} {:>10}  {:<8} function",
            "own (ms)", "total (ms)", "calls", "found", "kind"
        );
        let mut rows: Vec<&ProfileRow> = self.rows.values().collect();
        rows.sort_by(|a, b| b.own.cmp(&a.own).then_with(|| b.calls.cmp(&a.calls)));
        for row in rows {
            let (kind, what) = described(&row.site, loaded);
            let _ = writeln!(
                text,
                "{:>12.3} {:>12.3} {:>10} {:>10}  {kind:<8} {what}",
                millis(row.own),
                millis(row.total),
                row.calls,
                row.found,
            );
        }
        text
    }
}

/// What `site` is, its kind and its name, a function's where its body
/// begins in `loaded`'s sources (see [The GCSL profile](#the-gcsl-profile)).
fn described(site: &Profiled, loaded: &Loaded) -> (&'static str, String) {
    match site {
        Profiled::Builtin(name) => ("built-in", (*name).to_owned()),
        Profiled::Function {
            input,
            span,
            name,
            kind,
        } => {
            let kind = match kind {
                FnKind::Function => "fn",
                FnKind::Memo => "memo",
                FnKind::Future => "lazy",
            };
            let name = name.as_deref().unwrap_or("(anonymous)");
            let at = match (
                loaded.names.get(*input as usize),
                loaded.sources.get(*input as usize),
            ) {
                (Some(file), Some(source)) => {
                    format!("{file}:{}", source.line_col(span.start).0)
                }
                _ => "(the target's invocation)".to_owned(),
            };
            (kind, format!("{name}  {at}"))
        }
    }
}

/// `count` as a number of times: `1 time`, `2 times`.
fn times(count: u64) -> String {
    if count == 1 {
        "1 time".to_owned()
    } else {
        format!("{count} times")
    }
}

/// `duration` in seconds, to the millisecond, as the profile shows times.
fn seconds(duration: Duration) -> String {
    format!("{:.3} s", duration.as_secs_f64())
}

/// `duration` in milliseconds.
fn millis(duration: Duration) -> f64 {
    duration.as_secs_f64() * 1000.0
}
