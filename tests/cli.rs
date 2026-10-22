use std::fs;
use std::os::unix::fs::{PermissionsExt, symlink};
use std::path::Path;
use std::process::{Command, Output};

use sha2::{Digest, Sha256};

/// Runs pekit in `dir` with its cache in `cache`.
fn pekit(dir: &Path, cache: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_pekit"))
        .args(args)
        .current_dir(dir)
        .env("XDG_CACHE_HOME", cache)
        .output()
        .unwrap()
}

fn stdout(output: &Output) -> String {
    String::from_utf8(output.stdout.clone()).unwrap()
}

/// The test recipe's package, as `pekit package` prints it.
const HELLO: &str = "dev.peios.hello@1.0-2";

/// The identity `pekit package` printed for its one package, `package`,
/// followed by a newline, as a ref holds it.
fn printed_identity(output: &Output, package: &str) -> String {
    let printed = stdout(output);
    let identity = printed
        .strip_prefix(&format!("{package} "))
        .unwrap_or_else(|| panic!("{printed}"));
    assert_eq!(identity.lines().count(), 1, "{printed}");
    identity.to_owned()
}

fn stderr(output: &Output) -> String {
    String::from_utf8(output.stderr.clone()).unwrap()
}

fn path(p: &Path) -> &str {
    p.to_str().unwrap()
}

/// A tarball of a `hello-1.0` source tree in `dir`, and its SHA-256.
fn tarball(dir: &Path) -> (String, String) {
    let tree = dir.join("hello-1.0");
    fs::create_dir_all(tree.join("bin")).unwrap();
    fs::create_dir_all(tree.join("share/doc")).unwrap();
    fs::write(tree.join("bin/hello"), "#!/bin/sh\necho hello\n").unwrap();
    fs::set_permissions(tree.join("bin/hello"), fs::Permissions::from_mode(0o755)).unwrap();
    symlink("hello", tree.join("bin/hi")).unwrap();
    fs::write(tree.join("share/doc/README"), "read me\n").unwrap();
    fs::write(tree.join("extra"), "left out\n").unwrap();
    // Every time the same, but the newest's: README's.
    touch(&tree, "@1700000000");
    touch(&tree.join("share/doc/README"), "@1700000100");
    let archive = dir.join("hello-1.0.tar.gz");
    let status = Command::new("tar")
        .arg("-czf")
        .arg(&archive)
        .arg("-C")
        .arg(dir)
        .arg("hello-1.0")
        .status()
        .unwrap();
    assert!(status.success());
    let hash = Sha256::digest(fs::read(&archive).unwrap());
    let hex = hash.iter().map(|byte| format!("{byte:02x}")).collect();
    (format!("file://{}", path(&archive)), hex)
}

/// Sets the modification time of `path`, and of everything under it, to
/// `time`, as touch(1) reads it.
fn touch(path: &Path, time: &str) {
    let status = Command::new("find")
        .arg(path)
        .args(["-exec", "touch", "-h", "-d", time, "{}", "+"])
        .status()
        .unwrap();
    assert!(status.success());
}

/// The `SOURCE_DATE_EPOCH` each build of [`recipe`] ran with, one line each,
/// in the log in `dir`.
fn builds(dir: &Path) -> String {
    fs::read_to_string(dir.join("builds")).unwrap_or_default()
}

/// The builds whose work directories the cache in `dir` keeps, failing if
/// anything else is left in its work directory or there is more than one
/// workspace's.
fn work_dirs(dir: &Path) -> Vec<String> {
    let names = |dir: &Path| -> Vec<String> {
        let mut names: Vec<String> = fs::read_dir(dir)
            .unwrap()
            .map(|entry| entry.unwrap().file_name().into_string().unwrap())
            .collect();
        names.sort();
        names
    };
    let work = dir.join("peios/pekit/work");
    let workspaces = names(&work);
    match workspaces.as_slice() {
        [] => Vec::new(),
        [workspace] => names(&work.join(workspace)),
        _ => panic!("{workspaces:?}"),
    }
}

/// A recipe for `dev.peios.hello` from the source at `url`, whose files are
/// `files`, whose build logs to `dir`.
fn recipe(url: &str, sha256: &str, files: &str, dir: &Path) -> String {
    let log = path(&dir.join("builds")).to_owned();
    format!(
        r#"pekit = 1
version = "1.0"
revision = "2"

def source hello-tarball() {{
    url = "{url}"
    sha256 = "{sha256}"
}}

defmeta build hello {{
    source = source hello-tarball
    dependencies = []
}}

def build hello(source_path, out_path, arch) {{
    sh("cp -a {{source_path}}/. {{out_path}}/")
    sh("echo {{arch}} > {{out_path}}/arch")
    sh("echo $SOURCE_DATE_EPOCH >> {log}")
    = out_path
}}

def package dev.peios.hello() {{
    build = build hello
    files = {{
{files}
    }}
}}
"#
    )
}

/// The usual files: a directory's entries, a directory, and a renamed file.
const FILES: &str = r#"        "bin/*" = "bin/*"
        "share/doc" = "share/doc"
        "lib/hello/arch" = "arch""#;

/// A recipe workspace in `dir`, initialised, with the recipe `recipe`.
fn workspace(dir: &Path, recipe: &str) -> std::path::PathBuf {
    let root = dir.join("pkgs");
    fs::create_dir_all(root.join("dev.peios.hello")).unwrap();
    let output = pekit(&root, dir, &["init"]);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    fs::write(root.join("dev.peios.hello/pekit.gcsl"), recipe).unwrap();
    root
}

#[test]
fn init_writes_the_include_and_keeps_an_existing_workspace_file() {
    let dir = tempfile::tempdir().unwrap();
    let output = pekit(dir.path(), dir.path(), &["init"]);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    let include = fs::read_to_string(dir.path().join("pekit-include.gcsl")).unwrap();
    assert!(include.contains("#[def_type]"), "{include}");
    assert_eq!(
        fs::read_to_string(dir.path().join("workspace.gcsl")).unwrap(),
        ""
    );

    // Again, over a changed include and a workspace file of the user's.
    fs::write(dir.path().join("pekit-include.gcsl"), "changed").unwrap();
    fs::write(dir.path().join("workspace.gcsl"), "x = 1\n").unwrap();
    let output = pekit(dir.path(), dir.path(), &["init"]);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    let again = fs::read_to_string(dir.path().join("pekit-include.gcsl")).unwrap();
    assert_eq!(again, include);
    assert_eq!(
        fs::read_to_string(dir.path().join("workspace.gcsl")).unwrap(),
        "x = 1\n"
    );
}

#[test]
fn package_builds_the_recipe_and_packs_the_files_it_chooses() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let root = workspace(dir.path(), &recipe(&url, &sha256, FILES, dir.path()));
    let artifacts = dir.path().join("artifacts");
    let args = [
        "package",
        "dev.peios.hello",
        "--artifact-dir",
        path(&artifacts),
    ];
    let output = pekit(&root.join("dev.peios.hello"), dir.path(), &args);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    let identity = printed_identity(&output, HELLO);
    let ref_path = artifacts
        .join("refs/dev.peios.hello/1.0-2")
        .join(ppk::HOST_ARCH);
    assert_eq!(fs::read_to_string(ref_path).unwrap(), identity);

    let hex = identity.trim().strip_prefix("sha256:").unwrap();
    let manifest = fs::read_to_string(artifacts.join("manifests/sha256").join(hex)).unwrap();
    let files: Vec<&str> = manifest
        .lines()
        .skip_while(|line| *line != "[files]")
        .skip(1)
        .map(|line| line.split(" = ").next().unwrap())
        .collect();
    assert_eq!(
        files,
        [
            "\"bin/hello\"",
            "\"bin/hi\"",
            "\"lib/hello/arch\"",
            "\"share/doc/README\"",
        ],
        "{manifest}"
    );
    assert!(
        manifest.contains("\"bin/hi\" = { symlink = \"hello\" }"),
        "{manifest}"
    );
    assert!(manifest.contains("exec = true"), "{manifest}");

    // The source is kept by its hash, and the build's work directory, under
    // the workspace's, for its next run; nothing else is left in work.
    assert!(
        dir.path()
            .join("peios/pekit/sources/sha256")
            .join(&sha256)
            .is_file()
    );
    assert_eq!(work_dirs(dir.path()), ["hello"]);
    // The build ran once, with its time the source's newest.
    assert_eq!(builds(dir.path()), "1700000100\n");

    // Packed already, it is not built again.
    let again = pekit(&root, dir.path(), &args);
    assert_eq!(again.status.code(), Some(0), "{}", stderr(&again));
    assert_eq!(printed_identity(&again, HELLO), identity);
    let id = format!("dev.peios.hello@1.0-2?arch={}", ppk::HOST_ARCH);
    assert_eq!(stderr(&again), format!("pekit: {id} is already packed\n"));
    assert_eq!(builds(dir.path()), "1700000100\n");
}

#[test]
fn package_refuses_an_include_that_is_not_pekits() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let root = workspace(dir.path(), &recipe(&url, &sha256, FILES, dir.path()));
    fs::write(root.join("pekit-include.gcsl"), "# mine\n").unwrap();
    let output = pekit(&root, dir.path(), &["package", "dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(1));
    assert!(
        stderr(&output).contains("not this pekit's; run `pekit init`"),
        "{}",
        stderr(&output)
    );

    fs::remove_file(root.join("pekit-include.gcsl")).unwrap();
    let output = pekit(&root, dir.path(), &["package", "dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(1));
    assert!(
        stderr(&output).contains("not found; run `pekit init`"),
        "{}",
        stderr(&output)
    );
}

#[test]
fn package_refuses_a_source_whose_hash_differs() {
    let dir = tempfile::tempdir().unwrap();
    let (url, _) = tarball(dir.path());
    let wrong = "0".repeat(64);
    let root = workspace(dir.path(), &recipe(&url, &wrong, FILES, dir.path()));
    let artifacts = dir.path().join("artifacts");
    let args = [
        "package",
        "dev.peios.hello",
        "--artifact-dir",
        path(&artifacts),
    ];
    let output = pekit(&root, dir.path(), &args);
    assert_eq!(output.status.code(), Some(1));
    let message = stderr(&output);
    assert!(
        message.contains(&format!("but the recipe says {wrong}")),
        "{message}"
    );
    // Nothing unchecked is kept.
    let kept = fs::read_dir(dir.path().join("peios/pekit/sources/sha256")).unwrap();
    assert_eq!(kept.count(), 0);
}

#[test]
fn package_packs_into_the_artifact_dir_the_workspace_sets() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let root = workspace(dir.path(), &recipe(&url, &sha256, FILES, dir.path()));
    fs::write(
        root.join("workspace.gcsl"),
        "pekitconf: PekitConfig = { artifact_dir = \"_artifacts\" }\n",
    )
    .unwrap();
    let refs = format!("refs/dev.peios.hello/1.0-2/{}", ppk::HOST_ARCH);

    // Relative to the workspace's root, not the current directory.
    let output = pekit(
        &root.join("dev.peios.hello"),
        dir.path(),
        &["package", "dev.peios.hello"],
    );
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    let identity = printed_identity(&output, HELLO);
    assert_eq!(
        fs::read_to_string(root.join("_artifacts").join(&refs)).unwrap(),
        identity
    );

    // --artifact-dir wins.
    let other = dir.path().join("other");
    let args = ["package", "dev.peios.hello", "--artifact-dir", path(&other)];
    let output = pekit(&root, dir.path(), &args);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    assert_eq!(fs::read_to_string(other.join(&refs)).unwrap(), identity);
}

#[test]
fn package_refuses_a_workspace_setting_without_an_artifact_dir() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let root = workspace(dir.path(), &recipe(&url, &sha256, FILES, dir.path()));
    fs::write(
        root.join("workspace.gcsl"),
        "pekitconf.artifactdir = \"a\"\n",
    )
    .unwrap();
    let output = pekit(&root, dir.path(), &["package", "dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(1));
    assert_eq!(
        stderr(&output),
        "pekit: workspace.gcsl: pekitconf: {artifactdir = \"a\"} has no field artifact_dir\n"
    );
}

#[test]
fn package_refuses_an_artifact_dir_it_cannot_write_before_building() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let root = workspace(dir.path(), &recipe(&url, &sha256, FILES, dir.path()));
    let locked = dir.path().join("locked");
    fs::create_dir(&locked).unwrap();
    fs::set_permissions(&locked, fs::Permissions::from_mode(0o555)).unwrap();
    let artifacts = locked.join("artifacts");
    let args = [
        "package",
        "dev.peios.hello",
        "--artifact-dir",
        path(&artifacts),
    ];
    let output = pekit(&root, dir.path(), &args);
    fs::set_permissions(&locked, fs::Permissions::from_mode(0o755)).unwrap();
    assert_eq!(output.status.code(), Some(1));
    let message = stderr(&output);
    assert!(
        message.starts_with(&format!(
            "pekit: cannot use the artifact directory {} (choose another with --artifact-dir): ",
            path(&artifacts)
        )),
        "{message}"
    );
    // Nothing was fetched or built.
    assert_eq!(builds(dir.path()), "");
    assert!(!dir.path().join("peios/pekit/sources").exists());
}

#[test]
fn files_takes_a_star_only_at_the_end_of_both_paths() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let files = r#"        "bin/*" = "bin/hello""#;
    let root = workspace(dir.path(), &recipe(&url, &sha256, files, dir.path()));
    let artifacts = dir.path().join("artifacts");
    let args = [
        "package",
        "dev.peios.hello",
        "--artifact-dir",
        path(&artifacts),
    ];
    let output = pekit(&root, dir.path(), &args);
    assert_eq!(output.status.code(), Some(1));
    let message = stderr(&output);
    assert!(
        message.contains("`*` must end both paths or neither"),
        "{message}"
    );
    assert!(!artifacts.join("refs").exists());
}

/// Runs git in `dir`, asserting that it succeeds, and returns its output.
fn git(dir: &Path, args: &[&str]) -> String {
    let output = Command::new("git")
        .arg("-C")
        .arg(dir)
        .args([
            "-c",
            "user.name=pekit",
            "-c",
            "user.email=pekit@example.org",
        ])
        .args(["-c", "commit.gpgsign=false"])
        .args(args)
        .output()
        .unwrap();
    let message = String::from_utf8_lossy(&output.stderr);
    assert!(output.status.success(), "git {args:?}: {message}");
    String::from_utf8(output.stdout).unwrap().trim().to_owned()
}

/// Commits everything in the repository `root`, returning the commit.
fn commit(root: &Path) -> String {
    git(root, &["add", "-A"]);
    git(root, &["commit", "-q", "-m", "recipes"]);
    git(root, &["rev-parse", "HEAD"])
}

/// A recipe workspace in `dir`, as [`workspace`] makes one, in a Git
/// repository with everything committed, and its commit.
fn committed_workspace(dir: &Path, recipe: &str) -> (std::path::PathBuf, String) {
    let root = workspace(dir, recipe);
    git(&root, &["init", "-q"]);
    let head = commit(&root);
    (root, head)
}

/// The commit `tag` names in the repository `root`, or `None`.
fn tagged(root: &Path, tag: &str) -> Option<String> {
    let output = Command::new("git")
        .arg("-C")
        .arg(root)
        .args(["rev-parse", "--quiet", "--verify"])
        .arg(format!("refs/tags/{tag}^{{commit}}"))
        .output()
        .unwrap();
    output
        .status
        .success()
        .then(|| String::from_utf8(output.stdout).unwrap().trim().to_owned())
}

/// Runs `pekit release` with `args` in `root`, storing packages in
/// `dir/artifacts`.
fn release(root: &Path, dir: &Path, args: &[&str]) -> Output {
    let artifacts = dir.join("artifacts");
    let mut all = vec!["release"];
    all.extend(args);
    all.extend(["--artifact-dir", path(&artifacts)]);
    pekit(root, dir, &all)
}

/// A second package from the hello build.
const DOC: &str = r#"
def package dev.peios.hello-doc() {
    build = build hello
    files = {
        "share/doc" = "share/doc"
    }
}
"#;

#[test]
fn release_builds_a_recipe_once_and_tags_each_package_at_head() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let recipe = recipe(&url, &sha256, FILES, dir.path()) + DOC;
    let (root, head) = committed_workspace(dir.path(), &recipe);

    let output = release(&root, dir.path(), &["--recipe", "dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    let lines: Vec<String> = stdout(&output).lines().map(str::to_owned).collect();
    assert_eq!(lines.len(), 2, "{lines:?}");
    assert!(lines[0].starts_with("dev.peios.hello@1.0-2 sha256:"));
    assert!(lines[1].starts_with("dev.peios.hello-doc@1.0-2 sha256:"));
    // The packages share one run of their build.
    assert_eq!(builds(dir.path()), "1700000100\n");
    for tag in ["dev.peios.hello@1.0-2", "dev.peios.hello-doc@1.0-2"] {
        assert_eq!(tagged(&root, tag).as_deref(), Some(head.as_str()));
        assert!(stderr(&output).contains(&format!("pekit: tagged {tag}\n")));
    }

    // Again, by name: built again, matching what is packed, and tagged
    // already.
    let again = release(&root, dir.path(), &["dev.peios.hello"]);
    assert_eq!(again.status.code(), Some(0), "{}", stderr(&again));
    assert_eq!(stdout(&again), format!("{}\n", lines[0]));
    assert_eq!(builds(dir.path()), "1700000100\n1700000100\n");
    assert!(!stderr(&again).contains("tagged"), "{}", stderr(&again));
}

#[test]
fn package_builds_several_packages_sharing_one_build_once() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let recipe = recipe(&url, &sha256, FILES, dir.path()) + DOC;
    let root = workspace(dir.path(), &recipe);
    let artifacts = dir.path().join("artifacts");
    let args = [
        "package",
        "dev.peios.hello-doc",
        "dev.peios.hello",
        "dev.peios.hello-doc",
        "--artifact-dir",
        path(&artifacts),
    ];
    let output = pekit(&root, dir.path(), &args);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    let printed = stdout(&output);
    let packages: Vec<&str> = printed
        .lines()
        .map(|line| line.split(' ').next().unwrap())
        .collect();
    assert_eq!(packages, ["dev.peios.hello-doc@1.0-2", HELLO]);
    assert_eq!(builds(dir.path()), "1700000100\n");

    // One packed already: only the other is packed, from the cached build,
    // but both are printed.
    fs::remove_dir_all(artifacts.join("refs/dev.peios.hello-doc")).unwrap();
    let output = pekit(&root, dir.path(), &args);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    assert_eq!(stdout(&output), printed);
    assert_eq!(builds(dir.path()), "1700000100\n");
    let id = format!("{HELLO}?arch={}", ppk::HOST_ARCH);
    assert!(
        stderr(&output).contains(&format!("pekit: {id} is already packed\n")),
        "{}",
        stderr(&output)
    );
}

#[test]
fn release_keeps_a_tag_an_earlier_commit_made_the_same_package_at() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let recipe = recipe(&url, &sha256, FILES, dir.path());
    let (root, first) = committed_workspace(dir.path(), &recipe);
    let output = release(&root, dir.path(), &["dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));

    let file = root.join("dev.peios.hello/pekit.gcsl");
    fs::write(
        &file,
        recipe + "\n# A change that makes the same package.\n",
    )
    .unwrap();
    commit(&root);
    let output = release(&root, dir.path(), &["dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    assert_eq!(
        stderr(&output),
        format!(
            "pekit: building hello\npekit: dev.peios.hello@1.0-2 is already tagged, at {first}\n"
        )
    );
    assert_eq!(tagged(&root, "dev.peios.hello@1.0-2"), Some(first));
}

#[test]
fn release_tags_nothing_when_a_package_differs_from_what_is_packed() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let recipe = recipe(&url, &sha256, FILES, dir.path());
    let (root, first) = committed_workspace(dir.path(), &recipe);
    let output = release(&root, dir.path(), &["dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));

    // The build's output changes, but its revision does not.
    let changed = recipe.replace("echo {arch} >", "echo changed-{arch} >");
    assert_ne!(changed, recipe);
    fs::write(root.join("dev.peios.hello/pekit.gcsl"), changed + DOC).unwrap();
    commit(&root);
    let output = release(&root, dir.path(), &["--recipe", "dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(1));
    let message = stderr(&output);
    assert!(
        message.contains("a packed version cannot be replaced"),
        "{message}"
    );
    assert_eq!(tagged(&root, "dev.peios.hello@1.0-2"), Some(first));
    assert_eq!(tagged(&root, "dev.peios.hello-doc@1.0-2"), None);
}

#[test]
fn release_refuses_uncommitted_changes_before_building() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let recipe = recipe(&url, &sha256, FILES, dir.path());
    let root = workspace(dir.path(), &recipe);
    git(&root, &["init", "-q"]);
    fs::write(root.join(".gitignore"), "/ignored\n").unwrap();
    commit(&root);
    fs::write(root.join("ignored"), "").unwrap();
    fs::write(root.join("dev.peios.hello/notes.gcsl"), "x = 1\n").unwrap();

    let output = release(&root, dir.path(), &["dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(1));
    let message = stderr(&output);
    assert!(
        message.ends_with("has changes that are not committed:\n?? dev.peios.hello/notes.gcsl\n"),
        "{message}"
    );
    assert_eq!(builds(dir.path()), "");

    // An ignored file is not a change.
    fs::remove_file(root.join("dev.peios.hello/notes.gcsl")).unwrap();
    let output = release(&root, dir.path(), &["dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
}

#[test]
fn release_refuses_a_version_git_cannot_tag_before_building() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let recipe = recipe(&url, &sha256, FILES, dir.path());
    let recipe = recipe.replace("version = \"1.0\"", "version = \"1.0~rc1\"");
    let (root, _) = committed_workspace(dir.path(), &recipe);
    let output = release(&root, dir.path(), &["dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(1));
    assert_eq!(
        stderr(&output),
        "pekit: dev.peios.hello@1.0~rc1-2 cannot be a Git tag's name\n"
    );
    assert_eq!(builds(dir.path()), "");
}

/// [`recipe`], with its source the tree at `local_path` rather than a
/// download.
fn local_recipe(local_path: &str, dir: &Path) -> String {
    let fetched = recipe("URL", "SHA", FILES, dir);
    let local = fetched.replace(
        "    url = \"URL\"\n    sha256 = \"SHA\"\n",
        &format!("    local_path = \"{local_path}\"\n"),
    );
    assert_ne!(local, fetched);
    local
}

/// The paths the manifest of `identity` in `artifacts` lists.
fn manifest_files(artifacts: &Path, identity: &str) -> Vec<String> {
    let hex = identity.trim().strip_prefix("sha256:").unwrap();
    let manifest = fs::read_to_string(artifacts.join("manifests/sha256").join(hex)).unwrap();
    manifest
        .lines()
        .skip_while(|line| *line != "[files]")
        .skip(1)
        .map(|line| {
            line.split(" = ")
                .next()
                .unwrap()
                .trim_matches('"')
                .to_owned()
        })
        .collect()
}

#[test]
fn package_builds_a_local_git_checkout_as_it_is_on_disk() {
    let dir = tempfile::tempdir().unwrap();
    tarball(dir.path());
    let tree = dir.path().join("hello-1.0");
    fs::write(tree.join(".gitignore"), "*.log\n").unwrap();
    touch(&tree.join(".gitignore"), "@1700000000");
    git(&tree, &["init", "-q"]);
    commit(&tree);
    // Untracked, ignored, and deleted but not committed.
    fs::write(tree.join("share/doc/NEW"), "new\n").unwrap();
    touch(&tree.join("share/doc/NEW"), "@1700000200");
    fs::write(tree.join("share/doc/debug.log"), "log\n").unwrap();
    touch(&tree.join("share/doc/debug.log"), "@1800000000");
    fs::remove_file(tree.join("extra")).unwrap();

    let root = workspace(dir.path(), &local_recipe("../hello-1.0", dir.path()));
    let artifacts = dir.path().join("artifacts");
    let args = [
        "package",
        "dev.peios.hello",
        "--artifact-dir",
        path(&artifacts),
    ];
    let output = pekit(&root, dir.path(), &args);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    assert_eq!(
        manifest_files(&artifacts, &printed_identity(&output, HELLO)),
        [
            "bin/hello",
            "bin/hi",
            "lib/hello/arch",
            "share/doc/NEW",
            "share/doc/README",
        ]
    );
    // The time of the newest file copied, which the ignored file is not.
    assert_eq!(builds(dir.path()), "1700000200\n");
}

#[test]
fn package_copies_a_local_tree_outside_git_whole() {
    let dir = tempfile::tempdir().unwrap();
    tarball(dir.path());
    let tree = dir.path().join("hello-1.0");
    fs::write(tree.join("share/doc/debug.log"), "log\n").unwrap();
    touch(&tree.join("share/doc/debug.log"), "@1700000300");

    let root = workspace(dir.path(), &local_recipe("../hello-1.0", dir.path()));
    let artifacts = dir.path().join("artifacts");
    let args = [
        "package",
        "dev.peios.hello",
        "--artifact-dir",
        path(&artifacts),
    ];
    let output = pekit(&root, dir.path(), &args);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    let files = manifest_files(&artifacts, &printed_identity(&output, HELLO));
    assert!(
        files.contains(&"share/doc/debug.log".to_owned()),
        "{files:?}"
    );
    assert_eq!(builds(dir.path()), "1700000300\n");
}

#[test]
fn release_refuses_a_local_source_before_building() {
    let dir = tempfile::tempdir().unwrap();
    tarball(dir.path());
    let (root, _) = committed_workspace(dir.path(), &local_recipe("../hello-1.0", dir.path()));
    let output = release(&root, dir.path(), &["dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(1));
    assert_eq!(
        stderr(&output),
        "pekit: dev.peios.hello: its source, hello-tarball, is a local path, which a tag \
         cannot pin; release it from a fetched source\n"
    );
    assert_eq!(builds(dir.path()), "");
}

#[test]
fn package_names_a_package_no_recipe_defines() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let root = workspace(dir.path(), &recipe(&url, &sha256, FILES, dir.path()));
    let output = pekit(&root, dir.path(), &["package", "dev.peios.nope"]);
    assert_eq!(output.status.code(), Some(1));
    assert_eq!(
        stderr(&output),
        "pekit: no recipe defines package dev.peios.nope\n"
    );
}

/// Runs `pekit package dev.peios.hello`, with `extra` arguments, storing it
/// in `dir/<artifacts>`, asserts that it succeeds, and returns what it
/// printed on standard error.
fn package_into(root: &Path, dir: &Path, artifacts: &str, extra: &[&str]) -> String {
    let artifacts = dir.join(artifacts);
    let mut args = vec!["package", "dev.peios.hello", "--artifact-dir"];
    args.push(path(&artifacts));
    args.extend(extra);
    let output = pekit(root, dir, &args);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    stderr(&output)
}

const UNCHANGED: &str = "pekit: hello is unchanged since it was last built\n";

#[test]
fn package_overwrite_replaces_a_packed_version() {
    let dir = tempfile::tempdir().unwrap();
    tarball(dir.path());
    let tree = dir.path().join("hello-1.0");
    let root = workspace(dir.path(), &local_recipe("../hello-1.0", dir.path()));
    let artifacts = dir.path().join("artifacts");
    let packed = || {
        fs::read_to_string(
            artifacts
                .join("refs/dev.peios.hello/1.0-2")
                .join(ppk::HOST_ARCH),
        )
        .unwrap()
    };
    package_into(&root, dir.path(), "artifacts", &[]);
    let first = packed();

    // Changed, but packed already: not built.
    fs::write(tree.join("share/doc/README"), "read me again\n").unwrap();
    let id = format!("{HELLO}?arch={}", ppk::HOST_ARCH);
    let printed = package_into(&root, dir.path(), "artifacts", &[]);
    assert_eq!(printed, format!("pekit: {id} is already packed\n"));
    assert_eq!(packed(), first);

    let printed = package_into(&root, dir.path(), "artifacts", &["--overwrite"]);
    let second = packed();
    assert_ne!(second, first);
    assert_eq!(
        printed,
        format!(
            "pekit: building hello\npekit: {id} was packed as {}; replaced it\n",
            first.trim()
        )
    );

    // Unchanged, it is packed from the cache, and the same.
    let printed = package_into(&root, dir.path(), "artifacts", &["--overwrite"]);
    assert_eq!(printed, UNCHANGED);
    assert_eq!(packed(), second);
}

#[test]
fn package_reuses_a_cached_build_until_its_recipe_changes() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let recipe = recipe(&url, &sha256, FILES, dir.path());
    let root = workspace(dir.path(), &recipe);
    package_into(&root, dir.path(), "a", &[]);
    assert_eq!(builds(dir.path()), "1700000100\n");

    // Not packed there, but the build is kept.
    let printed = package_into(&root, dir.path(), "b", &[]);
    assert_eq!(printed, UNCHANGED);
    assert_eq!(builds(dir.path()), "1700000100\n");
    let manifests = |artifacts: &str| {
        let dir = dir.path().join(artifacts).join("manifests/sha256");
        fs::read_dir(dir).unwrap().count()
    };
    assert_eq!(manifests("b"), 1);

    // --rebuild runs the build, making the same package.
    package_into(&root, dir.path(), "c", &["--rebuild"]);
    assert_eq!(builds(dir.path()), "1700000100\n1700000100\n");
    assert_eq!(
        fs::read_dir(dir.path().join("c/manifests/sha256"))
            .unwrap()
            .next()
            .unwrap()
            .unwrap()
            .file_name(),
        fs::read_dir(dir.path().join("a/manifests/sha256"))
            .unwrap()
            .next()
            .unwrap()
            .unwrap()
            .file_name()
    );

    // Any change to the recipe's file can change what the build does.
    let changed = recipe.clone() + "# A comment.\n";
    fs::write(root.join("dev.peios.hello/pekit.gcsl"), changed).unwrap();
    let printed = package_into(&root, dir.path(), "d", &[]);
    assert_eq!(printed, "pekit: building hello\n");
    assert_eq!(builds(dir.path()).lines().count(), 3);
    assert_eq!(work_dirs(dir.path()), ["hello"]);
}

#[test]
fn release_builds_afresh_whatever_the_cache_keeps() {
    let dir = tempfile::tempdir().unwrap();
    let (url, sha256) = tarball(dir.path());
    let recipe = recipe(&url, &sha256, FILES, dir.path());
    let (root, _) = committed_workspace(dir.path(), &recipe);
    package_into(&root, dir.path(), "cached", &[]);
    let output = release(&root, dir.path(), &["dev.peios.hello"]);
    assert_eq!(output.status.code(), Some(0), "{}", stderr(&output));
    assert_eq!(builds(dir.path()), "1700000100\n1700000100\n");
    // Its work directory is not kept.
    assert_eq!(work_dirs(dir.path()), ["hello"]);
}

/// The copy of its source the work directory of the hello build keeps, in
/// the cache in `dir`.
fn kept_source(dir: &Path) -> std::path::PathBuf {
    let work = dir.join("peios/pekit/work");
    let workspace = fs::read_dir(&work).unwrap().next().unwrap().unwrap();
    workspace
        .path()
        .join("hello")
        .join(std::env::consts::ARCH)
        .join("source")
}

#[test]
fn package_syncs_a_local_source_into_the_kept_copy() {
    let dir = tempfile::tempdir().unwrap();
    tarball(dir.path());
    let tree = dir.path().join("hello-1.0");
    // The build adds a file to its copy of the source, and changes one.
    let recipe = local_recipe("../hello-1.0", dir.path()).replace(
        "    = out_path\n",
        "    sh(\"echo made >> {source_path}/made\")\n    \
         sh(\"echo changed >> {source_path}/share/doc/README\")\n    = out_path\n",
    );
    let root = workspace(dir.path(), &recipe);
    package_into(&root, dir.path(), "a", &[]);
    let copy = kept_source(dir.path());
    let read = |path: &str| fs::read_to_string(copy.join(path)).unwrap();
    let modified = |path: &str| fs::metadata(copy.join(path)).unwrap().modified().unwrap();
    assert_eq!(read("made"), "made\n");
    let hello = modified("bin/hello");

    // A file deleted from the source is deleted from the copy, and one
    // added is added. What the build made is kept, what it changed is put
    // back before it runs, and what is unchanged is not rewritten.
    fs::remove_file(tree.join("extra")).unwrap();
    fs::write(tree.join("share/doc/NEW"), "new\n").unwrap();
    touch(&tree.join("share/doc/NEW"), "@1700000200");
    assert_eq!(
        package_into(&root, dir.path(), "b", &[]),
        "pekit: building hello\n"
    );
    assert!(!copy.join("extra").exists());
    assert_eq!(read("share/doc/NEW"), "new\n");
    assert_eq!(read("made"), "made\nmade\n");
    assert_eq!(read("share/doc/README"), "read me\nchanged\n");
    assert_eq!(modified("bin/hello"), hello);
    assert_eq!(builds(dir.path()), "1700000100\n1700000200\n");

    // A new time alone is a new SOURCE_DATE_EPOCH, so the build runs, but
    // the copy, whose content is the same, is not rewritten.
    touch(&tree.join("bin/hello"), "@1700000300");
    assert_eq!(
        package_into(&root, dir.path(), "c", &[]),
        "pekit: building hello\n"
    );
    assert_eq!(modified("bin/hello"), hello);
    assert_eq!(builds(dir.path()), "1700000100\n1700000200\n1700000300\n");

    // Unchanged, it is cached. --rebuild starts the copy afresh.
    assert_eq!(package_into(&root, dir.path(), "d", &[]), UNCHANGED);
    package_into(&root, dir.path(), "e", &["--rebuild"]);
    assert_eq!(read("made"), "made\n");
}
