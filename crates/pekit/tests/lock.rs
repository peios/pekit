//! Tests of `pekit lock` and of the lock every command reads, with a git
//! repository of libraries of the test's own.

mod skipping;

use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Command, Output};

use skipping::skip;

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
        .env("XDG_RUNTIME_DIR", cache.join("run"))
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
        skip("no `git`");
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
        skip("no `git`");
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

#[test]
fn a_library_dropped_leaves_the_libraries_its_directory_is_in_or_holds() {
    if !has_git() {
        eprintln!("skipped: no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    let repo = dir.path().join("repo");
    write(
        &repo,
        &[
            ("libs/sdk/sdk.gcsl", "def name() -> String { \"sdk\" }\n"),
            (
                "libs/sdk/tools/tools.gcsl",
                "def tool() -> String { \"tool\" }\n",
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
    let ws = dir.path().join("ws");
    let both = "import \"example.org/libs/sdk\"\n\
                import \"example.org/libs/sdk/tools\"\n\
                def build hi() { = \"{sdk.name()} {tools.tool()}\" }\n";
    write(
        &ws,
        &[
            ("workspace.gcsl", ""),
            ("app/build.gcsl", both),
            ("pekit.lock", &lock),
        ],
    );
    let cache = tempfile::tempdir().unwrap();
    let locked = pekit(&ws, cache.path(), &["lock"], None);
    assert!(locked.status.success(), "{:?}", shown(&locked));
    let sdk = ws.join(".gcsl/libraries/example.org/libs/sdk");
    assert!(sdk.join("tools/tools.gcsl").is_file());
    // The library inside another dropped: its directory is the other's.
    write(
        &ws,
        &[(
            "app/build.gcsl",
            "import \"example.org/libs/sdk\"\n\
             def build hi() { = sdk.name() }\n",
        )],
    );
    let dropped = pekit(&ws, cache.path(), &["lock"], None);
    assert_eq!(
        shown(&dropped).0,
        "dropped example.org/libs/sdk/tools, no longer imported\n"
    );
    assert!(sdk.join("tools/tools.gcsl").is_file());
    let built = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
    assert!(built.status.success(), "{:?}", shown(&built));
    // The library holding another dropped: the other stays.
    write(&ws, &[("app/build.gcsl", both)]);
    assert!(pekit(&ws, cache.path(), &["lock"], None).status.success());
    write(
        &ws,
        &[(
            "app/build.gcsl",
            "import \"example.org/libs/sdk/tools\"\n\
             def build hi() { = tools.tool() }\n",
        )],
    );
    let dropped = pekit(&ws, cache.path(), &["lock"], None);
    assert_eq!(
        shown(&dropped).0,
        "dropped example.org/libs/sdk, no longer imported\n"
    );
    assert!(sdk.join("tools/tools.gcsl").is_file());
    assert!(!sdk.join("sdk.gcsl").exists());
    let built = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
    assert!(built.status.success(), "{:?}", shown(&built));
}

/// The commit `rev` of the repository `dir`.
fn rev_parse(dir: &Path, rev: &str) -> String {
    let ran = Command::new("git")
        .args(["rev-parse", rev])
        .current_dir(dir)
        .output()
        .unwrap();
    assert!(ran.status.success(), "{ran:?}");
    String::from_utf8(ran.stdout).unwrap().trim().to_owned()
}

/// What a workspace that imports both `example.org/libs/sdk` and the
/// library in its directory, `example.org/libs/sdk/tools`, builds them to.
const BOTH: &str = "import \"example.org/libs/sdk\"\n\
                    import \"example.org/libs/sdk/tools\"\n\
                    def build hi() { = \"{sdk.name()} {tools.tool()}\" }\n";

/// A repository whose `libs/sdk` holds `libs/sdk/tools`, committed, and a
/// workspace whose `app` imports `sdk` alone, locked, its lock naming the
/// repository as the source of `example.org/libs`: each in a directory of
/// `dir`, the workspace's cache in `cache`. The repository, the workspace
/// and the commit pinned.
fn nested_libraries_locked(dir: &Path, cache: &Path) -> (PathBuf, PathBuf, String) {
    let repo = dir.join("repo");
    write(
        &repo,
        &[
            ("libs/sdk/sdk.gcsl", "def name() -> String { \"sdk\" }\n"),
            (
                "libs/sdk/tools/tools.gcsl",
                "def tool() -> String { \"tool\" }\n",
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
    let ws = dir.join("ws");
    write(
        &ws,
        &[
            ("workspace.gcsl", ""),
            (
                "app/build.gcsl",
                "import \"example.org/libs/sdk\"\n\
                 def build hi() { = sdk.name() }\n",
            ),
            ("pekit.lock", &lock),
        ],
    );
    let locked = pekit(&ws, cache, &["lock"], None);
    assert!(locked.status.success(), "{:?}", shown(&locked));
    let one = rev_parse(&repo, "HEAD");
    (repo, ws, one)
}

#[test]
fn a_library_in_another_s_directory_is_pinned_at_its_commit_or_refused() {
    if !has_git() {
        skip("no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    let cache = tempfile::tempdir().unwrap();
    let (repo, ws, one) = nested_libraries_locked(dir.path(), cache.path());
    // The repository moves on, and the library inside the one pinned is
    // imported: pinned at the source's revision, it would be at another
    // commit than the one its directory is in.
    write(
        &repo,
        &[(
            "libs/sdk/tools/tools.gcsl",
            "def tool() -> String { \"tool two\" }\n",
        )],
    );
    git(&repo, &["commit", "-qam", "two"]);
    let two = rev_parse(&repo, "HEAD");
    write(&ws, &[("app/build.gcsl", BOTH)]);
    let before = fs::read_to_string(ws.join("pekit.lock")).unwrap();
    let refused = pekit(&ws, cache.path(), &["lock"], None);
    let (out, err) = shown(&refused);
    assert!(!refused.status.success(), "{out}");
    let repo = repo.display();
    assert_eq!(
        err,
        format!(
            "error: `example.org/libs/sdk/tools`'s directory is in `example.org/libs/sdk`'s, \
             so both must be pinned at one commit, and are not:\n  \
             `example.org/libs/sdk` is `libs/sdk` of `{repo}` at {one}\n  \
             `example.org/libs/sdk/tools` is `libs/sdk/tools` of `{repo}` at {two}\n\
             pin both at one commit: give their sources in `pekit.lock` one repository and \
             revision, and run `pekit lock --update`\n"
        )
    );
    assert_eq!(fs::read_to_string(ws.join("pekit.lock")).unwrap(), before);
    // Both pinned at the repository's new commit: of one commit, the one's
    // directory the other's, which a strict build finds as pinned.
    let updated = pekit(&ws, cache.path(), &["lock", "--update"], None);
    assert!(updated.status.success(), "{:?}", shown(&updated));
    assert_eq!(
        shown(&updated).0,
        format!(
            "pinned example.org/libs/sdk at {two}\n\
             pinned example.org/libs/sdk/tools at {two}\n"
        )
    );
    let built = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
    assert!(built.status.success(), "{:?}", shown(&built));
}

#[test]
fn a_lock_pinning_a_library_in_another_s_directory_at_another_commit_is_refused() {
    if !has_git() {
        skip("no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    let cache = tempfile::tempdir().unwrap();
    let (repo, ws, one) = nested_libraries_locked(dir.path(), cache.path());
    write(&ws, &[("app/build.gcsl", BOTH)]);
    git(&repo, &["commit", "-q", "--allow-empty", "-m", "two"]);
    assert!(
        pekit(&ws, cache.path(), &["lock", "--update"], None)
            .status
            .success()
    );
    // A lock pinning them at two commits, as one written by hand might:
    // every command refuses it before putting either back, whichever order
    // it would put them back in.
    let mut written: serde_json::Value =
        serde_json::from_str(&fs::read_to_string(ws.join("pekit.lock")).unwrap()).unwrap();
    for pin in written["libraries"].as_array_mut().unwrap() {
        if pin["path"] == "example.org/libs/sdk/tools" {
            pin["commit"] = serde_json::Value::String(one.clone());
        }
    }
    fs::write(
        ws.join("pekit.lock"),
        serde_json::to_string_pretty(&written).unwrap(),
    )
    .unwrap();
    fs::remove_dir_all(ws.join(".gcsl")).unwrap();
    let refused = pekit(&ws, cache.path(), &["build", "hi"], None);
    let (_, err) = shown(&refused);
    assert!(!refused.status.success());
    assert!(
        err.starts_with(
            "error: `example.org/libs/sdk/tools`'s directory is in `example.org/libs/sdk`'s, \
             so both must be pinned at one commit, and are not:\n"
        ),
        "{err}"
    );
    assert!(
        err.contains(&format!(
            "`libs/sdk/tools` of `{}` at {one}",
            repo.display()
        )),
        "{err}"
    );
    assert!(!ws.join(".gcsl/libraries").exists());
}

#[test]
fn a_library_of_another_repository_in_another_s_directory_is_refused() {
    if !has_git() {
        skip("no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    let cache = tempfile::tempdir().unwrap();
    let (repo, ws, _) = nested_libraries_locked(dir.path(), cache.path());
    let other = dir.path().join("other");
    write(
        &other,
        &[("tools.gcsl", "def tool() -> String { \"other\" }\n")],
    );
    git(&other, &["init", "-q", "-b", "main"]);
    git(&other, &["add", "-A"]);
    git(&other, &["commit", "-qm", "one"]);
    let mut written: serde_json::Value =
        serde_json::from_str(&fs::read_to_string(ws.join("pekit.lock")).unwrap()).unwrap();
    written["sources"] = serde_json::json!([
        {"prefix": "example.org/libs/sdk/tools", "git": other.display().to_string(), "rev": "main"},
        {"prefix": "example.org/libs", "git": repo.display().to_string(), "dir": "libs", "rev": "main"},
    ]);
    fs::write(
        ws.join("pekit.lock"),
        serde_json::to_string_pretty(&written).unwrap(),
    )
    .unwrap();
    write(&ws, &[("app/build.gcsl", BOTH)]);
    let refused = pekit(&ws, cache.path(), &["lock"], None);
    let (_, err) = shown(&refused);
    assert!(!refused.status.success());
    assert!(
        err.contains(&format!(
            "`example.org/libs/sdk/tools` is the root of `{}` at",
            other.display()
        )),
        "{err}"
    );
}
