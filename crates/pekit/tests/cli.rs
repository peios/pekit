//! Tests of the `pekit` command, run as someone runs it.

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

/// A workspace whose `app` has a target `hello` that shouts `name.txt`.
fn workspace() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            (
                "app/build.gcsl",
                "#[hermetic] fn shout(src: File) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"tr a-z A-Z < {src.path} > out.txt\"])\n\
                 \x20   file(\"out.txt\")\n\
                 }\n\
                 def build hello() { out = shout(file(\"name.txt\")) }\n",
            ),
            ("app/name.txt", "world\n"),
        ],
    );
    dir
}

/// `pekit` run with `args` in `dir`, its cache in `cache`.
fn pekit(dir: &Path, cache: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_pekit"))
        .args(args)
        .current_dir(dir)
        .env("PEKIT_CACHE_DIR", cache)
        .env("PEKIT_STATE_DIR", cache.join("state"))
        .output()
        .unwrap()
}

/// What `output` printed to standard output, and to standard error.
fn printed(output: &Output) -> (String, String) {
    (
        String::from_utf8_lossy(&output.stdout).into_owned(),
        String::from_utf8_lossy(&output.stderr).into_owned(),
    )
}

#[test]
fn a_target_is_built_and_then_found_in_the_store() {
    let dir = workspace();
    let cache = tempfile::tempdir().unwrap();
    let app = dir.path().join("app");
    let first = pekit(&app, cache.path(), &["build", "hello"]);
    let (out, err) = printed(&first);
    assert!(first.status.success(), "{err}");
    assert_eq!(
        out,
        "built app/hello: 1 step run, 0 found in the store\n  ../pekit-out/app/hello/shout/out.txt\n"
    );
    assert!(err.contains("not hermetic"), "{err}");
    let built = dir.path().join("pekit-out/app/hello/shout/out.txt");
    assert_eq!(fs::read_to_string(built).unwrap(), "WORLD\n");
    // From the root, by its qualified name: found.
    let again = pekit(dir.path(), cache.path(), &["build", "app/hello"]);
    let (out, err) = printed(&again);
    assert!(again.status.success(), "{err}");
    assert_eq!(
        out,
        "built app/hello: 0 steps run, 1 found in the store\n  pekit-out/app/hello/shout/out.txt\n"
    );
}

#[test]
fn what_cannot_be_built_says_why_and_fails() {
    let dir = workspace();
    let cache = tempfile::tempdir().unwrap();
    let missing = pekit(dir.path(), cache.path(), &["build", "helo"]);
    let (_, err) = printed(&missing);
    assert_eq!(missing.status.code(), Some(1));
    assert!(
        err.starts_with("error: no `build` is named `helo`"),
        "{err}"
    );
    // A step that fails: its error, at its call.
    fs::write(dir.path().join("app/name.txt"), "").unwrap();
    fs::remove_file(dir.path().join("app/name.txt")).unwrap();
    let failed = pekit(dir.path(), cache.path(), &["build", "hello"]);
    let (_, err) = printed(&failed);
    assert_eq!(failed.status.code(), Some(1));
    assert!(err.contains("name.txt"), "{err}");
    // A command it does not understand.
    let unknown = pekit(dir.path(), cache.path(), &["bulid", "hello"]);
    assert_eq!(unknown.status.code(), Some(2));
    // Outside any workspace.
    let outside = tempfile::tempdir().unwrap();
    let nowhere = pekit(outside.path(), cache.path(), &["build", "hello"]);
    let (_, err) = printed(&nowhere);
    assert_eq!(nowhere.status.code(), Some(1));
    assert!(err.starts_with("error: no workspace here"), "{err}");
}

#[test]
fn a_build_s_trace_is_written_where_asked() {
    let dir = workspace();
    let cache = tempfile::tempdir().unwrap();
    let path = dir.path().join("trace.json");
    let built = pekit(
        dir.path(),
        cache.path(),
        &["build", "hello", "--trace", path.to_str().unwrap()],
    );
    assert!(built.status.success(), "{}", printed(&built).1);
    let trace: serde_json::Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
    let events = trace["traceEvents"].as_array().unwrap();
    assert!(
        events
            .iter()
            .any(|event| event["ph"] == "b" && event["name"] == "shout")
    );
    assert!(
        events
            .iter()
            .any(|event| event["ph"] == "X" && event["name"] == "exec sh")
    );
    // Not a terminal: no progress line.
    assert!(!printed(&built).1.contains('\r'));
}

#[test]
fn a_library_s_steps_key_the_same_wherever_the_library_is_found() {
    let library = "#[hermetic] def shout(src: File) -> File {\n\
                   \x20   let r = exec([\"sh\", \"-c\", \"tr a-z A-Z < {src.path} > out.txt\"])\n\
                   \x20   file(\"out.txt\")\n\
                   }\n";
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            (
                "app/build.gcsl",
                "import \"github.com/x/loud\"\n\
                 def build hello() { out = loud.shout(file(\"name.txt\")) }\n",
            ),
            ("app/name.txt", "lib\n"),
            (".gcsl/libraries/github.com/x/loud/loud.gcsl", library),
        ],
    );
    let cache = tempfile::tempdir().unwrap();
    let first = pekit(dir.path(), cache.path(), &["build", "hello"]);
    let (out, err) = printed(&first);
    assert!(first.status.success(), "{err}");
    assert!(out.starts_with("built app/hello: 1 step run"), "{out}");
    // The same library from elsewhere: the same receipts.
    let elsewhere = tempfile::tempdir().unwrap();
    write(
        elsewhere.path(),
        &[("github.com/x/loud/loud.gcsl", library)],
    );
    fs::remove_dir_all(dir.path().join(".gcsl")).unwrap();
    let again = Command::new(env!("CARGO_BIN_EXE_pekit"))
        .args(["build", "hello"])
        .current_dir(dir.path())
        .env("PEKIT_CACHE_DIR", cache.path())
        .env("PEKIT_STATE_DIR", cache.path().join("state"))
        .env("GCSL_LIBRARY_PATH", elsewhere.path())
        .output()
        .unwrap();
    let (out, err) = printed(&again);
    assert!(again.status.success(), "{err}");
    assert!(
        out.starts_with("built app/hello: 0 steps run, 1 found"),
        "{out}"
    );
}

#[test]
fn several_targets_are_built_and_shown_each() {
    let dir = workspace();
    write(
        dir.path(),
        &[(
            "other/build.gcsl",
            "def build plain() { = 1 }\ndef build broken() { = panic(\"no\") }",
        )],
    );
    let cache = tempfile::tempdir().unwrap();
    // A name and a glob: each target, then the steps of them all.
    let both = pekit(dir.path(), cache.path(), &["build", "hello", "other/**"]);
    let (out, err) = printed(&both);
    assert_eq!(both.status.code(), Some(1), "{out}{err}");
    assert_eq!(
        out,
        "built app/hello\n  pekit-out/app/hello/shout/out.txt\nbuilt other/plain\n1 step run, \
         0 found in the store\n"
    );
    assert!(err.contains("error: no"), "{err}");
    // `--rebuild` reuses nothing.
    let again = pekit(dir.path(), cache.path(), &["build", "hello", "--rebuild"]);
    let (out, _) = printed(&again);
    assert!(
        out.starts_with("built app/hello: 1 step run, 0 found"),
        "{out}"
    );
    // A glob that selects nothing, and a platform that is none.
    let none = pekit(dir.path(), cache.path(), &["build", "nowhere/*"]);
    let (_, err) = printed(&none);
    assert_eq!(none.status.code(), Some(1));
    assert_eq!(err, "error: no `build` is selected by `nowhere/*`\n");
    let bad = pekit(
        dir.path(),
        cache.path(),
        &["build", "hello", "--platform=-linux"],
    );
    let (_, err) = printed(&bad);
    assert_eq!(bad.status.code(), Some(1));
    assert!(
        err.contains("`--platform -linux` names no platform"),
        "{err}"
    );
}

#[test]
fn tests_report_each_and_one_that_is_no_step_runs_every_time() {
    let dir = workspace();
    write(
        dir.path(),
        &[(
            "app/tests.gcsl",
            "def test kept(out: File = build hello().out) { assert out.read() == \"WORLD\\n\" }\n\
             #[hermetic(false)] def test live() { assert 1 == 1 }\n\
             def test wrong() { assert 1 == 2 }\n",
        )],
    );
    let cache = tempfile::tempdir().unwrap();
    let first = pekit(dir.path(), cache.path(), &["test"]);
    let (out, _) = printed(&first);
    assert_eq!(first.status.code(), Some(1));
    assert_eq!(
        out,
        "passed           app/kept\npassed           app/live\nFAILED           app/wrong\n    \
         assertion failed: `1 == 2`\n2 passed, 1 failed\n"
    );
    // Named alone, the step found in the store, but the one that is none.
    let again = pekit(dir.path(), cache.path(), &["test", "kept", "live"]);
    let (out, _) = printed(&again);
    assert!(again.status.success(), "{out}");
    assert_eq!(
        out,
        "passed (cached)  app/kept\npassed           app/live\n2 passed, 0 failed\n"
    );
    let none = pekit(dir.path(), cache.path(), &["test", "nowhere/**"]);
    let (_, err) = printed(&none);
    assert_eq!(err, "error: no `test` is selected by `nowhere/**`\n");
}

#[test]
fn clean_removes_the_outputs_and_with_cache_the_store() {
    let dir = workspace();
    let cache = tempfile::tempdir().unwrap();
    let built = pekit(dir.path(), cache.path(), &["build", "hello"]);
    assert!(built.status.success());
    assert!(dir.path().join("pekit-out").is_dir());
    // `pekit-out/` alone: the step is found in the store again.
    let cleaned = pekit(&dir.path().join("app"), cache.path(), &["clean"]);
    let (out, _) = printed(&cleaned);
    assert!(cleaned.status.success());
    assert!(out.starts_with("removed "), "{out}");
    assert!(!dir.path().join("pekit-out").exists());
    let again = pekit(dir.path(), cache.path(), &["build", "hello"]);
    assert!(
        printed(&again)
            .0
            .starts_with("built app/hello: 0 steps run, 1 found")
    );
    // And the store: it runs again. The state directory stays.
    let cleaned = pekit(dir.path(), cache.path(), &["clean", "--cache"]);
    assert!(cleaned.status.success());
    assert!(!cache.path().join("store").exists());
    assert!(cache.path().join("state").exists());
    let again = pekit(dir.path(), cache.path(), &["build", "hello"]);
    assert!(
        printed(&again)
            .0
            .starts_with("built app/hello: 1 step run, 0 found")
    );
    // Cleaning what is clean removes nothing and succeeds.
    fs::remove_dir_all(dir.path().join("pekit-out")).unwrap();
    let nothing = pekit(dir.path(), cache.path(), &["clean"]);
    assert!(nothing.status.success());
    assert_eq!(printed(&nothing).0, "");
}
