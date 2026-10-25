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

/// Steps of which one writes the time, and so gives other outputs each
/// time it runs, and one gives the same.
const DRIFTING: &str = "#[hermetic] fn stamp() -> Dir {\n\
                        \x20   let r = exec([\"sh\", \"-c\", \"mkdir -p out/sub; echo same > out/a; \
                        date +%s%N > out/sub/t\"])\n\
                        \x20   dir(\"out\")\n\
                        }\n\
                        #[hermetic] fn steady() -> File {\n\
                        \x20   let r = exec([\"sh\", \"-c\", \"echo steady > s\"])\n\
                        \x20   file(\"s\")\n\
                        }\n\
                        def build drift() { = stamp() }\n\
                        def build calm() { = steady() }\n";

#[test]
fn check_reproducible_runs_every_step_again_and_names_what_differs() {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[("workspace.gcsl", ""), ("app/build.gcsl", DRIFTING)],
    );
    let cache = tempfile::tempdir().unwrap();
    let app = dir.path().join("app");
    let checked = pekit(
        &app,
        cache.path(),
        &["build", "calm", "--check-reproducible"],
    );
    let (out, err) = printed(&checked);
    assert!(checked.status.success(), "{err}");
    assert!(out.starts_with("built app/calm: 1 step run"), "{out}");
    // A step whose outputs differ is named, with each file that does, the
    // directory's own looked into.
    let checked = pekit(
        &app,
        cache.path(),
        &["build", "drift", "--check-reproducible"],
    );
    let (_, err) = printed(&checked);
    assert!(!checked.status.success());
    assert_eq!(
        err,
        "error: a step is not reproducible: run again with the same inputs, it gave other \
         outputs\n  app/drift/stamp\n    /pekit-out/app/drift/stamp/out/sub/t\n"
    );
    // What changes the world is not done twice to check it.
    let (_, err) = printed(&pekit(
        &app,
        cache.path(),
        &["run", "calm", "--check-reproducible"],
    ));
    assert!(err.contains("`pekit run` changes the world"), "{err}");
}

#[test]
fn a_strict_build_runs_no_target_effect_but_reads_and_borrows_no_host_tool() {
    let dir = tempfile::tempdir().unwrap();
    let text = format!(
        "{DRIFTING}def build own() {{\n\
         \x20   let r = exec([\"true\"])\n\
         \x20   = steady()\n\
         }}\n\
         def build outside() {{ = file(\"/etc/hostname\").read() }}\n\
         def build inside() {{ = file(\"name.txt\").read() }}\n"
    );
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            ("app/build.gcsl", &text),
            ("app/name.txt", "n\n"),
        ],
    );
    let cache = tempfile::tempdir().unwrap();
    let app = dir.path().join("app");
    let strict = |target: &str| {
        let built = pekit(&app, cache.path(), &["build", target, "--strict"]);
        (built.status.success(), printed(&built).1)
    };
    let (ok, err) = strict("own");
    assert!(!ok);
    assert!(
        err.starts_with(
            "error: a strict build refuses a target's own `exec`: a release's targets run \
             programs and write files only in steps\n"
        ),
        "{err}"
    );
    let (ok, err) = strict("outside");
    assert!(!ok);
    assert!(
        err.starts_with(
            "error: a strict build reads only the workspace and the build's outputs, and this \
             reads `/etc/hostname`\n"
        ),
        "{err}"
    );
    // The workspace's own files it reads.
    let (ok, err) = strict("inside");
    assert!(ok, "{err}");
    // A step sees nothing of the host: no `sh` to run, and no note that the
    // build was not hermetic.
    let (ok, err) = strict("calm");
    assert!(!ok);
    assert!(
        err.contains("execvp sh: No such file or directory"),
        "{err}"
    );
    assert!(!err.contains("not hermetic"), "{err}");
}

#[test]
fn a_step_found_in_the_store_has_its_outputs_placed_for_the_target_to_read() {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            (
                "app/build.gcsl",
                "#[hermetic] fn made() -> Dir {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"mkdir out; echo inside > out/f\"])\n\
                 \x20   dir(\"out\")\n\
                 }\n\
                 def build reads() { = made().file(\"f\").read() }\n",
            ),
        ],
    );
    let cache = tempfile::tempdir().unwrap();
    let app = dir.path().join("app");
    let first = pekit(&app, cache.path(), &["build", "reads"]);
    assert!(first.status.success(), "{:?}", printed(&first));
    // With `pekit-out/` gone, the step is found in the store, and what the
    // target reads of it is put back.
    assert!(pekit(&app, cache.path(), &["clean"]).status.success());
    let again = pekit(&app, cache.path(), &["build", "reads"]);
    let (out, err) = printed(&again);
    assert!(again.status.success(), "{err}");
    assert!(
        out.starts_with("built app/reads: 0 steps run, 1 found"),
        "{out}"
    );
    let placed = dir.path().join("pekit-out/app/reads/made/out/f");
    assert_eq!(fs::read_to_string(placed).unwrap(), "inside\n");
}
