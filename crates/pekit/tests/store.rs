//! Tests of the content store, receipts and records.

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

use gcsl::eval::plain::Plain;
use pekit::receipt::{ReceiptOf, receipt};
use pekit::record::{Kept, StepRecord, StoreError, keep_files, stored};
use pekit::store::{Digest, Store};

/// A store in a new temporary directory, and the directory.
fn store() -> (Store, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    (Store::open(&dir.path().join("store")).unwrap(), dir)
}

#[test]
fn a_file_is_kept_once_by_its_contents_and_taken_out_by_mode() {
    let (store, dir) = store();
    let a = dir.path().join("a");
    fs::write(&a, "same").unwrap();
    let b = dir.path().join("b");
    fs::write(&b, "same").unwrap();
    fs::set_permissions(&b, fs::Permissions::from_mode(0o755)).unwrap();
    let first = store.put_file(&a).unwrap();
    let second = store.put_file(&b).unwrap();
    assert_eq!(first.digest, second.digest);
    assert_eq!(first.digest, Digest::of(b"same"));
    assert_eq!(
        (first.size, first.executable, second.executable),
        (4, false, true)
    );
    assert!(store.has(&first.digest));
    // Each mode taken out as it was put in, read-only.
    let out = dir.path().join("out");
    store.materialise_file(&second, &out.join("x")).unwrap();
    let mode = fs::metadata(out.join("x")).unwrap().permissions().mode();
    assert_eq!(mode & 0o777, 0o555);
    store.materialise_file(&first, &out.join("y")).unwrap();
    assert_eq!(fs::read_to_string(out.join("y")).unwrap(), "same");
    // A copy is its own, and writable.
    store.copy_file(&first, &out.join("z")).unwrap();
    fs::write(out.join("z"), "changed").unwrap();
    assert_eq!(store.read_blob(&first.digest).unwrap(), b"same");
}

#[test]
fn a_directory_is_a_tree_whose_digest_is_its_whole_content_s() {
    let (store, dir) = store();
    let make = |root: &Path, text: &str| {
        fs::create_dir_all(root.join("sub")).unwrap();
        fs::write(root.join("sub/f"), text).unwrap();
        fs::write(root.join("g"), "g").unwrap();
        std::os::unix::fs::symlink("g", root.join("link")).unwrap();
    };
    make(&dir.path().join("one"), "f");
    make(&dir.path().join("two"), "f");
    make(&dir.path().join("three"), "F");
    let one = store.put_tree(&dir.path().join("one")).unwrap();
    assert_eq!(one, store.put_tree(&dir.path().join("two")).unwrap());
    assert_ne!(one, store.put_tree(&dir.path().join("three")).unwrap());
    let back = dir.path().join("back");
    store.materialise_tree(&one, &back).unwrap();
    assert_eq!(fs::read_to_string(back.join("sub/f")).unwrap(), "f");
    assert_eq!(
        fs::read_link(back.join("link")).unwrap(),
        PathBuf::from("g")
    );
}

#[test]
fn a_tree_s_identical_files_are_materialised_as_files_of_their_own() {
    use std::os::unix::fs::MetadataExt;
    let (store, dir) = store();
    let tree = dir.path().join("tree");
    fs::create_dir_all(tree.join("sub")).unwrap();
    for path in ["a", "b", "sub/c"] {
        fs::write(tree.join(path), "same").unwrap();
    }
    let digest = store.put_tree(&tree).unwrap();
    let back = dir.path().join("back");
    store.materialise_tree(&digest, &back).unwrap();
    let inode = |path: &str| fs::metadata(back.join(path)).unwrap().ino();
    // One blob, three files, each its own and read-only.
    assert_ne!(inode("a"), inode("b"));
    assert_ne!(inode("b"), inode("sub/c"));
    assert_ne!(inode("a"), inode("sub/c"));
    for path in ["a", "b", "sub/c"] {
        assert_eq!(fs::read_to_string(back.join(path)).unwrap(), "same");
        let mode = fs::metadata(back.join(path)).unwrap().permissions().mode();
        assert_eq!(mode & 0o222, 0, "{path}");
    }
}

/// The receipt of a step given `args`, its files' contents those of the
/// digests of `contents`, by path.
fn receipt_of(args: &[Plain], contents: &[(&str, &[u8])]) -> Digest {
    receipt_in("/pekit-out/app/hello/compile", args, contents)
}

/// The receipt of a step whose output directory is `own`, given `args`, its
/// files' contents those of the digests of `contents`, by path.
fn receipt_in(own: &str, args: &[Plain], contents: &[(&str, &[u8])]) -> Digest {
    let code = [("app/build.gcsl".to_owned(), Digest::of(b"code"))];
    let of = ReceiptOf {
        code: &code,
        args,
        captures: &[],
        context: &[],
        own,
        code_of: &|_| None,
        platform: "x86_64-linux",
    };
    let mut lookup = |path: &str, _dir: bool| {
        contents
            .iter()
            .find(|(at, _)| *at == path)
            .map(|(_, bytes)| Digest::of(bytes))
            .ok_or_else(|| format!("no {path}"))
    };
    receipt(of, &mut lookup).unwrap()
}

#[test]
fn a_receipt_keys_files_by_path_and_contents_and_secrets_by_identity() {
    let file = |path: &str| Plain::File {
        path: path.to_owned(),
        identity: None,
    };
    let a = [file("/w/a.c")];
    let base = receipt_of(&a, &[("/w/a.c", b"int x;")]);
    // The same contents at another path: another receipt.
    assert_ne!(
        base,
        receipt_of(&[file("/w/b.c")], &[("/w/b.c", b"int x;")])
    );
    // Other contents: another receipt; the same again: the same.
    assert_ne!(base, receipt_of(&a, &[("/w/a.c", b"int y;")]));
    assert_eq!(base, receipt_of(&a, &[("/w/a.c", b"int x;")]));
    // A record's fields in any order.
    let record = |fields: Vec<(&str, i64)>| {
        Plain::Record(
            fields
                .into_iter()
                .map(|(name, n)| (name.to_owned(), Plain::Int(n)))
                .collect(),
        )
    };
    assert_eq!(
        receipt_of(&[record(vec![("a", 1), ("b", 2)])], &[]),
        receipt_of(&[record(vec![("b", 2), ("a", 1)])], &[])
    );
    // A secret by its identity, never its value.
    let secret = |value: &str, identity: &str| Plain::Secret {
        identity: identity.to_owned(),
        revealed: true,
        value: Box::new(Plain::Str(value.to_owned())),
    };
    assert_eq!(
        receipt_of(&[secret("k3y", "test key")], &[]),
        receipt_of(&[secret("other", "test key")], &[])
    );
    assert_ne!(
        receipt_of(&[secret("k3y", "test key")], &[]),
        receipt_of(&[secret("k3y", "release key")], &[])
    );
    // Its output directory, which names what it makes.
    assert_ne!(
        receipt_of(&[], &[]),
        receipt_in("/pekit-out/app/hello/compile.2", &[], &[])
    );
    // A NaN keys as itself.
    assert_eq!(
        receipt_of(&[Plain::Float(f64::NAN)], &[]),
        receipt_of(&[Plain::Float(-f64::NAN)], &[])
    );
}

#[test]
fn a_result_is_recorded_with_its_files_and_given_back() {
    let (store, dir) = store();
    let out = dir.path().join("out");
    fs::create_dir_all(&out).unwrap();
    fs::write(out.join("hello"), "binary").unwrap();
    let host = |path: &str| out.join(path.trim_start_matches("/pekit-out/"));
    let result = Plain::Record(vec![
        (
            "exe".to_owned(),
            Plain::File {
                path: "/pekit-out/hello".to_owned(),
                identity: None,
            },
        ),
        ("n".to_owned(), Plain::Int(1)),
    ]);
    let files = keep_files(&result, &store, &host).unwrap();
    let kept = stored(&result, &files).unwrap();
    assert_eq!(kept.plain(), result);
    let [(path, Kept::File(entry))] = kept.files()[..] else {
        panic!("{:?}", kept.files());
    };
    assert_eq!(path, "/pekit-out/hello");
    assert_eq!(store.read_blob(&entry.digest).unwrap(), b"binary");
    // Kept by its receipt, and read back.
    let receipt = Digest::of(b"receipt");
    let record = StepRecord {
        receipt,
        result: kept,
        duration_ms: 12,
    };
    store.put_record(&receipt, &record).unwrap();
    assert_eq!(store.record::<StepRecord>(&receipt).unwrap(), Some(record));
    assert_eq!(
        store.record::<StepRecord>(&Digest::of(b"none")).unwrap(),
        None
    );
    // A secret is never stored, though the files beside it are kept.
    let secret = Plain::Secret {
        identity: "key".to_owned(),
        revealed: false,
        value: Box::new(Plain::Str("k3y".to_owned())),
    };
    let with_secret = Plain::List(vec![result.clone(), secret]);
    let files = keep_files(&with_secret, &store, &host).unwrap();
    assert!(files.contains_key("/pekit-out/hello"));
    assert!(matches!(
        stored(&with_secret, &files),
        Err(StoreError::Secret)
    ));
    // A file not kept is no record's.
    assert!(matches!(
        stored(&result, &std::collections::HashMap::new()),
        Err(StoreError::Unkept(path)) if path == "/pekit-out/hello"
    ));
}
