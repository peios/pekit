//! Tests of `pekit lock` and of the lock every command reads, with a git
//! repository of libraries of the test's own.

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

/// Runs `git` with `args` in `dir`, as no one's configuration says.
fn git(dir: &Path, args: &[&str]) {
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
}

/// A repository whose `libs/greet` imports `libs/util`, committed, and a
/// workspace whose `app` builds with `greet`, its lock naming the
/// repository as the source of `example.org/libs`: each in a directory of
/// `dir`.
fn repository_and_workspace(dir: &Path) {
    let repo = dir.join("repo");
    write(
        &repo,
        &[
            (
                "libs/greet/greet.gcsl",
                "import \"example.org/libs/util\"\n\
                 def hello(name: String) -> String { \"hello {util.loud(name)}\" }\n",
            ),
            (
                "libs/util/util.gcsl",
                "def loud(name: String) -> String { \"{name}!\" }\n",
            ),
        ],
    );
    git(&repo, &["init", "-q", "-b", "main"]);
    git(&repo, &["add", "-A"]);
    git(&repo, &["commit", "-qm", "one"]);
    let lock = format!(
        "{{\"sources\": [{{\"prefix\": \"example.org/libs\", \"git\": \"{}\", \"dir\": \
         \"libs\", \"rev\": \"main\"}}]}}\n",
        repo.display()
    );
    write(
        &dir.join("ws"),
        &[
            ("workspace.gcsl", ""),
            (
                "app/build.gcsl",
                "import \"example.org/libs/greet\"\n\
                 def build hi() { = greet.hello(\"you\") }\n",
            ),
            ("pekit.lock", &lock),
        ],
    );
}

/// `pekit` run with `args` in `dir`, its cache and state in `cache`, and
/// `$GCSL_LIBRARY_PATH` as `library_path` says.
fn pekit(dir: &Path, cache: &Path, args: &[&str], library_path: Option<&str>) -> Output {
    let mut command = Command::new(env!("CARGO_BIN_EXE_pekit"));
    command
        .args(args)
        .current_dir(dir)
        .env("PEKIT_CACHE_DIR", cache)
        .env("PEKIT_STATE_DIR", cache.join("state"))
        .env("GIT_CONFIG_GLOBAL", "/dev/null");
    match library_path {
        Some(path) => command.env("GCSL_LIBRARY_PATH", path),
        None => command.env_remove("GCSL_LIBRARY_PATH"),
    };
    command.output().unwrap()
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
fn lock_pins_every_library_imported_and_every_command_puts_one_back() {
    if !has_git() {
        eprintln!("skipped: no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    repository_and_workspace(dir.path());
    let ws = dir.path().join("ws");
    let cache = tempfile::tempdir().unwrap();
    let locked = pekit(&ws, cache.path(), &["lock"], None);
    let (out, err) = shown(&locked);
    assert!(locked.status.success(), "{err}");
    // Each, the library a library imports among them.
    let lines: Vec<&str> = out.lines().collect();
    assert_eq!(lines.len(), 2, "{out}");
    assert!(
        lines[0].starts_with("pinned example.org/libs/greet at "),
        "{out}"
    );
    assert!(
        lines[1].starts_with("pinned example.org/libs/util at "),
        "{out}"
    );
    let libraries = ws.join(".gcsl/libraries/example.org/libs");
    assert!(libraries.join("greet/greet.gcsl").is_file());
    assert!(libraries.join("util/util.gcsl").is_file());
    // Locked again, nothing changes; a library gone is put back, and a
    // strict build finds what the lock pins.
    let again = pekit(&ws, cache.path(), &["lock"], None);
    assert_eq!(shown(&again).0, "");
    fs::remove_dir_all(ws.join(".gcsl")).unwrap();
    let built = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
    assert!(built.status.success(), "{:?}", shown(&built));
    assert!(libraries.join("greet/greet.gcsl").is_file());
    // A new commit is pinned only when asked.
    let repo = dir.path().join("repo");
    write(
        &repo,
        &[("libs/util/more.gcsl", "def more() -> String { \"more\" }\n")],
    );
    git(&repo, &["add", "-A"]);
    git(&repo, &["commit", "-qm", "two"]);
    assert_eq!(shown(&pekit(&ws, cache.path(), &["lock"], None)).0, "");
    assert!(!libraries.join("util/more.gcsl").exists());
    // Both are of the repository's new commit, though only one changed.
    let updated = pekit(&ws, cache.path(), &["lock", "--update"], None);
    let (out, _) = shown(&updated);
    let commits: Vec<&str> = out
        .lines()
        .filter_map(|line| line.rsplit(' ').next())
        .collect();
    assert_eq!(commits.len(), 2, "{out}");
    assert_eq!(commits[0], commits[1]);
    assert!(!lines[0].ends_with(commits[0]), "{out}");
    assert!(libraries.join("util/more.gcsl").is_file());
    // No longer imported, a library is dropped, its directory with it.
    write(&ws, &[("app/build.gcsl", "def build hi() { = 1 }\n")]);
    let dropped = pekit(&ws, cache.path(), &["lock"], None);
    assert_eq!(
        shown(&dropped).0,
        "dropped example.org/libs/greet, no longer imported\n\
         dropped example.org/libs/util, no longer imported\n"
    );
    assert!(!libraries.join("greet").exists());
}

#[test]
fn a_strict_build_fails_on_libraries_the_lock_does_not_pin() {
    if !has_git() {
        eprintln!("skipped: no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    repository_and_workspace(dir.path());
    let ws = dir.path().join("ws");
    let cache = tempfile::tempdir().unwrap();
    assert!(pekit(&ws, cache.path(), &["lock"], None).status.success());
    // A library changed where it was put: a warning, and a strict failure.
    let file = ws.join(".gcsl/libraries/example.org/libs/util/util.gcsl");
    let text = fs::read_to_string(&file).unwrap();
    fs::write(&file, format!("{text}# changed\n")).unwrap();
    let built = pekit(&ws, cache.path(), &["build", "hi"], None);
    let (_, err) = shown(&built);
    assert!(built.status.success(), "{err}");
    assert!(
        err.starts_with(
            "warning: `.gcsl/libraries/example.org/libs/util` differs from the commit \
             `pekit.lock` pins"
        ),
        "{err}"
    );
    let strict = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
    let (_, err) = shown(&strict);
    assert!(!strict.status.success());
    assert!(
        err.starts_with(
            "error: a strict build uses the libraries `pekit.lock` pins, and only them:\n  \
             `.gcsl/libraries/example.org/libs/util` differs"
        ),
        "{err}"
    );
    fs::write(&file, text).unwrap();
    // Libraries found elsewhere, or none pinned.
    let elsewhere = pekit(
        &ws,
        cache.path(),
        &["build", "hi", "--strict"],
        Some("/nowhere"),
    );
    assert!(shown(&elsewhere).1.contains("`$GCSL_LIBRARY_PATH` is set"));
    fs::remove_file(ws.join("pekit.lock")).unwrap();
    let unlocked = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
    assert!(
        shown(&unlocked)
            .1
            .contains("the workspace has no `pekit.lock`: run `pekit lock`"),
        "{:?}",
        shown(&unlocked)
    );
    // Without `--strict`, libraries put there by hand still build.
    let built = pekit(&ws, cache.path(), &["build", "hi"], None);
    assert!(built.status.success(), "{:?}", shown(&built));
}
