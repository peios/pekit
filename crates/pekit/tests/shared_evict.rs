//! Tests of evicting a store while other processes have it open (see
//! `pekit::evict`, "Shared eviction", and `pekit::using`).

mod skipping;

use std::fs;
use std::io::Write as _;
use std::os::unix::fs::PermissionsExt as _;
use std::path::{Path, PathBuf};
use std::process::{Child, Command};
use std::time::{Duration, Instant};

use pekit::store::{Digest, Store};

use skipping::skip;

/// Puts `bytes` into the store at `root` as a blob, as another process
/// would, without this process publishing it: its digest.
fn blob(root: &Path, bytes: &[u8]) -> Digest {
    let digest = Digest::of(bytes);
    let hex = digest.hex();
    let path = root.join("cas").join(&hex[..2]).join(&hex);
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(&path, bytes).unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o444)).unwrap();
    digest
}

/// Whether the store at `root` holds the blob `digest`.
fn holds(root: &Path, digest: &Digest) -> bool {
    let hex = digest.hex();
    root.join("cas").join(&hex[..2]).join(&hex).is_file()
}

/// An entry of a file of `using/` that publishes the blob `digest`.
fn entry(digest: &Digest) -> Vec<u8> {
    let mut entry = vec![b'b'];
    entry.extend_from_slice(digest.bytes());
    entry
}

/// Another process holding the store at `root` open, as a pekit does: its
/// lock held shared, and, if `publishing`, a file of its own in `using/`
/// that publishes `uses`, held exclusively. Its file, once it holds both.
fn holder(root: &Path, publishing: bool, uses: &[Digest]) -> Option<(Child, PathBuf)> {
    let script = root.join(format!("holder-{publishing}.sh"));
    let entries = root.join(format!("entries-{publishing}"));
    fs::write(&entries, uses.iter().flat_map(entry).collect::<Vec<u8>>()).unwrap();
    let body = if publishing {
        // The holder's number names its file, as a pekit's does.
        "mkdir -p \"$2/using\"\n\
         cat \"$3\" > \"$2/using/$$-test\"\n\
         exec flock -F -s \"$2/lock\" flock -F -x \"$2/using/$$-test\" sleep 60\n"
    } else {
        "exec flock -F -s \"$2/lock\" sleep 60\n"
    };
    fs::write(&script, body).unwrap();
    let child = Command::new("sh")
        .arg(&script)
        .arg("sh")
        .arg(root)
        .arg(&entries)
        .spawn()
        .ok()?;
    let file = root.join("using").join(format!("{}-test", child.id()));
    // Once both are held: its lock in `/proc/locks`, and its file there.
    let given_up = Instant::now() + Duration::from_secs(10);
    while Instant::now() < given_up {
        let locked = fs::read_to_string("/proc/locks")
            .is_ok_and(|locks| locks.contains(&format!(" {} ", child.id())));
        if locked && (!publishing || file.is_file()) {
            std::thread::sleep(Duration::from_millis(200));
            return Some((child, file));
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    None
}

#[test]
fn a_store_another_publishing_process_has_is_evicted_but_what_it_uses() {
    if Command::new("flock").arg("--version").output().is_err() {
        skip("no `flock`");
        return;
    }
    let state = tempfile::tempdir().unwrap();
    let root = state.path().join("store");
    let store = Store::open(&root).unwrap();
    let theirs = blob(&root, &vec![b't'; 1 << 16]);
    let nobody = blob(&root, &vec![b'n'; 1 << 16]);
    // An evictor that died left aside a blob the holder uses.
    let left = root.join("tmp/evicting-dead");
    fs::create_dir_all(&left).unwrap();
    let hex = theirs.hex();
    fs::rename(root.join("cas").join(&hex[..2]).join(&hex), left.join(&hex)).unwrap();
    let Some((mut child, _)) = holder(&root, true, &[theirs]) else {
        skip("no holder could be started");
        return;
    };
    // Evicted to almost nothing beside it: what it uses stays.
    let evicted = pekit::evict::evict(&store, &store.used(), 1);
    let _ = child.kill();
    let _ = child.wait();
    let evicted = evicted.unwrap().expect("nothing evicted");
    assert!(evicted.after < evicted.before, "{evicted:?}");
    // It is put back, and kept.
    assert!(holds(&root, &theirs));
    assert!(!holds(&root, &nobody));
    assert!(!left.exists());
}

#[test]
fn a_store_this_process_cannot_publish_for_is_left() {
    if Command::new("flock").arg("--version").output().is_err() {
        skip("no `flock`");
        return;
    }
    let state = tempfile::tempdir().unwrap();
    let root = state.path().join("store");
    let store = Store::open(&root).unwrap();
    let nobody = blob(&root, &vec![b'n'; 1 << 16]);
    let Some((mut child, _)) = holder(&root, true, &[]) else {
        skip("no holder could be started");
        return;
    };
    // This process's own file could not be written, the disk full say.
    pekit::using::publisher(&root).unwrap().break_for_test();
    // Measured just now, over its limit, so that it says why it is left.
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    fs::write(root.join("size"), format!("{} {now}\n", 1u64 << 30)).unwrap();
    let evicted = pekit::evict::evict(&store, &store.used(), 1);
    let _ = child.kill();
    let _ = child.wait();
    let skipped = evicted.unwrap_err();
    assert!(
        skipped.to_string().contains("does not say what it uses"),
        "{skipped}"
    );
    assert!(holds(&root, &nobody));
}

#[test]
fn a_store_another_process_has_that_does_not_publish_is_left() {
    if Command::new("flock").arg("--version").output().is_err() {
        skip("no `flock`");
        return;
    }
    let state = tempfile::tempdir().unwrap();
    let root = state.path().join("store");
    let store = Store::open(&root).unwrap();
    let nobody = blob(&root, &vec![b'n'; 1 << 16]);
    // An older pekit, say, which holds the store but publishes nothing.
    let Some((mut child, _)) = holder(&root, false, &[]) else {
        skip("no holder could be started");
        return;
    };
    let evicted = pekit::evict::evict(&store, &store.used(), 1);
    let _ = child.kill();
    let _ = child.wait();
    assert!(matches!(evicted, Ok(None) | Err(_)), "{evicted:?}");
    assert!(holds(&root, &nobody));
}

#[test]
fn a_dead_process_s_file_of_what_it_used_is_ignored_and_removed() {
    let state = tempfile::tempdir().unwrap();
    let root = state.path().join("store");
    let _store = Store::open(&root).unwrap();
    let dead = root.join("using/4000000-dead");
    let mut file = fs::File::create(&dead).unwrap();
    file.write_all(&entry(&Digest::of(b"gone"))).unwrap();
    drop(file);
    let live = pekit::using::live(&root).unwrap();
    // Only this process's own, which holds its file.
    assert_eq!(live.len(), 1);
    assert_eq!(live[0].pid, std::process::id());
    assert!(!live[0].blobs.contains(&Digest::of(b"gone")));
    assert!(!dead.exists());
}

#[test]
fn what_is_published_or_put_again_while_aside_is_kept() {
    if Command::new("flock").arg("--version").output().is_err() {
        skip("no `flock`");
        return;
    }
    let state = tempfile::tempdir().unwrap();
    let root = state.path().join("store");
    let store = Store::open(&root).unwrap();
    let again = blob(&root, &vec![b'a'; 1 << 16]);
    let later = blob(&root, &vec![b'l'; 1 << 16]);
    let nobody = blob(&root, &vec![b'n'; 1 << 16]);
    let Some((mut child, file)) = holder(&root, true, &[]) else {
        skip("no holder could be started");
        return;
    };
    // While all three are aside, the holder publishes two: one it finds
    // gone and puts again first, as a process that looked meanwhile would,
    // and one it publishes only.
    let (root_again, file_again) = (root.clone(), file.clone());
    pekit::evict::between_in_next_shared(move || {
        blob(&root_again, &vec![b'a'; 1 << 16]);
        let mut using = fs::OpenOptions::new()
            .append(true)
            .open(&file_again)
            .unwrap();
        using.write_all(&entry(&again)).unwrap();
        using.write_all(&entry(&later)).unwrap();
    });
    let evicted = pekit::evict::evict(&store, &store.used(), 1);
    let _ = child.kill();
    let _ = child.wait();
    evicted.unwrap().expect("nothing evicted");
    // The one put again stays as it was put; the one published is put
    // back; the other goes.
    assert!(holds(&root, &again));
    assert!(holds(&root, &later));
    assert!(!holds(&root, &nobody));
    let hex = again.hex();
    assert_eq!(
        fs::read(root.join("cas").join(&hex[..2]).join(&hex)).unwrap(),
        vec![b'a'; 1 << 16]
    );
}
