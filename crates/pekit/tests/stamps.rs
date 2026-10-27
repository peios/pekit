//! Tests of hashing a workspace's files through stamps kept between builds
//! (see `design/additive.md`, F4).

mod building;

use std::fs::{self, File};
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

/// A workspace whose `app` copies its `name.txt`, `text`, and its `dir`.
fn workspace(text: &str) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                "#[hermetic] def copy(src: File, d: Dir) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"cat {src.path} {d.path}/x > out\"])\n\
                 \x20   = working_dir().file(\"out\")\n\
                 }\n",
            ),
            ("app/name.txt", text),
            ("app/dir/x", "x\n"),
            (
                "app/build.gcsl",
                "def build both() { = copy(file(\"name.txt\"), dir(\"dir\")) }\n",
            ),
        ],
    );
    dir
}

/// Writes `text` over the file at `path`, its modification time put back
/// as it was: a change only its change time tells.
fn rewrite_quietly(path: &Path, text: &str) {
    let modified = fs::metadata(path).unwrap().modified().unwrap();
    fs::write(path, text).unwrap();
    File::options()
        .write(true)
        .open(path)
        .unwrap()
        .set_modified(modified)
        .unwrap();
}

#[test]
fn a_workspace_file_changed_is_read_again_however_quietly() {
    let dir = workspace("one\n");
    let state = tempfile::tempdir().unwrap();
    let out = dir.path().join("pekit-out/app/both/copy/out");
    build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(fs::read_to_string(&out).unwrap(), "one\nx\n");
    let again = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((again.ran, again.cached), (0, 1));
    // Of the same size, its modification time as it was: read again.
    rewrite_quietly(&dir.path().join("app/name.txt"), "two\n");
    let file = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((file.ran, file.cached), (1, 0));
    assert_eq!(fs::read_to_string(&out).unwrap(), "two\nx\n");
    // And so of a file of a directory given.
    rewrite_quietly(&dir.path().join("app/dir/x"), "y\n");
    let tree = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!((tree.ran, tree.cached), (1, 0));
    assert_eq!(fs::read_to_string(&out).unwrap(), "two\ny\n");
}

#[test]
fn a_workspace_s_stamps_are_kept_between_builds() {
    let dir = workspace("one\n");
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    let kept: Vec<_> = fs::read_dir(state.path().join("store/seen"))
        .unwrap()
        .flatten()
        .collect();
    let [stamps] = &kept[..] else {
        panic!("{kept:?}");
    };
    let text = fs::read_to_string(stamps.path()).unwrap();
    assert!(text.contains("name.txt"), "{text}");
    assert!(text.contains("dir/x"), "{text}");
    // A file gone is forgotten.
    fs::remove_file(dir.path().join("app/name.txt")).unwrap();
    write(dir.path(), &[("app/other.txt", "o\n")]);
    write(
        dir.path(),
        &[(
            "app/build.gcsl",
            "def build both() { = copy(file(\"other.txt\"), dir(\"dir\")) }\n",
        )],
    );
    build_in(dir.path(), state.path(), "both").unwrap();
    let text = fs::read_to_string(stamps.path()).unwrap();
    assert!(!text.contains("name.txt"), "{text}");
    // The workspace has no file of its own left behind.
    assert!(
        fs::read_dir(dir.path())
            .unwrap()
            .flatten()
            .all(|entry| !entry.file_name().to_string_lossy().starts_with(".pekit")),
    );
}
