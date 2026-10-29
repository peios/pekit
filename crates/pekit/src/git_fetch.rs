//! Answering `fetch_git`: a git repository's files at a commit, from the
//! store (see `design/image.md`, I7).
//!
//! # Fetching from git
//!
//! GCSL's `fetch_git(url, commit)` gives the files of the repository at
//! `url` as the commit `commit` has them, pinned by the commit's whole name,
//! which names its files as a digest does. Pekit answers it itself, as it
//! answers `fetch` ([`crate::fetch`]), and never in a step, which has no
//! network:
//!
//! - **Found:** its record, by the URL and the commit, names the tree of its
//!   files, which the store holds whole.
//! - **Else fetched:** into a bare repository of the URL kept in the
//!   store's `git/`, the commit alone, shallowly, or, if the server won't
//!   give a commit by its name, with every branch and tag; checked out into
//!   a scratch directory, without `.git` or submodules, and put into the
//!   store; and recorded, with how long it took, for an eviction to weigh.
//!   Fetches of one repository take turns, since a build fetches several
//!   crates of one repository at once into one bare repository; one that
//!   waited finds the record another made.
//!
//! Git runs with no configuration but its own, the user's and the system's
//! left out, so that nothing of the machine, a line-ending conversion or a
//! filter, changes the files a commit gives: only the repository's own
//! attributes do, as they do for anyone who checks the commit out.
//!
//! # Credentials
//!
//! But for credentials: git's one credential helper asks the user's git,
//! with the user's configuration and environment, so that a private
//! repository is fetched with whatever credentials the user's git would
//! use, a `gh auth git-credential` say ([`credential_helper`]). It asks only
//! to `get` them; storing and erasing them is left undone, so a fetch
//! never changes what the user's git keeps, and nothing prompts. What a
//! pinned commit gives is the same whoever fetches it, so credentials name
//! nothing a record is keyed by, and no step sees them.

use std::collections::HashMap;
use std::env;
use std::ffi::{OsStr, OsString};
use std::fs;
use std::os::unix::ffi::{OsStrExt as _, OsStringExt as _};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex, OnceLock, PoisonError};
use std::time::Instant;

use serde::{Deserialize, Serialize};

use crate::layers::remove_tree;
use crate::sandbox::OUTPUTS;
use crate::store::{Digest, Store};

/// Why `fetch_git` is answered only outside steps.
pub const IN_A_STEP: &str = "`fetch_git` runs outside hermetic steps: a step has no network; \
                             fetch in a parameter default, or in the caller, and pass the \
                             directory in";

/// The record of a repository's files at a commit.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
struct FetchedGit {
    /// The tree of its files.
    tree: Digest,
    /// How long fetching it took, in milliseconds.
    duration_ms: u64,
}

/// The number of the next directory checked out into, in this process.
static NEXT: AtomicU32 = AtomicU32::new(0);

/// The key of the record of `url`'s files at `commit`.
fn key(url: &str, commit: &str) -> Digest {
    Digest::of(format!("pekit-git-1\0{url}\0{commit}").as_bytes())
}

/// Where `url`'s files at `commit` are, as GCSL code names them: a
/// directory named by the repository, under one named by the record's key.
#[must_use]
pub fn git_path(url: &str, commit: &str) -> String {
    let name = gcsl::eval::fetched_name(url);
    let name = name.strip_suffix(".git").unwrap_or(&name);
    format!("{OUTPUTS}/.git/{}/{name}", key(url, commit).hex())
}

/// The tree of `url`'s files at `commit`, from `store`'s record of it, else
/// fetched, checked out in a directory under `scratch` and put into the
/// store (see [Fetching from git](#fetching-from-git)).
///
/// # Errors
///
/// GCSL's errors of `fetch_git`: a pin that is no commit's name, or a
/// commit that can't be fetched.
pub fn fetch_git(
    store: &Store,
    (url, commit): (&str, &str),
    scratch: &Path,
) -> Result<Digest, String> {
    let pin = commit.trim().to_ascii_lowercase();
    if pin.len() != 40 || !pin.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return Err(format!(
            "`fetch_git` of `{url}` is pinned by `{commit}`, which is no commit's name: 40 \
             hexadecimal digits, not a branch or a tag, which can move"
        ));
    }
    let key = key(url, &pin);
    // A record whose contents were evicted is fetched again.
    if let Ok(Some(FetchedGit { tree, .. })) = store.record::<FetchedGit>(&key)
        && store.holds_tree(&tree)
    {
        return Ok(tree);
    }
    let began = Instant::now();
    let fail = |reason: String| format!("`fetch_git` could not fetch `{pin}` of `{url}`: {reason}");
    let path = repository_path(store, url);
    // Fetches of one repository take turns: a build fetches several crates
    // of one repository at once, into one bare repository.
    let lock = lock_of(&path);
    let _turn = lock.lock().unwrap_or_else(PoisonError::into_inner);
    // Another fetch of the commit may have recorded it while this waited.
    if let Ok(Some(FetchedGit { tree, .. })) = store.record::<FetchedGit>(&key)
        && store.holds_tree(&tree)
    {
        return Ok(tree);
    }
    let repo = repository(&path).map_err(fail)?;
    if !holds(&repo, &pin) {
        // A server that won't give a commit by its name gives every branch
        // and tag, among which it may be.
        let shallow = git(&repo, &["fetch", "--quiet", "--depth", "1", url, &pin]);
        if shallow.is_err() || !holds(&repo, &pin) {
            git(
                &repo,
                &[
                    "fetch",
                    "--quiet",
                    url,
                    "+refs/heads/*:refs/heads/*",
                    "+refs/tags/*:refs/tags/*",
                ],
            )
            .map_err(fail)?;
        }
        if !holds(&repo, &pin) {
            return Err(fail("the repository has no such commit".to_owned()));
        }
    }
    let number = NEXT.fetch_add(1, Ordering::Relaxed);
    let dir = scratch.join(format!("git-{}-{number}", crate::sweep::token()));
    let tree = checked_out(store, &repo, (&pin, &dir)).map_err(fail);
    let _ = remove_tree(&dir);
    let _ = fs::remove_file(dir.with_extension("index"));
    let tree = tree?;
    let fetched = FetchedGit {
        tree,
        duration_ms: u64::try_from(began.elapsed().as_millis()).unwrap_or(u64::MAX),
    };
    store
        .put_record(&key, &fetched)
        .map_err(|error| fail(error.to_string()))?;
    Ok(tree)
}

/// Where the bare repository of `url` is kept in `store`'s `git/`, named by
/// its URL's digest.
fn repository_path(store: &Store, url: &str) -> PathBuf {
    store
        .root()
        .join("git")
        .join(&Digest::of(url.as_bytes()).hex()[..32])
}

/// The bare repository at `repo`, made if it is not there.
fn repository(repo: &Path) -> Result<PathBuf, String> {
    if !repo.join("HEAD").is_file() {
        fs::create_dir_all(repo).map_err(|error| error.to_string())?;
        git(repo, &["init", "--quiet", "--bare"])?;
    }
    Ok(repo.to_path_buf())
}

/// The lock of the repository at `repo`, which a fetch into it holds, one
/// for each repository the process fetches into.
fn lock_of(repo: &Path) -> Arc<Mutex<()>> {
    static LOCKS: OnceLock<Mutex<HashMap<PathBuf, Arc<Mutex<()>>>>> = OnceLock::new();
    let locks = LOCKS.get_or_init(|| Mutex::new(HashMap::new()));
    let mut locks = locks.lock().unwrap_or_else(PoisonError::into_inner);
    Arc::clone(locks.entry(repo.to_path_buf()).or_default())
}

/// Whether the repository `repo` holds the commit `commit`.
fn holds(repo: &Path, commit: &str) -> bool {
    git(repo, &["cat-file", "-e", &format!("{commit}^{{commit}}")]).is_ok()
}

/// The commit `commit` of `repo` checked out into `dir`, a directory that
/// is not there, and put into `store`: its tree.
fn checked_out(store: &Store, repo: &Path, (commit, dir): (&str, &Path)) -> Result<Digest, String> {
    remove_tree(dir).map_err(|error| error.to_string())?;
    fs::create_dir_all(dir).map_err(|error| error.to_string())?;
    // An index of its own, so that the bare repository is left as it was.
    let ran = command(repo)
        .env("GIT_INDEX_FILE", dir.with_extension("index"))
        .arg("--work-tree")
        .arg(dir)
        .args(["checkout", "--quiet", "--force", commit, "--", "."])
        .output()
        .map_err(|error| format!("cannot run `git`: {error}"))?;
    if !ran.status.success() {
        return Err(String::from_utf8_lossy(&ran.stderr).trim().to_owned());
    }
    store.put_tree(dir).map_err(|error| error.to_string())
}

/// The environment that gives git configuration besides a repository's
/// own: what a fetch's git runs without, and the user's git with.
/// `GIT_CONFIG_PARAMETERS` is how git passes its `-c` settings to what it
/// runs, a credential helper among them; `GIT_CONFIG_COUNT` says how many
/// `GIT_CONFIG_KEY_<n>` and `GIT_CONFIG_VALUE_<n>` to read.
const CONFIGURATION: [&str; 4] = [
    "GIT_CONFIG_GLOBAL",
    "GIT_CONFIG_NOSYSTEM",
    "GIT_CONFIG_PARAMETERS",
    "GIT_CONFIG_COUNT",
];

/// `git` on the bare repository `repo`, with no configuration but its own
/// and a credential helper that asks the user's git.
fn command(repo: &Path) -> Command {
    let helper = credential_helper(|name| env::var_os(name));
    let mut command = isolated(&helper);
    command.arg("--git-dir").arg(repo);
    command
}

/// `git` with no configuration but a repository's own, prompting for
/// nothing, and with `helper` its one credential helper.
fn isolated(helper: &OsStr) -> Command {
    let mut setting = OsString::from("credential.helper=");
    setting.push(helper);
    let mut command = Command::new("git");
    command
        .env_remove("GIT_CONFIG_PARAMETERS")
        .env_remove("GIT_CONFIG_COUNT")
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_TERMINAL_PROMPT", "0")
        // An empty helper clears any before it, so this one is alone.
        .args(["-c", "credential.helper="])
        .arg("-c")
        .arg(setting);
    command
}

/// A credential helper that answers `get` with what `git credential fill`
/// gives under the user's configuration, `user` giving the user's value of
/// each of [`CONFIGURATION`], unset if `None`, and does nothing else (see
/// [Credentials](#credentials)). Given a fetch's `GIT_CONFIG_PARAMETERS`
/// in place of the user's, the user's git would be given this helper, and
/// ask itself.
fn credential_helper(user: impl Fn(&str) -> Option<OsString>) -> OsString {
    let settings = CONFIGURATION.map(|name| (name, user(name)));
    let mut helper = b"!f() { test \"$1\" = get || exit 0; exec env".to_vec();
    // `env` takes its `-u`s before its assignments.
    for (name, _) in settings.iter().filter(|(_, value)| value.is_none()) {
        helper.extend_from_slice(format!(" -u {name}").as_bytes());
    }
    for (name, value) in settings {
        if let Some(value) = value {
            helper.extend_from_slice(format!(" {name}=").as_bytes());
            helper.extend_from_slice(&quoted(&value));
        }
    }
    helper.extend_from_slice(b" git credential fill; }; f");
    OsString::from_vec(helper)
}

/// `value` quoted for `sh`, as one word.
fn quoted(value: &OsStr) -> Vec<u8> {
    let mut quoted = vec![b'\''];
    for &byte in value.as_bytes() {
        if byte == b'\'' {
            quoted.extend_from_slice(b"'\\''");
        } else {
            quoted.push(byte);
        }
    }
    quoted.push(b'\'');
    quoted
}

/// What `git` with `args` prints, run on the bare repository `repo`.
fn git(repo: &Path, args: &[&str]) -> Result<String, String> {
    let ran = command(repo)
        .args(args)
        .output()
        .map_err(|error| format!("cannot run `git`: {error}"))?;
    if !ran.status.success() {
        return Err(String::from_utf8_lossy(&ran.stderr).trim().to_owned());
    }
    Ok(String::from_utf8_lossy(&ran.stdout).trim().to_owned())
}

#[cfg(test)]
mod tests {
    use std::io::Write as _;
    use std::os::unix::fs::PermissionsExt as _;
    use std::process::{Output, Stdio};

    use super::*;

    /// A user's git configuration, in a directory whose name needs quoting,
    /// whose credential helper gives `u` and `p` and notes a `store` in
    /// `stored` beside that directory: the configuration's path and the
    /// directory holding both.
    fn user() -> (PathBuf, tempfile::TempDir) {
        let dir = tempfile::tempdir().unwrap();
        let home = dir.path().join("it's $HOME");
        fs::create_dir(&home).unwrap();
        // The helper's path is run by a shell, as the user's would be; the
        // configuration's is what the helper here has to quote.
        let helper = dir.path().join("helper");
        fs::write(
            &helper,
            format!(
                "#!/bin/sh\n\
                 case \"$1\" in\n\
                 get) printf 'username=u\\npassword=p\\n' ;;\n\
                 store) touch '{}' ;;\n\
                 esac\n",
                dir.path().join("stored").display()
            ),
        )
        .unwrap();
        fs::set_permissions(&helper, fs::Permissions::from_mode(0o755)).unwrap();
        let config = home.join("config");
        fs::write(
            &config,
            format!("[credential]\n\thelper = {}\n", helper.display()),
        )
        .unwrap();
        (config, dir)
    }

    /// What `git credential <action>` gives, run as a fetch runs git, with
    /// `helper` and `input` its input.
    fn credential(helper: &OsStr, action: &str, input: &str) -> Output {
        let mut child = isolated(helper)
            .args(["credential", action])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        child
            .stdin
            .take()
            .unwrap()
            .write_all(input.as_bytes())
            .unwrap();
        child.wait_with_output().unwrap()
    }

    #[test]
    fn a_fetch_takes_its_credentials_from_the_user_s_git() {
        let (config, _dir) = user();
        let helper = credential_helper(|name| match name {
            "GIT_CONFIG_GLOBAL" => Some(config.clone().into_os_string()),
            "GIT_CONFIG_NOSYSTEM" => Some("1".into()),
            _ => None,
        });
        let filled = credential(&helper, "fill", "protocol=https\nhost=example.invalid\n\n");
        let out = String::from_utf8_lossy(&filled.stdout);
        assert!(
            filled.status.success(),
            "{}",
            String::from_utf8_lossy(&filled.stderr)
        );
        assert!(
            out.contains("username=u\n") && out.contains("password=p\n"),
            "{out}"
        );
    }

    #[test]
    fn a_fetch_never_stores_credentials_in_the_user_s_git() {
        let (config, _dir) = user();
        let helper = credential_helper(|name| {
            (name == "GIT_CONFIG_GLOBAL").then(|| config.clone().into_os_string())
        });
        let approved = credential(
            &helper,
            "approve",
            "protocol=https\nhost=example.invalid\nusername=u\npassword=p\n\n",
        );
        assert!(
            approved.status.success(),
            "{}",
            String::from_utf8_lossy(&approved.stderr)
        );
        assert!(!config.parent().unwrap().with_file_name("stored").exists());
    }

    #[test]
    fn a_fetch_without_credentials_fails_without_prompting() {
        let helper = credential_helper(|name| match name {
            "GIT_CONFIG_GLOBAL" => Some("/dev/null".into()),
            "GIT_CONFIG_NOSYSTEM" => Some("1".into()),
            _ => None,
        });
        let filled = credential(&helper, "fill", "protocol=https\nhost=example.invalid\n\n");
        assert!(!filled.status.success());
        assert!(
            String::from_utf8_lossy(&filled.stderr).contains("terminal prompts disabled"),
            "{}",
            String::from_utf8_lossy(&filled.stderr)
        );
    }

    #[test]
    fn the_user_s_git_is_given_the_user_s_settings_from_the_environment() {
        let (_, dir) = user();
        // The user's own `-c` settings, which name the helper.
        let parameters = format!(
            "'credential.helper'='{}'",
            dir.path().join("helper").display()
        );
        let helper = credential_helper(|name| match name {
            "GIT_CONFIG_GLOBAL" => Some("/dev/null".into()),
            "GIT_CONFIG_NOSYSTEM" => Some("1".into()),
            "GIT_CONFIG_PARAMETERS" => Some(parameters.clone().into()),
            _ => None,
        });
        let filled = credential(&helper, "fill", "protocol=https\nhost=example.invalid\n\n");
        let out = String::from_utf8_lossy(&filled.stdout);
        assert!(
            filled.status.success(),
            "{}",
            String::from_utf8_lossy(&filled.stderr)
        );
        assert!(out.contains("password=p\n"), "{out}");
    }
}
