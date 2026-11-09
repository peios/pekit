//! A tree deeper than this process may open descriptors, and longer than a
//! path may be, as a step can leave in its `/tmp`: removed, and swept. A
//! test binary of its own, since it lowers the process's descriptor limit.

use std::fs;
use std::os::fd::OwnedFd;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;

use pekit::store::Store;
use rustix::fs::{CWD, Mode, OFlags, mkdirat, openat};
use rustix::process::{Resource, Rlimit, getrlimit, setrlimit};

/// The descriptor limit the test lowers this process's to.
const LIMIT: u64 = 64;

/// How many levels deep each tree is: past [`LIMIT`], and, with each
/// level's name, past the longest path, 4,096 bytes.
const DEPTH: usize = 400;

/// Each level's name.
const LEVEL: &str = "a-directory-name-of-some-length";

/// Makes a tree [`DEPTH`] levels deep at `top`, a file at its foot and
/// every tenth level read-only and unlistable by its owner, as a sealed
/// tree's or an overlay's work directory is: walking down by descriptors,
/// one open at a time, since no path can name the foot.
fn deep_tree(top: &Path) {
    fs::create_dir_all(top).unwrap();
    let open = |at: &OwnedFd, name: &str| -> OwnedFd {
        openat(
            at,
            name,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
            Mode::empty(),
        )
        .unwrap()
    };
    let mut at = openat(
        CWD,
        top,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .unwrap();
    for level in 0..DEPTH {
        mkdirat(&at, LEVEL, Mode::from_raw_mode(0o755)).unwrap();
        let below = open(&at, LEVEL);
        // Once its one entry is made.
        if level % 10 == 9 {
            rustix::fs::fchmod(&at, Mode::from_raw_mode(0o100)).unwrap();
        }
        at = below;
    }
    let file = openat(
        &at,
        "left",
        OFlags::WRONLY | OFlags::CREATE | OFlags::CLOEXEC,
        Mode::from_raw_mode(0o444),
    )
    .unwrap();
    drop(file);
}

#[test]
fn a_tree_deeper_than_the_descriptor_limit_is_removed_and_swept() {
    let dir = tempfile::tempdir().unwrap();
    let before = getrlimit(Resource::Nofile);
    setrlimit(
        Resource::Nofile,
        Rlimit {
            current: Some(LIMIT),
            maximum: before.maximum,
        },
    )
    .unwrap();
    // Removed where it is, and its top read-only too.
    let tree = dir.path().join("tree");
    deep_tree(&tree);
    fs::set_permissions(&tree, fs::Permissions::from_mode(0o500)).unwrap();
    pekit::layers::remove_tree(&tree).unwrap();
    assert!(!tree.exists());
    // Left in the scratch directory by a build that is gone, and swept.
    let store = Store::open(&dir.path().join("store")).unwrap();
    let scratch = dir.path().join("scratch");
    deep_tree(&scratch.join("step-0123456789abcdef-1-0/tmp"));
    assert!(pekit::sweep::sweep_stale(&store, &scratch).unwrap());
    assert_eq!(fs::read_dir(&scratch).unwrap().count(), 0);
    setrlimit(Resource::Nofile, before).unwrap();
}
