//! Tests of the machine's budget: its limits, how programs wait for it, and
//! what they are expected to use (see [`pekit::budget`]).

use std::path::Path;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Barrier};
use std::thread;
use std::time::{Duration, Instant};

use pekit::budget::{Budget, DEFAULT_UNSEEN, Limits, STARVED, UNIT, keep_peak};

/// The environment `vars` sets, as [`Limits::of_machine`] reads one.
fn env<'a>(vars: &'a [(&str, &str)]) -> impl Fn(&str) -> Option<String> + 'a {
    move |name| {
        vars.iter()
            .find(|(set, _)| *set == name)
            .map(|(_, value)| (*value).to_owned())
    }
}

/// A budget in `dir` of `jobs` jobs and `units` units of memory.
fn budget(dir: &Path, jobs: usize, units: u64) -> Budget {
    let limits = Limits {
        jobs,
        memory: units * UNIT,
        unseen: UNIT,
    };
    Budget::open(&dir.join("budget"), limits, (&dir.join("peaks"), None)).unwrap()
}

/// The most programs that held `budgets` at once, `programs` of them, each
/// on a thread of its own, its budget the next of `budgets` in turn,
/// expected to use `bytes` and holding it for `hold`.
fn most_at_once(budgets: &[Arc<Budget>], programs: usize, bytes: u64, hold: Duration) -> usize {
    let (now, most) = (Arc::new(AtomicUsize::new(0)), Arc::new(AtomicUsize::new(0)));
    let start = Arc::new(Barrier::new(programs));
    let threads: Vec<_> = (0..programs)
        .map(|n| {
            let budget = Arc::clone(&budgets[n % budgets.len()]);
            let (now, most, start) = (Arc::clone(&now), Arc::clone(&most), Arc::clone(&start));
            thread::spawn(move || {
                start.wait();
                let held = budget.admit(bytes);
                let running = now.fetch_add(1, Ordering::SeqCst) + 1;
                most.fetch_max(running, Ordering::SeqCst);
                thread::sleep(hold);
                now.fetch_sub(1, Ordering::SeqCst);
                drop(held);
            })
        })
        .collect();
    for thread in threads {
        thread.join().unwrap();
    }
    most.load(Ordering::SeqCst)
}

#[test]
fn a_machine_has_a_job_per_processor_and_three_quarters_of_its_memory() {
    let limits = Limits::of_machine(&env(&[]), None, (8, 32 << 30)).unwrap();
    assert_eq!(
        limits,
        Limits {
            jobs: 8,
            memory: 24 << 30,
            unseen: DEFAULT_UNSEEN,
        }
    );
}

#[test]
fn the_machines_configuration_sets_limits_and_the_environment_overrides_it() {
    let file = "# this machine\njobs = 4\nmemory = 12GiB # its share\n\nunseen = 2GiB\n";
    let limits = Limits::of_machine(&env(&[]), Some(file), (8, 32 << 30)).unwrap();
    assert_eq!(
        limits,
        Limits {
            jobs: 4,
            memory: 12 << 30,
            unseen: 2 << 30,
        }
    );
    let vars = [("PEKIT_MACHINE_JOBS", "2"), ("PEKIT_MACHINE_MEMORY", "1GiB")];
    let limits = Limits::of_machine(&env(&vars), Some(file), (8, 32 << 30)).unwrap();
    assert_eq!(
        limits,
        Limits {
            jobs: 2,
            memory: 1 << 30,
            unseen: 2 << 30,
        }
    );
}

#[test]
fn a_limit_that_says_nothing_is_an_error() {
    let machine = (8, 32 << 30);
    let error = Limits::of_machine(&env(&[]), Some("jobs = 0\n"), machine).unwrap_err();
    assert!(error.contains("line 1") && error.contains("number of jobs"), "{error}");
    let error = Limits::of_machine(&env(&[]), Some("\ncores = 4\n"), machine).unwrap_err();
    assert!(error.contains("line 2") && error.contains("`cores`"), "{error}");
    let error = Limits::of_machine(&env(&[]), Some("jobs\n"), machine).unwrap_err();
    assert!(error.contains("KEY = VALUE"), "{error}");
    let vars = [("PEKIT_MACHINE_MEMORY", "lots")];
    let error = Limits::of_machine(&env(&vars), None, machine).unwrap_err();
    assert!(error.contains("PEKIT_MACHINE_MEMORY") && error.contains("no size"), "{error}");
}

#[test]
fn a_program_takes_at_least_one_unit_and_at_most_all() {
    let limits = Limits {
        jobs: 4,
        memory: 4 * UNIT,
        unseen: UNIT,
    };
    assert_eq!(limits.units(), 4);
    assert_eq!(limits.units_for(0), 1);
    assert_eq!(limits.units_for(UNIT + 1), 2);
    assert_eq!(limits.units_for(100 * UNIT), 4);
}

#[test]
fn no_more_programs_run_at_once_than_the_budget_has_jobs() {
    let dir = tempfile::tempdir().unwrap();
    let budgets = [Arc::new(budget(dir.path(), 2, 64))];
    let most = most_at_once(&budgets, 8, UNIT, Duration::from_millis(30));
    assert_eq!(most, 2);
}

#[test]
fn no_more_memory_is_expected_to_be_used_than_the_budget_has() {
    let dir = tempfile::tempdir().unwrap();
    let budgets = [Arc::new(budget(dir.path(), 8, 3))];
    // Each expected to use more than one unit takes two of the three.
    let most = most_at_once(&budgets, 6, UNIT + 1, Duration::from_millis(30));
    assert_eq!(most, 1);
    let most = most_at_once(&budgets, 6, UNIT, Duration::from_millis(30));
    assert_eq!(most, 3);
}

#[test]
fn processes_share_one_budget() {
    let dir = tempfile::tempdir().unwrap();
    // Each opens the lock files for itself, as each process does.
    let budgets: Vec<Arc<Budget>> = (0..3)
        .map(|_| Arc::new(budget(dir.path(), 3, 64)))
        .collect();
    let most = most_at_once(&budgets, 12, UNIT, Duration::from_millis(30));
    assert_eq!(most, 3);
}

#[test]
fn a_program_larger_than_the_budget_runs_alone() {
    let dir = tempfile::tempdir().unwrap();
    let small = Arc::new(budget(dir.path(), 4, 2));
    let large = budget(dir.path(), 4, 2);
    let held = small.admit(UNIT);
    let waiting = thread::spawn(move || {
        let held = large.admit(100 * UNIT);
        let slots = held.slots();
        drop(held);
        slots
    });
    thread::sleep(Duration::from_millis(100));
    assert!(!waiting.is_finished(), "it ran beside another program");
    drop(held);
    // Its job and every unit.
    assert_eq!(waiting.join().unwrap(), 3);
}

#[test]
fn a_large_program_is_not_starved_by_small_ones() {
    let dir = tempfile::tempdir().unwrap();
    let small = Arc::new(budget(dir.path(), 4, 2));
    let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
    // Two programs one after another on each of two threads, so that one
    // unit or both are always held.
    let threads: Vec<_> = (0..2)
        .map(|_| {
            let (small, stop) = (Arc::clone(&small), Arc::clone(&stop));
            thread::spawn(move || {
                while !stop.load(Ordering::SeqCst) {
                    let held = small.admit(UNIT);
                    thread::sleep(Duration::from_millis(20));
                    drop(held);
                }
            })
        })
        .collect();
    thread::sleep(Duration::from_millis(50));
    let large = budget(dir.path(), 4, 2);
    let began = Instant::now();
    let held = large.admit(2 * UNIT);
    let waited = began.elapsed();
    drop(held);
    stop.store(true, Ordering::SeqCst);
    for thread in threads {
        thread.join().unwrap();
    }
    assert!(
        waited < STARVED + Duration::from_secs(2),
        "it waited {waited:?}"
    );
}

#[test]
fn a_jobserver_borrows_only_free_jobs_and_none_while_a_program_waits() {
    let dir = tempfile::tempdir().unwrap();
    let budget = Arc::new(budget(dir.path(), 2, 64));
    assert!(!budget.others_wait().unwrap());
    let first = budget.admit(UNIT);
    let lent = budget.try_job().unwrap().expect("a job is free");
    assert!(budget.try_job().unwrap().is_none());
    // A program that waits for the budget is seen waiting.
    let waiting = {
        let budget = Arc::clone(&budget);
        thread::spawn(move || budget.admit(UNIT).waited())
    };
    let began = Instant::now();
    while !budget.others_wait().unwrap() {
        assert!(began.elapsed() < Duration::from_secs(5), "it was never seen");
        thread::sleep(Duration::from_millis(5));
    }
    drop(lent);
    waiting.join().unwrap();
    drop(first);
    assert!(!budget.others_wait().unwrap());
}

#[test]
fn a_program_is_expected_to_use_a_quarter_more_than_it_did() {
    let dir = tempfile::tempdir().unwrap();
    let budget = budget(dir.path(), 2, 64);
    let peaks = dir.path().join("peaks");
    assert_eq!(budget.estimate("compile/aaaa", "rustc"), UNIT);
    keep_peak(&peaks, ("compile/aaaa", "rustc"), 400 << 20).unwrap();
    keep_peak(&peaks, ("compile/aaaa", "rustc"), 800 << 20).unwrap();
    assert_eq!(budget.estimate("compile/aaaa", "rustc"), 1000 << 20);
    // Another step of its kind, never metered, is expected to use what its
    // kind did; another program, never metered, the default.
    assert_eq!(budget.estimate("compile/bbbb", "rustc"), 1000 << 20);
    assert_eq!(budget.estimate("compile/aaaa", "cc"), UNIT);
    // Only the last few peaks count.
    for _ in 0..4 {
        keep_peak(&peaks, ("compile/aaaa", "rustc"), 100 << 20).unwrap();
    }
    assert_eq!(budget.estimate("compile/aaaa", "rustc"), 125 << 20);
}
