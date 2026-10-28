//! Tests of a step's compositions and overlay sandboxes, through its
//! sandbox's effect context.

use std::fs;
use std::path::Path;
use std::process::Command;

use gcsl::eval::Effects;
use pekit::record::Kept;
use pekit::sandbox::StepSandbox;
use pekit::store::Store;

/// A temporary directory removed, sealed directories and all, when it is
/// dropped, even by a test that fails: an overlay's work directory, which
/// the kernel makes with no permissions, is more than `tempfile`'s own
/// removal can remove.
struct Scratch(tempfile::TempDir);

impl Drop for Scratch {
    fn drop(&mut self) {
        let _ = pekit::layers::remove_tree(self.0.path());
    }
}

/// Runs `script` under the static busybox in the sandbox at `root` of
/// `sandbox`: whether it succeeded.
fn run_in(sandbox: &StepSandbox, root: &str, script: &str) -> bool {
    let mut inner = Command::new("/bin/sh");
    inner.args(["-c", script]).current_dir("/").env_clear();
    let mut command = sandbox.sandbox_command(root, inner).unwrap().unwrap();
    command.status().unwrap().success()
}

#[test]
fn a_sandbox_of_a_staged_directory_is_an_overlay_read_upper_over_lower() {
    let busybox = Path::new("/usr/bin/busybox");
    if !busybox.exists() {
        eprintln!("skipped: no static busybox on this host");
        return;
    }
    let dir = Scratch(tempfile::tempdir().unwrap());
    let store = Store::open(&dir.0.path().join("store")).unwrap();
    let tree = dir.0.path().join("tree");
    for sub in ["bin", "keep", "redo"] {
        fs::create_dir_all(tree.join(sub)).unwrap();
    }
    fs::copy(busybox, tree.join("bin/busybox")).unwrap();
    std::os::unix::fs::symlink("busybox", tree.join("bin/sh")).unwrap();
    fs::write(tree.join("keep/a"), "low").unwrap();
    fs::write(tree.join("redo/b"), "low").unwrap();
    fs::write(tree.join("gone"), "low").unwrap();
    let digest = store.put_tree(&tree).unwrap();
    let sandbox = StepSandbox::new(&dir.0.path().join("stage"), "t/s").unwrap();
    sandbox.stage(&store, "/w/root", Kept::Dir(digest)).unwrap();
    // A staged input: an overlay, nothing copied.
    let root = sandbox.temp_dir().unwrap().unwrap();
    assert_eq!(sandbox.make_sandbox("/w/root", &root), Some(Ok(())));
    let upper = sandbox.host_path(&root).unwrap();
    assert_eq!(fs::read_dir(&upper).unwrap().count(), 0);
    let script = "busybox mv /gone /keep/moved; busybox mv /redo /redo-old; busybox mkdir /redo; \
                  echo new > /redo/c; echo up > /keep/a2";
    assert!(run_in(&sandbox, &root, script));
    // Its writes are in the upper layer, never the lower.
    assert!(upper.join("keep/a2").exists());
    assert!(sandbox.host_path("/w/root/gone").unwrap().exists());
    // Read upper over lower.
    let read =
        |path: &str| fs::read_to_string(sandbox.host_path(&format!("{root}{path}")).unwrap());
    assert_eq!(read("/keep/a").unwrap(), "low");
    assert_eq!(read("/keep/a2").unwrap(), "up\n");
    assert!(read("/gone").is_err(), "a whiteout hides the lower's file");
    let names = |path: &str| {
        let mut names: Vec<String> =
            fs::read_dir(sandbox.host_path(&format!("{root}{path}")).unwrap())
                .unwrap()
                .map(|entry| entry.unwrap().file_name().to_string_lossy().into_owned())
                .collect();
        names.sort();
        names
    };
    assert_eq!(names("/keep"), ["a", "a2", "moved"]);
    // An opaque directory hides all the lower had below it.
    assert_eq!(names("/redo"), ["c"]);
    // A directory the step itself changes is copied, as plain GCSL does.
    let own = sandbox.temp_dir().unwrap().unwrap();
    let other = sandbox.temp_dir().unwrap().unwrap();
    assert_eq!(sandbox.make_sandbox(&own, &other), None);
}

#[test]
fn a_composition_links_what_nothing_changes_and_is_sealed() {
    use std::os::unix::fs::MetadataExt;
    let dir = Scratch(tempfile::tempdir().unwrap());
    let store = Store::open(&dir.0.path().join("store")).unwrap();
    fs::write(dir.0.path().join("x"), "x").unwrap();
    let entry = store.put_file(&dir.0.path().join("x")).unwrap();
    let sandbox = StepSandbox::new(&dir.0.path().join("stage"), "t/s").unwrap();
    sandbox.stage(&store, "/w/x", Kept::File(entry)).unwrap();
    let composed = sandbox.temp_dir().unwrap().unwrap();
    let at = sandbox.host_path(&composed).unwrap();
    let staged = sandbox.host_path("/w/x").unwrap();
    sandbox.place_file(&staged, &at.join("x")).unwrap();
    assert_eq!(
        fs::metadata(&staged).unwrap().ino(),
        fs::metadata(at.join("x")).unwrap().ino()
    );
    // The same file twice is one link and one copy, two files of their own.
    sandbox.place_file(&staged, &at.join("again")).unwrap();
    assert_ne!(
        fs::metadata(&staged).unwrap().ino(),
        fs::metadata(at.join("again")).unwrap().ino()
    );
    assert_eq!(fs::read_to_string(at.join("again")).unwrap(), "x");
    // What the step made is copied.
    let made = sandbox.host_path("/pekit-out/t/s/made").unwrap();
    fs::write(&made, "m").unwrap();
    sandbox.place_file(&made, &at.join("made")).unwrap();
    assert_ne!(
        fs::metadata(&made).unwrap().ino(),
        fs::metadata(at.join("made")).unwrap().ino()
    );
    // A copy placed elsewhere, as a sandbox's copy is, is no part of it.
    let elsewhere = sandbox
        .host_path(&sandbox.temp_dir().unwrap().unwrap())
        .unwrap();
    sandbox.place_file(&made, &elsewhere.join("made")).unwrap();
    // Sealed: none of its files can be changed, the linked blob least of
    // all, and a sandbox of it is an overlay.
    sandbox.composed(&composed);
    assert!(fs::write(at.join("x"), "changed").is_err());
    assert!(fs::write(at.join("made"), "changed").is_err());
    assert!(fs::write(at.join("again"), "changed").is_err());
    fs::write(elsewhere.join("made"), "changed").unwrap();
    assert_eq!(fs::read_to_string(&staged).unwrap(), "x");
    let root = sandbox.temp_dir().unwrap().unwrap();
    assert_eq!(sandbox.make_sandbox(&composed, &root), Some(Ok(())));
    // Its staging directory can still be removed.
    pekit::layers::remove_tree(&dir.0.path().join("stage")).unwrap();
    assert!(!dir.0.path().join("stage").exists());
}

#[test]
fn a_file_a_composition_places_twice_is_a_link_to_a_copy_the_store_keeps() {
    use std::os::unix::fs::MetadataExt;
    let dir = Scratch(tempfile::tempdir().unwrap());
    let store = Store::open(&dir.0.path().join("store")).unwrap();
    fs::write(dir.0.path().join("x"), "x").unwrap();
    let entry = store.put_file(&dir.0.path().join("x")).unwrap();
    let sandbox = StepSandbox::new(&dir.0.path().join("stage"), "t/s")
        .unwrap()
        .with_contexts(std::collections::HashMap::new(), store.clone());
    sandbox.stage(&store, "/w/x", Kept::File(entry)).unwrap();
    let staged = sandbox.host_path("/w/x").unwrap();
    // Each of two compositions places it twice: the blob and its first
    // copy, the same two in each, so that the second copies nothing.
    let mut placed = Vec::new();
    for _ in 0..2 {
        let composed = sandbox.temp_dir().unwrap().unwrap();
        let at = sandbox.host_path(&composed).unwrap();
        for name in ["x", "again"] {
            sandbox.place_file(&staged, &at.join(name)).unwrap();
        }
        sandbox.composed(&composed);
        let inode = |name: &str| fs::metadata(at.join(name)).unwrap().ino();
        placed.push((inode("x"), inode("again")));
        assert_eq!(fs::read_to_string(at.join("again")).unwrap(), "x");
        assert!(fs::write(at.join("again"), "changed").is_err());
    }
    assert_eq!(placed[0], placed[1]);
    let (blob, copy) = placed[0];
    assert_eq!(blob, fs::metadata(&staged).unwrap().ino());
    assert_ne!(blob, copy);
}
