//! Tests of unpacking archives into the store.

use std::fs;
use std::io::Write;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;

use pekit::store::Store;
use pekit::unpack::{unpack, unpack_key};

/// A tar archive of `entries`, each a path and its contents, `None` for a
/// directory, with a symbolic link `top/link` to `a.txt` and a hard link
/// `top/hard` to `top/a.txt`.
fn tar_of(entries: &[(&str, Option<&str>)]) -> Vec<u8> {
    let mut builder = tar::Builder::new(Vec::new());
    for (path, contents) in entries {
        let mut header = tar::Header::new_gnu();
        if let Some(text) = contents {
            let executable = Path::new(path).extension().is_some_and(|ext| ext == "sh");
            header.set_size(text.len() as u64);
            header.set_mode(if executable { 0o755 } else { 0o644 });
            header.set_entry_type(tar::EntryType::Regular);
            builder
                .append_data(&mut header, path, text.as_bytes())
                .unwrap();
        } else {
            header.set_size(0);
            header.set_mode(0o755);
            header.set_entry_type(tar::EntryType::Directory);
            builder.append_data(&mut header, path, &[][..]).unwrap();
        }
    }
    let mut link = tar::Header::new_gnu();
    link.set_entry_type(tar::EntryType::Symlink);
    link.set_size(0);
    builder.append_link(&mut link, "top/link", "a.txt").unwrap();
    let mut hard = tar::Header::new_gnu();
    hard.set_entry_type(tar::EntryType::Link);
    hard.set_size(0);
    builder
        .append_link(&mut hard, "top/hard", "top/a.txt")
        .unwrap();
    builder.into_inner().unwrap()
}

/// `bytes` gzipped.
fn gzipped(bytes: &[u8]) -> Vec<u8> {
    let mut encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
    encoder.write_all(bytes).unwrap();
    encoder.finish().unwrap()
}

/// `bytes` compressed with xz.
fn xzed(bytes: &[u8]) -> Vec<u8> {
    let mut encoder = liblzma::write::XzEncoder::new(Vec::new(), 1);
    encoder.write_all(bytes).unwrap();
    encoder.finish().unwrap()
}

#[test]
fn an_archive_unpacks_to_a_tree_once_whatever_its_compression() {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::open(&dir.path().join("store")).unwrap();
    let tar = tar_of(&[
        ("top", None),
        ("top/a.txt", Some("a\n")),
        ("top/run.sh", Some("#!/bin/sh\n")),
        ("top/sub/b.txt", Some("b\n")),
    ]);
    let mut trees = Vec::new();
    for bytes in [tar.clone(), gzipped(&tar), xzed(&tar)] {
        fs::write(dir.path().join("archive"), &bytes).unwrap();
        let entry = store.put_file(&dir.path().join("archive")).unwrap();
        let tree = unpack(&store, (&entry, "archive"), 1, dir.path()).unwrap();
        trees.push(tree);
        // Recorded by its archive and strip.
        assert!(
            store
                .record::<serde_json::Value>(&unpack_key(&entry.digest, 1))
                .unwrap()
                .is_some()
        );
    }
    // The same files whatever the compression.
    assert!(trees.windows(2).all(|pair| pair[0] == pair[1]));
    let out = dir.path().join("out");
    store.materialise_tree(&trees[0], &out).unwrap();
    assert_eq!(fs::read_to_string(out.join("a.txt")).unwrap(), "a\n");
    assert_eq!(fs::read_to_string(out.join("sub/b.txt")).unwrap(), "b\n");
    assert_eq!(
        fs::read_link(out.join("link")).unwrap().to_str(),
        Some("a.txt")
    );
    assert_eq!(fs::read_to_string(out.join("hard")).unwrap(), "a\n");
    let mode = fs::metadata(out.join("run.sh"))
        .unwrap()
        .permissions()
        .mode();
    assert_ne!(mode & 0o111, 0);
    // Nothing left in the scratch directory.
    let left: Vec<_> = fs::read_dir(dir.path())
        .unwrap()
        .flatten()
        .filter(|entry| entry.file_name().to_string_lossy().starts_with("unpack-"))
        .collect();
    assert!(left.is_empty());
}

#[test]
fn an_entry_that_would_leave_the_directory_fails_the_unpacking() {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::open(&dir.path().join("store")).unwrap();
    // A link out, and an entry written through it.
    let mut builder = tar::Builder::new(Vec::new());
    let mut link = tar::Header::new_gnu();
    link.set_entry_type(tar::EntryType::Symlink);
    link.set_size(0);
    builder.append_link(&mut link, "out", "/tmp").unwrap();
    let mut header = tar::Header::new_gnu();
    header.set_size(2);
    header.set_mode(0o644);
    builder
        .append_data(&mut header, "out/evil", &b"x\n"[..])
        .unwrap();
    fs::write(dir.path().join("archive"), builder.into_inner().unwrap()).unwrap();
    let entry = store.put_file(&dir.path().join("archive")).unwrap();
    let error = unpack(&store, (&entry, "evil.tar"), 0, dir.path()).unwrap_err();
    assert!(
        error.starts_with("`archive.unpack` could not unpack `evil.tar`"),
        "{error}"
    );
    assert!(error.contains("passes through a link"), "{error}");
}
