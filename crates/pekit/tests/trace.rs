//! Tests of the Chrome trace made from a build's events.

use std::time::Duration;

use pekit::events::{Event, EventKind};
use pekit::trace::Trace;

/// The event `kind` at `ms` milliseconds.
fn at(ms: u64, kind: EventKind) -> Event {
    Event {
        at: Duration::from_millis(ms),
        kind,
    }
}

#[test]
fn a_trace_has_a_span_per_attempt_and_per_program_on_its_thread() {
    let mut trace = Trace::new();
    let step = || "cc".to_owned();
    for event in [
        at(0, EventKind::Discovered { step: step() }),
        at(
            1,
            EventKind::Cached {
                step: "ld".to_owned(),
            },
        ),
        at(
            2,
            EventKind::Started {
                step: step(),
                attempt: 0,
            },
        ),
        at(
            3,
            EventKind::EffectQueued {
                node: 4,
                step: Some(step()),
                effect: "exec cc".to_owned(),
            },
        ),
        at(
            9,
            EventKind::EffectRan {
                node: 4,
                step: Some(step()),
                effect: "exec cc".to_owned(),
                worker: 1,
                began: Duration::from_millis(4),
                ok: false,
            },
        ),
        at(
            10,
            EventKind::Failed {
                step: step(),
                retrying: true,
            },
        ),
        at(
            11,
            EventKind::Started {
                step: step(),
                attempt: 1,
            },
        ),
        at(20, EventKind::Finished { step: step() }),
    ] {
        trace.add(&event);
    }
    let json = trace.to_json();
    let events = json["traceEvents"].as_array().unwrap();
    let find = |ph: &str| -> Vec<&serde_json::Value> {
        events.iter().filter(|event| event["ph"] == ph).collect()
    };
    // Two attempts, each a span on the steps row.
    let begins = find("b");
    let ends = find("e");
    assert_eq!(begins.len(), 2);
    assert_eq!(ends.len(), 2);
    assert_eq!(begins[0]["id"], "cc#0");
    assert_eq!(ends[0]["id"], "cc#0");
    assert_eq!(ends[0]["args"]["retrying"], true);
    assert_eq!(begins[1]["id"], "cc#1");
    assert_eq!(ends[1]["id"], "cc#1");
    assert_eq!(ends[1]["ts"], 20_000);
    // The program, on its thread's row, from when it began for as long as
    // it ran.
    let ran = find("X");
    assert_eq!(ran.len(), 1);
    assert_eq!(ran[0]["name"], "exec cc");
    assert_eq!(ran[0]["tid"], 3);
    assert_eq!(ran[0]["ts"], 4_000);
    assert_eq!(ran[0]["dur"], 5_000);
    assert_eq!(ran[0]["args"]["step"], "cc");
    // The step found in the store, a mark.
    assert_eq!(find("i")[0]["name"], "ld (found in the store)");
    // Each row named.
    let names: Vec<&str> = find("M")
        .iter()
        .filter_map(|event| event["args"]["name"].as_str())
        .collect();
    assert_eq!(
        names,
        ["pekit build", "steps", "evaluation", "worker 1", "worker 2"]
    );
}

#[test]
fn a_trace_has_a_span_per_run_of_a_target_s_or_a_step_s_code() {
    let mut trace = Trace::new();
    for (ms, unit, step) in [(3, "/app", false), (5, "cc-1a2b", true)] {
        trace.add(&at(
            ms,
            EventKind::Evaluated {
                unit: unit.to_owned(),
                step,
                began: Duration::from_millis(ms - 2),
            },
        ));
    }
    let json = trace.to_json();
    let events = json["traceEvents"].as_array().unwrap();
    // Each run a span on the evaluation row, from when it began.
    let runs: Vec<(&str, u64, u64, bool)> = events
        .iter()
        .filter(|event| event["cat"] == "evaluation")
        .map(|event| {
            assert_eq!(event["tid"], 1);
            (
                event["name"].as_str().unwrap(),
                event["ts"].as_u64().unwrap(),
                event["dur"].as_u64().unwrap(),
                event["args"]["step"].as_bool().unwrap(),
            )
        })
        .collect();
    assert_eq!(
        runs,
        [
            ("/app", 1_000, 2_000, false),
            ("cc-1a2b", 3_000, 2_000, true)
        ]
    );
}
