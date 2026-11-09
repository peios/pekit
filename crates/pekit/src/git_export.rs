//! Exports: a git tree unpacked into a directory, as `pekit lock` puts a
//! library into a workspace and `pekit release` a commit into a directory
//! of its own.
//!
//! # Exports
//!
//! A tree is exported by unpacking the tar `git archive` writes, read from
//! its standard output as it is written. Git ends the archive with two
//! zeroed blocks and pads it to a whole record of 10240 bytes; the
//! unpacking stops at the first zeroed block, up to 10 KiB short of the end.
//! The rest is read and discarded before git is waited for: closed early,
//! the pipe would kill a git still writing it with SIGPIPE, and an export
//! unpacked whole would fail, git having said nothing.
//!
//! The rest is read when the unpacking fails too, so that git fails only
//! of itself: a git that failed, as on a commit it lacks or a corrupt
//! repository, is what is reported, with its status and what it wrote to
//! its standard error; else the unpacking's failure. An unpacking that fails
//! early thus still reads the whole archive.

use std::fmt;
use std::io;
use std::path::Path;
use std::process::{Command, ExitStatus, Stdio};

/// Why a tree could not be exported.
#[derive(Debug)]
pub enum ExportError {
    /// Git could not be run.
    Run(io::Error),
    /// Git failed: its status, and what it wrote to its standard error,
    /// trimmed.
    Failed {
        /// How it ended.
        status: ExitStatus,
        /// What it said, trimmed, perhaps nothing.
        stderr: String,
    },
    /// The archive could not be unpacked.
    Unpack(io::Error),
}

impl fmt::Display for ExportError {
    /// What befell git, to follow the command: `failed (exit status: 128):
    /// fatal: ...`, or `failed (signal: 13 (SIGPIPE)), saying nothing`.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Run(error) => write!(f, "could not be run: {error}"),
            Self::Failed { status, stderr } if stderr.is_empty() => {
                write!(f, "failed ({status}), saying nothing")
            }
            Self::Failed { status, stderr } => write!(f, "failed ({status}): {stderr}"),
            Self::Unpack(error) => write!(f, "could not be unpacked: {error}"),
        }
    }
}

/// Unpacks into `into` the tar that `git`, a `git archive --format=tar`
/// command, writes (see [Exports](self#exports)).
///
/// # Errors
///
/// That git cannot be run or fails, or the archive cannot be unpacked.
pub fn export(git: &mut Command, into: &Path) -> Result<(), ExportError> {
    let mut child = git
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(ExportError::Run)?;
    let unpacked = match child.stdout.take() {
        Some(out) => {
            let mut archive = tar::Archive::new(out);
            let unpacked = archive.unpack(into);
            // What follows the end of the archive: its padding, or, if the
            // unpacking failed, the rest of it.
            let _ = io::copy(&mut archive.into_inner(), &mut io::sink());
            unpacked
        }
        None => Ok(()),
    };
    let finished = child.wait_with_output().map_err(ExportError::Run)?;
    if !finished.status.success() {
        return Err(ExportError::Failed {
            status: finished.status,
            stderr: String::from_utf8_lossy(&finished.stderr).trim().to_owned(),
        });
    }
    unpacked.map_err(ExportError::Unpack)
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::os::unix::process::ExitStatusExt as _;

    use super::*;

    /// Runs `git` with `args` in `dir`, as no one's configuration says.
    fn git(dir: &Path, args: &[&str]) {
        let ran = Command::new("git")
            .args([
                "-c",
                "user.name=pekit",
                "-c",
                "user.email=pekit@example.org",
            ])
            .args(["-c", "commit.gpgsign=false"])
            .args(args)
            .current_dir(dir)
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .output()
            .unwrap();
        assert!(ran.status.success(), "{ran:?}");
    }

    /// A repository in `dir`'s `repo` holding `a` and `b/c`, committed.
    fn repository(dir: &Path) -> std::path::PathBuf {
        let repo = dir.join("repo");
        fs::create_dir_all(repo.join("b")).unwrap();
        fs::write(repo.join("a"), "a\n").unwrap();
        fs::write(repo.join("b/c"), "c\n").unwrap();
        git(&repo, &["init", "-q", "-b", "main"]);
        git(&repo, &["add", "-A"]);
        git(&repo, &["commit", "-qm", "one"]);
        repo
    }

    #[test]
    fn an_export_reads_what_follows_the_end_of_the_archive() {
        // A tar ended, then more padding than a pipe holds: a git that has
        // not finished writing when the unpacking stops.
        let dir = tempfile::tempdir().unwrap();
        let tar = dir.path().join("a.tar");
        let mut builder = tar::Builder::new(fs::File::create(&tar).unwrap());
        let mut header = tar::Header::new_gnu();
        header.set_size(2);
        header.set_mode(0o644);
        builder.append_data(&mut header, "a", &b"a\n"[..]).unwrap();
        builder.into_inner().unwrap();
        let into = dir.path().join("into");
        let exported = export(
            Command::new("sh")
                .args(["-c", "cat \"$1\" && exec head -c 1048576 /dev/zero", "sh"])
                .arg(&tar),
            &into,
        );
        assert!(exported.is_ok(), "{exported:?}");
        assert_eq!(fs::read_to_string(into.join("a")).unwrap(), "a\n");
    }

    #[test]
    fn an_export_unpacks_a_commit_s_tree() {
        let dir = tempfile::tempdir().unwrap();
        let repo = repository(dir.path());
        let into = dir.path().join("into");
        export(
            Command::new("git")
                .arg("-C")
                .arg(&repo)
                .args(["archive", "--format=tar", "HEAD"]),
            &into,
        )
        .unwrap();
        assert_eq!(fs::read_to_string(into.join("a")).unwrap(), "a\n");
        assert_eq!(fs::read_to_string(into.join("b/c")).unwrap(), "c\n");
    }

    #[test]
    fn a_git_that_fails_is_reported_with_its_status_and_what_it_said() {
        let dir = tempfile::tempdir().unwrap();
        let repo = repository(dir.path());
        let error = export(
            Command::new("git").arg("-C").arg(&repo).args([
                "archive",
                "--format=tar",
                "no-such-commit",
            ]),
            &dir.path().join("into"),
        )
        .unwrap_err();
        let shown = error.to_string();
        assert!(
            matches!(&error, ExportError::Failed { status, .. } if status.code() == Some(128)),
            "{error:?}"
        );
        assert!(shown.starts_with("failed (exit status: 128): "), "{shown}");
        assert!(shown.contains("no-such-commit"), "{shown}");
    }

    #[test]
    fn a_corrupt_repository_is_git_s_failure_not_the_unpacking_s() {
        // A blob gone: git fails partway through the archive, which is cut
        // short.
        let dir = tempfile::tempdir().unwrap();
        let repo = repository(dir.path());
        let blob = Command::new("git")
            .arg("-C")
            .arg(&repo)
            .args(["rev-parse", "HEAD:b/c"])
            .output()
            .unwrap();
        let blob = String::from_utf8(blob.stdout).unwrap();
        let blob = blob.trim();
        fs::remove_file(repo.join(".git/objects").join(&blob[..2]).join(&blob[2..])).unwrap();
        let error = export(
            Command::new("git")
                .arg("-C")
                .arg(&repo)
                .args(["archive", "--format=tar", "HEAD"]),
            &dir.path().join("into"),
        )
        .unwrap_err();
        let ExportError::Failed { status, stderr } = &error else {
            panic!("{error:?}");
        };
        assert!(!status.success());
        assert!(stderr.contains(blob), "{stderr}");
    }

    #[test]
    fn a_git_killed_saying_nothing_is_reported_by_its_signal() {
        let dir = tempfile::tempdir().unwrap();
        let error = export(
            Command::new("sh").args(["-c", "kill -PIPE $$"]),
            &dir.path().join("into"),
        )
        .unwrap_err();
        assert!(
            matches!(&error, ExportError::Failed { status, .. } if status.signal() == Some(13)),
            "{error:?}"
        );
        assert_eq!(
            error.to_string(),
            "failed (signal: 13 (SIGPIPE)), saying nothing"
        );
    }
}
