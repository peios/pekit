//! Tests of eviction: keeping the store within its limit by GreedyDual-Size
//! (see `design/additive.md`, F3).

mod building;

use std::fs;
use std::path::Path;

use building::{Options, build_in, observed_with};
use pekit::evict::{DEFAULT_LIMIT, limit_of};
use pekit::store::Store;

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// A workspace whose `app` builds two steps of a megabyte each: `quick`,
/// which takes no time to make again, and `slow`, which takes a second.
fn workspace() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                "#[hermetic] def quick() -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"head -c 1048576 /dev/zero | tr '\\\\0' q > out\"])\n\
                 \x20   = working_dir().file(\"out\")\n\
                 }\n\
                 #[hermetic] def slow() -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"sleep 1; head -c 1048576 /dev/zero | tr '\\\\0' s > out\"])\n\
                 \x20   = working_dir().file(\"out\")\n\
                 }\n",
            ),
            (
                "app/build.gcsl",
                "def build both() { = [quick(), slow()] }\n",
            ),
        ],
    );
    dir
}

/// Builds `both` in `dir` with its store in `state`, the store's limit
/// `limit`: how many steps ran and were found.
fn built(dir: &Path, state: &Path, limit: Option<u64>) -> (usize, usize) {
    let options = Options {
        cache_limit: limit,
        ..Options::default()
    };
    let built = observed_with(dir, state, "both", options).0.unwrap();
    (built.ran, built.cached)
}

#[test]
fn a_limit_is_a_size_its_default_or_none() {
    assert_eq!(limit_of(None), Ok(Some(DEFAULT_LIMIT)));
    assert_eq!(limit_of(Some("50GiB")), Ok(Some(50 << 30)));
    assert_eq!(limit_of(Some("500MB")), Ok(Some(500_000_000)));
    assert_eq!(limit_of(Some("1024")), Ok(Some(1024)));
    assert_eq!(limit_of(Some("0")), Ok(None));
    assert!(limit_of(Some("lots")).is_err());
    assert!(limit_of(Some("5 parsecs")).is_err());
}

#[test]
fn a_store_over_its_limit_evicts_what_is_cheapest_to_make_again() {
    let dir = workspace();
    let state = tempfile::tempdir().unwrap();
    assert_eq!(built(dir.path(), state.path(), None), (2, 0));
    // Over its limit as it ends, the store evicts the quick step's
    // contents, a megabyte that took no time to make, and keeps the slow
    // one's.
    let limit = Some(3 << 19);
    assert_eq!(built(dir.path(), state.path(), limit), (0, 2));
    assert_eq!(built(dir.path(), state.path(), None), (1, 1));
    let quick = dir.path().join("pekit-out/app/both/quick/out");
    assert_eq!(fs::read(&quick).unwrap().len(), 1 << 20);
    assert!(fs::read(&quick).unwrap().iter().all(|byte| *byte == b'q'));
    // What ran again put its contents back, and is found.
    assert_eq!(built(dir.path(), state.path(), None), (0, 2));
}

#[test]
fn a_store_another_process_has_is_not_evicted() {
    let dir = workspace();
    let state = tempfile::tempdir().unwrap();
    built(dir.path(), state.path(), None);
    // Another holds the store: nothing is evicted.
    let other = Store::open(&state.path().join("store")).unwrap();
    assert_eq!(built(dir.path(), state.path(), Some(1)), (0, 2));
    drop(other);
    assert_eq!(built(dir.path(), state.path(), None), (0, 2));
}

#[test]
fn a_scratch_directory_is_evicted_whole_unless_a_run_holds_it() {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                "#[hermetic] #[scratch] def warm(src: File) -> File {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"head -c 1048576 /dev/zero > /pekit-scratch/state; \
                 cat {src.path} > out\"])\n\
                 \x20   = working_dir().file(\"out\")\n\
                 }\n",
            ),
            ("app/name.txt", "one\n"),
            (
                "app/build.gcsl",
                "def build both() { = warm(file(\"name.txt\")) }\n",
            ),
        ],
    );
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    let kept = state.path().join("store/scratch");
    let count = || {
        fs::read_dir(&kept)
            .unwrap()
            .flatten()
            .filter(|entry| entry.path().is_dir())
            .count()
    };
    assert_eq!(count(), 1);
    // Over the limit: the scratch directory, costing nothing to make
    // again, goes.
    let options = Options {
        cache_limit: Some(1 << 19),
        ..Options::default()
    };
    observed_with(dir.path(), state.path(), "both", options)
        .0
        .unwrap();
    assert_eq!(count(), 0);
}

#[test]
fn a_store_within_its_limit_reads_nothing_more() {
    let dir = workspace();
    let state = tempfile::tempdir().unwrap();
    assert_eq!(built(dir.path(), state.path(), Some(DEFAULT_LIMIT)), (2, 0));
    // What it used is journalled for an eviction that does something, and
    // no eviction kept anything.
    let store = state.path().join("store");
    let journals = fs::read_dir(store.join("usage")).unwrap().count();
    assert_eq!(journals, 1);
    assert!(!store.join("eviction.json").exists());
    // An eviction that does something reads them.
    assert_eq!(built(dir.path(), state.path(), Some(3 << 19)), (0, 2));
    assert!(store.join("eviction.json").exists());
    assert_eq!(fs::read_dir(store.join("usage")).unwrap().count(), 0);
}
