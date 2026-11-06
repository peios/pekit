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

/// A library declaring a context `flavour`, with a default, and two steps,
/// one that reads it and one that doesn't.
const FLAVOURS: &str = "context flavour: String = \"plain\"\n\
                        #[hermetic] def plainly(src: File) -> File {\n\
                        \x20   let r = exec([\"sh\", \"-c\", \"tr a-z A-Z < {src.path} > out\"])\n\
                        \x20   = working_dir().file(\"out\")\n\
                        }\n\
                        #[hermetic] def flavoured(src: File) -> File {\n\
                        \x20   let r = exec([\"sh\", \"-c\", \"echo {^flavour} > out\"])\n\
                        \x20   = working_dir().file(\"out\")\n\
                        }\n";

/// A workspace with `FLAVOURS` as a library and `workspace.gcsl` of `conf`,
/// whose `app` imports it and builds both steps, `both`, and the flavoured
/// one in its own `with`, `own`.
fn library_workspace(conf: &str) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                ".gcsl/libraries/github.com/x/flavours/flavours.gcsl",
                FLAVOURS,
            ),
            (
                "app/build.gcsl",
                "import \"github.com/x/flavours\"\n\
                 def build both() {\n\
                 \x20   = [flavours.plainly(file(\"name.txt\")), flavours.flavoured(file(\"name.txt\"))]\n\
                 }\n\
                 def build own() {\n\
                 \x20   = flavours.flavoured(file(\"name.txt\")) with { flavour = \"sour\" }\n\
                 }\n",
            ),
            ("app/name.txt", "n\n"),
        ],
    );
    reconf(dir.path(), conf);
    dir
}

/// Writes `workspace.gcsl` of the workspace at `root` as `conf`, which
/// needn't import the library to set its context: `app` imports it.
fn reconf(root: &Path, conf: &str) {
    write(root, &[("workspace.gcsl", conf)]);
}

#[test]
fn the_workspace_sets_a_library_s_context_for_every_target() {
    let dir = library_workspace("");
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((built.ran, built.cached), (2, 0));
    let flavoured = dir.path().join("pekit-out/app/both/flavoured/out");
    assert_eq!(fs::read_to_string(&flavoured).unwrap(), "plain\n");
    // Set from `pekitconf.contexts`: only the step that reads it runs again.
    reconf(
        dir.path(),
        "pekitconf = {contexts = {flavour = \"sweet\"}}\n",
    );
    let set = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((set.ran, set.cached), (1, 1));
    assert_eq!(fs::read_to_string(&flavoured).unwrap(), "sweet\n");
    // So is it for a new value.
    reconf(
        dir.path(),
        "pekitconf = {contexts = {flavour = \"salt\"}}\n",
    );
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((again.ran, again.cached), (1, 1));
    assert_eq!(fs::read_to_string(&flavoured).unwrap(), "salt\n");
    // A target's own `with` binds over the workspace's.
    let own = build_in(dir.path(), state.path(), "own").unwrap();
    assert_eq!(own.ran, 1);
    let out = dir.path().join("pekit-out/app/own/flavoured/out");
    assert_eq!(fs::read_to_string(&out).unwrap(), "sour\n");
    // And the same value bound either way is the same step.
    reconf(
        dir.path(),
        "pekitconf = {contexts = {flavour = \"sour\"}}\n",
    );
    let same = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((same.ran, same.cached), (0, 2));
}

/// Writes a workspace whose `app` makes one step in two contexts that only
/// `stage` tells apart, each step's result taken by another, all with
/// `unrelated` bound to `value`, which no step reads.
fn twice(root: &Path, value: &str) {
    write(
        root,
        &[
            (
                "workspace.gcsl",
                "context stage: Int = 1\n\
                 context unrelated: String = \"a\"\n\
                 #[hermetic] def made() -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"echo made > out\"])\n\
                 \x20   = working_dir().file(\"out\")\n\
                 }\n\
                 #[hermetic] def taken(from: File, tag: String) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"cat {from.path} > out; echo {tag} >> out\"])\n\
                 \x20   = working_dir().file(\"out\")\n\
                 }\n",
            ),
            (
                "app/build.gcsl",
                &format!(
                    "def build both() {{\n\
                     \x20   = [taken(made() with {{ stage = 1 }}, \"one\"), \
                     taken(made() with {{ stage = 3 }}, \"three\")] \
                     with {{ unrelated = \"{value}\" }}\n\
                     }}\n"
                ),
            ),
        ],
    );
}

#[test]
fn a_context_two_steps_of_one_identity_share_names_neither() {
    let dir = tempfile::tempdir().unwrap();
    let state = tempfile::tempdir().unwrap();
    twice(dir.path(), "a");
    let built = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((built.ran, built.cached), (4, 0));
    // `unrelated` names neither `made`, though the later met is named by
    // the context it differs in, so nothing runs again.
    twice(dir.path(), "b");
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((again.ran, again.cached), (0, 4));
}

#[test]
fn the_workspace_sets_only_contexts_declared_and_not_pekit_s_own() {
    let state = tempfile::tempdir().unwrap();
    for (conf, expected) in [
        (
            "pekitconf = {contexts = {flavor = \"sweet\"}}\n",
            "`pekitconf.contexts.flavor` in `workspace.gcsl` names no context: neither the \
             workspace nor a library it imports declares `context flavor`",
        ),
        (
            "pekitconf = {contexts = {host_platform = {}}}\n",
            "`pekitconf.contexts.host_platform` in `workspace.gcsl` cannot set \
             `host_platform`: pekit sets it from the machine pekit runs on",
        ),
        (
            "pekitconf = {contexts = 1}\n",
            "`pekitconf.contexts` in `workspace.gcsl` must be a record whose fields name \
             contexts, as `contexts = {NAME = VALUE}`",
        ),
    ] {
        let dir = library_workspace(conf);
        let error = build_in(dir.path(), state.path(), "both").unwrap_err();
        assert!(error.contains(expected), "{conf}: {error}");
    }
    // A value not of the context's type is GCSL's own error, as a `with`'s.
    let dir = library_workspace("pekitconf = {contexts = {flavour = 1}}\n");
    let error = build_in(dir.path(), state.path(), "both").unwrap_err();
    assert!(error.contains("mismatched types"), "{error}");
}
