//! Tests of `pekit release`, in a git repository of the test's own.

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

/// What `git` with `args` prints, run in `dir`, as no one's configuration
/// says.
fn git(dir: &Path, args: &[&str]) -> String {
    let ran = Command::new("git")
        .args([
            "-c",
            "user.name=pekit",
            "-c",
            "user.email=pekit@example.org",
            "-c",
            "commit.gpgsign=false",
        ])
        .args(args)
        .current_dir(dir)
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .output()
        .unwrap();
    assert!(ran.status.success(), "{ran:?}");
    String::from_utf8_lossy(&ran.stdout).trim().to_owned()
}

/// A packager of the workspace's own: it packs a package's files in a step,
/// composed, and publishes by copying them to `STORE/NAME-VERSION`, in
/// place of what is there.
const WORKSPACE: &str = "import fs\n\
                         import packaging\n\
                         #[hermetic] fn composed(pkg: Package) -> Dir { fs.compose_dir(pkg.files) }\n\
                         fn copied(pkg: Package, packed: Dir, store: String) -> {} {\n\
                         \x20   let at = \"{store}/{pkg.name}-{pkg.version}\"\n\
                         \x20   let r = exec([\"sh\", \"-c\", \"rm -rf {at} && cp -rT {packed.path} {at}\"])\n\
                         }\n\
                         def copier() -> packaging.Packager {\n\
                         \x20   name = \"copy\"\n\
                         \x20   pack = fn(pkg: Package) -> Dir { composed(pkg) }\n\
                         \x20   publish = fn(pkg: Package, packed: Dir, store: String) -> {} { copied(pkg, packed, store) }\n\
                         }\n\
                         pekitconf = {packagers = [copier()]}\n";

/// A repository, its workspace in `ws`, whose package `dev.example.data`
/// of version `version` holds `data.txt`, committed.
fn repository(dir: &Path, version: &str) {
    let package = format!(
        "def package dev.example.data() {{\n\
         \x20   version = \"{version}\"\n\
         \x20   files = {{\"share/data.txt\" = file(\"data.txt\")}}\n\
         }}\n"
    );
    write(
        &dir.join("ws"),
        &[
            ("workspace.gcsl", WORKSPACE),
            ("app/package.gcsl", &package),
            ("app/data.txt", "data\n"),
        ],
    );
    git(dir, &["init", "-q", "-b", "main"]);
    git(dir, &["add", "-A"]);
    git(dir, &["commit", "-qm", "one"]);
}

/// `pekit` run with `args` in `dir`, its cache and state in `cache`.
fn pekit(dir: &Path, cache: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_pekit"))
        .args(args)
        .current_dir(dir)
        .env("PEKIT_CACHE_DIR", cache)
        .env("PEKIT_STATE_DIR", cache.join("state"))
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .env_remove("GCSL_LIBRARY_PATH")
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

/// Whether `git` can be run here.
fn has_git() -> bool {
    Command::new("git").arg("--version").output().is_ok()
}

#[test]
fn a_release_is_built_from_head_published_and_tagged() {
    if !has_git() {
        eprintln!("skipped: no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    repository(dir.path(), "1.0");
    let ws = dir.path().join("ws");
    let cache = tempfile::tempdir().unwrap();
    let store = tempfile::tempdir().unwrap();
    let to = format!("--to={}", store.path().display());
    // The build's own outputs make no difference to whether it is clean.
    let built = pekit(&ws, cache.path(), &["package"]);
    assert!(built.status.success(), "{:?}", shown(&built));
    let released = pekit(&ws, cache.path(), &["release", "dev.example.data", &to]);
    let (out, err) = shown(&released);
    assert!(released.status.success(), "{err}");
    let head = git(dir.path(), &["rev-parse", "HEAD"]);
    assert_eq!(
        out,
        format!(
            "published dev.example.data@1.0 with copy into {}\ntagged dev.example.data@1.0 at \
             {head}\n",
            store.path().display()
        )
    );
    let published = store.path().join("dev.example.data-1.0/share/data.txt");
    assert_eq!(fs::read_to_string(published).unwrap(), "data\n");
    assert_eq!(
        git(dir.path(), &["rev-parse", "dev.example.data@1.0"]),
        head
    );
    // Released again from the same commit, it is as it was.
    let again = pekit(&ws, cache.path(), &["release", "dev.example.data", &to]);
    assert!(again.status.success(), "{:?}", shown(&again));
    // What is not committed is never released.
    write(&ws, &[("app/data.txt", "changed\n")]);
    let dirty = pekit(&ws, cache.path(), &["release", "dev.example.data", &to]);
    let (_, err) = shown(&dirty);
    assert!(!dirty.status.success());
    assert_eq!(
        err,
        "error: a release is built from `HEAD`, and the workspace differs from it: commit or \
         remove each of\n   M ws/app/data.txt\n"
    );
    // A version released from another commit is refused before anything
    // is published.
    git(dir.path(), &["commit", "-qam", "two"]);
    fs::remove_dir_all(store.path().join("dev.example.data-1.0")).unwrap();
    let refused = pekit(&ws, cache.path(), &["release", "dev.example.data", &to]);
    let (_, err) = shown(&refused);
    assert!(!refused.status.success());
    assert!(
        err.starts_with(&format!(
            "error: the tag `dev.example.data@1.0` is on {head}, not on `HEAD`"
        )),
        "{err}"
    );
    assert!(!store.path().join("dev.example.data-1.0").exists());
}

#[test]
fn a_release_needs_a_commit() {
    if !has_git() {
        eprintln!("skipped: no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    write(dir.path(), &[("workspace.gcsl", "")]);
    let cache = tempfile::tempdir().unwrap();
    let released = pekit(dir.path(), cache.path(), &["release", "x", "--to=/nowhere"]);
    assert!(!released.status.success());
    assert!(
        shown(&released).1.contains("is in no git repository"),
        "{:?}",
        shown(&released)
    );
}
