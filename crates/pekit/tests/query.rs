//! Tests of `pekit query`: what targets reach, found without running a
//! step, run as someone runs it.

use std::fs;
use std::path::Path;
use std::process::{Command, Output};

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// A workspace whose `app` has `chain`, which joins `in.txt` shouted and
/// a file of its own, `waits`, which reads what a step made, and a test of
/// `chain`; and whose `lib` has a build of its own.
fn workspace() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            (
                "app/build.gcsl",
                "#[hermetic] fn upper(src: File) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"tr a-z A-Z < {src.path} > up\"])\n\
                 \x20   working_dir().file(\"up\")\n\
                 }\n\
                 #[hermetic] fn steady() -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"echo steady > s\"])\n\
                 \x20   working_dir().file(\"s\")\n\
                 }\n\
                 #[hermetic] fn joined(a: File, b: File) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"cat {a.path} {b.path} > j\"])\n\
                 \x20   working_dir().file(\"j\")\n\
                 }\n\
                 def build chain() { = joined(upper(file(\"in.txt\")), steady()) }\n\
                 def build waits() {\n\
                 \x20   let made = steady()\n\
                 \x20   = made.read()\n\
                 }\n\
                 def test joins(made: File = build chain()) { assert made.read() != \"\" }\n",
            ),
            ("app/in.txt", "in\n"),
            (
                "lib/build.gcsl",
                "def build words() { = file(\"words.txt\").read() }\n",
            ),
            ("lib/words.txt", "w\n"),
        ],
    );
    dir
}

/// `pekit` run with `args` in `dir`, its cache and state in `cache`.
fn pekit(dir: &Path, cache: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_pekit"))
        .args(args)
        .current_dir(dir)
        .env("PEKIT_CACHE_DIR", cache)
        .env("PEKIT_STATE_DIR", cache.join("state"))
        .output()
        .unwrap()
}

/// `text` with each step's identity, `.steps/<function>/<identity>`, as
/// `.steps/<function>/*`: it is a digest of what the step is, the platform
/// among it.
fn masked(text: &str) -> String {
    let mut out = String::new();
    let mut rest = text;
    while let Some(at) = rest.find(".steps/") {
        let (before, after) = rest.split_at(at + ".steps/".len());
        out.push_str(before);
        let function = after.find('/').map_or(after.len(), |end| end + 1);
        out.push_str(&after[..function]);
        let after = &after[function..];
        let digits = after.chars().take_while(char::is_ascii_hexdigit).count();
        out.push('*');
        rest = &after[digits..];
    }
    out.push_str(rest);
    out
}

/// What `output` printed on its standard output and standard error.
fn shown(output: &Output) -> (String, String) {
    (
        String::from_utf8_lossy(&output.stdout).into_owned(),
        String::from_utf8_lossy(&output.stderr).into_owned(),
    )
}

#[test]
fn deps_lists_each_step_a_target_reaches_and_what_each_reads() {
    let dir = workspace();
    let cache = tempfile::tempdir().unwrap();
    let app = dir.path().join("app");
    let queried = pekit(
        &app,
        cache.path(),
        &["query", "deps", "chain", "waits", "lib/words"],
    );
    let (out, err) = shown(&queried);
    assert!(queried.status.success(), "{err}");
    // `waits` reaches the step `chain` does, the same call: one step.
    assert_eq!(
        masked(&out),
        "app/chain\n\
         \x20 code: app/build.gcsl\n\
         \x20 step .steps/upper/*\n\
         \x20   code: app/build.gcsl\n\
         \x20   reads: app/in.txt\n\
         \x20 step .steps/steady/*\n\
         \x20   code: app/build.gcsl\n\
         \x20 step .steps/joined/*\n\
         \x20   code: app/build.gcsl\n\
         \x20   after: .steps/steady/*, .steps/upper/*\n\
         app/waits\n\
         \x20 code: app/build.gcsl\n\
         \x20 step .steps/steady/*\n\
         \x20   code: app/build.gcsl\n\
         \x20 unknown: it waits at app/build.gcsl:15 for a step's result, so what it reaches \
         after is known only once that step runs\n\
         lib/words\n\
         \x20 code: lib/build.gcsl\n\
         \x20 reads: lib/words.txt\n"
    );
    // No step ran.
    assert!(!dir.path().join("pekit-out").exists());
}

#[test]
fn rdeps_lists_the_targets_that_read_a_file() {
    let dir = workspace();
    let cache = tempfile::tempdir().unwrap();
    let rdeps = |args: &[&str]| {
        let mut all = vec!["query", "rdeps"];
        all.extend(args);
        let queried = pekit(dir.path(), cache.path(), &all);
        assert!(queried.status.success(), "{:?}", shown(&queried));
        shown(&queried)
    };
    // A step's input; `waits` might read it after the step it waits for.
    let (out, err) = rdeps(&["app/in.txt"]);
    assert_eq!(out, "app/chain\napp/waits\n");
    assert_eq!(
        err,
        "note: `app/waits` waits at app/build.gcsl:15 for a step's result, so it might read \
         `app/in.txt` after: it is listed\n"
    );
    // A file its own code reads, and `waits` again, which might read any.
    assert_eq!(rdeps(&["lib/words.txt"]).0, "app/waits\nlib/words\n");
    // A GCSL file is read by what is in its directory or below, there yet
    // or not; the workspace's by everything.
    assert_eq!(rdeps(&["lib/more.gcsl"]).0, "app/waits\nlib/words\n");
    assert_eq!(
        rdeps(&["workspace.gcsl"]).0,
        "app/chain\napp/waits\nlib/words\n"
    );
    // Tests, among those named.
    assert_eq!(rdeps(&["app/in.txt", "--kind", "test"]).0, "app/joins\n");
    assert_eq!(rdeps(&["app/in.txt", "lib/*"]).0, "");
    // A file outside the workspace is no file of it.
    let outside = pekit(
        dir.path(),
        cache.path(),
        &["query", "rdeps", "/etc/hostname"],
    );
    assert!(!outside.status.success());
    assert!(
        shown(&outside)
            .1
            .starts_with("error: `/etc/hostname` is not in the workspace")
    );
}
