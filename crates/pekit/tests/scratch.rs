//! Tests of scratch directories: what a step marked `#[scratch]` keeps
//! between its runs (see `design/additive.md`, F2).

mod building;

use std::fs::{self, File};
use std::path::{Path, PathBuf};

use building::{Options, build_in, observed_with};

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// A workspace whose `app` builds a step marked `#[scratch]`, given
/// `name.txt`, `text`, which counts its runs in its scratch directory and
/// says how many runs it found there: what a step must not do, for a test
/// to see what it was given.
fn workspace(text: &str) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                "#[hermetic] #[scratch] def counts(src: File) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"echo run >> /pekit-scratch/runs; \
                 wc -l < /pekit-scratch/runs > out; cat {src.path} >> out\"])\n\
                 \x20   = working_dir().file(\"out\")\n\
                 }\n",
            ),
            ("app/name.txt", text),
            (
                "app/build.gcsl",
                "def build both() { = counts(file(\"name.txt\")) }\n",
            ),
        ],
    );
    dir
}

/// What the step wrote, built in `dir`.
fn out(dir: &Path) -> String {
    fs::read_to_string(dir.join("pekit-out/app/both/counts/out")).unwrap()
}

/// The scratch directories the store at `state` keeps.
fn kept(state: &Path) -> Vec<PathBuf> {
    let Ok(entries) = fs::read_dir(state.join("store/scratch")) else {
        return Vec::new();
    };
    let mut kept: Vec<PathBuf> = entries
        .flatten()
        .map(|entry| entry.path())
        .filter(|path| path.is_dir())
        .collect();
    kept.sort();
    kept
}

#[test]
fn a_step_s_scratch_directory_is_kept_between_its_runs() {
    let dir = workspace("one\n");
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(out(dir.path()), "1\none\n");
    // Another input: the step runs again, and finds what it kept.
    write(dir.path(), &[("app/name.txt", "two\n")]);
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((again.ran, again.cached), (1, 0));
    assert_eq!(out(dir.path()), "2\ntwo\n");
    let [kept] = &kept(state.path())[..] else {
        panic!("{:?}", kept(state.path()));
    };
    assert_eq!(fs::read_to_string(kept.join("runs")).unwrap(), "run\nrun\n");
}

#[test]
fn a_scratch_directory_keys_nothing() {
    let dir = workspace("one\n");
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    let [kept] = &kept(state.path())[..] else {
        panic!("{:?}", kept(state.path()));
    };
    fs::write(kept.join("other"), "anything").unwrap();
    // What it holds changed: the step is found all the same.
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((again.ran, again.cached), (0, 1));
}

#[test]
fn a_strict_build_gives_a_step_an_empty_scratch_directory() {
    let busybox = Path::new("/usr/bin/busybox");
    if !busybox.exists() {
        eprintln!("skipped: no static busybox on this host");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                "#[hermetic] #[scratch] def counts(bb: File, src: File) -> File {\n\
                 \x20   let r = exec([bb.path, \"sh\", \"-c\", \"echo run >> /pekit-scratch/runs; \
                 {bb.path} wc -l < /pekit-scratch/runs > out; {bb.path} cat {src.path} >> out\"])\n\
                 \x20   = working_dir().file(\"out\")\n\
                 }\n",
            ),
            ("app/name.txt", "one\n"),
            (
                "app/build.gcsl",
                "def build both() { = counts(file(\"busybox\"), file(\"name.txt\")) }\n",
            ),
        ],
    );
    fs::copy(busybox, dir.path().join("app/busybox")).unwrap();
    let state = tempfile::tempdir().unwrap();
    let strict = Options {
        strict: true,
        ..Options::default()
    };
    observed_with(dir.path(), state.path(), "both", strict)
        .0
        .unwrap();
    assert_eq!(out(dir.path()), "1\none\n");
    write(dir.path(), &[("app/name.txt", "two\n")]);
    observed_with(dir.path(), state.path(), "both", strict)
        .0
        .unwrap();
    assert_eq!(out(dir.path()), "1\ntwo\n");
    assert!(kept(state.path()).is_empty());
}

#[test]
fn a_scratch_directory_another_run_holds_is_not_shared() {
    let dir = workspace("one\n");
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    let [kept] = &kept(state.path())[..] else {
        panic!("{:?}", kept(state.path()));
    };
    // Another run holds it: this one is given an empty one of its own,
    // and neither waits.
    let lock = File::options()
        .write(true)
        .open(kept.with_extension("lock"))
        .unwrap();
    lock.lock().unwrap();
    write(dir.path(), &[("app/name.txt", "two\n")]);
    build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(out(dir.path()), "1\ntwo\n");
    assert_eq!(fs::read_to_string(kept.join("runs")).unwrap(), "run\n");
    drop(lock);
    // Free again, it is the step's.
    write(dir.path(), &[("app/name.txt", "three\n")]);
    build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(out(dir.path()), "2\nthree\n");
}

#[test]
fn a_step_s_programs_neither_keep_nor_place_its_scratch_directory() {
    let dir = tempfile::tempdir().unwrap();
    let text = |comment: &str| {
        format!(
            "# {comment}\n\
             #[hermetic] #[scratch] def warm(src: File) -> File {{\n\
             \x20   let r = exec([\"sh\", \"-c\", \"cat {{src.path}} > out; echo x >> /pekit-scratch/log\"])\n\
             \x20   = working_dir().file(\"out\")\n\
             }}\n"
        )
    };
    write(
        dir.path(),
        &[
            ("workspace.gcsl", &text("one")),
            ("app/name.txt", "n\n"),
            (
                "app/build.gcsl",
                "def build both() { = warm(file(\"name.txt\")) }\n",
            ),
        ],
    );
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    // Its code changed: its program is found, and its scratch directory is
    // left as it was, not put back nor written.
    write(dir.path(), &[("workspace.gcsl", &text("two"))]);
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((again.ran, again.programs_found), (1, 1));
    let [kept] = &kept(state.path())[..] else {
        panic!("{:?}", kept(state.path()));
    };
    assert_eq!(fs::read_to_string(kept.join("log")).unwrap(), "x\n");
}
