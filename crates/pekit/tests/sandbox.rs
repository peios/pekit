//! Tests of the effect contexts: the host's view of the logical layout,
//! and a hermetic step's sandbox.

use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

use gcsl::eval::Effects;
use pekit::record::Kept;
use pekit::sandbox::{HostView, StepSandbox};
use pekit::store::Store;

/// The host directories a test's shell needs, which a real step never
/// borrows.
fn host_tools() -> Vec<PathBuf> {
    ["/usr", "/bin", "/lib", "/lib64"]
        .iter()
        .map(PathBuf::from)
        .filter(|dir| dir.exists())
        .collect()
}

/// What `sh -c script` prints through `effects`, and whether it succeeded,
/// its environment `env`.
fn shell(effects: &dyn Effects, script: &str, env: &[(&str, &str)]) -> (String, bool) {
    let mut command = Command::new("/bin/sh");
    command.args(["-c", script]);
    for (name, value) in env {
        command.env(name, value);
    }
    let output = effects.command(command).unwrap().output().unwrap();
    (
        String::from_utf8_lossy(&output.stdout).trim().to_owned(),
        output.status.success(),
    )
}

#[test]
fn a_step_sees_only_what_it_is_given() {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::open(&dir.path().join("store")).unwrap();
    let sandbox = StepSandbox::new(&dir.path().join("stage"), "hello/compile")
        .unwrap()
        .with_host(&host_tools());
    // A workspace file given, and one not.
    fs::write(dir.path().join("given.c"), "int main;").unwrap();
    let entry = store.put_file(&dir.path().join("given.c")).unwrap();
    sandbox
        .stage(&store, "/w/app/given.c", Kept::File(entry))
        .unwrap();
    let (read, ok) = shell(&sandbox, "cat /w/app/given.c", &[]);
    assert!(ok);
    assert_eq!(read, "int main;");
    let (_, ok) = shell(&sandbox, "cat /w/app/other.c", &[]);
    assert!(!ok, "a file not given is not there");
    // Its own output directory is its working directory, and writable; the
    // workspace is not.
    let (cwd, _) = shell(&sandbox, "pwd", &[]);
    assert_eq!(cwd, "/pekit-out/hello/compile");
    let (_, ok) = shell(
        &sandbox,
        "echo out > hello && test -f /pekit-out/hello/compile/hello",
        &[],
    );
    assert!(ok);
    let written = dir.path().join("stage/out/hello/compile/hello");
    assert_eq!(fs::read_to_string(written).unwrap(), "out\n");
    let (_, ok) = shell(&sandbox, "echo x > /w/app/new.c", &[]);
    assert!(!ok, "the workspace is read-only");
    // Only what its `exec` sets.
    let (env, _) = shell(
        &sandbox,
        "env | sort | grep -v '^PWD=' | grep -v '^SHLVL=' | grep -v '^_='",
        &[("ONLY", "this")],
    );
    assert_eq!(env, "ONLY=this");
    // A path outside it is none of its.
    assert_eq!(
        sandbox.host_path("/home/x").unwrap_err(),
        "it is outside the step"
    );
    assert_eq!(
        sandbox.host_path("/pekit-out/hello/compile/hello").unwrap(),
        dir.path().join("stage/out/hello/compile/hello")
    );
    assert_eq!(
        sandbox.working_dir().unwrap(),
        Path::new("/pekit-out/hello/compile")
    );
}

#[test]
fn what_is_not_hermetic_sees_the_layout_over_the_host() {
    let dir = tempfile::tempdir().unwrap();
    let workspace = dir.path().join("ws");
    fs::create_dir_all(workspace.join("app")).unwrap();
    fs::write(workspace.join("app/x.txt"), "workspace file").unwrap();
    let view = HostView::new(
        &workspace,
        &dir.path().join("out"),
        "app",
        &dir.path().join("tmp"),
    );
    let (read, ok) = shell(&view, "cat /w/app/x.txt", &[]);
    assert!(ok, "{read}");
    assert_eq!(read, "workspace file");
    assert_eq!(
        view.host_path("/w/app/x.txt").unwrap(),
        workspace.join("app/x.txt")
    );
    assert_eq!(
        view.host_path("/usr/bin/cc").unwrap(),
        Path::new("/usr/bin/cc")
    );
    assert_eq!(view.working_dir().unwrap(), Path::new("/w/app"));
    let made = view.temp_dir().unwrap().unwrap();
    assert!(Path::new(&made).is_dir());
}
