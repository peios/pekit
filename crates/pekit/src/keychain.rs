//! The keychain: pekit's answer to GCSL's `keychain` module.
//!
//! # Secrets
//!
//! A build asks for a secret by name, `keychain.file("module-signing")`,
//! and GCSL finds it: in the workspace's configuration, `pekitconf.keychain`,
//! for the profile `pekit build --profile` names, `dev` by default, or else
//! in the environment (see `design/libraries.md`, L10). Pekit gives each
//! secret two things ([`Keychain::answer`]):
//!
//! - **Its identity,** what receipts record of it: its profile and name and
//!   a keyed hash of its contents, `dev/module-signing#<32 hex digits>`,
//!   HMAC-SHA-256 under a random key of this machine's, kept in the state
//!   directory, never in the store. A changed key is so a new identity, and
//!   reuses nothing made with the old one; and the identity tells nothing of
//!   the contents to whoever has it, since no one can test a guess at them
//!   without the machine's key. Two machines holding the same secret give
//!   it two identities, so a step given a secret is never found in a store
//!   shared between machines: it runs where its secret is.
//! - **For a file, its path,** `/pekit-secrets/<profile>/<name>`, the same
//!   on every machine, so that a program that records the path of its key
//!   records the same one everywhere. A step given the secret has the file
//!   bound there, read-only, from where the keychain found it
//!   ([`Keychain::host_file`]); no other step has it, and it is never put in
//!   the store.
//!
//! A step cannot ask the keychain itself: what it reads must be among its
//! inputs, so its secrets are resolved in a parameter default, or by its
//! caller, and given to it.

use std::collections::HashMap;
use std::fs;
use std::io::{self, Read, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use gcsl::eval::effects::{KeychainAnswer, KeychainSecret};
use sha2::{Digest as _, Sha256};

use crate::sandbox::SECRETS;

/// The file of the state directory that holds the machine's key.
const KEY_FILE: &str = "keychain-key";

/// Why a step's keychain is refused (see [Secrets](#secrets)).
pub const IN_A_STEP: &str = "a step is given its secrets: resolve them in a parameter default, \
                             or in the caller, and pass them in";

/// A build's keychain (see [Secrets](#secrets)).
#[derive(Debug)]
pub struct Keychain {
    /// The machine's key, which each identity's hash is keyed by.
    key: [u8; 32],
    /// Each secret file given, by its path as steps see it, and where it is.
    files: Mutex<HashMap<String, PathBuf>>,
}

impl Keychain {
    /// The keychain of the state directory `state`, its key made there the
    /// first time, a file only its owner can read: written beside it and
    /// linked into place, so that a build running at once finds it whole or
    /// not at all, and if it was first, uses its key.
    ///
    /// # Errors
    ///
    /// That the key cannot be read or made.
    pub fn open(state: &Path) -> io::Result<Self> {
        fs::create_dir_all(state)?;
        let path = state.join(KEY_FILE);
        if !path.exists() {
            let mut key = [0; 32];
            fs::File::open("/dev/urandom")?.read_exact(&mut key)?;
            let made = state.join(format!(".{KEY_FILE}.{}", std::process::id()));
            fs::OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o600)
                .open(&made)?
                .write_all(&key)?;
            let linked = fs::hard_link(&made, &path);
            fs::remove_file(&made)?;
            match linked {
                // Another build made it first: its key is the key.
                Ok(()) => {}
                Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {}
                Err(error) => return Err(error),
            }
        }
        let mut key = [0; 32];
        fs::File::open(&path)?.read_exact(&mut key)?;
        Ok(Self {
            key,
            files: Mutex::new(HashMap::new()),
        })
    }

    /// The identity of `secret`, and for a file its path as steps see it,
    /// which [`Keychain::host_file`] then knows to be `host`, the file's
    /// place on the host (see [Secrets](#secrets)).
    ///
    /// # Errors
    ///
    /// That the secret's name cannot be a path, or that another file was
    /// given the same path.
    pub fn answer(
        &self,
        secret: &KeychainSecret<'_>,
        host: Option<PathBuf>,
    ) -> Result<KeychainAnswer, String> {
        let mac = hmac(&self.key, secret.contents);
        let identity = format!(
            "{}/{}#{}",
            secret.profile,
            secret.name,
            hex(mac.get(..16).unwrap_or_default())
        );
        let (Some(file), Some(host)) = (secret.file, host) else {
            return Ok(KeychainAnswer {
                identity,
                path: None,
            });
        };
        let plain = |part: &str| {
            !part.is_empty()
                && part != "."
                && part != ".."
                && part
                    .chars()
                    .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
        };
        if !plain(secret.name) || !plain(secret.profile) {
            return Err(format!(
                "pekit gives a secret file a path of its profile and name, which `{}` and `{}` \
                 cannot both be: use letters, digits, `-`, `_` and `.`",
                secret.profile, secret.name
            ));
        }
        let path = format!("{SECRETS}/{}/{}", secret.profile, secret.name);
        let mut files = self
            .files
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        match files.get(&path) {
            Some(known) if *known != host => {
                return Err(format!(
                    "two files are given as `{path}`: `{}` and `{file}`",
                    known.display()
                ));
            }
            Some(_) => {}
            None => {
                files.insert(path.clone(), host);
            }
        }
        Ok(KeychainAnswer {
            identity,
            path: Some(path),
        })
    }

    /// Where on the host the secret file a step sees at `path` is, if the
    /// keychain gave one there.
    #[must_use]
    pub fn host_file(&self, path: &str) -> Option<PathBuf> {
        self.files
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .get(path)
            .cloned()
    }
}

/// HMAC-SHA-256 of `message` under `key` (RFC 2104).
#[must_use]
pub fn hmac(key: &[u8], message: &[u8]) -> [u8; 32] {
    const BLOCK: usize = 64;
    let mut padded = [0u8; BLOCK];
    if key.len() > BLOCK {
        let hashed = Sha256::digest(key);
        padded[..hashed.len()].copy_from_slice(&hashed);
    } else {
        padded[..key.len()].copy_from_slice(key);
    }
    let inner_pad: Vec<u8> = padded.iter().map(|byte| byte ^ 0x36).collect();
    let outer_pad: Vec<u8> = padded.iter().map(|byte| byte ^ 0x5c).collect();
    let inner = Sha256::new()
        .chain_update(&inner_pad)
        .chain_update(message)
        .finalize();
    let outer = Sha256::new()
        .chain_update(&outer_pad)
        .chain_update(inner)
        .finalize();
    let mut mac = [0; 32];
    mac.copy_from_slice(&outer);
    mac
}

/// `bytes` in lower-case hexadecimal.
fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes.iter().fold(String::new(), |mut text, byte| {
        let _ = write!(text, "{byte:02x}");
        text
    })
}
