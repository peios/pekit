//! Tests of `pekit publish --strict`, which builds strictly and then
//! publishes as a release does, and of `pekitconf.require_strict`, which
//! refuses every build, test, package and publish that is not strict.

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

/// A packager of the workspace's own: it packs a package's files in a step,
/// composed, and publishes by copying them to `STORE/NAME-VERSION`, in
/// place of what is there; and the configuration `CONF`, written after it.
fn workspace(conf: &str) -> String {
    format!(
        "import fs\n\
         import packaging\n\
         #[hermetic] fn composed(pkg: Package) -> Dir {{ fs.compose_dir(pkg.files) }}\n\
         fn copied(pkg: Package, packed: Dir, store: String) -> {{}} {{\n\
         \x20   let at = \"{{store}}/{{pkg.name}}-{{pkg.version}}\"\n\
         \x20   let r = exec([\"sh\", \"-c\", \"rm -rf {{at}} && cp -rT {{packed.path}} {{at}}\"])\n\
         }}\n\
         def copier() -> packaging.Packager {{\n\
         \x20   name = \"copy\"\n\
         \x20   pack = fn(pkg: Package) -> Dir {{ composed(pkg) }}\n\
         \x20   publish = fn(pkg: Package, packed: Dir, store: String) -> {{}} {{ copied(pkg, \
         packed, store) }}\n\
         }}\n\
         {conf}"
    )
}

/// The workspace's packages: `dev.example.data`, of the workspace's own
/// `data.txt`, and a build.
const PACKAGES: &str = "def package dev.example.data() {\n\
                        \x20   version = \"1.0\"\n\
                        \x20   files = {\"share/data.txt\" = file(\"data.txt\")}\n\
                        }\n\
                        def build data() { = file(\"data.txt\") }\n";

/// A package that packs only outside a strict build, which reads only the
/// workspace: one that asks of the host's `/etc`.
const HOST_PACKAGE: &str = "import fs\n\
                            def package dev.example.host() {\n\
                            \x20   let host = fs.exists(\"/etc\")\n\
                            \x20   version = \"1.0\"\n\
                            \x20   files = {\"share/data.txt\" = file(\"data.txt\")}\n\
                            }\n";

/// A workspace in `dir` configured with `conf`, packages in `app`.
fn workspace_in(dir: &Path, conf: &str) {
    write(
        dir,
        &[
            ("workspace.gcsl", &workspace(conf)),
            ("app/package.gcsl", PACKAGES),
            ("app/data.txt", "data\n"),
        ],
    );
}

/// `pekit` run with `args` in `dir`, its cache and state in `cache`.
fn pekit(dir: &Path, cache: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_pekit"))
        .args(args)
        .current_dir(dir)
        .env("PEKIT_CACHE_DIR", cache)
        .env("PEKIT_STATE_DIR", cache.join("state"))
        .env("XDG_RUNTIME_DIR", cache.join("run"))
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

#[test]
fn publish_strict_packs_strictly_then_publishes() {
    let ws = tempfile::tempdir().unwrap();
    workspace_in(ws.path(), "pekitconf = {packagers = [copier()]}\n");
    let cache = tempfile::tempdir().unwrap();
    let store = tempfile::tempdir().unwrap();
    let to = format!("--to={}", store.path().display());
    // The packager's copy is the target's own effect, which a strict build
    // refuses; the publishing after the strict build allows it, and finds
    // the build's steps.
    let published = pekit(
        ws.path(),
        cache.path(),
        &["publish", "dev.example.data", &to, "--strict"],
    );
    let (out, err) = shown(&published);
    assert!(published.status.success(), "{err}");
    assert!(
        out.starts_with("published app/dev.example.data with copy\n")
            && out.ends_with("1 step run, 0 found in the store\n"),
        "{out}"
    );
    let copied = store.path().join("dev.example.data-1.0/share/data.txt");
    assert_eq!(fs::read_to_string(&copied).unwrap(), "data\n");
    // Again, with `--rebuild`: the strict build runs its step again, and
    // the publishing finds it.
    fs::remove_dir_all(store.path().join("dev.example.data-1.0")).unwrap();
    let again = pekit(
        ws.path(),
        cache.path(),
        &["publish", "dev.example.data", &to, "--strict", "--rebuild"],
    );
    let (out, err) = shown(&again);
    assert!(again.status.success(), "{err}");
    assert!(out.ends_with("1 step run, 0 found in the store\n"), "{out}");
    assert!(copied.is_file());
}

#[test]
fn publish_strict_publishes_nothing_unless_every_package_packs_strictly() {
    let ws = tempfile::tempdir().unwrap();
    workspace_in(ws.path(), "pekitconf = {packagers = [copier()]}\n");
    write(ws.path(), &[("app/host.gcsl", HOST_PACKAGE)]);
    let cache = tempfile::tempdir().unwrap();
    let store = tempfile::tempdir().unwrap();
    let to = format!("--to={}", store.path().display());
    let refused = pekit(
        ws.path(),
        cache.path(),
        &["publish", "app/*", &to, "--strict"],
    );
    let (out, err) = shown(&refused);
    assert!(!refused.status.success(), "{out}");
    assert!(
        err.contains(
            "a strict build reads only the workspace and the build's outputs, and this \
                      reads `/etc`"
        ),
        "{err}"
    );
    assert!(
        err.ends_with("error: nothing is published, since 1 of 2 packages did not pack strictly\n"),
        "{err}"
    );
    assert_eq!(fs::read_dir(store.path()).unwrap().count(), 0);
}

#[test]
fn a_workspace_that_requires_strict_refuses_what_is_not() {
    let ws = tempfile::tempdir().unwrap();
    workspace_in(
        ws.path(),
        "pekitconf = {packagers = [copier()]; require_strict = true}\n",
    );
    let cache = tempfile::tempdir().unwrap();
    let store = tempfile::tempdir().unwrap();
    let to = format!("--to={}", store.path().display());
    for (command, args) in [
        ("build", vec!["build", "data"]),
        ("test", vec!["test"]),
        ("package", vec!["package"]),
        ("publish", vec!["publish", "dev.example.data", &to]),
    ] {
        let refused = pekit(ws.path(), cache.path(), &args);
        let (_, err) = shown(&refused);
        assert!(!refused.status.success(), "{command}");
        assert_eq!(
            err,
            format!(
                "error: this workspace requires `--strict`, as `pekitconf.require_strict` in \
                 `workspace.gcsl` says, so that every build is keyed and checked as a strict \
                 one: run `pekit {command} --strict`\n"
            )
        );
    }
    let built = pekit(ws.path(), cache.path(), &["build", "data", "--strict"]);
    assert!(built.status.success(), "{:?}", shown(&built));
    let published = pekit(
        ws.path(),
        cache.path(),
        &["publish", "dev.example.data", &to, "--strict"],
    );
    assert!(published.status.success(), "{:?}", shown(&published));
    // A query asks what a build would read, and builds nothing.
    let queried = pekit(ws.path(), cache.path(), &["query", "deps", "data"]);
    assert!(queried.status.success(), "{:?}", shown(&queried));
}

#[test]
fn require_strict_is_read_as_written() {
    let ws = tempfile::tempdir().unwrap();
    let cache = tempfile::tempdir().unwrap();
    // Written as a dotted field, as pekit reads no field of `pekitconf`, it
    // is refused rather than ignored.
    workspace_in(ws.path(), "pekitconf.require_strict = true\n");
    let refused = pekit(ws.path(), cache.path(), &["build", "data", "--strict"]);
    assert_eq!(
        shown(&refused).1,
        "error: `pekitconf.require_strict` in `workspace.gcsl` is read only in `pekitconf`'s \
         record: write `pekitconf = {require_strict = true}`\n"
    );
    // `false` requires nothing.
    workspace_in(
        ws.path(),
        "pekitconf = {packagers = [copier()]; require_strict = false}\n",
    );
    let packed = pekit(ws.path(), cache.path(), &["package"]);
    assert!(packed.status.success(), "{:?}", shown(&packed));
    // Anything but `true` or `false` is read only by running the file,
    // which pekit does not do before it builds.
    workspace_in(
        ws.path(),
        "let strict = true\npekitconf = {packagers = [copier()]; require_strict = strict}\n",
    );
    for args in [&["package"][..], &["package", "--strict"]] {
        let refused = pekit(ws.path(), cache.path(), args);
        let (_, err) = shown(&refused);
        assert!(!refused.status.success());
        assert_eq!(
            err,
            "error: `pekitconf.require_strict` in `workspace.gcsl` must be written `true` or \
             `false`, as `pekitconf = {require_strict = true}`: pekit reads it as written, \
             before it builds\n"
        );
    }
}

#[test]
fn publish_strict_refuses_a_recipe_whose_source_is_not_what_is_recorded() {
    let ws = tempfile::tempdir().unwrap();
    workspace_in(ws.path(), "pekitconf = {packagers = [copier()]}\n");
    let cache = tempfile::tempdir().unwrap();
    let store = tempfile::tempdir().unwrap();
    let to = format!("--to={}", store.path().display());
    let publish = |ws: &Path| {
        pekit(
            ws,
            cache.path(),
            &["publish", "dev.example.data", &to, "--strict"],
        )
    };
    // Recorded, it publishes.
    let recorded = pekit(ws.path(), cache.path(), &["sources", "--strict"]);
    let (out, err) = shown(&recorded);
    assert!(recorded.status.success(), "{err}");
    assert_eq!(
        out,
        "recorded app: 0 fetched, 0 git, 3 files, 0 directories\n"
    );
    let sources = fs::read_to_string(ws.path().join("pekit.sources")).unwrap();
    for path in [
        "\"app/data.txt\"",
        "\"app/package.gcsl\"",
        "\"workspace.gcsl\"",
        "\"dev.example.data\"",
    ] {
        assert!(sources.contains(path), "{sources}");
    }
    let published = publish(ws.path());
    assert!(published.status.success(), "{}", shown(&published).1);
    // A package that reads another file is built from what isn't recorded,
    // so nothing is published until it is.
    write(
        ws.path(),
        &[
            (
                "app/package.gcsl",
                &PACKAGES.replace(
                    "files = {\"share/data.txt\" = file(\"data.txt\")}",
                    "files = {\"share/data.txt\" = file(\"data.txt\"); \
                     \"share/more.txt\" = file(\"more.txt\")}",
                ),
            ),
            ("app/more.txt", "more\n"),
        ],
    );
    let refused = publish(ws.path());
    let (_, err) = shown(&refused);
    assert!(!refused.status.success());
    assert!(
        err.contains(
            "error: nothing is published, since `pekit.sources` does not record what app is \
             built from: run `pekit sources --strict` for it, and review the change"
        ),
        "{err}"
    );
    assert!(
        !store
            .path()
            .join("dev.example.data-1.0/share/more.txt")
            .exists()
    );
    let again = pekit(ws.path(), cache.path(), &["sources", "--strict"]);
    assert!(again.status.success(), "{}", shown(&again).1);
    let published = publish(ws.path());
    assert!(published.status.success(), "{}", shown(&published).1);
    assert!(
        store
            .path()
            .join("dev.example.data-1.0/share/more.txt")
            .is_file()
    );
}

#[test]
fn a_workspace_without_a_sources_file_publishes_as_before() {
    let ws = tempfile::tempdir().unwrap();
    workspace_in(ws.path(), "pekitconf = {packagers = [copier()]}\n");
    let cache = tempfile::tempdir().unwrap();
    let store = tempfile::tempdir().unwrap();
    let to = format!("--to={}", store.path().display());
    let published = pekit(
        ws.path(),
        cache.path(),
        &["publish", "dev.example.data", &to, "--strict"],
    );
    assert!(published.status.success(), "{}", shown(&published).1);
    assert!(!ws.path().join("pekit.sources").exists());
}

#[test]
fn publishing_one_package_of_a_recipe_checks_the_recipe_s_whole_record() {
    let ws = tempfile::tempdir().unwrap();
    workspace_in(ws.path(), "pekitconf = {packagers = [copier()]}\n");
    write(
        ws.path(),
        &[
            (
                "app/other.gcsl",
                "def package dev.example.other() {\n\
                 \x20   version = \"1.0\"\n\
                 \x20   files = {\"share/other.txt\" = file(\"other.txt\")}\n\
                 }\n",
            ),
            ("app/other.txt", "other\n"),
        ],
    );
    let cache = tempfile::tempdir().unwrap();
    let store = tempfile::tempdir().unwrap();
    let to = format!("--to={}", store.path().display());
    // Recording one package records its recipe, both packages.
    let recorded = pekit(
        ws.path(),
        cache.path(),
        &["sources", "dev.example.data", "--strict"],
    );
    let (out, err) = shown(&recorded);
    assert!(recorded.status.success(), "{err}");
    assert_eq!(
        out,
        "recorded app: 0 fetched, 0 git, 5 files, 0 directories\n"
    );
    let published = pekit(
        ws.path(),
        cache.path(),
        &["publish", "dev.example.other", &to, "--strict"],
    );
    assert!(published.status.success(), "{}", shown(&published).1);
}

#[test]
fn a_package_added_to_a_recipe_is_recorded_before_anything_is_published() {
    let ws = tempfile::tempdir().unwrap();
    workspace_in(ws.path(), "pekitconf = {packagers = [copier()]}\n");
    let cache = tempfile::tempdir().unwrap();
    let store = tempfile::tempdir().unwrap();
    let to = format!("--to={}", store.path().display());
    let recorded = pekit(ws.path(), cache.path(), &["sources", "--strict"]);
    assert!(recorded.status.success(), "{}", shown(&recorded).1);
    // A package of the same files: the source is the same, the packages
    // are not.
    write(
        ws.path(),
        &[(
            "app/other.gcsl",
            "def package dev.example.other() {\n\
             \x20   version = \"1.0\"\n\
             \x20   files = {\"share/data.txt\" = file(\"data.txt\")}\n\
             }\n",
        )],
    );
    let refused = pekit(
        ws.path(),
        cache.path(),
        &["publish", "dev.example.data", &to, "--strict"],
    );
    assert!(!refused.status.success());
    assert!(
        shown(&refused)
            .1
            .contains("does not record what app is built from"),
        "{}",
        shown(&refused).1
    );
}
