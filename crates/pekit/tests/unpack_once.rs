//! A test of unpacking an archive once when several want it at once, alone
//! in its test program, since it counts every unpacking the process makes.

use std::fs;
use std::sync::{Arc, Barrier};

use pekit::store::Store;
use pekit::unpack::{unpack, unpackings};

/// A tar archive of `files` files, each a few bytes, under `top/`: enough
/// that unpacking it takes long enough for others to want it meanwhile.
fn many_files(files: usize) -> Vec<u8> {
    let mut builder = tar::Builder::new(Vec::new());
    for n in 0..files {
        let text = format!("{n}\n");
        let mut header = tar::Header::new_gnu();
        header.set_size(text.len() as u64);
        header.set_mode(0o644);
        header.set_entry_type(tar::EntryType::Regular);
        builder
            .append_data(
                &mut header,
                format!("top/d{}/f{n}", n % 16),
                text.as_bytes(),
            )
            .unwrap();
    }
    builder.into_inner().unwrap()
}

#[test]
fn an_archive_several_want_at_once_is_unpacked_once() {
    let dir = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open(&dir.path().join("store")).unwrap());
    fs::write(dir.path().join("archive"), many_files(4000)).unwrap();
    let entry = store.put_file(&dir.path().join("archive")).unwrap();
    let before = unpackings();
    // Eight at once, as a build's steps that need one toolchain are.
    let start = Arc::new(Barrier::new(8));
    let threads: Vec<_> = (0..8)
        .map(|_| {
            let (store, start, scratch) = (
                Arc::clone(&store),
                Arc::clone(&start),
                dir.path().to_path_buf(),
            );
            std::thread::spawn(move || {
                start.wait();
                unpack(&store, (&entry, "archive"), 1, &scratch).unwrap()
            })
        })
        .collect();
    let trees: Vec<_> = threads
        .into_iter()
        .map(|thread| thread.join().unwrap())
        .collect();
    // One unpacking, whose tree every other found.
    assert_eq!(unpackings() - before, 1);
    assert!(trees.windows(2).all(|pair| pair[0] == pair[1]));
    assert!(store.holds_tree(&trees[0]));
}
