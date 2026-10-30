//! Tests of eviction: keeping the store within its limit by GreedyDual-Size
//! (see `design/additive.md`, F3).

mod building;

use std::fs;
use std::path::Path;
use std::time::{SystemTime, UNIX_EPOCH};

use building::{Options, build_in, observed_with};
use pekit::evict::{DEFAULT_LIMIT, limit_of};
use pekit::store::{Digest, Store, TreeEntry};

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

/// The bytes of blobs the ledger of the store in `state` says it has.
fn ledgered(state: &Path) -> u64 {
    let text = fs::read_to_string(state.join("store/size")).unwrap();
    text.split(' ').next().unwrap().parse().unwrap()
}

/// Writes the ledger of the store in `state`: `bytes` measured `ago`
/// seconds ago.
fn ledger(state: &Path, bytes: u64, ago: u64) {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs();
    fs::create_dir_all(state.join("store")).unwrap();
    fs::write(state.join("store/size"), format!("{bytes} {}\n", now - ago)).unwrap();
}

#[test]
fn a_store_is_walked_only_when_its_ledger_may_be_over_its_limit() {
    let dir = workspace();
    let state = tempfile::tempdir().unwrap();
    ledger(state.path(), 0, 0);
    // What a build wrote is added to the ledger.
    assert_eq!(built(dir.path(), state.path(), Some(DEFAULT_LIMIT)), (2, 0));
    assert!(
        ledgered(state.path()) >= 2 << 20,
        "{}",
        ledgered(state.path())
    );
    // A ledger within the limit is trusted: nothing is walked or evicted.
    ledger(state.path(), 0, 0);
    assert_eq!(built(dir.path(), state.path(), Some(3 << 19)), (0, 2));
    assert_eq!(built(dir.path(), state.path(), None), (0, 2));
    // One a day old is not: the store is walked, and evicted.
    ledger(state.path(), 0, 2 * 24 * 60 * 60);
    assert_eq!(built(dir.path(), state.path(), Some(3 << 19)), (0, 2));
    assert!(
        ledgered(state.path()) < 3 << 19,
        "{}",
        ledgered(state.path())
    );
    assert_eq!(built(dir.path(), state.path(), None), (1, 1));
    // One over the limit is walked too.
    ledger(state.path(), 4 << 20, 0);
    assert_eq!(built(dir.path(), state.path(), Some(3 << 19)), (0, 2));
    assert_eq!(built(dir.path(), state.path(), None), (1, 1));
}

#[test]
fn a_tree_known_complete_is_walked_again_once_an_eviction_ran() {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                "#[hermetic] def quick() -> Dir {\n\
                 \x20   let r = exec([\"sh\", \"-c\", \"mkdir out; head -c 1048576 /dev/zero | tr '\\\\0' q > out/q\"])\n\
                 \x20   = working_dir().dir(\"out\")\n\
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
    let state = tempfile::tempdir().unwrap();
    assert_eq!(built(dir.path(), state.path(), None), (2, 0));
    // Found, its tree is marked complete.
    assert_eq!(built(dir.path(), state.path(), None), (0, 2));
    let marks = state.path().join("store/complete");
    assert_eq!(fs::read_dir(&marks).unwrap().count(), 1);
    // An eviction takes its contents, and every mark: it is made again.
    assert_eq!(built(dir.path(), state.path(), Some(3 << 19)), (0, 2));
    assert!(!marks.exists());
    assert_eq!(built(dir.path(), state.path(), None), (1, 1));
    assert_eq!(built(dir.path(), state.path(), None), (0, 2));
}

#[test]
fn a_subtree_two_records_name_stays_until_both_are_evicted() {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::open(&dir.path().join("store")).unwrap();
    let tree = dir.path().join("tree");
    let (outer, inner) = ("o".repeat(1 << 20), "i".repeat(1 << 20));
    write(&tree, &[("outer", &outer), ("shared/inner", &inner)]);
    let whole = store.put_tree(&tree).unwrap();
    let entries = store.read_tree(&whole).unwrap().entries;
    let digest_of = |wanted: &str| {
        entries
            .iter()
            .find_map(|entry| match entry {
                TreeEntry::File { name, file, .. } if name == wanted => Some(file.digest),
                TreeEntry::Dir { name, tree, .. } if name == wanted => Some(*tree),
                _ => None,
            })
            .unwrap()
    };
    let (outer, shared) = (digest_of("outer"), digest_of("shared"));
    let inner = store
        .read_tree(&shared)
        .unwrap()
        .entries
        .iter()
        .find_map(|entry| match entry {
            TreeEntry::File { file, .. } => Some(file.digest),
            _ => None,
        })
        .unwrap();
    // One record names the whole tree and was quick to make; another names
    // its subtree alone and was slow.
    let record = |name: &str, tree: Digest, cost: u64| {
        let key = Digest::of(name.as_bytes());
        let value = serde_json::json!({"tree": tree.hex(), "duration_ms": cost});
        store.put_record(&key, &value).unwrap();
    };
    record("whole", whole, 0);
    record("shared", shared, 1 << 40);
    let state = dir.path();
    ledger(state, 0, 2 * 86_400);
    // Over its limit by a megabyte: the whole tree's record is evicted, and
    // the subtree the other names stays, with its file.
    pekit::evict::evict(&store, &store.used(), 3 << 19).unwrap();
    assert!(!store.has(&whole) && !store.has(&outer));
    assert!(store.has(&shared) && store.has(&inner));
    // Both evicted, nothing is left.
    pekit::evict::evict(&store, &store.used(), 1).unwrap();
    assert!(!store.has(&shared) && !store.has(&inner));
}

#[test]
fn a_blob_s_copies_are_counted_in_the_store_and_evicted_as_unnamed() {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::open(&dir.path().join("store")).unwrap();
    let tree = dir.path().join("tree");
    let big = "x".repeat(1 << 20);
    write(&tree, &[("a", &big), ("b", &big)]);
    let digest = store.put_tree(&tree).unwrap();
    // Its second file a link to the blob's first copy.
    let made = dir.path().join("made");
    store.materialise_tree(&digest, &made).unwrap();
    let copies = || -> usize {
        fs::read_dir(dir.path().join("store/cas"))
            .unwrap()
            .flatten()
            .flat_map(|shard| fs::read_dir(shard.path()).unwrap().flatten())
            .filter(|blob| blob.file_name().to_string_lossy().ends_with(".r1"))
            .count()
    };
    assert_eq!(copies(), 1);
    // Measured with the blob: a store of two megabytes and more.
    let state = dir.path();
    ledger(state, 0, 2 * 86_400);
    pekit::evict::evict(&store, &store.used(), 64 << 20).unwrap();
    assert!(ledgered(state) > 2 << 20, "{}", ledgered(state));
    // No record names it: evicted with what is cheapest to make again,
    // the files made from it still there.
    pekit::evict::evict(&store, &store.used(), 1).unwrap();
    assert_eq!(copies(), 0);
    assert_eq!(fs::read_to_string(made.join("b")).unwrap(), big);
}

#[test]
fn an_eviction_removes_every_materialised_tree_before_any_blob() {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::open(&dir.path().join("store")).unwrap();
    let tree = dir.path().join("tree");
    write(&tree, &[("a", &"x".repeat(1 << 20))]);
    let digest = store.put_tree(&tree).unwrap();
    let materialised = store.materialised(&digest, &[]).unwrap();
    // An eviction that need evict nothing leaves it.
    ledger(dir.path(), 0, 0);
    pekit::evict::evict(&store, &store.used(), 64 << 20).unwrap();
    assert!(materialised.is_dir());
    ledger(dir.path(), 0, 2 * 86_400);
    pekit::evict::evict(&store, &store.used(), 1).unwrap();
    assert!(!store.trees_root().exists());
}

#[test]
fn what_a_killed_build_left_is_swept_once_no_other_process_has_the_store() {
    let dir = tempfile::tempdir().unwrap();
    let store = Store::open(&dir.path().join("store")).unwrap();
    let scratch = dir.path().join("scratch");
    let own = pekit::sweep::token();
    assert_eq!(own.len(), 16);
    let tmp = dir.path().join("store/tmp");
    write(
        dir.path(),
        &[
            ("store/tmp/0123456789abcdef-3", "left"),
            (&format!("store/tmp/{own}-4"), "mine"),
            ("scratch/step-0123456789abcdef-1-0/w/a", "left"),
            ("scratch/swept-0123456789abcdef-2/a", "left"),
            (&format!("scratch/step-{own}-1-0/w/a"), "mine"),
            ("scratch/named/a", "a step's scratch directory"),
        ],
    );
    // Not while another process has the store open.
    let other = Store::open(store.root()).unwrap();
    assert!(!pekit::sweep::sweep_stale(&store, &scratch).unwrap());
    assert!(tmp.join("0123456789abcdef-3").exists());
    drop(other);
    assert!(pekit::sweep::sweep_stale(&store, &scratch).unwrap());
    let names = |dir: &Path| -> Vec<String> {
        let mut names: Vec<String> = fs::read_dir(dir)
            .unwrap()
            .map(|entry| entry.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        names.sort();
        names
    };
    assert_eq!(names(&tmp), [format!("{own}-4")]);
    assert_eq!(
        names(&scratch),
        ["named".to_owned(), format!("step-{own}-1-0")]
    );
    // The store is held shared again, as every process holds it.
    assert!(Store::open(store.root()).is_ok());
}
