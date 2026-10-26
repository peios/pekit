//! Tests of answering `fetch_git` from the store (see `design/image.md`,
//! I7).

mod building;

use std::fs;
use std::path::Path;
use std::process::Command;

use building::{Options, build_in, observed_with};

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// A git repository at `dir` of one commit, `a.txt`, `sub/b.txt` and the
/// link `l` to `a.txt`: the commit's name, or `None` where `git` can't run.
fn repository(dir: &Path) -> Option<String> {
    write(dir, &[("a.txt", "a\n"), ("sub/b.txt", "b\n")]);
    std::os::unix::fs::symlink("a.txt", dir.join("l")).unwrap();
    let git = |args: &[&str]| {
        Command::new("git")
            .arg("-C")
            .arg(dir)
            .args([
                "-c",
                "user.name=t",
                "-c",
                "user.email=t@t",
                "-c",
                "commit.gpgsign=false",
            ])
            .args(args)
            .output()
            .ok()
            .filter(|output| output.status.success())
            .map(|output| String::from_utf8_lossy(&output.stdout).trim().to_owned())
    };
    git(&["init", "--quiet"])?;
    git(&["add", "."])?;
    git(&["commit", "--quiet", "-m", "one"])?;
    git(&["rev-parse", "HEAD"])
}

/// A workspace whose `app` builds `fetch_git` of `url` at `commit`, and a
/// step of what it gives.
fn workspace(url: &str, commit: &str) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                "#[hermetic] def list(src: Dir) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"cat {src.path}/a.txt {src.path}/sub/b.txt {src.path}/l > out\"])\n\
                 \x20   = file(\"out\")\n\
                 }\n",
            ),
            (
                "app/build.gcsl",
                &format!(
                    "def build src() {{ = fetch_git(\"{url}\", \"{commit}\") }}\n\
                     def build both() {{ = list(fetch_git(\"{url}\", \"{commit}\")) }}\n"
                ),
            ),
        ],
    );
    dir
}

#[test]
fn a_repository_s_files_at_a_commit_are_fetched_once() {
    let origin = tempfile::tempdir().unwrap();
    let Some(commit) = repository(origin.path()) else {
        eprintln!("skipped: no `git` on this host");
        return;
    };
    let url = format!("file://{}", origin.path().display());
    let dir = workspace(&url, &commit);
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "both").unwrap();
    assert_eq!(built.ran, 1);
    let out = dir.path().join("pekit-out/app/both/list/out");
    assert_eq!(fs::read_to_string(&out).unwrap(), "a\nb\na\n");
    // The repository gone, the commit is found in the store.
    drop(origin);
    let src = build_in(dir.path(), state.path(), "src").unwrap();
    let [(path, _)] = src.value.files()[..] else {
        panic!("{:?}", src.value);
    };
    let at = dir.path().join(path.trim_start_matches('/'));
    assert_eq!(fs::read_to_string(at.join("sub/b.txt")).unwrap(), "b\n");
    assert_eq!(fs::read_link(at.join("l")).unwrap(), Path::new("a.txt"));
    assert!(!at.join(".git").exists());
    // And strictly, as a pinned fetch.
    let strict = Options {
        strict: true,
        ..Options::default()
    };
    observed_with(dir.path(), state.path(), "src", strict)
        .0
        .unwrap();
}

#[test]
fn a_fetch_git_is_pinned_by_a_commit_and_made_outside_steps() {
    let origin = tempfile::tempdir().unwrap();
    let Some(commit) = repository(origin.path()) else {
        eprintln!("skipped: no `git` on this host");
        return;
    };
    let url = format!("file://{}", origin.path().display());
    let state = tempfile::tempdir().unwrap();
    let moving = build_in(workspace(&url, "main").path(), state.path(), "src").unwrap_err();
    assert!(
        moving.contains("which is no commit's name: 40 hexadecimal digits"),
        "{moving}"
    );
    let absent = "0".repeat(40);
    let missing = build_in(workspace(&url, &absent).path(), state.path(), "src").unwrap_err();
    assert!(
        missing.contains(&format!(
            "`fetch_git` could not fetch `{absent}` of `{url}`"
        )),
        "{missing}"
    );
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                &format!(
                    "#[hermetic] def inside() -> Dir {{ = fetch_git(\"{url}\", \"{commit}\") }}\n"
                ),
            ),
            ("app/build.gcsl", "def build both() { = inside() }\n"),
        ],
    );
    let refused = build_in(dir.path(), state.path(), "both").unwrap_err();
    assert!(
        refused.contains("`fetch_git` runs outside hermetic steps"),
        "{refused}"
    );
}
