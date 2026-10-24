//! The host snapshot: the host's own tools, which steps run with until a
//! pinned build environment is given.
//!
//! # The host snapshot
//!
//! A hermetic step sees only what it is given. Until the libraries give a
//! pinned build environment (layer D), there is nothing to give it a shell
//! or a compiler from but the host. So every step's sandbox binds the host's
//! system directories, `/usr`, `/bin`, `/lib`, `/lib64` and `/sbin`, and of
//! `/etc` only what programs there need to start, `/etc/alternatives`, where
//! Debian's `cc` and the like lead, and `/etc/ld.so.cache`, read-only where
//! they are: the *host snapshot*. `/usr/local` is hidden, since no package
//! manager manages it, and a step whose `exec` sets no `PATH` runs with the
//! package manager's directories as its `PATH`. A build that uses the
//! snapshot is not hermetic, and pekit says so.
//!
//! Those directories are inputs of every step, and too large to hash on
//! every build. Instead the snapshot is identified by its *fingerprint*: a
//! digest of the host's `/etc/os-release`, of its package manager's
//! database, which changes whenever a package is installed, upgraded or
//! removed, and of what the snapshot's parts of `/etc` say, which an
//! administrator can change without a package: each alternative's target,
//! and the linker's cache. The fingerprint is part of the platform every receipt names, so
//! a step recorded before the host's compiler was upgraded is not found
//! after it. A host whose package database pekit does not know has no
//! fingerprint, and its steps are recorded but never found in the store:
//! nothing would say when its tools changed.

use std::fs;
use std::path::{Path, PathBuf};

use sha2::{Digest as _, Sha256};

use crate::sandbox::HostTools;
use crate::store::Digest;

/// The host's system directories and files every step binds (see [The host
/// snapshot](#the-host-snapshot)).
const SYSTEM_PATHS: [&str; 7] = [
    "/usr",
    "/bin",
    "/lib",
    "/lib64",
    "/sbin",
    "/etc/alternatives",
    "/etc/ld.so.cache",
];

/// What of the bound directories no package manager manages, which steps
/// therefore never see: its changes would change no fingerprint.
const UNMANAGED: [&str; 1] = ["/usr/local"];

/// The `PATH` a step runs with if its `exec` sets none: the package
/// manager's directories alone.
const DEFAULT_PATH: &str = "/usr/bin:/bin:/usr/sbin:/sbin";

/// Debian's alternatives, each a link to the program it chooses.
const ALTERNATIVES: &str = "/etc/alternatives";

/// The dynamic linker's cache.
const LINKER_CACHE: &str = "/etc/ld.so.cache";

/// The package databases a fingerprint is taken from, each a file whose
/// contents change with the host's packages: dpkg's, rpm's in either of its
/// homes, and apk's.
const PACKAGE_FILES: [&str; 4] = [
    "/var/lib/dpkg/status",
    "/var/lib/rpm/rpmdb.sqlite",
    "/usr/lib/sysimage/rpm/rpmdb.sqlite",
    "/lib/apk/db/installed",
];

/// Pacman's database, a directory of one entry per installed package and
/// version.
const PACMAN_DIR: &str = "/var/lib/pacman/local";

/// The host snapshot (see [The host snapshot](#the-host-snapshot)).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HostSnapshot {
    /// The system directories and files there are, which every step binds.
    pub dirs: Vec<PathBuf>,
    /// Its fingerprint, if the host's package database is one pekit knows.
    pub fingerprint: Option<Digest>,
}

impl HostSnapshot {
    /// The snapshot of the host pekit runs on.
    #[must_use]
    pub fn of_host() -> Self {
        Self::of_root(Path::new("/"))
    }

    /// The snapshot of the system whose root is `root`: what
    /// [`HostSnapshot::of_host`] reads, for a test to point elsewhere.
    #[must_use]
    pub fn of_root(root: &Path) -> Self {
        let at = |path: &str| root.join(path.trim_start_matches('/'));
        let dirs = SYSTEM_PATHS
            .iter()
            .filter(|path| at(path).exists())
            .map(PathBuf::from)
            .collect();
        Self {
            dirs,
            fingerprint: fingerprint(&at),
        }
    }

    /// The host tools every step borrows: its system paths bound, what no
    /// package manager manages hidden, and its `PATH`.
    #[must_use]
    pub fn tools(&self) -> HostTools {
        HostTools {
            bind: self.dirs.clone(),
            hide: UNMANAGED
                .iter()
                .filter(|path| Path::new(path).is_dir())
                .map(PathBuf::from)
                .collect(),
            path: Some(DEFAULT_PATH.to_owned()),
        }
    }

    /// The platform receipts name for steps run with this snapshot on the
    /// machine `machine`, `x86_64-linux`: the machine and the fingerprint.
    #[must_use]
    pub fn platform(&self, machine: &str) -> String {
        match &self.fingerprint {
            Some(fingerprint) => format!("{machine} host-snapshot:{}", fingerprint.hex()),
            None => format!("{machine} host-snapshot:unknown"),
        }
    }
}

/// The fingerprint of the system whose paths `at` gives, if its package
/// database is one pekit knows.
fn fingerprint(at: &dyn Fn(&str) -> PathBuf) -> Option<Digest> {
    let mut hasher = Sha256::new();
    hasher.update(b"pekit-host-snapshot-1\0");
    hasher.update(fs::read(at("/etc/os-release")).unwrap_or_default());
    let mut found = false;
    for file in PACKAGE_FILES {
        if let Ok(bytes) = fs::read(at(file)) {
            hasher.update(file.as_bytes());
            hasher.update((bytes.len() as u64).to_le_bytes());
            hasher.update(&bytes);
            found = true;
        }
    }
    if let Ok(entries) = fs::read_dir(at(PACMAN_DIR)) {
        let mut names: Vec<String> = entries
            .flatten()
            .map(|entry| entry.file_name().to_string_lossy().into_owned())
            .collect();
        names.sort();
        hasher.update(PACMAN_DIR.as_bytes());
        for name in names {
            hasher.update(name.as_bytes());
            hasher.update(b"\0");
        }
        found = true;
    }
    if !found {
        return None;
    }
    hasher.update(LINKER_CACHE.as_bytes());
    hasher.update(fs::read(at(LINKER_CACHE)).unwrap_or_default());
    if let Ok(entries) = fs::read_dir(at(ALTERNATIVES)) {
        let mut links: Vec<(String, String)> = entries
            .flatten()
            .map(|entry| {
                let target = fs::read_link(entry.path()).unwrap_or_default();
                (
                    entry.file_name().to_string_lossy().into_owned(),
                    target.to_string_lossy().into_owned(),
                )
            })
            .collect();
        links.sort();
        hasher.update(ALTERNATIVES.as_bytes());
        for (name, target) in links {
            hasher.update(name.as_bytes());
            hasher.update(b"\0");
            hasher.update(target.as_bytes());
            hasher.update(b"\0");
        }
    }
    Some(Digest::from_hasher(hasher))
}
