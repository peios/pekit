//! Tests of `pekit run`: scripts, and builds that give a program, run as
//! someone runs them.

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

/// The steps the tests build: one that shouts a file, and one that makes a
/// program that shows its arguments and exits with status 3.
const STEPS: &str = "#[hermetic] fn shout(src: File) -> File {\n\
                     \x20   let r = exec([\"sh\", \"-c\", \"tr a-z A-Z < {src.path} > out.txt\"])\n\
                     \x20   file(\"out.txt\")\n\
                     }\n\
                     #[hermetic] fn tool() -> File {\n\
                     \x20   let r = exec([\"sh\", \"-c\", \"printf '#!/bin/sh\\\\necho got \\\"$@\\\"\\\\nexit 3\\\\n' > tool; chmod +x tool\"])\n\
                     \x20   file(\"tool\")\n\
                     }\n";

/// A workspace whose `app` has builds `loud`, which shouts `name.txt`,
/// and `prog`, which gives a program, and scripts `greet`, which shows
/// what `loud` built and its words, and `twice`, which runs `greet` twice.
fn workspace() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    let build = format!(
        "{STEPS}def build loud() {{ = shout(file(\"name.txt\")) }}\n\
         def build prog() {{ = tool() }}\n\
         def build record() {{ out = tool() }}\n\
         def script greet() {{\n\
         \x20   let made = build loud()\n\
         \x20   let shown = exec([\"cat\", made.path], interactive = true)\n\
         \x20   let words = exec([\"echo\", \"words {{^script_session.args}}\"], interactive = true)\n\
         }}\n\
         def script twice() {{\n\
         \x20   let first = script greet()\n\
         \x20   let second = script greet()\n\
         }}\n"
    );
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            ("app/build.gcsl", &build),
            ("app/name.txt", "hi\n"),
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

/// What `output` printed on its standard output and standard error.
fn shown(output: &Output) -> (String, String) {
    (
        String::from_utf8_lossy(&output.stdout).into_owned(),
        String::from_utf8_lossy(&output.stderr).into_owned(),
    )
}

#[test]
fn a_script_runs_with_its_words_and_builds_as_a_build_does() {
    let dir = workspace();
    let cache = tempfile::tempdir().unwrap();
    let app = dir.path().join("app");
    let ran = pekit(&app, cache.path(), &["run", "greet", "one", "--two"]);
    let (out, err) = shown(&ran);
    assert!(ran.status.success(), "{err}");
    assert_eq!(out, "HI\nwords [\"one\", \"--two\"]\n");
    // What it built is `loud`'s, named by its definition, so building
    // `loud` finds it in the store.
    let built = pekit(&app, cache.path(), &["build", "loud"]);
    let (out, _) = shown(&built);
    assert!(
        out.starts_with("built app/loud: 0 steps run, 1 found in the store\n"),
        "{out}"
    );
    assert!(
        dir.path()
            .join("pekit-out/app/loud/shout/out.txt")
            .is_file()
    );
    assert!(!dir.path().join("pekit-out/app/greet").exists());
    // A script may run another, each time it is called, given no words.
    let twice = pekit(&app, cache.path(), &["run", "twice"]);
    let (out, err) = shown(&twice);
    assert!(twice.status.success(), "{err}");
    assert_eq!(out, "HI\nwords []\nHI\nwords []\n");
}

#[test]
fn a_build_that_gives_a_program_runs_it_and_exits_as_it_does() {
    let dir = workspace();
    let cache = tempfile::tempdir().unwrap();
    let app = dir.path().join("app");
    let ran = pekit(&app, cache.path(), &["run", "prog", "a", "--b"]);
    let (out, _) = shown(&ran);
    assert_eq!(out, "got a --b\n");
    assert_eq!(ran.status.code(), Some(3));
    // A file that is no program, or a value that is no file, is not run.
    let (_, err) = shown(&pekit(&app, cache.path(), &["run", "loud"]));
    let step = err
        .strip_prefix("error: `/pekit-out/.steps/shout/")
        .and_then(|rest| {
            rest.strip_suffix("/out.txt` is no executable file, so it cannot be run\n")
        })
        .unwrap_or_else(|| panic!("{err}"));
    assert_eq!(step.len(), 16, "{err}");
    let (_, err) = shown(&pekit(&app, cache.path(), &["run", "record"]));
    assert_eq!(
        err,
        "error: `app/record` gives no program to run: `pekit run` runs a build that gives one \
         executable file\n"
    );
}

#[test]
fn run_names_one_script_or_build() {
    let dir = workspace();
    let cache = tempfile::tempdir().unwrap();
    let run = |args: &[&str]| {
        let ran = pekit(dir.path(), cache.path(), args);
        assert!(!ran.status.success());
        shown(&ran).1
    };
    assert_eq!(
        run(&["run", "nope"]),
        "error: no `script` or `build` is named `nope`\n"
    );
    assert_eq!(
        run(&["run", "app/*"]),
        "error: `pekit run` runs one script or build, which `app/*` does not name\n"
    );
    // A script and a build of one name in two directories: name one.
    write(
        dir.path(),
        &[("other/build.gcsl", "def build greet() { = 1 }")],
    );
    assert_eq!(
        run(&["run", "greet"]),
        "error: `greet` names the script `app/greet` and the build `other/greet`: name the one \
         to run by its directory\n"
    );
    // In one directory, they are an error for every command.
    write(
        dir.path(),
        &[("app/more.gcsl", "def build twice() { = 2 }")],
    );
    let (_, err) = shown(&pekit(dir.path(), cache.path(), &["build", "loud"]));
    assert_eq!(
        err,
        "error: `app/twice` is both a build and a script, so `pekit run` could run either: \
         rename one\n"
    );
}

#[test]
fn a_build_cannot_call_a_script() {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            (
                "app/build.gcsl",
                "def script hello() { let r = exec([\"true\"]) }\n\
                 def build b() { = script hello() }\n",
            ),
        ],
    );
    let cache = tempfile::tempdir().unwrap();
    let ran = pekit(dir.path(), cache.path(), &["build", "b"]);
    let (_, err) = shown(&ran);
    assert!(!ran.status.success());
    assert!(
        err.contains("error: context `script_session` has no value in this definition"),
        "{err}"
    );
}
