//! The Chrome trace: a build's events as a timeline.
//!
//! # The trace
//!
//! `pekit build --trace FILE` writes the build's [events](crate::events) as
//! a trace in the Trace Event Format, which `chrome://tracing` and Perfetto
//! show. Each attempt of a step is a span on the *steps* row, a step found
//! in the store a mark there, and each program run a span on the row of the
//! pool's thread that ran it, named by what it ran and labelled with its
//! step. Times are in microseconds since the build began.

use serde_json::{Value, json};

use crate::events::{Event, EventKind};

/// The process every row of the trace is in.
const PID: u32 = 1;

/// The row steps are on; pool threads' rows follow it.
const STEPS_ROW: usize = 0;

/// A trace being made from a build's events (see [The trace](#the-trace)).
#[derive(Debug, Default)]
pub struct Trace {
    /// Its trace events so far.
    events: Vec<Value>,
    /// How many pool threads' rows it has named.
    workers: usize,
}

/// `duration` in whole microseconds, as the trace has times.
fn micros(duration: std::time::Duration) -> u64 {
    u64::try_from(duration.as_micros()).unwrap_or(u64::MAX)
}

impl Trace {
    /// A trace of no events.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Adds `event` to the trace.
    pub fn add(&mut self, event: &Event) {
        let ts = micros(event.at);
        let step_span = |phase: &str, step: &str, attempt: u32| {
            json!({
                "name": step, "cat": "step", "ph": phase, "ts": ts,
                "pid": PID, "tid": STEPS_ROW,
                "id": format!("{step}#{attempt}"),
            })
        };
        match &event.kind {
            EventKind::Discovered { .. } | EventKind::EffectQueued { .. } => {}
            EventKind::Cached { step } => self.events.push(json!({
                "name": format!("{step} (found in the store)"), "cat": "step",
                "ph": "i", "s": "t", "ts": ts, "pid": PID, "tid": STEPS_ROW,
            })),
            EventKind::EffectFound { step, effect, .. } => self.events.push(json!({
                "name": format!("{effect} (found in the store)"), "cat": "effect",
                "ph": "i", "s": "t", "ts": ts, "pid": PID, "tid": STEPS_ROW,
                "args": { "step": step },
            })),
            EventKind::Started { step, attempt } => {
                self.events.push(step_span("b", step, *attempt));
            }
            EventKind::Finished { step } => {
                let attempt = self.last_attempt(step);
                self.events.push(step_span("e", step, attempt));
            }
            EventKind::Failed { step, retrying } => {
                let attempt = self.last_attempt(step);
                let mut end = step_span("e", step, attempt);
                end["args"] = json!({ "failed": true, "retrying": retrying });
                self.events.push(end);
            }
            EventKind::EffectRan {
                step,
                effect,
                worker,
                began,
                ok,
                ..
            } => {
                self.name_workers(worker + 1);
                let began = micros(*began);
                self.events.push(json!({
                    "name": effect, "cat": "effect", "ph": "X",
                    "ts": began, "dur": ts.saturating_sub(began),
                    "pid": PID, "tid": worker + 1 + STEPS_ROW,
                    "args": { "step": step.as_deref().unwrap_or("(the target)"), "ok": ok },
                }));
            }
        }
    }

    /// The number of the attempt of `step` that began last.
    fn last_attempt(&self, step: &str) -> u32 {
        self.events
            .iter()
            .rev()
            .find(|event| event["ph"] == "b" && event["name"] == step)
            .and_then(|event| event["id"].as_str())
            .and_then(|id| id.rsplit('#').next())
            .and_then(|attempt| attempt.parse().ok())
            .unwrap_or(0)
    }

    /// Names the rows of the first `count` pool threads, if they are not.
    fn name_workers(&mut self, count: usize) {
        while self.workers < count {
            self.workers += 1;
            self.events.push(json!({
                "name": "thread_name", "ph": "M", "pid": PID,
                "tid": self.workers + STEPS_ROW,
                "args": { "name": format!("worker {}", self.workers) },
            }));
        }
    }

    /// The trace as the Trace Event Format's JSON.
    #[must_use]
    pub fn to_json(&self) -> Value {
        let mut events = vec![
            json!({
                "name": "process_name", "ph": "M", "pid": PID,
                "args": { "name": "pekit build" },
            }),
            json!({
                "name": "thread_name", "ph": "M", "pid": PID, "tid": STEPS_ROW,
                "args": { "name": "steps" },
            }),
        ];
        events.extend(self.events.iter().cloned());
        json!({ "traceEvents": events, "displayTimeUnit": "ms" })
    }
}
