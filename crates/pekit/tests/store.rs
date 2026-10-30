//! Tests of the content store, receipts and records.

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

use gcsl::eval::plain::Plain;
use pekit::receipt::{IdentityOf, ReceiptOf, identity, receipt};
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

/// The files in the store's `tmp/`, by name.
fn temporary(dir: &Path) -> Vec<String> {
    fs::read_dir(dir.join("store/tmp"))
        .map(|entries| {
            entries
                .flatten()
                .map(|entry| entry.file_name().to_string_lossy().into_owned())
                .collect()
        })
        .unwrap_or_default()
}

#[test]
fn a_tree_is_put_at_once_and_a_failed_put_leaves_nothing() {
    let (store, dir) = store();
    // A tree of many files, two alike, and directories within directories.
    let tree = dir.path().join("tree");
    for n in 0..300 {
        let at = tree.join(format!("d{}/e{}", n % 7, n % 3));
        fs::create_dir_all(&at).unwrap();
        fs::write(at.join(format!("f{n}")), format!("{n}\n")).unwrap();
    }
    fs::write(tree.join("one"), "alike").unwrap();
    fs::write(tree.join("two"), "alike").unwrap();
    let digest = store.put_tree(&tree).unwrap();
    // Every blob it names is in place, and nothing is left in `tmp/`.
    assert!(store.holds_tree(&digest));
    assert_eq!(temporary(dir.path()), Vec::<String>::new());
    let back = dir.path().join("back");
    store.materialise_tree(&digest, &back).unwrap();
    assert_eq!(
        fs::read_to_string(back.join("d3/e1/f220")).unwrap(),
        "220\n"
    );
    // A put that can't read a file fails, and puts in place nothing it
    // wrote, the blob of a file read before it among them.
    let failing = dir.path().join("failing");
    fs::create_dir_all(&failing).unwrap();
    fs::write(failing.join("a"), "read first").unwrap();
    fs::write(failing.join("b"), "unreadable").unwrap();
    fs::set_permissions(failing.join("b"), fs::Permissions::from_mode(0o000)).unwrap();
    if fs::read(failing.join("b")).is_ok() {
        eprintln!("skipped the failing put: this user reads any file");
        return;
    }
    assert!(store.put_tree(&failing).is_err());
    assert!(!store.has(&Digest::of(b"read first")));
    assert_eq!(temporary(dir.path()), Vec::<String>::new());
    // The thread puts as it did before.
    fs::set_permissions(failing.join("b"), fs::Permissions::from_mode(0o644)).unwrap();
    let file = store.put_file(&failing.join("a")).unwrap();
    assert!(store.has(&file.digest));
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
    // The first a link to the blob, the others to copies of it the store
    // keeps, which the same tree made again links, copying nothing more.
    let again = dir.path().join("again");
    store.materialise_tree(&digest, &again).unwrap();
    let again_inode = |path: &str| fs::metadata(again.join(path)).unwrap().ino();
    for path in ["a", "b", "sub/c"] {
        assert_eq!(inode(path), again_inode(path), "{path}");
    }
    assert!(store.is_materialised(&digest, &again));
}

/// The receipt of a step given `args`, its files' contents those of the

#[test]
fn a_tree_materialised_is_known_so_until_anything_of_it_changes() {
    let (store, dir) = store();
    let source = dir.path().join("source");
    fs::create_dir_all(source.join("sub")).unwrap();
    fs::write(source.join("a"), "same").unwrap();
    fs::write(source.join("sub/b"), "same").unwrap();
    fs::write(source.join("c"), "other").unwrap();
    std::os::unix::fs::symlink("a", source.join("l")).unwrap();
    let tree = store.put_tree(&source).unwrap();
    let fresh = || {
        let at = dir.path().join("made");
        let _ = pekit::layers::remove_tree(&at);
        store.materialise_tree(&tree, &at).unwrap();
        at
    };
    let at = fresh();
    assert!(store.is_materialised(&tree, &at));
    // Something more.
    fs::write(at.join("extra"), "x").unwrap();
    assert!(!store.is_materialised(&tree, &at));
    // A file put in a link's place, of the same contents.
    let at = fresh();
    fs::remove_file(at.join("c")).unwrap();
    fs::write(at.join("c"), "other").unwrap();
    fs::set_permissions(at.join("c"), fs::Permissions::from_mode(0o444)).unwrap();
    assert!(!store.is_materialised(&tree, &at));
    // A file of its own put in the place of the second of a blob, a link
    // to the store's copy of it, of other contents or the same.
    for contents in ["sane", "same"] {
        let at = fresh();
        fs::remove_file(at.join("sub/b")).unwrap();
        fs::write(at.join("sub/b"), contents).unwrap();
        fs::set_permissions(at.join("sub/b"), fs::Permissions::from_mode(0o444)).unwrap();
        assert_eq!(store.is_materialised(&tree, &at), contents == "same");
    }
    // A link that leads elsewhere.
    let at = fresh();
    fs::remove_file(at.join("l")).unwrap();
    std::os::unix::fs::symlink("c", at.join("l")).unwrap();
    assert!(!store.is_materialised(&tree, &at));
    // Gone.
    let _ = pekit::layers::remove_tree(&at);
    assert!(!store.is_materialised(&tree, &at));
}

/// digests of `contents`, by path.
fn receipt_of(args: &[Plain], contents: &[(&str, &[u8])]) -> Digest {
    receipt_in("/pekit-out/.steps/compile/0123456789abcdef", args, contents)
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
    let files = keep_files(&result, &store, &host, &std::collections::HashMap::new()).unwrap();
    let kept = stored(&result, &files, &store).unwrap();
    assert_eq!(kept.plain(&store).unwrap(), result);
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
    let files = keep_files(
        &with_secret,
        &store,
        &host,
        &std::collections::HashMap::new(),
    )
    .unwrap();
    assert!(files.contains_key("/pekit-out/hello"));
    assert!(matches!(
        stored(&with_secret, &files, &store),
        Err(StoreError::Secret)
    ));
    // A file not kept is no record's.
    assert!(matches!(
        stored(&result, &std::collections::HashMap::new(), &store),
        Err(StoreError::Unkept(path)) if path == "/pekit-out/hello"
    ));
}

#[test]
fn a_long_string_of_a_result_is_kept_as_a_blob_the_record_names() {
    let (store, _dir) = store();
    let none = std::collections::HashMap::new();
    // A program's output of NULs, which JSON writes six bytes each, and a
    // short string, which the record holds itself.
    let long = "\0".repeat(pekit::record::INLINE_TEXT + 1);
    let result = Plain::Record(vec![
        ("stdout".to_owned(), Plain::Str(long.clone())),
        ("code".to_owned(), Plain::Str("0".to_owned())),
    ]);
    let kept = stored(&result, &none, &store).unwrap();
    let digest = Digest::of(long.as_bytes());
    assert_eq!(kept.texts(), [digest]);
    let receipt = Digest::of(b"long");
    let record = StepRecord {
        receipt,
        result: kept,
        duration_ms: 1,
    };
    store.put_record(&receipt, &record).unwrap();
    let bytes = store.record_bytes(&receipt).unwrap().unwrap();
    assert!(bytes.len() < 1024, "{} bytes", bytes.len());
    // Read back whole.
    let found = store.record::<StepRecord>(&receipt).unwrap().unwrap();
    assert_eq!(found.result.plain(&store).unwrap(), result);
    // Its blob gone, the result can't be given back.
    let blob = store.blob_path(&digest, false);
    fs::set_permissions(&blob, fs::Permissions::from_mode(0o644)).unwrap();
    fs::remove_file(&blob).unwrap();
    assert!(found.result.plain(&store).is_err());
}

/// The identity of a step of the function number `function` of
/// `app/build.gcsl`, given `args`, its earlier steps' names by node.
fn identity_of(function: u32, args: &[Plain], steps: &[(u32, &str)]) -> Digest {
    let callee = Plain::Closure {
        input: 0,
        function,
        captures: Vec::new(),
    };
    let code_name = |input: u32| (input == 0).then(|| "app/build.gcsl".to_owned());
    let step_of = |node: u32| {
        steps
            .iter()
            .find(|(number, _)| *number == node)
            .map(|(_, name)| (*name).to_owned())
    };
    identity(IdentityOf {
        callee: &callee,
        args,
        context: &[],
        platform: "x86_64-linux",
        code_name: &code_name,
        step_of: &step_of,
    })
    .unwrap()
}

#[test]
fn an_identity_is_what_a_step_is_apart_from_its_inputs_contents() {
    let file = |path: &str| Plain::File {
        path: path.to_owned(),
        identity: None,
    };
    let base = identity_of(1, &[file("/w/a.c")], &[]);
    // Its files by their paths alone: no contents are asked for, and the
    // same path is the same identity, whatever is there.
    assert_eq!(base, identity_of(1, &[file("/w/a.c")], &[]));
    assert_ne!(base, identity_of(1, &[file("/w/b.c")], &[]));
    // Another function, or other data: another identity.
    assert_ne!(base, identity_of(2, &[file("/w/a.c")], &[]));
    assert_ne!(base, identity_of(1, &[file("/w/a.c"), Plain::Int(1)], &[]));
    // A step's result, by that step's name.
    let after = identity_of(1, &[Plain::Step(3)], &[(3, "compile/0011223344556677")]);
    assert_eq!(
        after,
        identity_of(1, &[Plain::Step(7)], &[(7, "compile/0011223344556677")])
    );
    assert_ne!(
        after,
        identity_of(1, &[Plain::Step(3)], &[(3, "compile/8899aabbccddeeff")])
    );
    // A step's result in a receipt, which only an identity names, is an
    // error, as one of a step no name is known for in an identity is.
    let callee = Plain::Closure {
        input: 0,
        function: 1,
        captures: Vec::new(),
    };
    let unnamed = identity(IdentityOf {
        callee: &callee,
        args: &[Plain::Step(3)],
        context: &[],
        platform: "x86_64-linux",
        code_name: &|_| Some("app/build.gcsl".to_owned()),
        step_of: &|_| None,
    });
    assert!(unnamed.is_err());
    let code = [("app/build.gcsl".to_owned(), Digest::of(b"code"))];
    let of = ReceiptOf {
        code: &code,
        args: &[Plain::Step(3)],
        captures: &[],
        context: &[],
        own: "/pekit-out/.steps/x/0",
        code_of: &|_| None,
        platform: "x86_64-linux",
    };
    assert!(receipt(of, &mut |_, _| Ok(Digest::of(b""))).is_err());
}
