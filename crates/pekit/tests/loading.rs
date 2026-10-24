//! Tests of loading a workspace and naming its targets.

use std::fs;
use std::path::Path;

use pekit::load::{LoadError, load};
use pekit::target::{TargetError, resolve};

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// A workspace with `hello` defined in `app/one` and `app/two`, and
/// `world` in `app/one` alone.
fn two_hellos() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            (
                "app/one/build.gcsl",
                "def build hello() { \"one\" }\ndef build world() { \"w\" }",
            ),
            ("app/two/build.gcsl", "def build hello() { \"two\" }"),
        ],
    );
    dir
}

#[test]
fn a_workspace_loads_from_any_directory_in_it() {
    let dir = two_hellos();
    let loaded = load(&dir.path().join("app/one")).unwrap();
    assert_eq!(loaded.root, dir.path().canonicalize().unwrap());
    assert_eq!(loaded.here, "app/one");
    assert_eq!(load(dir.path()).unwrap().here, "");
}

#[test]
fn a_name_alone_means_the_one_definition_of_it_or_the_one_here() {
    let dir = two_hellos();
    let loaded = load(dir.path()).unwrap();
    // One `world`: found from anywhere.
    let world = resolve(&loaded.workspace, ("build", "world"), "").unwrap();
    assert_eq!(world.qualified(), "app/one/world");
    // Two `hello`s: the one here, or none.
    let here = resolve(&loaded.workspace, ("build", "hello"), "app/two").unwrap();
    assert_eq!(here.qualified(), "app/two/hello");
    let error = resolve(&loaded.workspace, ("build", "hello"), "").unwrap_err();
    assert_eq!(
        error,
        TargetError::Ambiguous {
            def_type: "build".to_owned(),
            name: "hello".to_owned(),
            candidates: vec!["app/one/hello".to_owned(), "app/two/hello".to_owned()],
        }
    );
    assert_eq!(
        error.message(),
        "error: `build hello` is defined in several places; name one:\n  app/one/hello\n  \
         app/two/hello"
    );
    // A qualified name always means its own.
    let qualified = resolve(&loaded.workspace, ("build", "app/one/hello"), "app/two").unwrap();
    assert_eq!(qualified.definition.dir, "app/one");
}

#[test]
fn a_name_that_matches_nothing_suggests_the_closest() {
    let dir = two_hellos();
    let loaded = load(dir.path()).unwrap();
    let error = resolve(&loaded.workspace, ("build", "helo"), "").unwrap_err();
    assert_eq!(
        error.message(),
        "error: no `build` is named `helo`\n  help: there are `app/one/hello`, `app/two/hello`"
    );
    assert_eq!(
        resolve(&loaded.workspace, ("biuld", "hello"), "")
            .unwrap_err()
            .message(),
        "error: no def-type is named `biuld`"
    );
}

#[test]
fn a_workspace_with_no_builds_has_none_of_any_name() {
    let dir = tempfile::tempdir().unwrap();
    write(dir.path(), &[("workspace.gcsl", ""), ("app.gcsl", "x = 1")]);
    let loaded = load(dir.path()).unwrap();
    let error = resolve(&loaded.workspace, ("build", "hello"), "").unwrap_err();
    assert_eq!(error.message(), "error: no `build` is named `hello`");
}

#[test]
fn a_workspace_that_cannot_load_says_why() {
    let empty = tempfile::tempdir().unwrap();
    let error = load(empty.path()).unwrap_err();
    assert!(matches!(error, LoadError::NoWorkspace { .. }));
    assert!(
        error.render().starts_with("error: no workspace here:"),
        "{}",
        error.render()
    );
    // A check's errors are shown against their files.
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[("workspace.gcsl", ""), ("bad.gcsl", "x = 1 +")],
    );
    let error = load(dir.path()).unwrap_err();
    let shown = error.render();
    assert!(shown.contains("bad.gcsl"), "{shown}");
}
