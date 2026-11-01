//! Signs a tree of Linux kernel modules: `modsig sign IN OUT REPORT`.
//!
//! The kernel loads a module only if a signature by a key it trusts is
//! appended to it: a PKCS#7 message, an information block and a magic
//! string. This signs with ML-DSA-65 deterministically and with no signed
//! attributes, so the same module and key always give the same bytes and a
//! package stays reproducible, which the kernel's `sign-file` cannot do with
//! OpenSSL 3. The kernel checks such a signature as pure ML-DSA over the
//! whole module, with an empty context string.
//!
//! It copies the tree `IN` to `OUT`, each module signed. The key is the PEM
//! file `$MODSIG_KEY` names, holding an ML-DSA-65 private key and its
//! certificate. `IN` needs only reading: a step's input can't be changed.
//!
//! A step can't report what a program given a secret printed, so the program
//! writes it to `REPORT`: nothing if it signed the tree, or else why it did
//! not. No file means it never finished. A report names files of the tree,
//! and never holds anything read from the key.

use std::env;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::sync::Mutex;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::thread;

use anyhow::{Context, Result, anyhow, bail};
use base64::Engine;
use ml_dsa::{MlDsa65, SigningKey, VerifyingKey};
use zstd::bulk::Compressor;

/// The string that ends a signed module.
const MAGIC: &[u8] = b"~Module signature appended~\n";

/// The level a signed module is compressed at. It is part of what a package
/// holds, so changing it changes every package that signs modules.
const LEVEL: i32 = 19;

/// The DER encodings of the object identifiers a signature names.
const ML_DSA_65: &[u8] = &[
    0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x03, 0x12,
];
const SHA512: &[u8] = &[
    0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x02, 0x03,
];
const SIGNED_DATA: &[u8] = &[
    0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x07, 0x02,
];
const DATA: &[u8] = &[
    0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x07, 0x01,
];

/// The DER tags read and written here.
const INTEGER: u8 = 0x02;
const BIT_STRING: u8 = 0x03;
const OCTET_STRING: u8 = 0x04;
const SEQUENCE: u8 = 0x30;
const SET: u8 = 0x31;
/// A private key's seed alone, `[0] IMPLICIT OCTET STRING`.
const SEED: u8 = 0x80;
/// `[0] EXPLICIT`, as content in a `ContentInfo` and a certificate's version.
const EXPLICIT_0: u8 = 0xa0;

/// The file names of a module that can't be signed: the signature has to be
/// inside the compression, and the only compression signed here is zstd's.
const UNSIGNABLE: [&str; 3] = [".ko", ".ko.gz", ".ko.xz"];

fn main() -> ExitCode {
    let args: Vec<PathBuf> = env::args_os().skip(1).map(PathBuf::from).collect();
    let [command, from, to, report] = args.as_slice() else {
        eprintln!("usage: modsig sign IN OUT REPORT");
        return ExitCode::from(2);
    };
    if command != Path::new("sign") {
        eprintln!("usage: modsig sign IN OUT REPORT");
        return ExitCode::from(2);
    }
    let (status, reported) = match sign(from, to) {
        Ok(()) => (ExitCode::SUCCESS, String::new()),
        Err(error) => (ExitCode::FAILURE, format!("{error:#}")),
    };
    if let Err(error) = fs::write(report, &reported) {
        eprintln!("modsig: cannot write the report: {error}");
        return ExitCode::FAILURE;
    }
    eprint!("{reported}");
    status
}

/// Copies the tree `from` to `to`, each module signed with `$MODSIG_KEY`'s
/// key. Refuses a tree it can't sign before it reads the key.
fn sign(from: &Path, to: &Path) -> Result<()> {
    let mut tree = Tree::default();
    tree.read(from, Path::new(""))?;
    if tree.modules.is_empty() {
        bail!("no .ko.zst module to sign");
    }
    let key = env::var_os("MODSIG_KEY").context("MODSIG_KEY is not set")?;
    let key = Key::load(Path::new(&key))?;
    tree.write(from, to)?;
    sign_modules(from, to, &tree.modules, &key)
}

/// What a tree holds, each path relative to it, in order.
#[derive(Default)]
struct Tree {
    dirs: Vec<PathBuf>,
    links: Vec<(PathBuf, PathBuf)>,
    files: Vec<PathBuf>,
    modules: Vec<PathBuf>,
}

impl Tree {
    /// Adds what the directory `within` of the tree `root` holds.
    fn read(&mut self, root: &Path, within: &Path) -> Result<()> {
        let dir = root.join(within);
        let mut names = Vec::new();
        for entry in fs::read_dir(&dir).with_context(|| within.display().to_string())? {
            names.push(
                entry
                    .with_context(|| within.display().to_string())?
                    .file_name(),
            );
        }
        names.sort();
        for name in names {
            let path = within.join(&name);
            let shown = || path.display().to_string();
            let kind = fs::symlink_metadata(root.join(&path))
                .with_context(shown)?
                .file_type();
            let name = name.to_string_lossy();
            if kind.is_dir() {
                self.dirs.push(path.clone());
                self.read(root, &path)?;
            } else if kind.is_symlink() {
                let target = fs::read_link(root.join(&path)).with_context(shown)?;
                self.links.push((path, target));
            } else if !kind.is_file() {
                bail!("{}: neither a file, a directory nor a link", path.display());
            } else if name.ends_with(".ko.zst") {
                self.modules.push(path);
            } else if UNSIGNABLE.iter().any(|end| name.ends_with(end)) {
                bail!(
                    "{}: a module that is not compressed as .ko.zst, the only kind that can be \
                     signed",
                    path.display()
                );
            } else {
                self.files.push(path);
            }
        }
        Ok(())
    }

    /// Makes the tree's directories and links under `to`, and copies its
    /// files from `from`: everything but its modules.
    fn write(&self, from: &Path, to: &Path) -> Result<()> {
        fs::create_dir_all(to).with_context(|| to.display().to_string())?;
        for dir in &self.dirs {
            fs::create_dir(to.join(dir)).with_context(|| dir.display().to_string())?;
        }
        for (link, target) in &self.links {
            std::os::unix::fs::symlink(target, to.join(link))
                .with_context(|| link.display().to_string())?;
        }
        for file in &self.files {
            fs::copy(from.join(file), to.join(file)).with_context(|| file.display().to_string())?;
        }
        Ok(())
    }
}

/// Signs each of `modules`, from the tree `from` into the tree `to`.
fn sign_modules(from: &Path, to: &Path, modules: &[PathBuf], key: &Key) -> Result<()> {
    let next = AtomicUsize::new(0);
    let failed = Mutex::new(None);
    let threads = thread::available_parallelism().map_or(1, std::num::NonZero::get);
    thread::scope(|scope| {
        for _ in 0..threads {
            scope.spawn(|| {
                let fail = |error: anyhow::Error| {
                    if let Ok(mut failed) = failed.lock() {
                        failed.get_or_insert(error);
                    }
                };
                // One for each thread: making one costs more than using it.
                let mut compressor = match Compressor::new(LEVEL) {
                    Ok(compressor) => compressor,
                    Err(error) => return fail(anyhow!(error).context("cannot compress")),
                };
                while let Some(path) = modules.get(next.fetch_add(1, Ordering::Relaxed)) {
                    if failed.lock().is_ok_and(|failed| failed.is_some()) {
                        return;
                    }
                    if let Err(error) =
                        key.sign_file(&from.join(path), &to.join(path), &mut compressor)
                    {
                        return fail(error.context(path.display().to_string()));
                    }
                }
            });
        }
    });
    match failed.into_inner() {
        Ok(Some(error)) => Err(error),
        Ok(None) => Ok(()),
        Err(_) => bail!("internal error: a signing thread panicked"),
    }
}

/// A module-signing key: the private key, and the certificate the kernel
/// names it by.
struct Key {
    signing: SigningKey<MlDsa65>,
    verifying: VerifyingKey<MlDsa65>,
    /// The certificate's issuer and serial number, as DER, which a signature
    /// names its key by.
    issuer: Vec<u8>,
    serial: Vec<u8>,
}

impl Key {
    /// Reads the key at `path`: a PEM file holding an ML-DSA-65 private key,
    /// whose seed it needs, and its certificate. No error tells of what the
    /// file holds, since a report is not a secret.
    fn load(path: &Path) -> Result<Key> {
        let text = fs::read(path).map_err(|_| anyhow!("cannot read the key"))?;
        let text = String::from_utf8(text).map_err(|_| anyhow!("the key is not a PEM file"))?;
        let seed = seed(&pem(&text, "PRIVATE KEY")?)?;
        let certificate = pem(&text, "CERTIFICATE")?;
        let (issuer, serial, public) = certificate_parts(&certificate)?;
        let signing = SigningKey::<MlDsa65>::from_seed(&seed.into());
        let verifying = signing.expanded_key().verifying_key();
        if verifying.encode().as_slice() != public {
            bail!("the certificate is not the private key's");
        }
        Ok(Key {
            signing,
            verifying,
            issuer: issuer.to_vec(),
            serial: serial.to_vec(),
        })
    }

    /// What signing `module` appends to it.
    fn signature(&self, module: &[u8]) -> Result<Vec<u8>> {
        let signature = self
            .signing
            .expanded_key()
            .sign_deterministic(module, &[])
            .map_err(|_| anyhow!("cannot sign"))?;
        // Checked, so that a fault in signing shows here rather than as a
        // module the kernel refuses.
        if !self.verifying.verify_with_context(module, &[], &signature) {
            bail!("the signature made does not verify");
        }
        let signature = signature.encode();
        let algorithm = |oid: &[u8]| der(SEQUENCE, oid);
        let signer_info = der(
            SEQUENCE,
            &[
                der(INTEGER, &[1]),
                der(SEQUENCE, &[self.issuer.as_slice(), &self.serial].concat()),
                algorithm(SHA512),
                algorithm(ML_DSA_65),
                der(OCTET_STRING, signature.as_slice()),
            ]
            .concat(),
        );
        let signed_data = der(
            SEQUENCE,
            &[
                der(INTEGER, &[1]),
                der(SET, &algorithm(SHA512)),
                // The module is the content, outside the message.
                der(SEQUENCE, DATA),
                der(SET, &signer_info),
            ]
            .concat(),
        );
        let message = der(
            SEQUENCE,
            &[SIGNED_DATA, &der(EXPLICIT_0, &signed_data)].concat(),
        );
        let length = u32::try_from(message.len()).context("the signature is too long")?;
        // struct module_signature: no algorithm, hash, signer or key ID,
        // which the message gives; id_type PKEY_ID_PKCS7; the message's
        // length, big-endian.
        let mut info = [0u8; 12];
        info[2] = 2;
        info[8..].copy_from_slice(&length.to_be_bytes());
        Ok([message.as_slice(), &info, MAGIC].concat())
    }

    /// Signs the compressed module `from` into `to`, compressing it again
    /// with `compressor`. It keeps the module's mode.
    fn sign_file(&self, from: &Path, to: &Path, compressor: &mut Compressor) -> Result<()> {
        let compressed = fs::read(from)?;
        let mut module = zstd::decode_all(compressed.as_slice()).context("cannot decompress")?;
        if module.ends_with(MAGIC) {
            bail!("already signed");
        }
        module.extend(self.signature(&module)?);
        let signed = compressor.compress(&module).context("cannot compress")?;
        fs::write(to, signed)?;
        fs::set_permissions(to, fs::metadata(from)?.permissions())?;
        Ok(())
    }
}

/// The bytes of the PEM block labelled `label` in `text`.
fn pem(text: &str, label: &str) -> Result<Vec<u8>> {
    let begin = format!("-----BEGIN {label}-----");
    let end = format!("-----END {label}-----");
    let body = text
        .split_once(&begin)
        .and_then(|(_, rest)| rest.split_once(&end))
        .map(|(body, _)| body)
        .ok_or_else(|| anyhow!("the key file has no {label}"))?;
    let body: String = body.split_whitespace().collect();
    base64::engine::general_purpose::STANDARD
        .decode(body)
        .map_err(|_| anyhow!("the key file's {label} is not base64"))
}

/// The seed of the ML-DSA-65 private key `der`, a PKCS#8 `PrivateKeyInfo`
/// holding the seed alone or the seed and the expanded key.
fn seed(key: &[u8]) -> Result<[u8; 32]> {
    let invalid = || anyhow!("the private key is not a PKCS#8 ML-DSA-65 key");
    let (info, _) = tlv(key, SEQUENCE).ok_or_else(invalid)?;
    let (_, rest) = tlv(info, INTEGER).ok_or_else(invalid)?;
    let (algorithm, rest) = tlv(rest, SEQUENCE).ok_or_else(invalid)?;
    if algorithm != ML_DSA_65 {
        bail!("the private key is not an ML-DSA-65 key");
    }
    let (private, _) = tlv(rest, OCTET_STRING).ok_or_else(invalid)?;
    let seed = match private.first() {
        Some(&SEED) => tlv(private, SEED).map(|(seed, _)| seed),
        Some(&SEQUENCE) => tlv(private, SEQUENCE)
            .and_then(|(both, _)| tlv(both, OCTET_STRING))
            .map(|(seed, _)| seed),
        _ => bail!("the private key holds no seed, which signing needs"),
    };
    seed.and_then(|seed| seed.try_into().ok())
        .ok_or_else(invalid)
}

/// The issuer and serial number, as DER, and the ML-DSA-65 public key of the
/// certificate `der`.
fn certificate_parts(certificate: &[u8]) -> Result<(&[u8], &[u8], &[u8])> {
    let invalid = || anyhow!("the certificate is not an X.509 certificate");
    let (certificate, _) = tlv(certificate, SEQUENCE).ok_or_else(invalid)?;
    let (mut tbs, _) = tlv(certificate, SEQUENCE).ok_or_else(invalid)?;
    if tbs.first() == Some(&EXPLICIT_0) {
        tbs = tlv(tbs, EXPLICIT_0).ok_or_else(invalid)?.1;
    }
    let serial = whole(tbs, INTEGER).ok_or_else(invalid)?;
    let rest = &tbs[serial.len()..];
    let (_, rest) = tlv(rest, SEQUENCE).ok_or_else(invalid)?;
    let issuer = whole(rest, SEQUENCE).ok_or_else(invalid)?;
    let rest = &rest[issuer.len()..];
    let (_, rest) = tlv(rest, SEQUENCE).ok_or_else(invalid)?;
    let (_, rest) = tlv(rest, SEQUENCE).ok_or_else(invalid)?;
    let (public_key_info, _) = tlv(rest, SEQUENCE).ok_or_else(invalid)?;
    let (algorithm, rest) = tlv(public_key_info, SEQUENCE).ok_or_else(invalid)?;
    if algorithm != ML_DSA_65 {
        bail!("the certificate's key is not an ML-DSA-65 key");
    }
    let (bits, _) = tlv(rest, BIT_STRING).ok_or_else(invalid)?;
    match bits.split_first() {
        Some((0, public)) => Ok((issuer, serial, public)),
        _ => Err(invalid()),
    }
}

/// The content of the DER element with tag `tag` that `input` starts with,
/// and what follows it.
fn tlv(input: &[u8], tag: u8) -> Option<(&[u8], &[u8])> {
    let element = whole(input, tag)?;
    let header = header_length(element)?;
    Some((&element[header..], &input[element.len()..]))
}

/// The whole DER element with tag `tag` that `input` starts with.
fn whole(input: &[u8], tag: u8) -> Option<&[u8]> {
    if *input.first()? != tag {
        return None;
    }
    let header = header_length(input)?;
    let length = match input[1] {
        short @ 0..=0x7f => short as usize,
        // At most four octets, which header_length checks.
        _ => input[2..header]
            .iter()
            .fold(0usize, |length, byte| length << 8 | *byte as usize),
    };
    input.get(..header.checked_add(length)?)
}

/// The length of the tag and length octets of the DER element `input`
/// starts with.
fn header_length(input: &[u8]) -> Option<usize> {
    match *input.get(1)? {
        0..=0x7f => Some(2),
        0x81..=0x84 => {
            let header = 2 + (input[1] & 0x7f) as usize;
            (input.len() >= header).then_some(header)
        }
        _ => None,
    }
}

/// The DER element with tag `tag` and content `content`.
fn der(tag: u8, content: &[u8]) -> Vec<u8> {
    let length = content.len();
    let mut element = vec![tag];
    if let Ok(short @ 0..=0x7f) = u8::try_from(length) {
        element.push(short);
    } else {
        let bytes = length.to_be_bytes();
        let skip = bytes.iter().take_while(|byte| **byte == 0).count();
        // At most the eight octets of a usize.
        element.push(0x80 | u8::try_from(bytes.len() - skip).unwrap_or(8));
        element.extend(&bytes[skip..]);
    }
    element.extend(content);
    element
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn der_lengths_round_trip() {
        for length in [0, 1, 0x7f, 0x80, 0xff, 0x100, 0xffff, 0x10000] {
            let content = vec![7u8; length];
            let element = der(OCTET_STRING, &content);
            let (read, rest) = tlv(&element, OCTET_STRING).expect("reads back");
            assert_eq!(read, content.as_slice());
            assert!(rest.is_empty());
        }
    }
}
