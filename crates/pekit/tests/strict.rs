//! Tests of `pekit publish --strict`, which builds strictly and then
//! publishes as a release does.

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
