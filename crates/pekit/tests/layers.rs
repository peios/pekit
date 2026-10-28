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

/// A tree of `count` files under `lib`, one named `dup` of the contents
/// `x`, made at `at` and put into `store`: its digest.
fn large_tree(store: &Store, at: &Path, count: u64) -> pekit::store::Digest {
    fs::create_dir_all(at.join("lib")).unwrap();
    for number in 0..count {
        fs::write(at.join(format!("lib/f{number}")), format!("{number}")).unwrap();
    }
    fs::write(at.join("dup"), "x").unwrap();
    store.put_tree(at).unwrap()
}

/// The words of `command`, as text.
fn words(command: &Command) -> Vec<String> {
    command
        .get_args()
        .map(|word| word.to_string_lossy().into_owned())
        .collect()
}

#[test]
fn a_large_given_directory_is_mounted_from_its_materialised_tree() {
    use std::collections::HashMap;
    let dir = Scratch(tempfile::tempdir().unwrap());
    let store = Store::open(&dir.0.path().join("store")).unwrap();
    let large = large_tree(&store, &dir.0.path().join("large"), 70);
    let small = large_tree(&store, &dir.0.path().join("small"), 3);
    let sandbox = StepSandbox::new(&dir.0.path().join("stage"), "t/s")
        .unwrap()
        .with_contexts(HashMap::new(), store.clone());
    sandbox.stage(&store, "/w/large", Kept::Dir(large)).unwrap();
    sandbox.stage(&store, "/w/small", Kept::Dir(small)).unwrap();
    // The large one is its materialised tree, on an empty directory; the
    // small one linked file by file.
    let tree = store.trees_root().join(large.hex());
    assert_eq!(
        sandbox.host_path("/w/large/lib/f3").unwrap(),
        tree.join("lib/f3")
    );
    let point = dir.0.path().join("stage/w/large");
    assert_eq!(fs::read_dir(&point).unwrap().count(), 0);
    assert_eq!(sandbox.mounted(), HashMap::from([(point.clone(), large)]));
    assert_eq!(sandbox.contents(&point), Some(tree.clone()));
    assert!(
        sandbox
            .host_path("/w/small/lib/f1")
            .unwrap()
            .starts_with(dir.0.path().join("stage"))
    );
    // Its programs see it read-only.
    let command = sandbox.command(Command::new("true")).unwrap();
    let words = words(&command);
    let bound = words.windows(3).any(|three| {
        three[0] == "--ro-bind" && three[1] == tree.to_str().unwrap() && three[2] == "/w/large"
    });
    assert!(bound, "{words:?}");
    // GCSL changes nothing of it, which every step shares.
    for path in ["/w/large/new", "/w/large/lib/f1", "/w/small/lib/f1"] {
        assert!(sandbox.write_path(path).is_err(), "{path}");
    }
    assert!(sandbox.write_path("/pekit-out/t/s/new").is_ok());
    // Kept as its tree, unread.
    let result = gcsl::eval::plain::Plain::Dir {
        path: "/w/large".to_owned(),
        identity: None,
    };
    let host = |path: &str| sandbox.host_path(path).unwrap();
    let kept = pekit::record::keep_files(&result, &store, &host, &sandbox.mounted()).unwrap();
    assert_eq!(kept["/w/large"], Kept::Dir(large));
}

#[test]
fn a_composition_mounts_a_large_tree_and_a_sandbox_of_it_nests_an_overlay() {
    use std::collections::HashMap;
    use std::os::unix::fs::MetadataExt;
    let busybox = Path::new("/usr/bin/busybox");
    if !busybox.exists() {
        eprintln!("skipped: no static busybox on this host");
        return;
    }
    let dir = Scratch(tempfile::tempdir().unwrap());
    let store = Store::open(&dir.0.path().join("store")).unwrap();
    let large = large_tree(&store, &dir.0.path().join("large"), 70);
    fs::write(dir.0.path().join("x"), "x").unwrap();
    let dup = store.put_file(&dir.0.path().join("x")).unwrap();
    let shell = store.put_file(busybox).unwrap();
    let sandbox = StepSandbox::new(&dir.0.path().join("stage"), "t/s")
        .unwrap()
        .with_contexts(HashMap::new(), store.clone());
    sandbox.stage(&store, "/w/large", Kept::Dir(large)).unwrap();
    sandbox.stage(&store, "/w/x", Kept::File(dup)).unwrap();
    sandbox
        .stage(&store, "/w/busybox", Kept::File(shell))
        .unwrap();
    // A composition of busybox, `x` and the large tree.
    let composed = sandbox.temp_dir().unwrap().unwrap();
    let at = sandbox.host_path(&composed).unwrap();
    fs::create_dir_all(at.join("bin")).unwrap();
    fs::create_dir_all(at.join("opt")).unwrap();
    let host = |path: &str| sandbox.host_path(path).unwrap();
    sandbox
        .place_file(&host("/w/busybox"), &at.join("bin/busybox"))
        .unwrap();
    std::os::unix::fs::symlink("busybox", at.join("bin/sh")).unwrap();
    sandbox.place_file(&host("/w/x"), &at.join("dup")).unwrap();
    let source = host("/w/large");
    assert!(matches!(
        sandbox.place_tree(&source, &at.join("opt/large")),
        Some(Ok(()))
    ));
    // A tree that is no tree of the store is GCSL's to place.
    assert!(
        sandbox
            .place_tree(&dir.0.path().join("large"), &at.join("other"))
            .is_none()
    );
    sandbox.composed(&composed);
    // The tree is mounted, an instance whose `dup` is no file the
    // composition has already.
    let mounted = host(&format!("{composed}/opt/large/dup"));
    assert!(mounted.starts_with(store.trees_root()));
    assert_ne!(
        fs::metadata(&mounted).unwrap().ino(),
        fs::metadata(at.join("dup")).unwrap().ino()
    );
    assert_eq!(fs::read_to_string(&mounted).unwrap(), "x");
    // Kept as the tree of the same files copied would be.
    let copied = dir.0.path().join("copied");
    fs::create_dir_all(copied.join("bin")).unwrap();
    fs::copy(busybox, copied.join("bin/busybox")).unwrap();
    std::os::unix::fs::symlink("busybox", copied.join("bin/sh")).unwrap();
    fs::write(copied.join("dup"), "x").unwrap();
    store.copy_tree(&large, &copied.join("opt/large")).unwrap();
    let result = gcsl::eval::plain::Plain::Dir {
        path: composed.clone(),
        identity: None,
    };
    let kept = pekit::record::keep_files(&result, &store, &host, &sandbox.mounted()).unwrap();
    assert_eq!(kept[&composed], Kept::Dir(store.put_tree(&copied).unwrap()));
    // A sandbox of it: the tree an overlay of its own, which a program
    // writes in, the tree unchanged, but its mount point stays.
    let root = sandbox.temp_dir().unwrap().unwrap();
    assert_eq!(sandbox.make_sandbox(&composed, &root), Some(Ok(())));
    let script = "cat /opt/large/dup && echo new > /opt/large/new && echo top > /top";
    assert!(run_in(&sandbox, &root, script));
    assert!(!run_in(&sandbox, &root, "busybox mv /opt/large /opt/gone"));
    let read = |path: &str| fs::read_to_string(host(&format!("{root}{path}")));
    assert_eq!(read("/opt/large/new").unwrap(), "new\n");
    assert_eq!(read("/opt/large/lib/f5").unwrap(), "5");
    assert_eq!(read("/top").unwrap(), "top\n");
    assert!(!store.trees_root().join(large.hex()).join("new").exists());
    // A directory that holds the mount point shows what the overlay does.
    let opt = host(&format!("{root}/opt/large"));
    assert!(opt.join("new").exists() && opt.join("lib/f69").exists());
    let parent = host(&format!("{root}/opt"));
    assert!(parent.join("large/new").exists() && parent.join("large/dup").exists());
    // GCSL writes in the sandbox's layers, never the tree.
    let written = sandbox
        .write_path(&format!("{root}/opt/large/lib/g"))
        .unwrap();
    assert!(!written.starts_with(store.trees_root()));
    fs::write(&written, "g").unwrap();
    assert_eq!(read("/opt/large/lib/g").unwrap(), "g");
}
