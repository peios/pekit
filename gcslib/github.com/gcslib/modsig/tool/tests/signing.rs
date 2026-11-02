//! Tests of `modsig sign`, the program, with the test key, each signature
//! checked by OpenSSL. The key is outside this crate so that a workspace
//! that takes the library never holds it.

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Output};

const TEST_KEY: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../../../../../tests/fixtures/modsig/test-key.pem"
);

const MAGIC: &[u8] = b"~Module signature appended~\n";

/// The length of the information block before the magic string.
const INFO: usize = 12;

/// The length of an ML-DSA-65 signature.
const SIGNATURE: usize = 3309;

/// `content` compressed as a build would.
fn compressed(content: &[u8]) -> Vec<u8> {
    zstd::encode_all(content, 3).unwrap()
}

/// Writes `content` at `path` of the tree `dir`.
fn put(dir: &Path, path: &str, content: &[u8]) {
    let at = dir.join(path);
    fs::create_dir_all(at.parent().unwrap()).unwrap();
    fs::write(at, content).unwrap();
}

/// A tree with two modules, the paths and contents of which it gives, and
/// files that are not modules.
fn tree(dir: &Path) -> Vec<(String, Vec<u8>)> {
    let made = vec![
        (
            "1.0/kernel/a.ko.zst".to_owned(),
            b"\x7fELF module a".to_vec(),
        ),
        (
            "1.0/kernel/sub/b.ko.zst".to_owned(),
            b"\x7fELF module b".repeat(1000),
        ),
    ];
    for (path, module) in &made {
        put(dir, path, &compressed(module));
    }
    put(dir, "1.0/modules.dep", b"kernel/a.ko.zst:\n");
    put(dir, "1.0/hook", b"#!/bin/sh\n");
    fs::set_permissions(dir.join("1.0/hook"), fs::Permissions::from_mode(0o755)).unwrap();
    std::os::unix::fs::symlink("kernel", dir.join("1.0/alias")).unwrap();
    made
}

/// What `modsig sign` did.
struct Signed {
    output: Output,
    out: PathBuf,
    report: Result<String, std::io::Error>,
}

/// Runs `modsig sign` on the tree `from`, with the key `key`, into a new
/// directory of `scratch` named `name`.
fn sign(from: &Path, key: &str, scratch: &Path, name: &str) -> Signed {
    let out = scratch.join(name);
    let report = scratch.join(format!("{name}.report"));
    let output = Command::new(env!("CARGO_BIN_EXE_modsig"))
        .arg("sign")
        .args([from, &out, &report])
        .env("MODSIG_KEY", key)
        .output()
        .unwrap();
    Signed {
        output,
        out,
        report: fs::read_to_string(report),
    }
}

/// `signed` signing with [`TEST_KEY`], which has to succeed.
fn signed(from: &Path, scratch: &Path, name: &str) -> PathBuf {
    let done = sign(from, TEST_KEY, scratch, name);
    assert_eq!(done.report.unwrap(), "");
    assert!(done.output.status.success());
    done.out
}

/// Runs `openssl` with `args`, giving what it wrote.
fn openssl(args: &[&str]) -> Output {
    let output = Command::new("openssl").args(args).output().unwrap();
    assert!(
        output.status.success(),
        "openssl {args:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    output
}

/// The path of `file` as an argument.
fn arg(file: &Path) -> &str {
    file.to_str().unwrap()
}

/// The PKCS#7 message of `signed`, `module` with a signature appended,
/// having checked what surrounds it.
fn message<'a>(signed: &'a [u8], module: &[u8]) -> &'a [u8] {
    let rest = signed.strip_prefix(module).expect("the module, unchanged");
    let rest = rest.strip_suffix(MAGIC).expect("the magic string");
    let (message, info) = rest.split_at(rest.len() - INFO);
    // No algorithm, hash, signer or key ID; PKEY_ID_PKCS7; the length.
    assert_eq!(info[..8], [0, 0, 2, 0, 0, 0, 0, 0]);
    let length = u32::from_be_bytes(info[8..].try_into().unwrap());
    assert_eq!(length as usize, message.len());
    message
}

#[test]
fn a_signature_is_openssl_s_deterministic_pure_ml_dsa_over_the_module() {
    let dir = tempfile::tempdir().unwrap();
    let from = dir.path().join("from");
    let made = tree(&from);
    let out = signed(&from, dir.path(), "out");
    let public = dir.path().join("public.pem");
    fs::write(
        &public,
        openssl(&["x509", "-in", TEST_KEY, "-noout", "-pubkey"]).stdout,
    )
    .unwrap();
    for (path, module) in &made {
        let signed = zstd::decode_all(&fs::read(out.join(path)).unwrap()[..]).unwrap();
        let cms = message(&signed, module);
        let cms_file = dir.path().join("cms.der");
        fs::write(&cms_file, cms).unwrap();
        let module_file = dir.path().join("module");
        fs::write(&module_file, module).unwrap();

        // A detached SignedData naming the certificate's issuer and serial
        // number, with no signed attributes.
        let printed = openssl(&[
            "cms",
            "-cmsout",
            "-print",
            "-inform",
            "DER",
            "-in",
            arg(&cms_file),
        ]);
        let printed = String::from_utf8(printed.stdout).unwrap();
        let serial = openssl(&["x509", "-in", TEST_KEY, "-noout", "-serial"]);
        let serial = String::from_utf8(serial.stdout).unwrap();
        let serial = serial.trim().strip_prefix("serial=").unwrap();
        for expected in [
            "pkcs7-signedData",
            "eContent: <ABSENT>",
            "signedAttrs:\n          <ABSENT>",
            "O=Peios, CN=pekit test module signing key",
            "ML-DSA-65",
            serial,
        ] {
            assert!(printed.contains(expected), "{expected} in {printed}");
        }

        // The signature ends the message's last element.
        let signature = &cms[cms.len() - SIGNATURE..];
        let signature_file = dir.path().join("signature");
        fs::write(&signature_file, signature).unwrap();
        let verified = openssl(&[
            "pkeyutl",
            "-verify",
            "-pubin",
            "-inkey",
            arg(&public),
            "-rawin",
            "-in",
            arg(&module_file),
            "-sigfile",
            arg(&signature_file),
        ]);
        assert!(
            String::from_utf8_lossy(&verified.stdout).contains("Signature Verified Successfully")
        );
        let theirs = openssl(&[
            "pkeyutl",
            "-sign",
            "-inkey",
            TEST_KEY,
            "-rawin",
            "-in",
            arg(&module_file),
            "-pkeyopt",
            "deterministic:1",
        ]);
        assert_eq!(theirs.stdout, signature);
    }
}

/// Every file of the tree `dir`, by its path, with its contents.
fn contents(dir: &Path) -> Vec<(PathBuf, Vec<u8>)> {
    let mut found = Vec::new();
    let mut pending = vec![dir.to_path_buf()];
    while let Some(at) = pending.pop() {
        for entry in fs::read_dir(at).unwrap() {
            let path = entry.unwrap().path();
            if path.is_symlink() || path.is_file() {
                let bytes = fs::read(&path).unwrap_or_default();
                found.push((path.strip_prefix(dir).unwrap().to_path_buf(), bytes));
            } else {
                pending.push(path);
            }
        }
    }
    found.sort();
    found
}

#[test]
fn signing_again_gives_the_same_bytes_on_any_number_of_cores() {
    let dir = tempfile::tempdir().unwrap();
    let from = dir.path().join("from");
    tree(&from);
    for module in 0..64 {
        put(
            &from,
            &format!("1.0/kernel/{module}.ko.zst"),
            &compressed(format!("\x7fELF {module}").repeat(100).as_bytes()),
        );
    }
    let first = signed(&from, dir.path(), "first");
    let second = signed(&from, dir.path(), "second");
    assert_eq!(contents(&first), contents(&second));
    let report = dir.path().join("one.report");
    let one = dir.path().join("one");
    let status = Command::new("taskset")
        .args(["-c", "0", env!("CARGO_BIN_EXE_modsig"), "sign"])
        .args([&from, &one, &report])
        .env("MODSIG_KEY", TEST_KEY)
        .status()
        .unwrap();
    assert!(status.success());
    assert_eq!(contents(&first), contents(&one));
}

#[test]
fn what_is_no_module_is_copied_as_it_is() {
    let dir = tempfile::tempdir().unwrap();
    let from = dir.path().join("from");
    tree(&from);
    let out = signed(&from, dir.path(), "out");
    assert_eq!(
        fs::read(out.join("1.0/modules.dep")).unwrap(),
        b"kernel/a.ko.zst:\n"
    );
    let hook = fs::metadata(out.join("1.0/hook")).unwrap();
    assert_eq!(hook.permissions().mode() & 0o777, 0o755);
    assert_eq!(
        fs::read_link(out.join("1.0/alias")).unwrap(),
        Path::new("kernel")
    );
}

#[test]
fn the_tree_it_reads_can_be_read_only_and_a_module_keeps_its_mode() {
    let dir = tempfile::tempdir().unwrap();
    let from = dir.path().join("from");
    tree(&from);
    let module = from.join("1.0/kernel/a.ko.zst");
    fs::set_permissions(&module, fs::Permissions::from_mode(0o444)).unwrap();
    let kernel = from.join("1.0/kernel");
    fs::set_permissions(&kernel, fs::Permissions::from_mode(0o555)).unwrap();
    let out = signed(&from, dir.path(), "out");
    fs::set_permissions(&kernel, fs::Permissions::from_mode(0o755)).unwrap();
    let mode = fs::metadata(out.join("1.0/kernel/a.ko.zst")).unwrap();
    assert_eq!(mode.permissions().mode() & 0o777, 0o444);
}

/// That signing the tree `from` fails, reporting `reason`.
fn refused(from: &Path, key: &str, scratch: &Path, reason: &str) {
    let done = sign(from, key, scratch, "refused");
    assert_eq!(done.output.status.code(), Some(1));
    let report = done.report.unwrap();
    assert!(report.contains(reason), "{reason} in {report}");
}

#[test]
fn a_module_that_cannot_be_signed_is_refused() {
    for (name, content) in [
        ("c.ko", &b"\x7fELF"[..]),
        ("c.ko.xz", b"\x7fELF"),
        ("c.ko.gz", b"\x7fELF"),
    ] {
        let dir = tempfile::tempdir().unwrap();
        let from = dir.path().join("from");
        tree(&from);
        put(&from, &format!("1.0/kernel/{name}"), content);
        refused(
            &from,
            TEST_KEY,
            dir.path(),
            &format!("1.0/kernel/{name}: a module that is not compressed as .ko.zst"),
        );
    }
}

#[test]
fn a_tree_it_cannot_sign_is_refused_whatever_the_key() {
    let dir = tempfile::tempdir().unwrap();
    let from = dir.path().join("from");
    put(&from, "1.0/modules.dep", b"");
    refused(
        &from,
        "/no/such/key",
        dir.path(),
        "no .ko.zst module to sign",
    );
}

#[test]
fn a_module_signed_already_or_not_zstd_is_refused() {
    let dir = tempfile::tempdir().unwrap();
    let from = dir.path().join("from");
    tree(&from);
    let once = signed(&from, dir.path(), "once");
    refused(&once, TEST_KEY, dir.path(), "already signed");
    let dir = tempfile::tempdir().unwrap();
    let from = dir.path().join("from");
    put(&from, "bad.ko.zst", b"not zstd");
    refused(&from, TEST_KEY, dir.path(), "bad.ko.zst: cannot decompress");
}

/// The block of the test key labelled `label`, as it is in the file.
fn block(label: &str) -> String {
    let text = fs::read_to_string(TEST_KEY).unwrap();
    let begin = text.find(&format!("-----BEGIN {label}-----")).unwrap();
    let end = format!("-----END {label}-----\n");
    let finish = text.find(&end).unwrap() + end.len();
    text[begin..finish].to_owned()
}

#[test]
fn a_bad_key_is_reported_and_the_report_holds_none_of_it() {
    let dir = tempfile::tempdir().unwrap();
    let from = dir.path().join("from");
    tree(&from);
    let key = dir.path().join("key.pem");
    let private = block("PRIVATE KEY");
    let certificate = block("CERTIFICATE");
    let broken = private.replacen('\n', "\n!!!!", 3);
    let other_key = dir.path().join("other-key.pem");
    let other_certificate = dir.path().join("other-certificate.pem");
    openssl(&[
        "req",
        "-x509",
        "-newkey",
        "ml-dsa-65",
        "-nodes",
        "-subj",
        "/CN=other",
        "-keyout",
        arg(&other_key),
        "-out",
        arg(&other_certificate),
    ]);
    let other = fs::read_to_string(other_certificate).unwrap();
    // Each report is the whole of it: it holds nothing read from the key.
    for (held, reason) in [
        ("not a key".to_owned(), "the key file has no PRIVATE KEY"),
        (private.clone(), "the key file has no CERTIFICATE"),
        (
            format!("{broken}{certificate}"),
            "the key file's PRIVATE KEY is not base64",
        ),
        (
            format!("{private}{other}"),
            "the certificate is not the private key's",
        ),
    ] {
        fs::write(&key, held).unwrap();
        let done = sign(&from, arg(&key), dir.path(), "refused");
        assert_eq!(done.output.status.code(), Some(1));
        assert_eq!(done.report.unwrap(), reason);
        assert_eq!(String::from_utf8_lossy(&done.output.stderr), reason);
    }
}
