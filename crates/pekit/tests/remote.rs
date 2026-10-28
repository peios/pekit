//! Tests of the remote cache (`design/additive.md`, F5): records and what
//! they name written to a cache of the tests' own, and found again in
//! another store.

mod building;
mod cache;

use std::fs;
use std::path::Path;
use std::sync::Arc;

use pekit::engine::{Built, build_observed};
use pekit::load::load;
use pekit::remote::{Remote, RemoteError, RemoteSettings};
use pekit::store::{Digest, Store};
use pekit::target::resolve;

/// A store in a new directory.
fn store() -> (tempfile::TempDir, Store) {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::open(&dir.path().join("store")).unwrap();
    (dir, store)
}

/// The remote cache at `url`, with `headers`.
fn remote(url: &str, headers: &[(&str, &str)]) -> Remote {
    Remote::connect(&RemoteSettings {
        url: url.to_owned(),
        instance: "main".to_owned(),
        headers: headers
            .iter()
            .map(|(name, value)| ((*name).to_owned(), (*value).to_owned()))
            .collect(),
        upload: true,
    })
    .unwrap()
}

/// A record in `store`, kept by its key: a file, an executable one, a big
/// one of `big` bytes, and a tree of a file, a link and a directory within,
/// named as a step's record names them.
fn a_record(store: &Store, at: &Path, big: usize) -> Digest {
    let tree = at.join("tree");
    fs::create_dir_all(tree.join("sub/deeper")).unwrap();
    fs::write(tree.join("a.txt"), "in the tree\n").unwrap();
    fs::write(tree.join("sub/deeper/b.txt"), "deeper\n").unwrap();
    fs::write(tree.join("sub/empty"), "").unwrap();
    std::os::unix::fs::symlink("a.txt", tree.join("link")).unwrap();
    let tree = store.put_tree(&tree).unwrap();
    let file = at.join("file.txt");
    fs::write(&file, "a file\n").unwrap();
    let file = store.put_file(&file).unwrap();
    let tool = at.join("tool");
    fs::write(&tool, "#!/bin/sh\n").unwrap();
    let mut mode = fs::metadata(&tool).unwrap().permissions();
    std::os::unix::fs::PermissionsExt::set_mode(&mut mode, 0o755);
    fs::set_permissions(&tool, mode).unwrap();
    let tool = store.put_file(&tool).unwrap();
    let large = at.join("large");
    let bytes: Vec<u8> = (0..big)
        .map(|n| u8::try_from(n % 251).unwrap_or_default())
        .collect();
    fs::write(&large, bytes).unwrap();
    let large = store.put_file(&large).unwrap();
    let record = serde_json::json!({
        "result": {
            "file": file.digest.hex(),
            "tool": tool.digest.hex(),
            "large": large.digest.hex(),
            "tree": tree.hex(),
            "not a blob": Digest::of(b"nothing").hex(),
        },
        "duration_ms": 7,
    });
    let key = Digest::of(b"a receipt");
    store.put_record(&key, &record).unwrap();
    key
}

/// Whether `found` holds every blob of `from` that `key`'s record names,
/// its record as `from` has it, and its tree as `from` has it.
fn same_record(from: &Store, found: &Store, key: &Digest) {
    let bytes = from.record_bytes(key).unwrap().unwrap();
    assert_eq!(found.record_bytes(key).unwrap().unwrap(), bytes);
    let record: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    let result = &record["result"];
    for name in ["file", "tool", "large"] {
        let digest = Digest::from_hex(result[name].as_str().unwrap()).unwrap();
        assert_eq!(
            found.read_blob(&digest).unwrap(),
            from.read_blob(&digest).unwrap(),
            "{name}"
        );
    }
    let tree = Digest::from_hex(result["tree"].as_str().unwrap()).unwrap();
    assert!(found.holds_tree(&tree));
    assert_eq!(
        found.read_tree(&tree).unwrap(),
        from.read_tree(&tree).unwrap()
    );
}

#[test]
fn a_record_written_is_found_with_all_it_names_in_another_store() {
    let served = cache::serve(1 << 20);
    let (dir, from) = store();
    let key = a_record(&from, dir.path(), 100);
    remote(&served.url, &[]).upload(&from, &key).unwrap();
    let (_other, found) = store();
    let cache = remote(&served.url, &[]);
    assert!(cache.find(&found, &key).unwrap());
    same_record(&from, &found, &key);
    // A key it does not have is no hit, and writes nothing.
    let other = Digest::of(b"another receipt");
    assert!(!cache.find(&found, &other).unwrap());
    assert!(found.record_bytes(&other).unwrap().is_none());
}

#[test]
fn a_blob_larger_than_a_batch_is_streamed_both_ways() {
    let served = cache::serve(4096);
    let (dir, from) = store();
    let key = a_record(&from, dir.path(), 50_000);
    remote(&served.url, &[]).upload(&from, &key).unwrap();
    assert_eq!(served.cache.held.lock().unwrap().streamed, 1);
    let (_other, found) = store();
    assert!(remote(&served.url, &[]).find(&found, &key).unwrap());
    assert_eq!(served.cache.held.lock().unwrap().streamed_out, 1);
    same_record(&from, &found, &key);
}

#[test]
fn only_what_the_cache_lacks_is_written() {
    let served = cache::serve(1 << 20);
    let (dir, from) = store();
    let key = a_record(&from, dir.path(), 100);
    let cache = remote(&served.url, &[]);
    cache.upload(&from, &key).unwrap();
    let written = served.cache.held.lock().unwrap().written;
    // The record, the action blob, three files, the tree's message and the
    // tree's two files with contents.
    assert_eq!(written, 8);
    cache.upload(&from, &key).unwrap();
    assert_eq!(served.cache.held.lock().unwrap().written, written);
}

#[test]
fn every_call_carries_the_headers_given() {
    let served = cache::serve(1 << 20);
    let (dir, from) = store();
    let key = a_record(&from, dir.path(), 100);
    remote(&served.url, &[("Authorization", "Bearer secret")])
        .upload(&from, &key)
        .unwrap();
    let held = served.cache.held.lock().unwrap();
    assert!(!held.authorizations.is_empty());
    assert!(
        held.authorizations
            .iter()
            .all(|value| value == "Bearer secret")
    );
}

#[test]
fn a_record_whose_contents_the_cache_lost_is_an_error_and_not_kept() {
    let served = cache::serve(1 << 20);
    let (dir, from) = store();
    let key = a_record(&from, dir.path(), 100);
    remote(&served.url, &[]).upload(&from, &key).unwrap();
    let file = fs::read(dir.path().join("file.txt")).unwrap();
    served
        .cache
        .held
        .lock()
        .unwrap()
        .blobs
        .remove(&Digest::of(&file).hex());
    let (_other, found) = store();
    let error = remote(&served.url, &[]).find(&found, &key).unwrap_err();
    assert!(matches!(error, RemoteError::Corrupt(_)), "{error}");
    assert!(found.record_bytes(&key).unwrap().is_none());
}

#[test]
fn contents_that_are_not_their_digest_s_are_an_error_and_not_kept() {
    let served = cache::serve(1 << 20);
    let (dir, from) = store();
    let key = a_record(&from, dir.path(), 100);
    remote(&served.url, &[]).upload(&from, &key).unwrap();
    let file = fs::read(dir.path().join("file.txt")).unwrap();
    served
        .cache
        .held
        .lock()
        .unwrap()
        .blobs
        .insert(Digest::of(&file).hex(), b"other contents\n".to_vec());
    let (_other, found) = store();
    let error = remote(&served.url, &[]).find(&found, &key).unwrap_err();
    assert!(error.to_string().contains("other contents"), "{error}");
    assert!(found.record_bytes(&key).unwrap().is_none());
}

#[test]
fn a_cache_that_cannot_be_reached_is_an_error() {
    // A port nothing listens on: one a listener had, closed.
    let port = std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    let (_dir, found) = store();
    let error = remote(&format!("grpc://127.0.0.1:{port}"), &[])
        .find(&found, &Digest::of(b"a receipt"))
        .unwrap_err();
    assert!(matches!(error, RemoteError::Call(_)), "{error}");
}

#[test]
fn a_cache_is_named_by_its_url() {
    let settings = RemoteSettings {
        url: "localhost:9092".to_owned(),
        ..RemoteSettings::default()
    };
    let error = Remote::connect(&settings).unwrap_err();
    assert!(error.to_string().contains("grpc://"), "{error}");
}

/// A workspace whose `app` shouts `name.txt` in a step.
fn shouting() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    fs::create_dir_all(dir.path().join("app")).unwrap();
    fs::write(dir.path().join("workspace.gcsl"), "").unwrap();
    fs::write(
        dir.path().join("app/build.gcsl"),
        "#[hermetic] fn shout(src: File) -> File {\n\
         \x20   let r = exec([\"/bin/sh\", \"-c\", \"tr a-z A-Z < {src.path} > out.txt\"])\n\
         \x20   working_dir().file(\"out.txt\")\n\
         }\n\
         def build hello() { = shout(file(\"name.txt\")) }\n",
    )
    .unwrap();
    fs::write(dir.path().join("app/name.txt"), "world\n").unwrap();
    dir
}

/// Builds `build hello` of the workspace at `root`, its store and scratch
/// in `state`, with the remote cache `remote`.
fn build_with(root: &Path, state: &Path, remote: Arc<Remote>) -> Result<Built, String> {
    let (root, state) = (root.to_path_buf(), state.to_path_buf());
    std::thread::Builder::new()
        .stack_size(gcsl::STACK_SIZE)
        .spawn(move || {
            let mut loaded = load(&root).map_err(|error| error.render())?;
            let target = resolve(&loaded.workspace, ("build", "hello"), "")
                .map_err(|error| error.message())?;
            let mut settings = building::settings(&root, &state, ("x86_64", "dev"));
            settings.remote = Some(remote);
            build_observed(&mut loaded, &target, &settings, &mut |_| {})
                .map_err(|error| error.render(&loaded))
        })
        .unwrap()
        .join()
        .unwrap()
}

/// The remote cache at `url`, which uploads, shared as a build's settings
/// hold it.
fn shared(url: &str) -> Arc<Remote> {
    Arc::new(remote(url, &[]))
}

#[test]
fn a_step_recorded_by_one_build_is_found_remotely_by_another_with_an_empty_store() {
    let served = cache::serve(1 << 20);
    let dir = shouting();
    let first = tempfile::tempdir().unwrap();
    let built = build_with(dir.path(), first.path(), shared(&served.url)).unwrap();
    assert_eq!((built.ran, built.cached, built.found_remotely), (1, 0, 0));
    // Its record and its manifest.
    assert_eq!(served.cache.held.lock().unwrap().actions.len(), 2);
    let second = tempfile::tempdir().unwrap();
    let found = build_with(dir.path(), second.path(), shared(&served.url)).unwrap();
    assert_eq!((found.ran, found.cached, found.found_remotely), (0, 1, 1));
    assert_eq!(found.value, built.value);
    // Now the store's own: found there, as a build with no cache finds it.
    let again = building::build_in(dir.path(), second.path(), "hello").unwrap();
    assert_eq!((again.ran, again.cached, again.found_remotely), (0, 1, 0));
}

#[test]
fn a_step_whose_record_the_cache_cannot_give_whole_runs() {
    let served = cache::serve(1 << 20);
    let dir = shouting();
    let first = tempfile::tempdir().unwrap();
    build_with(dir.path(), first.path(), shared(&served.url)).unwrap();
    // The step's output, `WORLD`, lost.
    served
        .cache
        .held
        .lock()
        .unwrap()
        .blobs
        .remove(&Digest::of(b"WORLD\n").hex());
    let second = tempfile::tempdir().unwrap();
    let built = build_with(dir.path(), second.path(), shared(&served.url)).unwrap();
    assert_eq!((built.ran, built.found_remotely), (1, 0));
}

#[test]
fn a_build_goes_on_without_a_cache_that_is_down() {
    let port = std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    let dir = shouting();
    let state = tempfile::tempdir().unwrap();
    let built = build_with(
        dir.path(),
        state.path(),
        shared(&format!("grpc://127.0.0.1:{port}")),
    )
    .unwrap();
    assert_eq!((built.ran, built.found_remotely), (1, 0));
}
