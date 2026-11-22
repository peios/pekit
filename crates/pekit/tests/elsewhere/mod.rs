//! What the tests of two file systems share: a temporary directory on
//! another file system than a given one.

use std::os::unix::fs::MetadataExt as _;
use std::path::Path;

/// A new temporary directory on another file system than `than`'s, if this
/// machine has one among the places tests may write: the test build's own
/// temporary directory, `/dev/shm`, and the system's temporary directory.
pub fn elsewhere(than: &Path) -> Option<tempfile::TempDir> {
    let device = |path: &Path| std::fs::metadata(path).ok().map(|meta| meta.dev());
    let here = device(than)?;
    [
        Path::new(env!("CARGO_TARGET_TMPDIR")).to_path_buf(),
        Path::new("/dev/shm").to_path_buf(),
        std::env::temp_dir(),
    ]
    .into_iter()
    .filter(|place| device(place).is_some_and(|there| there != here))
    .find_map(|place| tempfile::tempdir_in(place).ok())
}
