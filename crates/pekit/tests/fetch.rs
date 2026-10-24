//! Tests of pekit's answer to `fetch`: from the store by digest, else
//! downloaded once.

mod support;

use std::sync::atomic::Ordering;

use pekit::fetch::{IN_A_STEP, fetch, fetched_path};
use pekit::store::{Digest, Store};
use support::serve;

#[test]
fn a_pinned_file_is_downloaded_once_and_then_found_by_its_digest() {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::open(&dir.path().join("store")).unwrap();
    let (url, served) = serve(b"world\n", "files/name.txt?x=1");
    let digest = Digest::of(b"world\n");
    let pin = digest.hex().to_ascii_uppercase();
    let entry = fetch(&store, &url, &pin).unwrap();
    assert_eq!(entry.digest, digest);
    assert_eq!(store.read_blob(&digest).unwrap(), b"world\n");
    // Again, from the store: no request.
    assert_eq!(fetch(&store, &url, &pin).unwrap(), entry);
    assert_eq!(served.load(Ordering::SeqCst), 1);
    assert_eq!(
        fetched_path(&url, &digest),
        format!("/pekit-out/.fetch/{}/name.txt", digest.hex())
    );
}

#[test]
fn a_fetch_that_is_not_what_was_pinned_fails_with_what_it_is() {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::open(&dir.path().join("store")).unwrap();
    let (url, _) = serve(b"world\n", "name.txt");
    let found = Digest::of(b"world\n").hex();
    let (message, note) = fetch(&store, &url, "").unwrap_err();
    assert_eq!(
        message,
        format!("`fetch` of `{url}` is not pinned: its sha256 is `{found}`")
    );
    assert_eq!(
        note.unwrap(),
        format!("pin it: `fetch(\"{url}\", sha256 = \"{found}\")`")
    );
    let other = "0".repeat(64);
    let (message, _) = fetch(&store, &url, &other).unwrap_err();
    assert_eq!(
        message,
        format!("`fetch` of `{url}` gave a file whose sha256 is `{found}`, not `{other}`")
    );
    let (message, _) = fetch(&store, &url, "abc").unwrap_err();
    assert!(message.ends_with("which is no SHA-256 digest: 64 hexadecimal digits"));
    assert!(IN_A_STEP.starts_with("`fetch` runs outside hermetic steps"));
}
