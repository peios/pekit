//! Tests of keying a step by the contexts it read, and of giving it a
//! context's files only once it reads the context (see `design/engine.md`,
//! R).

mod building;

use std::fs;
use std::path::Path;

use building::build_in;

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// A workspace whose root declares a context `flavour`, with a default, and
/// two steps, one that reads it and one that doesn't; and whose `app`
/// builds both, `binding` around them.
fn workspace(binding: &str) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                "context flavour: String = \"plain\"\n\
                 #[hermetic] def plainly(src: File) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"tr a-z A-Z < {src.path} > out\"])\n\
                 \x20   = working_dir().file(\"out\")\n\
                 }\n\
                 #[hermetic] def flavoured(src: File) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"echo {^flavour} > out\"])\n\
                 \x20   = working_dir().file(\"out\")\n\
                 }\n",
            ),
            ("app/name.txt", "n\n"),
        ],
    );
    rebind(dir.path(), binding);
    dir
}

/// Writes `app`'s build with `binding` around its two steps.
fn rebind(root: &Path, binding: &str) {
    write(
        root,
        &[(
            "app/build.gcsl",
            &format!(
                "def build both() {{\n\
                 \x20   = [plainly(file(\"name.txt\")), flavoured(file(\"name.txt\"))]{binding}\n\
                 }}\n"
            ),
        )],
    );
}

#[test]
fn a_step_is_keyed_by_the_contexts_it_read_alone() {
    let dir = workspace(" with { flavour = \"sweet\" }");
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((built.ran, built.cached), (2, 0));
    let flavoured = dir.path().join("pekit-out/app/both/flavoured/out");
    assert_eq!(fs::read_to_string(&flavoured).unwrap(), "sweet\n");
    // Another value of `flavour`: the step that read it runs again, and the
    // one that didn't is found.
    rebind(dir.path(), " with { flavour = \"sour\" }");
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((again.ran, again.cached), (1, 1));
    assert_eq!(fs::read_to_string(&flavoured).unwrap(), "sour\n");
    // Its default, read where nothing binds it, is another input again.
    rebind(dir.path(), "");
    let unbound = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((unbound.ran, unbound.cached), (1, 1));
    assert_eq!(fs::read_to_string(&flavoured).unwrap(), "plain\n");
    // Bound to its default's value, it was bound, as no run before.
    rebind(dir.path(), " with { flavour = \"plain\" }");
    let bound = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((bound.ran, bound.cached), (1, 1));
    // And each before is found again.
    for (binding, expected) in [
        (" with { flavour = \"sweet\" }", "sweet\n"),
        ("", "plain\n"),
    ] {
        rebind(dir.path(), binding);
        let found = build_in(dir.path(), state.path(), "both").unwrap();
        assert_eq!((found.ran, found.cached), (0, 2), "{binding}");
        assert_eq!(fs::read_to_string(&flavoured).unwrap(), expected);
    }
}

#[test]
fn a_step_is_given_a_context_s_files_only_once_it_reads_it() {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                "context notes: List<File> = []\n\
                 #[hermetic] def peeks() -> List<File> {\n\
                 \x20   let look = \"if test -e /w/app/note.txt; then echo seen; else echo hidden; fi\"\n\
                 \x20   let before = exec([\"sh\", \"-c\", \"{look} > before\"])\n\
                 \x20   let read = len(^notes)\n\
                 \x20   let after = exec([\"sh\", \"-c\", \"{look} > after; echo {read} >> after\"])\n\
                 \x20   = [working_dir().file(\"before\"), working_dir().file(\"after\")]\n\
                 }\n",
            ),
            (
                "app/build.gcsl",
                "def build looked() { = peeks() with { notes = [file(\"note.txt\")] } }\n",
            ),
            ("app/note.txt", "a note\n"),
        ],
    );
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "looked").unwrap();
    let out = dir.path().join("pekit-out/app/looked/peeks");
    // Its first program ran before the step read `notes`, so the note was
    // not there for it; the second, after, saw it.
    assert_eq!(fs::read_to_string(out.join("before")).unwrap(), "hidden\n");
    assert_eq!(fs::read_to_string(out.join("after")).unwrap(), "seen\n1\n");
}
