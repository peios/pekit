//! Tests of the machine's budget as builds share it: several `pekit`
//! processes at once against one store, the peaks their programs are
//! metered at, and the jobserver they are offered (see [`pekit::budget`]
//! and [`pekit::meter`]).

use std::fs;
use std::path::Path;
use std::process::{Child, Command, Output, Stdio};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::thread;
use std::time::Duration;

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// A workspace whose `naps` naps `count` times at once, a step each, each
/// nap a shell whose last word is `marker`, numbered from `first`, so that
/// another workspace's naps are other steps.
fn napping(marker: &str, first: u32, count: u32) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    let naps: Vec<String> = (first..first + count)
        .map(|n| format!("nap({n})"))
        .collect();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            (
                "build.gcsl",
                &format!(
                    "#[hermetic] fn nap(n: Int) -> Int {{\n\
                     \x20   let r = exec([\"sh\", \"-c\", \"sleep 0.6; true\", \"{marker}\"])\n\
                     \x20   n\n\
                     }}\n\
                     def build naps() {{ all = [{}] }}\n",
                    naps.join(", ")
                ),
            ),
        ],
    );
    dir
}

/// `pekit` started with `args` in `dir`, its cache, its state and the
/// machine's budget in `cache`, the environment `env` set.
fn start(dir: &Path, cache: &Path, args: &[&str], env: &[(&str, &str)]) -> Child {
    let mut command = Command::new(env!("CARGO_BIN_EXE_pekit"));
    command
        .args(args)
        .current_dir(dir)
        .env("PEKIT_CACHE_DIR", cache)
        .env("PEKIT_STATE_DIR", cache.join("state"))
        .env("XDG_RUNTIME_DIR", cache.join("run"))
        .env("XDG_CONFIG_HOME", cache.join("config"))
        .env_remove("PEKIT_MACHINE_JOBS")
        .env_remove("PEKIT_MACHINE_MEMORY")
        .env_remove("PEKIT_MACHINE_UNSEEN")
        .env_remove("PEKIT_REMOTE_CACHE")
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    for (name, value) in env {
        command.env(name, value);
    }
    command.spawn().unwrap()
}

/// How many processes run now whose program is `sh` and whose last word is
/// `marker`.
fn running(marker: &str) -> usize {
    let Ok(entries) = fs::read_dir("/proc") else {
        return 0;
    };
    entries
        .flatten()
        .filter(|entry| {
            let Ok(line) = fs::read(entry.path().join("cmdline")) else {
                return false;
            };
            let words: Vec<&[u8]> = line
                .split(|&byte| byte == 0)
                .filter(|word| !word.is_empty())
                .collect();
            words.first().is_some_and(|word| word.ends_with(b"sh"))
                && words.last() == Some(&marker.as_bytes())
        })
        .count()
}

/// What `children` ended with, and the most processes [`running`] counted
/// for `marker` at once while they ran.
fn watched(children: Vec<Child>, marker: &str) -> (Vec<Output>, usize) {
    let (done, most) = (
        Arc::new(AtomicBool::new(false)),
        Arc::new(AtomicUsize::new(0)),
    );
    let watcher = {
        let (done, most, marker) = (Arc::clone(&done), Arc::clone(&most), marker.to_owned());
        thread::spawn(move || {
            while !done.load(Ordering::SeqCst) {
                most.fetch_max(running(&marker), Ordering::SeqCst);
                thread::sleep(Duration::from_millis(5));
            }
        })
    };
    let outputs = children
        .into_iter()
        .map(|child| child.wait_with_output().unwrap())
        .collect();
    done.store(true, Ordering::SeqCst);
    watcher.join().unwrap();
    (outputs, most.load(Ordering::SeqCst))
}

/// What `output` printed to standard output and to standard error.
fn printed(output: &Output) -> (String, String) {
    (
        String::from_utf8_lossy(&output.stdout).into_owned(),
        String::from_utf8_lossy(&output.stderr).into_owned(),
    )
}

#[test]
fn builds_at_once_never_run_more_programs_than_the_budget_has_jobs() {
    let marker = format!("pekit-budget-jobs-{}", std::process::id());
    let cache = tempfile::tempdir().unwrap();
    let workspaces: Vec<_> = (0..3).map(|n| napping(&marker, n * 10, 4)).collect();
    let budget = [("PEKIT_MACHINE_JOBS", "2")];
    let children = workspaces
        .iter()
        .map(|dir| start(dir.path(), cache.path(), &["build", "naps"], &budget))
        .collect();
    let (outputs, most) = watched(children, &marker);
    for output in &outputs {
        let (out, err) = printed(output);
        assert!(output.status.success(), "{err}");
        assert!(out.contains("4 steps run"), "{out}");
    }
    assert_eq!(most, 2, "programs at once");
}

#[test]
fn the_budget_reaches_no_receipt() {
    let marker = format!("pekit-budget-keys-{}", std::process::id());
    let cache = tempfile::tempdir().unwrap();
    let dir = napping(&marker, 0, 2);
    let first = start(
        dir.path(),
        cache.path(),
        &["build", "naps"],
        &[("PEKIT_MACHINE_JOBS", "1")],
    );
    let first = first.wait_with_output().unwrap();
    assert!(first.status.success(), "{}", printed(&first).1);
    let again = start(
        dir.path(),
        cache.path(),
        &["build", "naps"],
        &[("PEKIT_MACHINE_JOBS", "3")],
    );
    let (out, err) = printed(&again.wait_with_output().unwrap());
    assert!(
        out.contains("0 steps run, 2 found in the store"),
        "{out}{err}"
    );
}

#[test]
fn programs_expected_to_use_more_memory_than_is_left_wait() {
    let marker = format!("pekit-budget-memory-{}", std::process::id());
    let cache = tempfile::tempdir().unwrap();
    let dir = napping(&marker, 0, 4);
    // Each nap, never metered, is expected to use two of the budget's three
    // units, so one runs at a time, though there are jobs for all.
    let budget = [
        ("PEKIT_MACHINE_JOBS", "8"),
        ("PEKIT_MACHINE_MEMORY", "768MiB"),
        ("PEKIT_MACHINE_UNSEEN", "300MiB"),
    ];
    let child = start(dir.path(), cache.path(), &["build", "naps"], &budget);
    let (outputs, most) = watched(vec![child], &marker);
    assert!(outputs[0].status.success(), "{}", printed(&outputs[0]).1);
    assert_eq!(most, 1, "programs at once");
}

#[test]
fn a_programs_peak_is_kept() {
    let cache = tempfile::tempdir().unwrap();
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            (
                "build.gcsl",
                "#[hermetic] fn hold(n: Int) -> Int {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"x=$(head -c 200000000 /dev/zero | tr '\\\\0' a); true\"])\n\
                 \x20   n\n\
                 }\n\
                 def build held() { n = hold(1) }\n",
            ),
        ],
    );
    let child = start(dir.path(), cache.path(), &["build", "held"], &[]);
    let output = child.wait_with_output().unwrap();
    assert!(output.status.success(), "{}", printed(&output).1);
    // The step's own and its kind's, each the shell's peak, which held the
    // 200 MB it read.
    let peaks: Vec<u64> = fs::read_dir(cache.path().join("state/peaks"))
        .unwrap()
        .map(|entry| {
            let text = fs::read_to_string(entry.unwrap().path()).unwrap();
            text.trim().parse().unwrap()
        })
        .collect();
    assert_eq!(peaks.len(), 2, "{peaks:?}");
    assert!(
        peaks
            .iter()
            .all(|&peak| peak > 200_000_000 && peak < 2_000_000_000),
        "{peaks:?}"
    );
}

#[test]
fn make_borrows_the_budgets_jobs_through_its_jobserver() {
    let marker = format!("pekit-budget-make-{}", std::process::id());
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            (
                "Makefile",
                &format!(
                    "all: a b c d e f\n\
                     a b c d e f:\n\
                     \tsh -c 'sleep 0.6; true' {marker}\n"
                ),
            ),
            (
                "build.gcsl",
                "#[hermetic] fn made(rules: File, jobs: Int) -> String {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"echo $MAKEFLAGS; make -s -f {rules.path}\"])\n\
                 \x20   r.stdout\n\
                 }\n\
                 def build one() { flags = made(file(\"Makefile\"), 1) }\n\
                 def build three() { flags = made(file(\"Makefile\"), 3) }\n",
            ),
        ],
    );
    for (target, jobs) in [("one", 1), ("three", 3)] {
        let cache = tempfile::tempdir().unwrap();
        let budget = [("PEKIT_MACHINE_JOBS", &*jobs.to_string())];
        let child = start(dir.path(), cache.path(), &["build", target], &budget);
        let (outputs, most) = watched(vec![child], &marker);
        let (out, err) = printed(&outputs[0]);
        assert!(outputs[0].status.success(), "{err}");
        assert!(out.contains("1 step run"), "{out}");
        // One job, make's own, and as many more as are free, never more.
        if jobs == 1 {
            assert_eq!(most, 1, "jobs at once with one");
        } else {
            assert!(
                (2..=jobs).contains(&most),
                "{most} jobs at once with {jobs}"
            );
        }
    }
}

#[test]
fn a_program_is_offered_the_jobserver_unless_it_names_its_own() {
    let cache = tempfile::tempdir().unwrap();
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            (
                "build.gcsl",
                "#[hermetic] fn offered(n: Int) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"echo \\\"$MAKEFLAGS|$CARGO_MAKEFLAGS\\\" > out; \
                 test -p /dev/pekit-jobserver\"])\n\
                 \x20   working_dir().file(\"out\")\n\
                 }\n\
                 #[hermetic] fn own(n: Int) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"echo \\\"$MAKEFLAGS|$CARGO_MAKEFLAGS\\\" > out\"], \
                 env = {MAKEFLAGS = \"-j1\"})\n\
                 \x20   working_dir().file(\"out\")\n\
                 }\n\
                 def build flags() { offered = offered(1)\n own = own(1) }\n",
            ),
        ],
    );
    let child = start(dir.path(), cache.path(), &["build", "flags"], &[]);
    let output = child.wait_with_output().unwrap();
    assert!(output.status.success(), "{}", printed(&output).1);
    let out = dir.path().join("pekit-out/flags");
    let auth = "--jobserver-auth=fifo:/dev/pekit-jobserver";
    assert_eq!(
        fs::read_to_string(out.join("offered/out")).unwrap(),
        format!("{auth}|{auth}\n")
    );
    assert_eq!(
        fs::read_to_string(out.join("own/out")).unwrap(),
        format!("-j1|{auth}\n")
    );
}

/// Waits until a process [`running`] counts for `marker` runs, for ten
/// seconds at most.
fn wait_for(marker: &str) {
    for _ in 0..2000 {
        if running(marker) > 0 {
            return;
        }
        thread::sleep(Duration::from_millis(5));
    }
    panic!("nothing ran");
}

#[test]
fn a_step_two_builds_want_at_once_runs_once() {
    let marker = format!("pekit-claim-once-{}", std::process::id());
    let cache = tempfile::tempdir().unwrap();
    // Two worktrees at one commit.
    let (first, second) = (napping(&marker, 0, 1), napping(&marker, 0, 1));
    let one = start(first.path(), cache.path(), &["build", "naps"], &[]);
    wait_for(&marker);
    let two = start(second.path(), cache.path(), &["build", "naps"], &[]);
    let (outputs, most) = watched(vec![one, two], &marker);
    let shown: Vec<String> = outputs.iter().map(|output| printed(output).0).collect();
    for output in &outputs {
        assert!(output.status.success(), "{}", printed(output).1);
    }
    assert!(shown[0].contains("1 step run, 0 found"), "{shown:?}");
    assert!(
        shown[1].contains("0 steps run, 1 found in the store"),
        "{shown:?}"
    );
    assert_eq!(most, 1);
}

#[test]
fn a_step_whose_build_died_is_run_by_the_build_that_waited() {
    let marker = format!("pekit-claim-died-{}", std::process::id());
    let cache = tempfile::tempdir().unwrap();
    let (first, second) = (napping(&marker, 0, 1), napping(&marker, 0, 1));
    let mut one = start(first.path(), cache.path(), &["build", "naps"], &[]);
    wait_for(&marker);
    let two = start(second.path(), cache.path(), &["build", "naps"], &[]);
    thread::sleep(Duration::from_millis(200));
    one.kill().unwrap();
    one.wait().unwrap();
    let output = two.wait_with_output().unwrap();
    let (out, err) = printed(&output);
    assert!(output.status.success(), "{err}");
    assert!(out.contains("1 step run, 0 found"), "{out}");
}
