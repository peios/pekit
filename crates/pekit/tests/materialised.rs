//! Tests of the store's materialised trees.

use std::fs;
use std::os::unix::fs::MetadataExt;
use std::path::Path;

use pekit::store::{FileEntry, Store};

/// A store in a new temporary directory, and the directory.
fn store() -> (Store, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    (Store::open(&dir.path().join("store")).unwrap(), dir)
}

/// The device and inode of the file at `path`.
fn node(path: &Path) -> (u64, u64) {
    let meta = fs::symlink_metadata(path).unwrap();
    (meta.dev(), meta.ino())
}

/// A tree of `a`, `sub/b` and `sub/c`, these two of the same contents, and
/// a link `l`, put into `store`, and what it keeps of `sub/b`.
fn tree(store: &Store, dir: &Path) -> (pekit::store::Digest, FileEntry) {
    let tree = dir.join("tree");
    fs::create_dir_all(tree.join("sub")).unwrap();
    fs::write(tree.join("a"), "a").unwrap();
    fs::write(tree.join("sub/b"), "same").unwrap();
    fs::write(tree.join("sub/c"), "same").unwrap();
    std::os::unix::fs::symlink("a", tree.join("l")).unwrap();
    let digest = store.put_tree(&tree).unwrap();
    let same = store.put_file(&tree.join("sub/b")).unwrap();
    (digest, same)
}

#[test]
fn a_tree_is_materialised_once_and_found_again() {
    let (store, dir) = store();
    let (digest, _) = tree(&store, dir.path());
    let at = store.materialised(&digest, &[]).unwrap();
    assert!(at.starts_with(store.trees_root()));
    assert!(store.is_materialised(&digest, &at));
    let first = node(&at.join("a"));
    // Asked again, by this process or another, it is the one there.
    assert_eq!(store.materialised(&digest, &[]).unwrap(), at);
    let other = Store::open(store.root()).unwrap();
    assert_eq!(other.materialised(&digest, &[]).unwrap(), at);
    assert_eq!(node(&at.join("a")), first);
    // Nothing is left in the store's `tmp/`.
    assert_eq!(fs::read_dir(store.root().join("tmp")).unwrap().count(), 0);
    assert_eq!(store.file_count(&digest).unwrap(), 4);
}

#[test]
fn a_materialised_tree_s_directories_and_files_are_known_by_their_paths() {
    let (store, dir) = store();
    let (digest, same) = tree(&store, dir.path());
    let at = store.materialised(&digest, &[]).unwrap();
    assert_eq!(store.tree_in(&at), Some(digest));
    let sub = store.tree_in(&at.join("sub")).unwrap();
    assert_eq!(store.read_tree(&sub).unwrap().entries.len(), 2);
    assert_eq!(store.tree_in(&at.join("a")), None);
    assert_eq!(store.tree_in(&dir.path().join("tree")), None);
    // Each file of a content the blob, then its copies, in order.
    assert_eq!(store.file_in(&at.join("sub/b")), Some((same, 0)));
    assert_eq!(store.file_in(&at.join("sub/c")), Some((same, 1)));
    assert_eq!(store.file_in(&at.join("l")), None);
    assert_ne!(node(&at.join("sub/b")), node(&at.join("sub/c")));
    assert_eq!(store.blob_counts(&digest).unwrap().get(&same), Some(&2));
}

#[test]
fn an_instance_of_bases_links_later_copies_than_the_tree_s_own() {
    let (store, dir) = store();
    let (digest, same) = tree(&store, dir.path());
    let own = store.materialised(&digest, &[]).unwrap();
    let based = store.materialised(&digest, &[(same, 2)]).unwrap();
    assert_ne!(own, based);
    assert!(store.is_materialised(&digest, &own));
    assert_eq!(store.file_in(&based.join("sub/b")), Some((same, 2)));
    assert_eq!(store.file_in(&based.join("sub/c")), Some((same, 3)));
    // No file of one is a file of the other, but those of other contents.
    for name in ["sub/b", "sub/c"] {
        for other in ["sub/b", "sub/c"] {
            assert_ne!(node(&own.join(name)), node(&based.join(other)));
        }
    }
    assert_eq!(node(&own.join("a")), node(&based.join("a")));
    assert_eq!(fs::read_to_string(based.join("sub/c")).unwrap(), "same");
}

#[test]
fn materialised_trees_are_forgotten_whole() {
    let (store, dir) = store();
    let (digest, _) = tree(&store, dir.path());
    let at = store.materialised(&digest, &[]).unwrap();
    store.forget_materialised().unwrap();
    assert!(!at.exists());
    store.forget_materialised().unwrap();
    // Made again when it is next asked for.
    assert!(store.is_materialised(&digest, &store.materialised(&digest, &[]).unwrap()));
}
