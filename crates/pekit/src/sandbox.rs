//! Effect contexts: the logical layout every unit sees, and the sandbox a
//! hermetic step runs in.
//!
//! # The logical layout
//!
//! Every path a GCSL value holds is in one layout, the same on every machine
//! and checkout (see `design/engine.md`, E5): the workspace at `/w`, outputs
//! at `/pekit-out`. Everything the engine runs sees it:
//!
//! - **[`HostView`]**, for what is not hermetic: GCSL's own file-system work
//!   maps `/w` to the workspace's root and `/pekit-out` to its output
//!   directory, and any other absolute path to itself; a program runs under
//!   bubblewrap with the host's own directories bound where they are, and
//!   the workspace and the outputs at `/w` and `/pekit-out`, with the
//!   network and its whole environment, so that it sees the paths GCSL code
//!   gave it. The working directory is the target's directory under `/w`.
//! - **[`StepSandbox`]**, for a hermetic step: only what it is given, staged
//!   in a directory of its own. Its root is an empty file system; the
//!   workspace files it is given are at their `/w` paths and the outputs of
//!   earlier steps at their `/pekit-out` paths, read-only; its own output
//!   directory, `/pekit-out/<target>/<step>`, is writable and its working
//!   directory; `/tmp` is its own; and a program runs with every namespace
//!   unshared, the network's among them, and only the environment its
//!   `exec` sets, killed if pekit dies. A path outside what it is given is
//!   no path of its: "it is outside the step".
//!
//! Each counts its temporary directories itself, so both are `Sync`: a
//! step's effects run on a pool's threads.

use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicU32, Ordering};

use gcsl::eval::Effects;

use crate::record::Kept;
use crate::store::Store;

/// The workspace's root in the logical layout.
pub const WORKSPACE: &str = "/w";

/// The outputs' root in the logical layout.
pub const OUTPUTS: &str = "/pekit-out";

/// `path` under the logical root `root`, if it is it or under it: the rest,
/// relative, `""` for the root itself.
fn under<'a>(path: &'a str, root: &str) -> Option<&'a str> {
    let rest = path.strip_prefix(root)?;
    if rest.is_empty() {
        return Some("");
    }
    rest.strip_prefix('/')
}

/// The path of the program bubblewrap is, on `PATH`.
fn bwrap() -> Result<PathBuf, String> {
    let path = std::env::var_os("PATH").unwrap_or_default();
    std::env::split_paths(&path)
        .map(|dir| dir.join("bwrap"))
        .find(|candidate| candidate.is_file())
        .ok_or_else(|| {
            "pekit runs programs under bubblewrap (`bwrap`), which is not installed".to_owned()
        })
}

/// A command's words, working directory and environment, each variable
/// set or removed, as text.
type Parts = (Vec<String>, Option<String>, Vec<(String, Option<String>)>);

/// The words, working directory and environment `command` was given, as
/// text.
fn parts(command: &Command) -> Parts {
    let words = std::iter::once(command.get_program())
        .chain(command.get_args())
        .map(|word| word.to_string_lossy().into_owned())
        .collect();
    let cwd = command
        .get_current_dir()
        .map(|dir| dir.to_string_lossy().into_owned());
    let env = command
        .get_envs()
        .map(|(name, value)| {
            (
                name.to_string_lossy().into_owned(),
                value.map(|value| value.to_string_lossy().into_owned()),
            )
        })
        .collect();
    (words, cwd, env)
}

/// The effect context of what is not hermetic (see [The logical
/// layout](#the-logical-layout)).
#[derive(Debug)]
pub struct HostView {
    /// The workspace's root, on the host.
    workspace: PathBuf,
    /// The output directory, on the host.
    outputs: PathBuf,
    /// The target's directory relative to the workspace's root.
    here: String,
    /// Where temporary directories are made, on the host.
    temp: PathBuf,
    /// The number of the next temporary directory.
    temps: AtomicU32,
}

impl HostView {
    /// The view of the workspace at `workspace`, its outputs at `outputs`,
    /// for a target in its directory `here`, temporary directories made in
    /// `temp`.
    #[must_use]
    pub fn new(workspace: &Path, outputs: &Path, here: &str, temp: &Path) -> Self {
        Self {
            workspace: workspace.to_path_buf(),
            outputs: outputs.to_path_buf(),
            here: here.to_owned(),
            temp: temp.to_path_buf(),
            temps: AtomicU32::new(0),
        }
    }
}

impl Effects for HostView {
    fn command(&self, command: Command) -> Result<Command, String> {
        let (words, cwd, env) = parts(&command);
        let mut wrapped = Command::new(bwrap()?);
        // The host's own directories where they are, on a root of their own,
        // so that `/w` and `/pekit-out` can be made.
        wrapped.args(["--tmpfs", "/"]);
        let entries = fs::read_dir("/").map_err(|error| format!("cannot list `/`: {error}"))?;
        for entry in entries.flatten() {
            let name = entry.file_name().to_string_lossy().into_owned();
            if name == "w" || name == "pekit-out" {
                continue;
            }
            let at = format!("/{name}");
            if entry.file_type().is_ok_and(|kind| kind.is_symlink()) {
                if let Ok(target) = fs::read_link(&at) {
                    wrapped.args(["--symlink", &target.to_string_lossy(), &at]);
                }
                continue;
            }
            wrapped.args(["--dev-bind", &at, &at]);
        }
        wrapped.arg("--bind").arg(&self.workspace).arg(WORKSPACE);
        fs::create_dir_all(&self.outputs).map_err(|error| error.to_string())?;
        wrapped.arg("--bind").arg(&self.outputs).arg(OUTPUTS);
        wrapped.arg("--die-with-parent");
        if let Some(cwd) = cwd {
            wrapped.args(["--chdir", &cwd]);
        }
        wrapped.arg("--").args(&words);
        for (name, value) in env {
            match value {
                Some(value) => wrapped.env(name, value),
                None => wrapped.env_remove(name),
            };
        }
        Ok(wrapped)
    }

    fn host_path(&self, path: &str) -> Result<PathBuf, String> {
        if let Some(rest) = under(path, WORKSPACE) {
            return Ok(self.workspace.join(rest));
        }
        if let Some(rest) = under(path, OUTPUTS) {
            return Ok(self.outputs.join(rest));
        }
        Ok(PathBuf::from(path))
    }

    fn working_dir(&self) -> Result<PathBuf, String> {
        Ok(Path::new(WORKSPACE).join(&self.here))
    }

    fn temp_dir(&self) -> Option<Result<String, String>> {
        let number = self.temps.fetch_add(1, Ordering::Relaxed);
        let dir = self
            .temp
            .join(format!("pekit-{}-{number}", std::process::id()));
        Some(
            fs::create_dir_all(&dir)
                .map(|()| dir.to_string_lossy().into_owned())
                .map_err(|error| error.to_string()),
        )
    }
}

/// The sandbox a hermetic step runs in (see [The logical
/// layout](#the-logical-layout)): its staging directory on the host, which
/// holds its view.
#[derive(Debug)]
pub struct StepSandbox {
    /// The staging directory: `w` the workspace files it is given, `out`
    /// the outputs of earlier steps, with its own output directory among
    /// them, writable, and `tmp` its own `/tmp`.
    stage: PathBuf,
    /// Its own output directory's logical path, `/pekit-out/<target>/<step>`.
    own: String,
    /// Host directories bound read-only where they are, which make the step
    /// not hermetic: a host snapshot's tools, or a test's.
    host: Vec<PathBuf>,
    /// The number of the next temporary directory.
    temps: AtomicU32,
}

impl StepSandbox {
    /// A sandbox staged in `stage`, a directory that does not exist, for the
    /// step whose output directory is `own`, `<target>/<step>` under
    /// `/pekit-out`.
    ///
    /// # Errors
    ///
    /// That the staging directory cannot be made.
    pub fn new(stage: &Path, own: &str) -> io::Result<Self> {
        let own = format!("{OUTPUTS}/{own}");
        for part in ["w", "tmp"] {
            fs::create_dir_all(stage.join(part))?;
        }
        let relative = under(&own, OUTPUTS).unwrap_or_default();
        fs::create_dir_all(stage.join("out").join(relative))?;
        Ok(Self {
            stage: stage.to_path_buf(),
            own,
            host: Vec::new(),
            temps: AtomicU32::new(0),
        })
    }

    /// The sandbox with the host directories `dirs` bound read-only where
    /// they are, which makes the step not hermetic.
    #[must_use]
    pub fn with_host(mut self, dirs: &[PathBuf]) -> Self {
        self.host.extend_from_slice(dirs);
        self
    }

    /// Its own output directory, logically.
    #[must_use]
    pub fn own(&self) -> &str {
        &self.own
    }

    /// Stages `kept`, a file or a directory from `store`, at `path`, a path
    /// under `/w` or `/pekit-out`.
    ///
    /// # Errors
    ///
    /// That `path` is under neither, or it cannot be staged.
    pub fn stage(&self, store: &Store, path: &str, kept: Kept) -> io::Result<()> {
        let at = self.host_path(path).map_err(io::Error::other)?;
        if at.exists() {
            return Ok(());
        }
        match kept {
            Kept::File(entry) => store.materialise_file(&entry, &at),
            Kept::Dir(tree) => store.materialise_tree(&tree, &at),
        }
    }
}

impl Effects for StepSandbox {
    fn command(&self, command: Command) -> Result<Command, String> {
        let (words, cwd, env) = parts(&command);
        let mut wrapped = Command::new(bwrap()?);
        wrapped.args(["--unshare-all", "--die-with-parent", "--new-session"]);
        wrapped.args(["--tmpfs", "/"]);
        for dir in &self.host {
            wrapped.arg("--ro-bind").arg(dir).arg(dir);
        }
        wrapped
            .arg("--ro-bind")
            .arg(self.stage.join("w"))
            .arg(WORKSPACE);
        wrapped
            .arg("--ro-bind")
            .arg(self.stage.join("out"))
            .arg(OUTPUTS);
        let own = self.host_path(&self.own)?;
        wrapped.arg("--bind").arg(own).arg(&self.own);
        wrapped
            .arg("--bind")
            .arg(self.stage.join("tmp"))
            .arg("/tmp");
        wrapped.args(["--proc", "/proc", "--dev", "/dev"]);
        wrapped.args(["--chdir", cwd.as_deref().unwrap_or(&self.own)]);
        // Only what its `exec` sets.
        wrapped.arg("--clearenv");
        for (name, value) in env {
            if let Some(value) = value {
                wrapped.args(["--setenv", &name, &value]);
            }
        }
        wrapped.arg("--").args(&words);
        wrapped.env_clear();
        if let Some(path) = std::env::var_os("PATH") {
            wrapped.env("PATH", path);
        }
        Ok(wrapped)
    }

    fn host_path(&self, path: &str) -> Result<PathBuf, String> {
        if let Some(rest) = under(path, WORKSPACE) {
            return Ok(self.stage.join("w").join(rest));
        }
        if let Some(rest) = under(path, OUTPUTS) {
            return Ok(self.stage.join("out").join(rest));
        }
        if let Some(rest) = under(path, "/tmp") {
            return Ok(self.stage.join("tmp").join(rest));
        }
        Err("it is outside the step".to_owned())
    }

    fn working_dir(&self) -> Result<PathBuf, String> {
        Ok(PathBuf::from(&self.own))
    }

    fn temp_dir(&self) -> Option<Result<String, String>> {
        let number = self.temps.fetch_add(1, Ordering::Relaxed);
        let path = format!("/tmp/pekit-{number}");
        let made = self
            .host_path(&path)
            .and_then(|at| fs::create_dir_all(at).map_err(|error| error.to_string()))
            .map(|()| path);
        Some(made)
    }
}
