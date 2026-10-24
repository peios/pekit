//! Tests of the keychain: secrets' identities, and secret files given to
//! the steps that ask for them alone, never stored.

mod building;

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;

use pekit::keychain::{Keychain, hmac};
use pekit::store::{Digest, Store};

use building::{Options, build_in, observed_with};
use gcsl::eval::effects::KeychainSecret;

#[test]
fn hmac_is_rfc_4231_s() {
    let hex = |bytes: &[u8]| {
        use std::fmt::Write as _;
        bytes.iter().fold(String::new(), |mut text, byte| {
            let _ = write!(text, "{byte:02x}");
            text
        })
    };
    // Test cases 2 and 6: a short key, and one longer than a block.
    assert_eq!(
        hex(&hmac(b"Jefe", b"what do ya want for nothing?")),
        "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"
    );
    assert_eq!(
        hex(&hmac(
            &[0xaa; 131],
            b"Test Using Larger Than Block-Size Key - Hash Key First"
        )),
        "60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54"
    );
}

#[test]
fn a_secret_s_identity_is_its_name_and_a_keyed_hash_of_its_contents() {
    let one = tempfile::tempdir().unwrap();
    let other = tempfile::tempdir().unwrap();
    let identity = |state: &Path, contents: &[u8]| {
        let keychain = Keychain::open(state).unwrap();
        let asked = KeychainSecret {
            name: "signing",
            profile: "dev",
            contents,
            file: None,
        };
        keychain.answer(&asked, None).unwrap().identity
    };
    let first = identity(one.path(), b"key one");
    assert!(first.starts_with("dev/signing#"), "{first}");
    assert_eq!(first.len(), "dev/signing#".len() + 32);
    // The same machine's key, the same contents: the same identity.
    assert_eq!(identity(one.path(), b"key one"), first);
    // Other contents, or another machine: another.
    assert_ne!(identity(one.path(), b"key two"), first);
    assert_ne!(identity(other.path(), b"key one"), first);
    // The machine's key is its owner's alone.
    let mode = fs::metadata(one.path().join("keychain-key"))
        .unwrap()
        .permissions()
        .mode();
    assert_eq!(mode & 0o777, 0o600);
}

/// A workspace whose `app` signs `name.txt` with the keychain's `signing`
/// key, which is `keys/dev.pem` for `dev` and `keys/release.pem` for
/// `release`, and whose `peek` step, given no key, looks for one.
fn signing_workspace() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    let files = [
        (
            "workspace.gcsl",
            "import keychain\n\
             pekitconf = {keychain = [\n\
             \x20   {profile = \"dev\"; name = \"signing\"; file = \"keys/dev.pem\"},\n\
             \x20   {profile = \"release\"; name = \"signing\"; file = \"keys/release.pem\"},\n\
             ]}\n",
        ),
        (
            "app/build.gcsl",
            "import keychain\n\
             #[hermetic] fn sign(src: File, key: Secret<File> = keychain.file(\"signing\")) -> File {\n\
             \x20   let ran = exec_secret([\"/bin/sh\", \"-c\", \"wc -c < \\\"$KEY\\\" > signed.txt; cat {src.path} >> signed.txt\"], secrets = {KEY = key.reveal().path})\n\
             \x20   = file(\"signed.txt\")\n\
             }\n\
             #[hermetic] fn peek(src: File) -> File {\n\
             \x20   let ran = exec([\"/bin/sh\", \"-c\", \"(cat /pekit-secrets/dev/signing || echo none) > peeked.txt 2>/dev/null\"])\n\
             \x20   = file(\"peeked.txt\")\n\
             }\n\
             #[hermetic] fn asks() -> File {\n\
             \x20   let key = keychain.file(\"signing\")\n\
             \x20   = file(\"never\")\n\
             }\n\
             def build hello() { = sign(file(\"name.txt\")) }\n\
             def build both() {\n\
             \x20   signed = sign(file(\"name.txt\"))\n\
             \x20   peeked = peek(file(\"name.txt\"))\n\
             }\n\
             def build inside() { = asks() }\n",
        ),
        ("app/name.txt", "name\n"),
        ("keys/dev.pem", "DEV KEY\n"),
        ("keys/release.pem", "RELEASE KEY\n"),
    ];
    for (path, text) in files {
        let path = dir.path().join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
    dir
}

/// Whether any file under `dir` holds `text`.
fn anywhere(dir: &Path, text: &str) -> bool {
    let Ok(entries) = fs::read_dir(dir) else {
        return false;
    };
    entries.flatten().any(|entry| {
        let path = entry.path();
        if path.is_dir() && !path.is_symlink() {
            anywhere(&path, text)
        } else {
            fs::read(&path).is_ok_and(|bytes| {
                bytes
                    .windows(text.len())
                    .any(|window| window == text.as_bytes())
            })
        }
    })
}

#[test]
fn a_step_is_given_its_secret_file_and_the_store_never_holds_it() {
    let dir = signing_workspace();
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((built.ran, built.cached), (1, 0));
    let signed = dir.path().join("pekit-out/app/hello/sign/signed.txt");
    assert_eq!(fs::read_to_string(&signed).unwrap(), "8\nname\n");
    // The key is in neither the store nor the scratch, nor the outputs.
    let store = Store::open(&state.path().join("store")).unwrap();
    assert!(!store.has(&Digest::of(b"DEV KEY\n")));
    assert!(!anywhere(&state.path().join("store"), "DEV KEY"));
    assert!(!anywhere(&state.path().join("scratch"), "DEV KEY"));
    assert!(!anywhere(&dir.path().join("pekit-out"), "DEV KEY"));
    // Again: found in the store by the key's identity.
    let again = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((again.ran, again.cached), (0, 1));
    // A changed key is another identity: it runs again.
    fs::write(dir.path().join("keys/dev.pem"), "NEW DEV KEY\n").unwrap();
    let changed = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((changed.ran, changed.cached), (1, 0));
    assert_eq!(fs::read_to_string(&signed).unwrap(), "12\nname\n");
    // Another profile, its own key.
    let options = Options {
        profile: "release",
        ..Options::default()
    };
    let release = observed_with(dir.path(), state.path(), "hello", options)
        .0
        .unwrap();
    assert_eq!((release.ran, release.cached), (1, 0));
    assert_eq!(fs::read_to_string(&signed).unwrap(), "12\nname\n");
}

#[test]
fn a_secret_file_is_the_step_s_given_it_alone() {
    let dir = signing_workspace();
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "both").unwrap();
    let peeked = dir.path().join("pekit-out/app/both/peek/peeked.txt");
    assert_eq!(fs::read_to_string(peeked).unwrap(), "none\n");
    // A step asking the keychain itself is refused.
    let error = build_in(dir.path(), state.path(), "inside").unwrap_err();
    assert!(
        error.contains("a step is given its secrets: resolve them in a parameter default"),
        "{error}"
    );
}
