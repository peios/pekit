//! Releases: what `pekit release` asks of the git repository a workspace
//! is in.
//!
//! # Releases
//!
//! A release is built from a commit, never from a working tree: the
//! repository's `HEAD`, which the workspace must match, every file but the
//! build's own (`pekit-out/`, `.gcsl/`) committed ([`Repository::unclean`]).
//! The workspace's tree at `HEAD` is exported into a directory of its own
//! ([`Repository::export`]), so that no file the repository ignores or
//! does not hold reaches the build, and built there strictly. A release is
//! tagged `NAME@VERSION` at `HEAD` once it is published: a tag is only ever
//! added, and a tag of that name on another commit is an error, found
//! before anything is published ([`Repository::tagged`]).

use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

/// The git repository a workspace is in, at its `HEAD`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Repository {
    /// The repository's top directory.
    pub top: PathBuf,
    /// The workspace's directory relative to it, `/` separated, empty for
    /// the top itself.
    pub prefix: String,
    /// The commit `HEAD` names, in full.
    pub head: String,
}

impl Repository {
    /// The repository the workspace at `root` is in.
    ///
    /// # Errors
    ///
    /// That it is in none, or the repository has no commit.
    pub fn of(root: &Path) -> Result<Self, String> {
        let top = git(root, &["rev-parse", "--show-toplevel"]).map_err(|_| {
            format!(
                "`pekit release` builds from a git commit, and `{}` is in no git repository",
                root.display()
            )
        })?;
        let prefix = git(root, &["rev-parse", "--show-prefix"])?;
        let head = git(root, &["rev-parse", "--verify", "HEAD^{commit}"]).map_err(|_| {
            "`pekit release` builds from a git commit, and there is none yet".to_owned()
        })?;
        Ok(Self {
            top: PathBuf::from(top),
            prefix: prefix.trim_end_matches('/').to_owned(),
            head,
        })
    }

    /// The workspace's files at `root` that differ from `HEAD`: changed,
    /// added, removed or not committed, as `git status` shows each, its path
    /// from the repository's top, but the build's own, under `pekit-out/`
    /// and `.gcsl/`.
    ///
    /// # Errors
    ///
    /// That git fails.
    pub fn unclean(&self, root: &Path) -> Result<Vec<String>, String> {
        let status = git_untrimmed(
            root,
            &[
                "status",
                "--porcelain=v1",
                "--untracked-files=all",
                "--",
                ".",
                ":(exclude)pekit-out",
                ":(exclude).gcsl",
            ],
        )?;
        Ok(status
            .lines()
            .filter(|line| !line.is_empty())
            .map(str::to_owned)
            .collect())
    }

    /// Exports the workspace's tree at `HEAD` into `into`, which must be
    /// empty or not there.
    ///
    /// # Errors
    ///
    /// That git fails, or the tree cannot be written.
    pub fn export(&self, into: &Path) -> Result<(), String> {
        std::fs::create_dir_all(into)
            .map_err(|error| format!("cannot make `{}`: {error}", into.display()))?;
        let tree = if self.prefix.is_empty() {
            self.head.clone()
        } else {
            format!("{}:{}", self.head, self.prefix)
        };
        let mut child = Command::new("git")
            .arg("-C")
            .arg(&self.top)
            .args(["archive", "--format=tar", &tree])
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .map_err(|error| format!("cannot run `git archive`: {error}"))?;
        let unpacked = match child.stdout.take() {
            Some(out) => tar::Archive::new(out)
                .unpack(into)
                .map_err(|error| format!("cannot write `{}`: {error}", into.display())),
            None => Err("`git archive` gave no output".to_owned()),
        };
        let finished = child
            .wait_with_output()
            .map_err(|error| format!("cannot run `git archive`: {error}"))?;
        if !finished.status.success() {
            return Err(format!(
                "`git archive {tree}` failed: {}",
                String::from_utf8_lossy(&finished.stderr).trim()
            ));
        }
        unpacked
    }

    /// The commit the tag `tag` is on, if there is a tag of that name.
    ///
    /// # Errors
    ///
    /// That git fails other than to find no such tag.
    pub fn tagged(&self, tag: &str) -> Result<Option<String>, String> {
        let ran = Command::new("git")
            .arg("-C")
            .arg(&self.top)
            .args([
                "rev-parse",
                "-q",
                "--verify",
                &format!("refs/tags/{tag}^{{commit}}"),
            ])
            .output()
            .map_err(|error| format!("cannot run `git`: {error}"))?;
        match ran.status.code() {
            Some(0) => Ok(Some(String::from_utf8_lossy(&ran.stdout).trim().to_owned())),
            Some(1) => Ok(None),
            _ => Err(format!(
                "cannot read the tag `{tag}`: {}",
                String::from_utf8_lossy(&ran.stderr).trim()
            )),
        }
    }

    /// Adds the tag `tag` at `HEAD`.
    ///
    /// # Errors
    ///
    /// That git fails.
    pub fn tag(&self, tag: &str) -> Result<(), String> {
        git(&self.top, &["tag", tag, &self.head]).map(|_| ())
    }
}

/// What `git` with `args` prints, run in `dir`, trimmed.
fn git(dir: &Path, args: &[&str]) -> Result<String, String> {
    git_untrimmed(dir, args).map(|out| out.trim().to_owned())
}

/// What `git` with `args` prints, run in `dir`, as it prints it.
fn git_untrimmed(dir: &Path, args: &[&str]) -> Result<String, String> {
    let ran = Command::new("git")
        .arg("-C")
        .arg(dir)
        .args(args)
        .output()
        .map_err(|error| format!("cannot run `git`: {error}"))?;
    if !ran.status.success() {
        return Err(format!(
            "`git {}` failed: {}",
            args.join(" "),
            String::from_utf8_lossy(&ran.stderr).trim()
        ));
    }
    Ok(String::from_utf8_lossy(&ran.stdout).into_owned())
}
