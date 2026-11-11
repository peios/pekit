//! Tests of `pekit lock` and of the lock every command reads, with a git
//! repository of libraries of the test's own.

mod skipping;

use std::fs;
use std::os::unix::fs::PermissionsExt as _;
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
    let mut command = pekit_command(dir, cache, args);
    match library_path {
        Some(path) => command.env("GCSL_LIBRARY_PATH", path),
        None => command.env_remove("GCSL_LIBRARY_PATH"),
    };
    command.output().unwrap()
}

/// `pekit` with `args` to run in `dir`, its cache and state in `cache`.
fn pekit_command(dir: &Path, cache: &Path, args: &[&str]) -> Command {
    let mut command = Command::new(env!("CARGO_BIN_EXE_pekit"));
    command
        .args(args)
        .current_dir(dir)
        .env("PEKIT_CACHE_DIR", cache)
        .env("PEKIT_STATE_DIR", cache.join("state"))
        .env("XDG_RUNTIME_DIR", cache.join("run"))
        .env("GIT_CONFIG_GLOBAL", "/dev/null");
    command
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

/// Checks the records of a workspace whose libraries have just been exported.
fn assert_export_records(ws: &Path) {
    let lock: serde_json::Value =
        serde_json::from_slice(&fs::read(ws.join("pekit.lock")).unwrap()).unwrap();
    let exports: serde_json::Value =
        serde_json::from_slice(&fs::read(ws.join(".gcsl/pekit-exports.json")).unwrap()).unwrap();
    for pin in lock["libraries"].as_array().unwrap() {
        let exported = &exports[pin["path"].as_str().unwrap()];
        assert_eq!(exported["commit"], pin["commit"]);
        assert_eq!(exported["tree"], pin["tree"]);
    }
}

/// Moves only `ws`'s lock to the commit now in its libraries' repository,
/// as pulling a changed lock does, leaving its exports and records alone.
fn move_lock(ws: &Path, cache: &Path) {
    let next = ws.with_file_name("next");
    write(
        &next,
        &[
            ("workspace.gcsl", ""),
            (
                "app/build.gcsl",
                &fs::read_to_string(ws.join("app/build.gcsl")).unwrap(),
            ),
            (
                "pekit.lock",
                &fs::read_to_string(ws.join("pekit.lock")).unwrap(),
            ),
        ],
    );
    let locked = pekit(&next, cache, &["lock", "--update"], None);
    assert!(locked.status.success(), "{:?}", shown(&locked));
    fs::copy(next.join("pekit.lock"), ws.join("pekit.lock")).unwrap();
}

/// A workspace exported at one commit, with its lock moved to another.
fn workspace_with_moved_pin(dir: &Path, cache: &Path) -> PathBuf {
    repository_and_workspace(dir);
    let ws = dir.join("ws");
    let locked = pekit(&ws, cache, &["lock"], None);
    assert!(locked.status.success(), "{:?}", shown(&locked));
    assert_export_records(&ws);
    let repo = dir.join("repo");
    write(
        &repo,
        &[
            (
                "libs/greet/greet.gcsl",
                "import \"example.org/libs/util\"\n\
                 def hello(name: String) -> String { \"hi {util.loud(name)}\" }\n",
            ),
            ("libs/util/more.gcsl", "def more() -> String { \"more\" }\n"),
        ],
    );
    git(&repo, &["add", "-A"]);
    git(&repo, &["commit", "-qm", "two"]);
    move_lock(&ws, cache);
    ws
}

#[test]
fn every_build_refreshes_untouched_exports_when_the_lock_moves() {
    if !has_git() {
        skip("no `git`");
        return;
    }
    for args in [&["build", "hi"][..], &["build", "hi", "--strict"][..]] {
        let dir = tempfile::tempdir().unwrap();
        let cache = tempfile::tempdir().unwrap();
        let ws = workspace_with_moved_pin(dir.path(), cache.path());
        let built = pekit(&ws, cache.path(), args, None);
        assert!(built.status.success(), "{:?}", shown(&built));
        assert!(!shown(&built).1.contains("differs from the commit"));
        assert!(
            ws.join(".gcsl/libraries/example.org/libs/util/more.gcsl")
                .is_file()
        );
        assert_export_records(&ws);
        let strict = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
        assert!(strict.status.success(), "{:?}", shown(&strict));
    }
}

#[test]
fn an_unrecorded_export_as_its_pin_has_it_is_recorded_and_then_refreshed() {
    if !has_git() {
        skip("no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    let cache = tempfile::tempdir().unwrap();
    repository_and_workspace(dir.path());
    let ws = dir.path().join("ws");
    let locked = pekit(&ws, cache.path(), &["lock"], None);
    assert!(locked.status.success(), "{:?}", shown(&locked));
    // As an older pekit, which kept no record, leaves a workspace.
    fs::remove_file(ws.join(".gcsl/pekit-exports.json")).unwrap();
    let built = pekit(&ws, cache.path(), &["build", "hi"], None);
    assert!(built.status.success(), "{:?}", shown(&built));
    assert_export_records(&ws);
    let repo = dir.path().join("repo");
    write(
        &repo,
        &[("libs/util/more.gcsl", "def more() -> String { \"more\" }\n")],
    );
    git(&repo, &["add", "-A"]);
    git(&repo, &["commit", "-qm", "two"]);
    move_lock(&ws, cache.path());
    let strict = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
    assert!(strict.status.success(), "{:?}", shown(&strict));
    assert!(
        ws.join(".gcsl/libraries/example.org/libs/util/more.gcsl")
            .is_file()
    );
    assert_export_records(&ws);
}

#[test]
fn a_moved_pin_leaves_an_edited_export_alone_with_a_recovery_hint() {
    if !has_git() {
        skip("no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    let cache = tempfile::tempdir().unwrap();
    let ws = workspace_with_moved_pin(dir.path(), cache.path());
    let library = ws.join(".gcsl/libraries/example.org/libs/util");
    let file = library.join("util.gcsl");
    let text = format!("{}# edited by hand\n", fs::read_to_string(&file).unwrap());
    fs::write(&file, &text).unwrap();
    for args in [&["build", "hi"][..], &["build", "hi", "--strict"][..]] {
        let built = pekit(&ws, cache.path(), args, None);
        assert_eq!(built.status.success(), !args.contains(&"--strict"));
        assert!(
            shown(&built).1.contains(
                "remove `.gcsl/libraries/example.org/libs/util` and pekit will restore it from the pin"
            ),
            "{:?}",
            shown(&built)
        );
        assert_eq!(fs::read_to_string(&file).unwrap(), text);
        assert!(!library.join("more.gcsl").exists());
    }
    fs::remove_dir_all(&library).unwrap();
    let recovered = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
    assert!(recovered.status.success(), "{:?}", shown(&recovered));
    assert_export_records(&ws);
}

#[test]
fn a_moved_pin_leaves_an_unrecorded_export_alone_with_a_recovery_hint() {
    if !has_git() {
        skip("no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    let cache = tempfile::tempdir().unwrap();
    let ws = workspace_with_moved_pin(dir.path(), cache.path());
    let record = ws.join(".gcsl/pekit-exports.json");
    fs::remove_file(&record).unwrap();
    let libraries = ws.join(".gcsl/libraries/example.org/libs");
    let greet = fs::read(libraries.join("greet/greet.gcsl")).unwrap();
    let util = fs::read(libraries.join("util/util.gcsl")).unwrap();
    for args in [&["build", "hi"][..], &["build", "hi", "--strict"][..]] {
        let built = pekit(&ws, cache.path(), args, None);
        assert_eq!(built.status.success(), !args.contains(&"--strict"));
        let error = shown(&built).1;
        for path in ["greet", "util"] {
            assert!(
                error.contains(&format!(
                    "remove `.gcsl/libraries/example.org/libs/{path}` and pekit will restore it from the pin"
                )),
                "{error}"
            );
        }
        assert_eq!(fs::read(libraries.join("greet/greet.gcsl")).unwrap(), greet);
        assert_eq!(fs::read(libraries.join("util/util.gcsl")).unwrap(), util);
        assert!(!libraries.join("util/more.gcsl").exists());
        assert!(!record.exists());
    }
    fs::remove_dir_all(&libraries).unwrap();
    let recovered = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
    assert!(recovered.status.success(), "{:?}", shown(&recovered));
    assert_export_records(&ws);
}

#[test]
fn a_refresh_with_the_wrong_tree_digest_leaves_the_old_export_and_record() {
    if !has_git() {
        skip("no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    let cache = tempfile::tempdir().unwrap();
    let ws = workspace_with_moved_pin(dir.path(), cache.path());
    let path = ws.join("pekit.lock");
    let mut lock: serde_json::Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
    lock["libraries"][1]["tree"] = "0".repeat(64).into();
    fs::write(&path, serde_json::to_vec(&lock).unwrap()).unwrap();
    let record = ws.join(".gcsl/pekit-exports.json");
    let before: serde_json::Value = serde_json::from_slice(&fs::read(&record).unwrap()).unwrap();
    let library = ws.join(".gcsl/libraries/example.org/libs/util");
    let text = fs::read(library.join("util.gcsl")).unwrap();
    let built = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
    assert!(!built.status.success());
    assert!(
        shown(&built)
            .1
            .contains("does not match the tree digest `pekit.lock` pins"),
        "{:?}",
        shown(&built)
    );
    assert_eq!(fs::read(library.join("util.gcsl")).unwrap(), text);
    assert!(!library.join("more.gcsl").exists());
    let after: serde_json::Value = serde_json::from_slice(&fs::read(&record).unwrap()).unwrap();
    assert_eq!(
        before["example.org/libs/util"],
        after["example.org/libs/util"]
    );
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
    assert_export_records(&ws);
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

/// A directory in `dir` holding a `git` that runs the `git` on `$PATH`,
/// except that after an archive it writes 1 MiB more of zeroed padding:
/// more than a pipe holds, so that it is still writing when a reader
/// stopped at the end of the archive.
fn padding_git(dir: &Path) -> PathBuf {
    let real = std::env::split_paths(&std::env::var_os("PATH").unwrap())
        .map(|dir| dir.join("git"))
        .find(|git| git.is_file())
        .unwrap();
    let bin = dir.join("bin");
    fs::create_dir_all(&bin).unwrap();
    let git = bin.join("git");
    fs::write(
        &git,
        format!(
            "#!/bin/sh\n\
             for arg do\n\
             \tif [ \"$arg\" = archive ]; then\n\
             \t\t'{0}' \"$@\" || exit\n\
             \t\texec head -c 1048576 /dev/zero\n\
             \tfi\n\
             done\n\
             exec '{0}' \"$@\"\n",
            real.display()
        ),
    )
    .unwrap();
    fs::set_permissions(&git, fs::Permissions::from_mode(0o755)).unwrap();
    bin
}

#[test]
fn lock_reads_what_git_writes_after_the_end_of_an_archive() {
    if !has_git() {
        skip("no `git`");
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    repository_and_workspace(dir.path());
    let ws = dir.path().join("ws");
    let cache = tempfile::tempdir().unwrap();
    let path = std::env::join_paths(
        std::iter::once(padding_git(dir.path()))
            .chain(std::env::split_paths(&std::env::var_os("PATH").unwrap())),
    )
    .unwrap();
    let locked = pekit_command(&ws, cache.path(), &["lock"])
        .env("PATH", path)
        .env_remove("GCSL_LIBRARY_PATH")
        .output()
        .unwrap();
    assert!(locked.status.success(), "{:?}", shown(&locked));
    let libraries = ws.join(".gcsl/libraries/example.org/libs");
    assert!(libraries.join("greet/greet.gcsl").is_file());
    assert!(libraries.join("util/util.gcsl").is_file());
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
fn nested_exports_refresh_outer_first_and_preserve_edits_inside() {
    if !has_git() {
        skip("no `git`");
        return;
    }
    for edited in [false, true] {
        let dir = tempfile::tempdir().unwrap();
        let cache = tempfile::tempdir().unwrap();
        let (repo, ws, _) = nested_libraries_locked(dir.path(), cache.path());
        write(&ws, &[("app/build.gcsl", BOTH)]);
        let locked = pekit(&ws, cache.path(), &["lock"], None);
        assert!(locked.status.success(), "{:?}", shown(&locked));
        write(
            &repo,
            &[
                (
                    "libs/sdk/sdk.gcsl",
                    "def name() -> String { \"new sdk\" }\n",
                ),
                (
                    "libs/sdk/tools/tools.gcsl",
                    "def tool() -> String { \"new tool\" }\n",
                ),
            ],
        );
        git(&repo, &["add", "-A"]);
        git(&repo, &["commit", "-qm", "two"]);
        move_lock(&ws, cache.path());
        let path = ws.join("pekit.lock");
        let mut lock: serde_json::Value =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        lock["libraries"].as_array_mut().unwrap().reverse();
        fs::write(&path, serde_json::to_vec(&lock).unwrap()).unwrap();
        let sdk = ws.join(".gcsl/libraries/example.org/libs/sdk");
        let file = sdk.join("tools/tools.gcsl");
        if edited {
            let text = format!("{}# edited by hand\n", fs::read_to_string(&file).unwrap());
            fs::write(&file, text).unwrap();
        }
        let outer = fs::read(sdk.join("sdk.gcsl")).unwrap();
        let inner = fs::read(&file).unwrap();
        let record = ws.join(".gcsl/pekit-exports.json");
        let before = fs::read(&record).unwrap();
        let built = pekit(&ws, cache.path(), &["build", "hi", "--strict"], None);
        assert_eq!(built.status.success(), !edited, "{:?}", shown(&built));
        if edited {
            assert!(
                shown(&built)
                    .1
                    .contains("and pekit will restore it from the pin")
            );
            assert_eq!(fs::read(sdk.join("sdk.gcsl")).unwrap(), outer);
            assert_eq!(fs::read(&file).unwrap(), inner);
            assert_eq!(fs::read(&record).unwrap(), before);
        } else {
            assert_export_records(&ws);
            assert_eq!(
                fs::read(sdk.join("sdk.gcsl")).unwrap(),
                fs::read(repo.join("libs/sdk/sdk.gcsl")).unwrap()
            );
            assert_eq!(
                fs::read(&file).unwrap(),
                fs::read(repo.join("libs/sdk/tools/tools.gcsl")).unwrap()
            );
        }
    }
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
