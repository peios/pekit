//! Signing kernel modules, as a package with `sign_modules` asks.
//!
//! The kernel loads a module only if a signature by a key it trusts is
//! appended to it: a PKCS#7 message, an information block and a magic
//! string. pekit signs after the build, so the private key never reaches a
//! recipe's commands. It signs with ML-DSA-65 deterministically and with no
//! signed attributes, so the same module and key always give the same bytes
//! and a package stays reproducible, which the kernel's `sign-file` cannot
//! do with OpenSSL 3. The kernel checks such a signature as pure ML-DSA over
//! the whole module, with an empty context string.

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::thread;

use anyhow::{Context, Result, anyhow, bail};
use base64::Engine;
use ml_dsa::{MlDsa65, SigningKey, VerifyingKey};
use zstd::bulk::Compressor;

/// The string that ends a signed module.
const MAGIC: &[u8] = b"~Module signature appended~\n";

/// The level pekit compresses a signed module at. It is part of what a
/// package holds, so changing it changes every package that signs modules.
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
/// `[0] EXPLICIT`, as content in a ContentInfo and a certificate's version.
const EXPLICIT_0: u8 = 0xa0;

/// A module-signing key: the private key, and the certificate the kernel
/// names it by.
pub struct Key {
    signing: SigningKey<MlDsa65>,
    verifying: VerifyingKey<MlDsa65>,
    /// The certificate's issuer and serial number, as DER, which a signature
    /// names its key by.
    issuer: Vec<u8>,
    serial: Vec<u8>,
}

/// Where pekit reads the module-signing key from: `PEKIT_MODSIG_KEY`, or
/// `$XDG_CONFIG_HOME/peios/keys/modsig.pem`, or
/// `~/.config/peios/keys/modsig.pem`.
pub fn key_path() -> Result<PathBuf> {
    if let Some(path) = std::env::var_os("PEKIT_MODSIG_KEY").filter(|path| !path.is_empty()) {
        return Ok(PathBuf::from(path));
    }
    let base = match std::env::var_os("XDG_CONFIG_HOME").filter(|dir| !dir.is_empty()) {
        Some(dir) => PathBuf::from(dir),
        None => match std::env::var_os("HOME").filter(|dir| !dir.is_empty()) {
            Some(home) => PathBuf::from(home).join(".config"),
            None => bail!(
                "no module-signing key: none of PEKIT_MODSIG_KEY, XDG_CONFIG_HOME and HOME is set"
            ),
        },
    };
    Ok(base.join("peios").join("keys").join("modsig.pem"))
}

impl Key {
    /// Reads the key at `path`: a PEM file holding an ML-DSA-65 private key,
    /// whose seed it needs, and its certificate.
    pub fn load(path: &Path) -> Result<Key> {
        let text = fs::read_to_string(path)
            .with_context(|| format!("cannot read the module-signing key {}", path.display()))?;
        let read = || -> Result<Key> {
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
        };
        read().with_context(|| path.display().to_string())
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

    /// Signs the module compressed at `path`, in place, compressing it again
    /// with `compressor`.
    fn sign_file(&self, path: &Path, compressor: &mut Compressor) -> Result<()> {
        let compressed = fs::read(path)?;
        let mut module = zstd::decode_all(compressed.as_slice()).context("cannot decompress")?;
        if module.ends_with(MAGIC) {
            bail!("already signed");
        }
        module.extend(self.signature(&module)?);
        let signed = compressor.compress(&module).context("cannot compress")?;
        fs::write(path, signed)?;
        Ok(())
    }
}

/// Signs every module in the tree `dir`, in place. Each must be compressed
/// with zstd and named `.ko.zst`; an uncompressed `.ko` is refused, since
/// the signature has to be inside the compression. Symlinks are left alone.
pub fn sign_tree(dir: &Path, key: &Key) -> Result<()> {
    let mut modules = Vec::new();
    find_modules(dir, &mut modules)?;
    let next = AtomicUsize::new(0);
    let failed = Mutex::new(None);
    let threads = thread::available_parallelism().map_or(1, |n| n.get());
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
                    if let Err(error) = key.sign_file(path, &mut compressor) {
                        return fail(error.context(format!("cannot sign {}", path.display())));
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

/// Adds the path of every module file under `dir` to `modules`.
fn find_modules(dir: &Path, modules: &mut Vec<PathBuf>) -> Result<()> {
    for entry in fs::read_dir(dir).with_context(|| dir.display().to_string())? {
        let path = entry.with_context(|| dir.display().to_string())?.path();
        let kind = fs::symlink_metadata(&path)
            .with_context(|| path.display().to_string())?
            .file_type();
        let name = path.file_name().unwrap_or_default().to_string_lossy();
        if kind.is_dir() {
            find_modules(&path, modules)?;
        } else if kind.is_file() && name.ends_with(".ko.zst") {
            modules.push(path);
        } else if kind.is_file() && name.ends_with(".ko") {
            bail!(
                "{}: an uncompressed module; sign_modules signs modules compressed as .ko.zst",
                path.display()
            );
        }
    }
    Ok(())
}

/// The bytes of the PEM block labelled `label` in `text`.
fn pem(text: &str, label: &str) -> Result<Vec<u8>> {
    let begin = format!("-----BEGIN {label}-----");
    let end = format!("-----END {label}-----");
    let body = text
        .split_once(&begin)
        .and_then(|(_, rest)| rest.split_once(&end))
        .map(|(body, _)| body)
        .ok_or_else(|| anyhow!("no {label}"))?;
    let body: String = body.split_whitespace().collect();
    base64::engine::general_purpose::STANDARD
        .decode(body)
        .with_context(|| format!("the {label} is not base64"))
}

/// The seed of the ML-DSA-65 private key `der`, a PKCS#8 PrivateKeyInfo
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
        _ => bail!("the private key holds no seed, which pekit needs"),
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
    match length {
        0..=0x7f => element.push(length as u8),
        _ => {
            let bytes = length.to_be_bytes();
            let skip = bytes.iter().take_while(|byte| **byte == 0).count();
            element.push(0x80 | (bytes.len() - skip) as u8);
            element.extend(&bytes[skip..]);
        }
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
