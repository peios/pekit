//! The lock: the libraries a workspace builds with, pinned by commit.
//!
//! # The lock
//!
//! `pekit.lock`, at a workspace's root, pins what a build would otherwise
//! take from wherever it found it: each library the workspace imports, and
//! each a library imports, by its import path, the git repository and
//! directory it is from, the commit, and its tree's digest
//! ([`Lock::libraries`]). `pekit lock` writes it ([`lock`]): it finds the
//! libraries imported, fetches each that is not pinned yet, or each, with
//! `--update`, from its *source*, and exports it at the commit its source's
//! revision names into `.gcsl/libraries/<path>/`, the library root the
//! workspace's own libraries are found in. Every other command puts back,
//! from the commits pinned, a pinned library that is not there
//! ([`restore`]), and says where one there differs from its pin; a strict
//! build fails on that, on a library imported that the lock does not pin,
//! and on `$GCSL_LIBRARY_PATH`, which would find libraries elsewhere
//! ([`check_strict`]).
//!
//! A library's source is the first of the lock's sources ([`Source`]) whose
//! prefix its path is under: a git repository, the directory in it that the
//! prefix's libraries are under, and the revision to follow. A library no
//! source names follows the convention of Go's modules: `HOST/OWNER/REPO` is
//! the repository `https://HOST/OWNER/REPO`, its libraries under its root,
//! at `HEAD`; the source is added to the lock, which may be edited to point
//! a prefix elsewhere.
//!
//! Repositories are kept bare in the cache directory's `git/`, fetched only
//! when a revision is resolved or a commit pinned is missing, so that a
//! workspace whose libraries are pinned and cached builds offline. Git is
//! run as the `git` program.

use std::collections::BTreeSet;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

use serde::{Deserialize, Serialize};

use crate::layers::remove_tree;
use crate::store::{Digest, Store};

/// The lock's file, at the workspace's root.
pub const LOCK_FILE: &str = "pekit.lock";

/// The library root under the workspace's root that locked libraries are
/// exported into.
pub const LIBRARIES_DIR: &str = ".gcsl/libraries";

/// The lock (see [The lock](#the-lock)).
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Lock {
    /// Where libraries come from, by prefix of their paths, first match
    /// first.
    #[serde(default)]
    pub sources: Vec<Source>,
    /// Each library pinned, by its path, sorted.
    #[serde(default)]
    pub libraries: Vec<Pinned>,
}

/// Where the libraries under a prefix of import paths come from.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Source {
    /// The prefix, `github.com/gcslib`: its libraries are `PREFIX/REST`.
    pub prefix: String,
    /// The git repository, a URL or a path, as `git` reads it.
    pub git: String,
    /// The directory in the repository holding `REST`, empty for its root.
    #[serde(default)]
    pub dir: String,
    /// The revision followed: a branch, a tag or a commit.
    #[serde(default = "head")]
    pub rev: String,
}

/// The revision a source follows when the lock names none.
fn head() -> String {
    "HEAD".to_owned()
}

/// A library pinned.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Pinned {
    /// Its import path, `github.com/gcslib/gcc`.
    pub path: String,
    /// The git repository it is from.
    pub git: String,
    /// Its directory in the repository, empty for the repository's root.
    pub dir: String,
    /// The commit, in full.
    pub commit: String,
    /// The digest of its tree as exported (see [The store](crate::store)).
    pub tree: String,
}

/// Why the lock could not be read, written or met.
#[derive(Debug)]
pub enum LockError {
    /// The lock, a library or a repository could not be read or written.
    Io(String),
    /// Git failed: what was run, and what it said.
    Git(String),
    /// A strict build's libraries are not as the lock pins them.
    Strict(Vec<String>),
}

impl LockError {
    /// The error as the `pekit` command shows it.
    #[must_use]
    pub fn render(&self) -> String {
        match self {
            LockError::Io(message) | LockError::Git(message) => format!("error: {message}\n"),
            LockError::Strict(problems) => {
                let mut shown = String::from(
                    "error: a strict build uses the libraries `pekit.lock` pins, and only them:\n",
                );
                for problem in problems {
                    shown.push_str("  ");
                    shown.push_str(problem);
                    shown.push('\n');
                }
                shown
            }
        }
    }
}

/// `error` reading or writing `path`, as a lock error.
fn io_error(path: &Path, error: &io::Error) -> LockError {
    LockError::Io(format!("cannot use `{}`: {error}", path.display()))
}

impl Lock {
    /// The lock of the workspace at `root`, `None` if it has none.
    ///
    /// # Errors
    ///
    /// That it cannot be read, or is no lock.
    pub fn read(root: &Path) -> Result<Option<Lock>, LockError> {
        let path = root.join(LOCK_FILE);
        let text = match fs::read_to_string(&path) {
            Ok(text) => text,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(None),
            Err(error) => return Err(io_error(&path, &error)),
        };
        serde_json::from_str(&text)
            .map(Some)
            .map_err(|error| LockError::Io(format!("`{}` is no lock: {error}", path.display())))
    }

    /// Writes the lock into the workspace at `root`, through a temporary
    /// file, so that it is whole or as it was.
    ///
    /// # Errors
    ///
    /// That it cannot be written.
    pub fn write(&self, root: &Path) -> Result<(), LockError> {
        let path = root.join(LOCK_FILE);
        let mut text =
            serde_json::to_string_pretty(self).map_err(|error| LockError::Io(error.to_string()))?;
        text.push('\n');
        let temp = root.join(format!(".{LOCK_FILE}.{}", crate::sweep::token()));
        fs::write(&temp, text).map_err(|error| io_error(&temp, &error))?;
        fs::rename(&temp, &path).map_err(|error| io_error(&path, &error))
    }

    /// The pin of the library `path`, if it has one.
    #[must_use]
    pub fn pinned(&self, path: &str) -> Option<&Pinned> {
        self.libraries.iter().find(|pinned| pinned.path == path)
    }

    /// The source of the library `path`: the first source whose prefix it is
    /// under, and the directory in its repository; failing one, Go's
    /// convention's, which is added to the sources.
    fn source_of(&mut self, path: &str) -> (Source, String) {
        let under = |prefix: &str| {
            path == prefix
                || path
                    .strip_prefix(prefix)
                    .is_some_and(|rest| rest.starts_with('/'))
        };
        let found = self
            .sources
            .iter()
            .find(|source| under(&source.prefix))
            .cloned();
        let source = found.unwrap_or_else(|| {
            let prefix: Vec<&str> = path.splitn(4, '/').take(3).collect();
            let prefix = prefix.join("/");
            let source = Source {
                git: format!("https://{prefix}"),
                prefix,
                dir: String::new(),
                rev: head(),
            };
            self.sources.push(source.clone());
            source
        });
        let rest = path
            .strip_prefix(source.prefix.as_str())
            .unwrap_or_default()
            .trim_start_matches('/');
        let dir = [source.dir.as_str(), rest]
            .into_iter()
            .filter(|part| !part.is_empty())
            .collect::<Vec<_>>()
            .join("/");
        (source, dir)
    }
}

/// What `pekit lock` did: each library pinned anew, by its path and the
/// commit, and each unpinned, as no longer imported.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Locked {
    /// The libraries pinned anew, with their commits.
    pub pinned: Vec<(String, String)>,
    /// The libraries no longer imported, unpinned.
    pub dropped: Vec<String>,
}

/// Locks the libraries of the workspace at `root`, its repositories kept in
/// `cache`'s `git/` and its trees hashed into `store` (see [The
/// lock](#the-lock)): each imported and not pinned, or each, if `update`
/// says so, fetched and pinned at its source's revision, and each pinned and
/// no longer imported unpinned, its directory, which the lock put there,
/// removed.
///
/// # Errors
///
/// That the lock or a library cannot be read or written, or git fails.
pub fn lock(
    root: &Path,
    (cache, store): (&Path, &Store),
    update: bool,
) -> Result<Locked, LockError> {
    let mut lock = Lock::read(root)?.unwrap_or_default();
    let mut locked = Locked::default();
    let mut wanted: Vec<String> = workspace_imports(root)?;
    let mut seen = BTreeSet::new();
    let mut pins = Vec::new();
    while let Some(path) = wanted.pop() {
        if !seen.insert(path.clone()) {
            continue;
        }
        let pin = match lock.pinned(&path) {
            Some(pin) if !update => {
                let pin = pin.clone();
                export(root, cache, &pin)?;
                pin
            }
            _ => {
                let (source, dir) = lock.source_of(&path);
                let repo = fetched(cache, &source.git)?;
                let commit = git(
                    &repo,
                    &[
                        "rev-parse",
                        "--verify",
                        &format!("{}^{{commit}}", source.rev),
                    ],
                )?;
                let mut pin = Pinned {
                    path: path.clone(),
                    git: source.git.clone(),
                    dir,
                    commit,
                    tree: String::new(),
                };
                export(root, cache, &pin)?;
                pin.tree = tree_of(root, store, &path)?.hex();
                if lock.pinned(&path) != Some(&pin) {
                    locked.pinned.push((path.clone(), pin.commit.clone()));
                }
                pin
            }
        };
        wanted.extend(library_imports(&root.join(LIBRARIES_DIR).join(&path))?);
        pins.push(pin);
    }
    for old in &lock.libraries {
        if !seen.contains(&old.path) {
            let dir = root.join(LIBRARIES_DIR).join(&old.path);
            remove_tree(&dir).map_err(|error| io_error(&dir, &error))?;
            locked.dropped.push(old.path.clone());
        }
    }
    pins.sort_by(|a, b| a.path.cmp(&b.path));
    locked.pinned.sort();
    lock.libraries = pins;
    lock.write(root)?;
    Ok(locked)
}

/// Puts back each library the lock of the workspace at `root` pins that is
/// not in `.gcsl/libraries/`, from its commit, its repository kept in
/// `cache`'s `git/` (see [The lock](#the-lock)); and says, of each there,
/// whether it differs from its pin, by `store`'s digest of its tree. Nothing
/// for a workspace with no lock.
///
/// # Errors
///
/// That the lock cannot be read, or a library cannot be put back.
pub fn restore(root: &Path, (cache, store): (&Path, &Store)) -> Result<Vec<String>, LockError> {
    let Some(lock) = Lock::read(root)? else {
        return Ok(Vec::new());
    };
    let mut differ = Vec::new();
    for pin in &lock.libraries {
        let dir = root.join(LIBRARIES_DIR).join(&pin.path);
        if !dir.is_dir() {
            export(root, cache, pin)?;
            continue;
        }
        if tree_of(root, store, &pin.path)?.hex() != pin.tree {
            differ.push(format!(
                "`{}/{}` differs from the commit `pekit.lock` pins, {}",
                LIBRARIES_DIR, pin.path, pin.commit
            ));
        }
    }
    Ok(differ)
}

/// What makes a strict build's libraries other than the lock of the
/// workspace at `root` pins (see [The lock](#the-lock)): no lock; a library
/// differing from its pin, as `differ` says ([`restore`]); one of
/// `libraries`, the import paths of the libraries the workspace loaded,
/// that the lock does not pin; or `$GCSL_LIBRARY_PATH`. Empty if none.
///
/// # Errors
///
/// That the lock cannot be read.
pub fn check_strict(
    root: &Path,
    libraries: &[String],
    differ: Vec<String>,
) -> Result<Vec<String>, LockError> {
    let mut problems = differ;
    match Lock::read(root)? {
        None if !libraries.is_empty() => {
            problems.push("the workspace has no `pekit.lock`: run `pekit lock`".to_owned());
        }
        None => {}
        Some(lock) => {
            for library in libraries {
                if lock.pinned(library).is_none() {
                    problems.push(format!(
                        "`{library}` is imported and not pinned: run `pekit lock`"
                    ));
                }
            }
        }
    }
    if std::env::var_os("GCSL_LIBRARY_PATH").is_some_and(|path| !path.is_empty()) {
        problems.push(
            "`$GCSL_LIBRARY_PATH` is set, which would find libraries elsewhere: unset it"
                .to_owned(),
        );
    }
    Ok(problems)
}

/// The import paths of the libraries the `.gcsl` files of the workspace at
/// `root` import, sorted.
fn workspace_imports(root: &Path) -> Result<Vec<String>, LockError> {
    let sources =
        gcsl_disk::read_workspace(root, None).map_err(|error| LockError::Io(error.to_string()))?;
    let mut imports: Vec<String> = sources
        .relative
        .iter()
        .filter(|source| !source.name().starts_with(gcsl::workspace::LIBRARIES))
        .flat_map(gcsl::workspace::library_imports)
        .collect();
    imports.sort();
    imports.dedup();
    Ok(imports)
}

/// The import paths of the libraries the library at `dir`, its `.gcsl` files
/// directly in it, imports.
fn library_imports(dir: &Path) -> Result<Vec<String>, LockError> {
    let mut imports = Vec::new();
    let entries = fs::read_dir(dir).map_err(|error| io_error(dir, &error))?;
    for entry in entries.flatten() {
        let path = entry.path();
        if path.extension().is_none_or(|extension| extension != "gcsl") || !path.is_file() {
            continue;
        }
        let text = fs::read_to_string(&path).map_err(|error| io_error(&path, &error))?;
        let name = path.to_string_lossy().into_owned();
        let source = gcsl::SourceFile::new(name, text)
            .map_err(|error| LockError::Io(format!("cannot read `{}`: {error}", path.display())))?;
        imports.extend(gcsl::workspace::library_imports(&source));
    }
    Ok(imports)
}

/// The digest of the tree of the library `path` as exported under the
/// workspace at `root`, put into `store`.
fn tree_of(root: &Path, store: &Store, path: &str) -> Result<Digest, LockError> {
    let dir = root.join(LIBRARIES_DIR).join(path);
    store.put_tree(&dir).map_err(|error| io_error(&dir, &error))
}

/// Exports the library `pin` at its commit into the workspace at `root`,
/// replacing what is there, its repository kept in `cache`'s `git/` and
/// fetched if it lacks the commit.
fn export(root: &Path, cache: &Path, pin: &Pinned) -> Result<(), LockError> {
    let repo = cached(cache, &pin.git)?;
    let object = format!("{}^{{commit}}", pin.commit);
    if git(&repo, &["cat-file", "-e", &object]).is_err() {
        fetch(&repo)?;
    }
    let tree = if pin.dir.is_empty() {
        pin.commit.clone()
    } else {
        format!("{}:{}", pin.commit, pin.dir)
    };
    let dir = root.join(LIBRARIES_DIR).join(&pin.path);
    // Into a directory beside it, then in place of it, so that a library is
    // whole or as it was.
    let temp = dir.with_file_name(format!(
        ".{}.{}",
        dir.file_name()
            .map_or_else(String::new, |name| name.to_string_lossy().into_owned()),
        crate::sweep::token()
    ));
    if temp.exists() {
        remove_tree(&temp).map_err(|error| io_error(&temp, &error))?;
    }
    fs::create_dir_all(&temp).map_err(|error| io_error(&temp, &error))?;
    let mut archive = Command::new("git");
    archive
        .arg("--git-dir")
        .arg(&repo)
        .args(["archive", "--format=tar", &tree])
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let mut child = archive
        .spawn()
        .map_err(|error| LockError::Git(format!("cannot run `git archive`: {error}")))?;
    let unpacked = child
        .stdout
        .take()
        .ok_or_else(|| LockError::Git("`git archive` gave no output".to_owned()))
        .and_then(|out| {
            tar::Archive::new(out)
                .unpack(&temp)
                .map_err(|error| io_error(&temp, &error))
        });
    let finished = child
        .wait_with_output()
        .map_err(|error| LockError::Git(format!("cannot run `git archive`: {error}")))?;
    if !finished.status.success() {
        let _ = remove_tree(&temp);
        return Err(LockError::Git(format!(
            "`git archive {tree}` of `{}` failed: {}",
            pin.git,
            String::from_utf8_lossy(&finished.stderr).trim()
        )));
    }
    if let Err(error) = unpacked {
        let _ = remove_tree(&temp);
        return Err(error);
    }
    if dir.exists() {
        remove_tree(&dir).map_err(|error| io_error(&dir, &error))?;
    }
    fs::rename(&temp, &dir).map_err(|error| io_error(&dir, &error))
}

/// The bare repository of `git` kept in `cache`'s `git/`, cloned if it is
/// not there, and fetched.
fn fetched(cache: &Path, git_url: &str) -> Result<PathBuf, LockError> {
    let repo = cached(cache, git_url)?;
    fetch(&repo)?;
    Ok(repo)
}

/// The bare repository of `git` kept in `cache`'s `git/`, cloned if it is
/// not there, named by its URL's digest.
fn cached(cache: &Path, git_url: &str) -> Result<PathBuf, LockError> {
    let dir = cache.join("git");
    let repo = dir.join(&Digest::of(git_url.as_bytes()).hex()[..32]);
    if repo.join("HEAD").is_file() {
        return Ok(repo);
    }
    fs::create_dir_all(&dir).map_err(|error| io_error(&dir, &error))?;
    let ran = Command::new("git")
        .args(["clone", "--bare", "--quiet", git_url])
        .arg(&repo)
        .output()
        .map_err(|error| LockError::Git(format!("cannot run `git clone`: {error}")))?;
    if !ran.status.success() {
        let _ = remove_tree(&repo);
        return Err(LockError::Git(format!(
            "cannot clone `{git_url}`: {}",
            String::from_utf8_lossy(&ran.stderr).trim()
        )));
    }
    Ok(repo)
}

/// Fetches every branch and tag of the bare repository `repo`'s origin.
fn fetch(repo: &Path) -> Result<(), LockError> {
    git(
        repo,
        &[
            "fetch",
            "--quiet",
            "--force",
            "origin",
            "+refs/heads/*:refs/heads/*",
            "+refs/tags/*:refs/tags/*",
        ],
    )
    .map(|_| ())
}

/// What `git` with `args` prints, run on the bare repository `repo`, trimmed.
fn git(repo: &Path, args: &[&str]) -> Result<String, LockError> {
    let ran = Command::new("git")
        .arg("--git-dir")
        .arg(repo)
        .args(args)
        .output()
        .map_err(|error| LockError::Git(format!("cannot run `git`: {error}")))?;
    if !ran.status.success() {
        return Err(LockError::Git(format!(
            "`git {}` failed: {}",
            args.join(" "),
            String::from_utf8_lossy(&ran.stderr).trim()
        )));
    }
    Ok(String::from_utf8_lossy(&ran.stdout).trim().to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_library_s_source_is_the_first_whose_prefix_it_is_under() {
        let mut lock = Lock {
            sources: vec![Source {
                prefix: "github.com/gcslib".to_owned(),
                git: "/srv/pekit2".to_owned(),
                dir: "gcslib/github.com/gcslib".to_owned(),
                rev: "main".to_owned(),
            }],
            libraries: Vec::new(),
        };
        let (source, dir) = lock.source_of("github.com/gcslib/gcc");
        assert_eq!(source.git, "/srv/pekit2");
        assert_eq!(dir, "gcslib/github.com/gcslib/gcc");
        // A prefix's name alone is no prefix of another.
        let (source, dir) = lock.source_of("github.com/gcslibs/x/y");
        assert_eq!(source.git, "https://github.com/gcslibs/x");
        assert_eq!(dir, "y");
        assert_eq!(lock.sources.len(), 2);
        // Found once added.
        let (again, _) = lock.source_of("github.com/gcslibs/x/z");
        assert_eq!(again, source);
        assert_eq!(lock.sources.len(), 2);
    }
}
