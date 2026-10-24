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
