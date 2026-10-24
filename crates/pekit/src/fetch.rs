//! Fetching: pekit's answer to GCSL's `fetch`.
//!
//! # Fetching
//!
//! `fetch(url, sha256)` is the one thing in a build that touches the network
//! (see `design/libraries.md`, L2). Pekit answers it itself, never running
//! GCSL's `curl`: the store keeps every file by its SHA-256 digest, so a
//! pinned file it holds is found with no network at all, on any machine that
//! shares the store; else it is downloaded on the host, hashed as it is
//! streamed into the store, and kept only if its digest is the pin. Its path
//! is `/pekit-out/.fetch/<sha256>/<name>`, fixed by its contents, its name the
//! URL's last segment as GCSL names it ([`gcsl::eval::fetched_name`]), so
//! every receipt that holds it is stable. Its errors are GCSL's own: a pin
//! that is no digest, a file that is not the one pinned, and an unpinned
//! fetch, which downloads only to give the digest it found.
//!
//! A hermetic step cannot fetch: its sandbox has no network, and a file it
//! fetched would be neither its input nor its output. A toolchain is fetched
//! in a parameter default, which runs at the call, outside the step.

use std::time::Duration;

use gcsl::eval::fetched_name;

use crate::sandbox::OUTPUTS;
use crate::store::{Digest, FileEntry, Store};

/// Why a hermetic step's `fetch` is refused (see [Fetching](#fetching)).
pub const IN_A_STEP: &str = "`fetch` runs outside hermetic steps: a step has no network; fetch \
                             in a parameter default, or in the caller, and pass the file in";

/// How long a download may go without a byte before it fails.
const STALL: Duration = Duration::from_secs(60);

/// The path, as GCSL code names it, of the file fetched from `url` whose
/// digest is `digest` (see [Fetching](#fetching)).
#[must_use]
pub fn fetched_path(url: &str, digest: &Digest) -> String {
    format!("{OUTPUTS}/.fetch/{}/{}", digest.hex(), fetched_name(url))
}

/// The SHA-256 digest a pin names, or `None` for no pin: as GCSL reads one.
///
/// # Errors
///
/// That the pin is no digest, 64 hex digits.
fn pin_of(url: &str, sha256: &str) -> Result<Option<Digest>, String> {
    let pin = sha256.trim().to_ascii_lowercase();
    if pin.is_empty() {
        return Ok(None);
    }
    Digest::from_hex(&pin).map(Some).ok_or_else(|| {
        format!(
            "`fetch` of `{url}` is pinned by `{sha256}`, which is no SHA-256 digest: 64 \
             hexadecimal digits"
        )
    })
}

/// The file at `url` pinned by `sha256`, from `store`, downloaded into it
/// if it does not hold it (see [Fetching](#fetching)): what the store keeps
/// of it, and the note of the error if it fails.
///
/// # Errors
///
/// GCSL's errors of `fetch`, each a message and its note, if it has one.
pub fn fetch(
    store: &Store,
    url: &str,
    sha256: &str,
) -> Result<FileEntry, (String, Option<String>)> {
    let pin = pin_of(url, sha256).map_err(|message| (message, None))?;
    if let Some(entry) = pin.as_ref().and_then(|pin| store.entry(pin)) {
        return Ok(entry);
    }
    let entry = download(store, url).map_err(|reason| {
        (
            format!("`fetch` could not download `{url}`: {reason}"),
            None,
        )
    })?;
    let found = entry.digest.hex();
    match pin {
        None => Err((
            format!("`fetch` of `{url}` is not pinned: its sha256 is `{found}`"),
            Some(format!("pin it: `fetch(\"{url}\", sha256 = \"{found}\")`")),
        )),
        Some(pin) if pin != entry.digest => Err((
            format!(
                "`fetch` of `{url}` gave a file whose sha256 is `{found}`, not `{}`",
                pin.hex()
            ),
            Some(
                "the file behind the URL is not the one pinned, so nothing is built from it"
                    .to_owned(),
            ),
        )),
        Some(_) => Ok(entry),
    }
}

/// Downloads `url` into `store`.
fn download(store: &Store, url: &str) -> Result<FileEntry, String> {
    let agent: ureq::Agent = ureq::Agent::config_builder()
        .timeout_recv_body(Some(STALL))
        .timeout_connect(Some(STALL))
        .build()
        .into();
    let response = agent.get(url).call().map_err(|error| error.to_string())?;
    let mut body = response.into_body();
    let mut reader = body.as_reader();
    store
        .put_reader(&mut reader, false)
        .map_err(|error| error.to_string())
}
