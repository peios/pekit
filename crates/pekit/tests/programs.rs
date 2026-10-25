//! Tests of caching each program a step runs on its own (see
//! `design/additive.md`, F1).

mod building;

use std::fs;
use std::path::Path;

use building::{Options, build_in, observed_with};
use pekit::engine::{Built, Reuse};
use pekit::events::EventKind;

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// A step of two programs, the second reading what the first wrote to its
/// own directory and to `/tmp`, declared in the workspace's root file after
/// `comment`.
fn steps(comment: &str) -> String {
    format!(
        "# {comment}\n\
         #[hermetic] def twice(src: File, word: String) -> File {{\n\
         \x20   let a = exec([\"sh\", \"-c\", \"tr a-z A-Z < {{src.path}} > up; echo t > /tmp/t\"])\n\
         \x20   let b = exec([\"sh\", \"-c\", \"cat up /tmp/t > out; echo {{word}} >> out\"])\n\
         \x20   = file(\"out\")\n\
         }}\n"
    )
}

/// A workspace whose `app` builds `twice` of its `name.txt`, `text`, with
/// the word `word`.
fn workspace(text: &str, word: &str) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", &steps("one")),
            ("app/name.txt", text),
            (
                "app/build.gcsl",
                &format!("def build both() {{ = twice(file(\"name.txt\"), \"{word}\") }}\n"),
            ),
        ],
    );
    dir
}

/// What the step `twice` wrote, built in `dir`.
fn out(dir: &Path) -> String {
    fs::read_to_string(dir.join("pekit-out/app/both/twice/out")).unwrap()
}

/// How many steps ran, how many programs they found, and how many steps
/// were found.
fn counts(built: &Built) -> (usize, usize, usize) {
    (built.ran, built.programs_found, built.cached)
}

#[test]
fn a_step_run_again_finds_the_programs_that_see_what_they_saw() {
    let dir = workspace("n\n", "w");
    let state = tempfile::tempdir().unwrap();
    let first = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(counts(&first), (1, 0, 0));
    assert_eq!(out(dir.path()), "N\nt\nw\n");
    // Its code changed, by a comment: it runs, and finds both its programs,
    // the second seeing what the first, found, left.
    write(dir.path(), &[("workspace.gcsl", &steps("two"))]);
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(counts(&again), (1, 2, 0));
    assert_eq!(out(dir.path()), "N\nt\nw\n");
    // Another input, which the first program reads: neither is found.
    write(
        dir.path(),
        &[("app/name.txt", "m\n"), ("workspace.gcsl", &steps("three"))],
    );
    let other = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(counts(&other), (1, 0, 0));
    assert_eq!(out(dir.path()), "M\nt\nw\n");
}

#[test]
fn a_program_run_after_one_found_sees_what_the_found_one_left() {
    let dir = workspace("n\n", "w");
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    // Another word, which only the second program is given: the first is
    // found, and the second, run, reads what it left in the own directory
    // and in `/tmp`.
    write(
        dir.path(),
        &[(
            "app/build.gcsl",
            "def build both() { = twice(file(\"name.txt\"), \"v\") }\n",
        )],
    );
    let built = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(counts(&built), (1, 1, 0));
    assert_eq!(out(dir.path()), "N\nt\nv\n");
}

#[test]
fn a_program_found_is_told_and_counted() {
    let dir = workspace("n\n", "w");
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    write(dir.path(), &[("workspace.gcsl", &steps("two"))]);
    let (built, events) = observed_with(dir.path(), state.path(), "both", Options::default());
    assert_eq!(counts(&built.unwrap()), (1, 2, 0));
    let found: Vec<&str> = events
        .iter()
        .filter_map(|event| match &event.kind {
            EventKind::EffectFound { step, .. } => Some(step.as_str()),
            _ => None,
        })
        .collect();
    assert_eq!(found, ["twice", "twice"]);
}

#[test]
fn a_rebuild_or_a_check_finds_no_program() {
    let dir = workspace("n\n", "w");
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    write(dir.path(), &[("workspace.gcsl", &steps("two"))]);
    for reuse in [Reuse::Rerun, Reuse::Compare] {
        let options = Options {
            reuse,
            ..Options::default()
        };
        let built = observed_with(dir.path(), state.path(), "both", options)
            .0
            .unwrap();
        assert_eq!(counts(&built), (1, 0, 0), "{reuse:?}");
    }
}

#[test]
fn a_step_given_a_secret_has_no_program_found_or_recorded() {
    let dir = tempfile::tempdir().unwrap();
    let text = |comment: &str| {
        format!(
            "# {comment}\n\
             import keychain\n\
             pekitconf = {{keychain = [{{profile = \"dev\"; name = \"signing\"; file = \"key\"}}]}}\n\
             #[hermetic] def sign(src: File, key: Secret<File> = keychain.file(\"signing\")) -> File {{\n\
             \x20   let a = exec([\"sh\", \"-c\", \"cat {{src.path}} > signed\"])\n\
             \x20   let b = exec_secret([\"sh\", \"-c\", \"wc -c < \\\"$KEY\\\" >> signed\"], \
             secrets = {{KEY = key.reveal().path}})\n\
             \x20   = file(\"signed\")\n\
             }}\n"
        )
    };
    write(
        dir.path(),
        &[
            ("workspace.gcsl", &text("one")),
            ("key", "abc"),
            ("app/name.txt", "n\n"),
            (
                "app/build.gcsl",
                "def build both() { = sign(file(\"name.txt\")) }\n",
            ),
        ],
    );
    let state = tempfile::tempdir().unwrap();
    let first = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(counts(&first), (1, 0, 0));
    write(dir.path(), &[("workspace.gcsl", &text("two"))]);
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(counts(&again), (1, 0, 0));
    let signed = dir.path().join("pekit-out/app/both/sign/signed");
    assert_eq!(fs::read_to_string(signed).unwrap(), "n\n3\n");
}

#[test]
fn a_sandbox_s_program_found_leaves_its_writes_for_the_next() {
    let busybox = Path::new("/usr/bin/busybox");
    if !busybox.exists() {
        eprintln!("skipped: no static busybox on this host");
        return;
    }
    let text = |word: &str| {
        format!(
            "#[hermetic] fn inside(root: Dir, word: String) -> File {{\n\
             \x20   let s = root.sandbox()\n\
             \x20   let a = s.exec(\"/bin/sh\", \"-c\", \"echo one > /made.txt\")\n\
             \x20   let b = s.exec(\"/bin/sh\", \"-c\", \"cat /made.txt > /out.txt; echo {{word}} >> /out.txt\")\n\
             \x20   s.file(\"/out.txt\")\n\
             }}\n\
             def build both() {{ = inside(dir(\"root\"), \"{word}\") }}\n"
        )
    };
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[("workspace.gcsl", ""), ("app/build.gcsl", &text("a"))],
    );
    let bin = dir.path().join("app/root/bin");
    fs::create_dir_all(&bin).unwrap();
    fs::copy(busybox, bin.join("busybox")).unwrap();
    std::os::unix::fs::symlink("busybox", bin.join("sh")).unwrap();
    std::os::unix::fs::symlink("busybox", bin.join("cat")).unwrap();
    let state = tempfile::tempdir().unwrap();
    let read = |built: &Built| {
        let [(path, _)] = built.value.files()[..] else {
            panic!("{:?}", built.value);
        };
        fs::read_to_string(dir.path().join(path.trim_start_matches('/'))).unwrap()
    };
    let first = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(counts(&first), (1, 0, 0));
    assert_eq!(read(&first), "one\na\n");
    // Another word: the first program is found, its write to the
    // sandbox's upper layer placed again, and the second reads it.
    write(dir.path(), &[("app/build.gcsl", &text("b"))]);
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(counts(&again), (1, 1, 0));
    assert_eq!(read(&again), "one\nb\n");
}

#[test]
fn a_program_sees_what_a_context_read_staged_by_its_contents() {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                "context notes: List<File> = []\n\
                 #[hermetic] def reads() -> File {\n\
                 \x20   let a = exec([\"sh\", \"-c\", \"echo before > out\"])\n\
                 \x20   let n = ^notes\n\
                 \x20   let b = exec([\"sh\", \"-c\", \"cat /w/app/note.txt >> out\"])\n\
                 \x20   = file(\"out\")\n\
                 }\n",
            ),
            (
                "app/build.gcsl",
                "def build both() { = reads() with { notes = [file(\"note.txt\")] } }\n",
            ),
            ("app/note.txt", "one\n"),
        ],
    );
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    // The note's contents changed: the step, which read it, runs again;
    // the program before the read is found, and the one after is not.
    write(dir.path(), &[("app/note.txt", "two\n")]);
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(counts(&again), (1, 1, 0));
    let out = dir.path().join("pekit-out/app/both/reads/out");
    assert_eq!(fs::read_to_string(out).unwrap(), "before\ntwo\n");
}

#[test]
fn a_program_sees_what_the_step_wrote_before_it() {
    let text = |word: &str| {
        format!(
            "import fs\n\
             #[hermetic] def wrote() -> File {{\n\
             \x20   let at = \"{{dir(\".\").path}}/note\"\n\
             \x20   let w = fs.write(at, \"{word}\")\n\
             \x20   let r = exec([\"sh\", \"-c\", \"cat note > out\"])\n\
             \x20   = file(\"out\")\n\
             }}\n\
             def build both() {{ = wrote() }}\n"
        )
    };
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[("workspace.gcsl", ""), ("app/build.gcsl", &text("one"))],
    );
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    // What it writes changed, and so what its program sees: not found.
    write(dir.path(), &[("app/build.gcsl", &text("two"))]);
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(counts(&again), (1, 0, 0));
    let out = dir.path().join("pekit-out/app/both/wrote/out");
    assert_eq!(fs::read_to_string(&out).unwrap(), "two");
    // Back as it was: found.
    write(
        dir.path(),
        &[("app/build.gcsl", &format!("# again\n{}", text("one")))],
    );
    let back = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(counts(&back), (1, 1, 0));
    assert_eq!(fs::read_to_string(&out).unwrap(), "one");
}
