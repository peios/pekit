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
//!   no path of its: "it is outside the step". The temporary directories
//!   GCSL makes for it, a `Dir.sandbox`'s copy among them, are in its own
//!   output directory, `.pekit-tmp-<n>`, so that the files it gives from one
//!   have paths no other step's have. A secret file it is given is at its
//!   path under `/pekit-secrets`, bound read-only from where the keychain
//!   found it, never staged (see [Secrets](crate::keychain#secrets)); the
//!   keychain itself it cannot ask.
//!
//! A `Dir.sandbox`'s commands are run by each with the sandbox's copy as
//! their `/`, in a bubblewrap of their own beside the step's, never inside
//! it: a host such as Ubuntu forbids one inside another. A step's are cut
//! off as the step is; others' have their own users and processes, as plain
//! GCSL gives them.
//!
//! Each counts its temporary directories itself, so both are `Sync`: a
//! step's effects run on a pool's threads.

use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::collections::HashMap;
use std::sync::{Arc, Mutex, MutexGuard};
use std::sync::atomic::{AtomicU32, Ordering};

use gcsl::check::Builtin;
use gcsl::eval::Effects;
use gcsl::eval::effects::{KeychainAnswer, KeychainSecret};

use crate::layers::{Layers, Overlay};
use crate::record::Kept;
use crate::keychain::{IN_A_STEP, Keychain};
use crate::load::Libraries;
use crate::scratch::SCRATCH;
use crate::store::{Seen, Store};

/// The workspace's root in the logical layout.
pub const WORKSPACE: &str = "/w";

/// The outputs' root in the logical layout.
pub const OUTPUTS: &str = "/pekit-out";

/// The root of the secret files steps are given, in the logical layout.
pub const SECRETS: &str = "/pekit-secrets";

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

/// How much a sandbox's command is cut off from the host.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Isolation {
    /// Its own users and processes, as plain GCSL runs it: what is not
    /// hermetic.
    Plain,
    /// Every namespace its own, the network's among them: a hermetic step's.
    Hermetic,
}

/// Where a sandbox's root is on the host.
enum Root {
    /// A directory of its own.
    Dir(PathBuf),
    /// An overlay.
    Overlay(Overlay),
}

/// The command that runs `command`, a program of a sandbox whose root is
/// `root` on the host, with it as its `/`, cut off as `isolation` says: one
/// bubblewrap, never one inside another, which a host such as Ubuntu
/// forbids.
fn sandboxed(
    root: &Root,
    command: &Command,
    (isolation, devices, scratch): (Isolation, &[PathBuf], Option<&Path>),
) -> Result<Command, String> {
    let (words, cwd, env) = parts(command);
    let mut wrapped = Command::new(bwrap()?);
    match isolation {
        Isolation::Plain => wrapped.args(["--unshare-user", "--unshare-pid"]),
        Isolation::Hermetic => wrapped.args(["--unshare-all", "--new-session"]),
    };
    wrapped.arg("--die-with-parent");
    match root {
        Root::Dir(root) => wrapped.arg("--bind").arg(root).arg("/"),
        Root::Overlay(overlay) => wrapped
            .arg("--overlay-src")
            .arg(&overlay.lower)
            .arg("--overlay")
            .arg(&overlay.upper)
            .arg(&overlay.work)
            .arg("/"),
    };
    wrapped.args(["--proc", "/proc", "--dev", "/dev"]);
    for device in devices {
        wrapped.arg("--dev-bind").arg(device).arg(device);
    }
    if let Some(scratch) = scratch {
        wrapped.arg("--bind").arg(scratch).arg(SCRATCH);
    }
    wrapped.args(["--chdir", cwd.as_deref().unwrap_or("/")]);
    wrapped.arg("--clearenv");
    for (name, value) in env {
        if let Some(value) = value {
            wrapped.args(["--setenv", &name, &value]);
        }
    }
    wrapped.arg("--").args(&words);
    wrapped.env_clear();
    Ok(wrapped)
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
    /// The keychain that answers for the build's secrets, if it has one.
    keychain: Option<Arc<Keychain>>,
    /// Where the libraries the workspace imports were found (see
    /// [Libraries](crate::load#libraries)).
    libraries: Libraries,
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
            keychain: None,
            libraries: Libraries::default(),
        }
    }

    /// The view whose libraries were found where `libraries` says (see
    /// [Libraries](crate::load#libraries)).
    #[must_use]
    pub fn with_libraries(mut self, libraries: Libraries) -> Self {
        self.libraries = libraries;
        self
    }

    /// The view whose secrets `keychain` answers for (see
    /// [Secrets](crate::keychain#secrets)).
    #[must_use]
    pub fn with_keychain(mut self, keychain: Arc<Keychain>) -> Self {
        self.keychain = Some(keychain);
        self
    }
}

impl Effects for HostView {
    /// The effects pekit isolates in a target's own code: fetches, which
    /// are pinned by their digests or commits and kept in the store, the
    /// unpacking of archives into the store, and the reads of a target's
    /// files and directories. Every other effect a target makes is met in
    /// program order (see GCSL's graph mode).
    fn isolates(&self, builtin: Builtin) -> bool {
        matches!(
            builtin,
            Builtin::Fetch
                | Builtin::FetchGit
                | Builtin::ArchiveUnpack
                | Builtin::FileRead
                | Builtin::DirFiles
        )
    }

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

    fn sandbox_command(&self, root: &str, command: Command) -> Option<Result<Command, String>> {
        Some(
            self.host_path(root).and_then(|root| {
                sandboxed(&Root::Dir(root), &command, (Isolation::Plain, &[], None))
            }),
        )
    }

    fn host_path(&self, path: &str) -> Result<PathBuf, String> {
        if let Some(rest) = under(path, WORKSPACE) {
            return Ok(self.libraries.host(&self.workspace, rest));
        }
        if let Some(rest) = under(path, OUTPUTS) {
            return Ok(self.outputs.join(rest));
        }
        Ok(PathBuf::from(path))
    }

    fn working_dir(&self) -> Result<PathBuf, String> {
        Ok(Path::new(WORKSPACE).join(&self.here))
    }

    fn keychain(&self, secret: &KeychainSecret<'_>) -> Option<Result<KeychainAnswer, String>> {
        let keychain = self.keychain.as_ref()?;
        let host = match secret.file.map(|file| self.host_path(file)).transpose() {
            Ok(host) => host,
            Err(reason) => return Some(Err(reason)),
        };
        Some(keychain.answer(secret, host))
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

/// Host tools a step's sandbox borrows, which make its step not hermetic:
/// a host snapshot's (see [The host snapshot](crate::host#the-host-snapshot)),
/// or a test's.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct HostTools {
    /// Host paths bound read-only where they are.
    pub bind: Vec<PathBuf>,
    /// Paths under them hidden, each an empty directory instead.
    pub hide: Vec<PathBuf>,
    /// The `PATH` a program runs with if its `exec` sets none.
    pub path: Option<String>,
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
    /// The host tools it borrows, which make the step not hermetic.
    host: HostTools,
    /// The number of the next temporary directory.
    temps: AtomicU32,
    /// Its compositions and sandboxes.
    layers: Layers,
    /// The secret files it is given, each its path as it sees it and where
    /// it is on the host: its arguments' and captures' from its start, and
    /// a context's once it reads it.
    secrets: Mutex<Vec<(String, PathBuf)>>,
    /// The devices bound into it, and into the sandboxes it makes, each where
    /// it is on the host.
    devices: Vec<PathBuf>,
    /// Its scratch directory, on the host, if it is marked `#[scratch]`
    /// (see [Scratch directories](crate::scratch#scratch-directories)).
    scratch: Option<PathBuf>,
    /// What each context it can read gives it once it reads it, by the
    /// context's key, and the store they are staged from (see
    /// `design/engine.md`, R3).
    pending: Mutex<HashMap<String, Pending>>,
    /// The store `pending`'s files are staged from.
    store: Option<Store>,
    /// Each path staged from a context it read, its file or directory as the
    /// store keeps it.
    staged: Mutex<Vec<(String, Kept)>>,
    /// What each file of its writable trees was when it was last put into
    /// the store (see [Trees seen before](crate::store#trees-seen-before)).
    seen: Mutex<Seen>,
    /// Why staging a context's files failed, if it did: what its next
    /// program fails with, since a read can't.
    unstaged: Mutex<Option<String>>,
}

/// What a context gives a step once it reads it: the files and directories
/// its value names, each as the store keeps it, staged; and the secret files
/// it names, bound.
#[derive(Clone, Debug, Default)]
pub struct Pending {
    /// Each file and directory, by its logical path.
    pub files: Vec<(String, Kept)>,
    /// Each secret file, by its logical path and where it is on the host.
    pub secrets: Vec<(String, PathBuf)>,
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
        let own_host = stage.join("out").join(relative);
        fs::create_dir_all(&own_host)?;
        fs::create_dir_all(stage.join("layers"))?;
        let layers = Layers::new(
            vec![stage.join("w"), stage.join("out")],
            &own_host,
            &stage.join("layers"),
        );
        Ok(Self {
            stage: stage.to_path_buf(),
            own,
            host: HostTools::default(),
            temps: AtomicU32::new(0),
            layers,
            secrets: Mutex::new(Vec::new()),
            devices: Vec::new(),
            scratch: None,
            pending: Mutex::new(HashMap::new()),
            store: None,
            staged: Mutex::new(Vec::new()),
            seen: Mutex::new(Seen::default()),
            unstaged: Mutex::new(None),
        })
    }

    /// The sandbox given the secret files `secrets`, each its path as the
    /// step sees it and where it is on the host, bound read-only.
    #[must_use]
    pub fn with_secrets(mut self, secrets: Vec<(String, PathBuf)>) -> Self {
        self.secrets = Mutex::new(secrets);
        self
    }

    /// The sandbox given, of each context its step can read, by its key, what
    /// it gives the step once the step reads it, staged from `store`
    /// ([`Effects::context_read`]).
    #[must_use]
    pub fn with_contexts(mut self, pending: HashMap<String, Pending>, store: Store) -> Self {
        self.pending = Mutex::new(pending);
        self.store = Some(store);
        self
    }

    /// Its own output directory and its `/tmp`, on the host: what its
    /// programs can write.
    ///
    /// # Errors
    ///
    /// That its own directory has no place, which a sandbox's always has.
    pub fn writable(&self) -> Result<(PathBuf, PathBuf), String> {
        Ok((self.host_path(&self.own)?, self.stage.join("tmp")))
    }

    /// What each file of its writable trees was when it was last put into
    /// the store, for this thread alone while it is held; none if a thread
    /// that held it panicked.
    #[must_use]
    pub fn seen(&self) -> Option<MutexGuard<'_, Seen>> {
        self.seen.lock().ok()
    }

    /// Seals its sealed compositions again, once a program found in the
    /// store has had them placed again as copies.
    ///
    /// # Errors
    ///
    /// That one could not be made read-only.
    pub fn reseal(&self) -> io::Result<()> {
        self.layers.reseal()
    }

    /// Its sealed compositions, on the host.
    #[must_use]
    pub fn sealed(&self) -> Vec<PathBuf> {
        self.layers
            .sealed()
            .into_iter()
            .map(|(_, host)| host)
            .collect()
    }

    /// Whether it binds a secret's file, at its start or by a read.
    #[must_use]
    pub fn binds_secrets(&self) -> bool {
        self.secrets
            .lock()
            .map_or(true, |secrets| !secrets.is_empty())
    }

    /// Each path staged from a context the step read, as the store keeps it.
    #[must_use]
    pub fn staged(&self) -> Vec<(String, Kept)> {
        self.staged
            .lock()
            .map(|staged| staged.clone())
            .unwrap_or_default()
    }

    /// The sandbox with the devices `devices` bound into it, and into the
    /// sandboxes it makes, each where it is.
    #[must_use]
    pub fn with_devices(mut self, devices: Vec<PathBuf>) -> Self {
        self.devices = devices;
        self
    }

    /// The sandbox with the scratch directory at `scratch` on the host
    /// bound at [`SCRATCH`], into it and the sandboxes it makes.
    #[must_use]
    pub fn with_scratch(mut self, scratch: Option<PathBuf>) -> Self {
        self.scratch = scratch;
        self
    }

    /// The sandbox borrowing the host tools `tools`, which makes the step
    /// not hermetic.
    #[must_use]
    pub fn with_host(mut self, tools: &HostTools) -> Self {
        self.host = tools.clone();
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
        for path in &self.host.bind {
            wrapped.arg("--ro-bind").arg(path).arg(path);
        }
        for path in &self.host.hide {
            wrapped.arg("--tmpfs").arg(path);
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
        // Its compositions, which nothing may change.
        for (path, host) in self.layers.sealed() {
            wrapped.arg("--ro-bind").arg(host).arg(path);
        }
        if let Some(why) = self
            .unstaged
            .lock()
            .ok()
            .and_then(|unstaged| unstaged.clone())
        {
            return Err(why);
        }
        let secrets = self
            .secrets
            .lock()
            .map(|secrets| secrets.clone())
            .unwrap_or_default();
        for (path, host) in &secrets {
            wrapped.arg("--ro-bind").arg(host).arg(path);
        }
        wrapped
            .arg("--bind")
            .arg(self.stage.join("tmp"))
            .arg("/tmp");
        if let Some(scratch) = &self.scratch {
            wrapped.arg("--bind").arg(scratch).arg(SCRATCH);
        }
        wrapped.args(["--proc", "/proc", "--dev", "/dev"]);
        for device in &self.devices {
            wrapped.arg("--dev-bind").arg(device).arg(device);
        }
        wrapped.args(["--chdir", cwd.as_deref().unwrap_or(&self.own)]);
        // Only what its `exec` sets, and the host tools' `PATH` if it sets
        // none.
        wrapped.arg("--clearenv");
        let mut has_path = false;
        for (name, value) in env {
            if let Some(value) = value {
                has_path |= name == "PATH";
                wrapped.args(["--setenv", &name, &value]);
            }
        }
        if let Some(path) = self.host.path.as_ref().filter(|_| !has_path) {
            wrapped.args(["--setenv", "PATH", path]);
        }
        wrapped.arg("--").args(&words);
        wrapped.env_clear();
        if let Some(path) = std::env::var_os("PATH") {
            wrapped.env("PATH", path);
        }
        Ok(wrapped)
    }

    fn sandbox_command(&self, root: &str, command: Command) -> Option<Result<Command, String>> {
        let root = match self.layers.overlay(root) {
            Some(overlay) => Root::Overlay(overlay),
            None => match self.host_path(root) {
                Ok(root) => Root::Dir(root),
                Err(error) => return Some(Err(error)),
            },
        };
        Some(sandboxed(
            &root,
            &command,
            (Isolation::Hermetic, &self.devices, self.scratch.as_deref()),
        ))
    }

    fn place_file(&self, source: &Path, target: &Path) -> io::Result<()> {
        self.layers.place_file(source, target)
    }

    fn context_read(&self, key: &str) {
        let Some(pending) = self
            .pending
            .lock()
            .ok()
            .and_then(|mut pending| pending.remove(key))
        else {
            return;
        };
        let Some(store) = &self.store else {
            return;
        };
        for (path, kept) in &pending.files {
            if let Err(error) = self.stage(store, path, *kept) {
                if let Ok(mut unstaged) = self.unstaged.lock() {
                    unstaged.get_or_insert_with(|| format!("cannot stage `{path}`: {error}"));
                }
                return;
            }
        }
        if let Ok(mut staged) = self.staged.lock() {
            staged.extend(pending.files);
        }
        if let Ok(mut secrets) = self.secrets.lock() {
            secrets.extend(pending.secrets);
        }
    }

    fn composed(&self, root: &str) {
        if let Ok(host) = self.host_path(root) {
            // A composition that cannot be sealed is left as it is: it is
            // still the step's, and correct, only not guarded.
            let _ = self.layers.composed(root, &host);
        }
    }

    fn make_sandbox(&self, source: &str, root: &str) -> Option<Result<(), String>> {
        let (Ok(lower), Ok(upper)) = (self.host_path(source), self.host_path(root)) else {
            return None;
        };
        match self.layers.make_sandbox(root, &lower, &upper) {
            Ok(true) => Some(Ok(())),
            Ok(false) => None,
            Err(error) => Some(Err(error.to_string())),
        }
    }

    fn host_path(&self, path: &str) -> Result<PathBuf, String> {
        if let Some(resolved) = self.layers.resolve(path) {
            return resolved.map_err(|error| error.to_string());
        }
        if let Some(rest) = under(path, WORKSPACE) {
            return Ok(self.stage.join("w").join(rest));
        }
        if let Some(rest) = under(path, OUTPUTS) {
            return Ok(self.stage.join("out").join(rest));
        }
        if let Some(rest) = under(path, "/tmp") {
            return Ok(self.stage.join("tmp").join(rest));
        }
        if let (Some(scratch), Some(rest)) = (&self.scratch, under(path, SCRATCH)) {
            return Ok(scratch.join(rest));
        }
        let secrets = self
            .secrets
            .lock()
            .map(|secrets| secrets.clone())
            .unwrap_or_default();
        if let Some((_, host)) = secrets.iter().find(|(given, _)| given == path) {
            return Ok(host.clone());
        }
        Err("it is outside the step".to_owned())
    }

    fn keychain(&self, secret: &KeychainSecret<'_>) -> Option<Result<KeychainAnswer, String>> {
        let _ = secret;
        Some(Err(IN_A_STEP.to_owned()))
    }

    fn working_dir(&self) -> Result<PathBuf, String> {
        Ok(PathBuf::from(&self.own))
    }

    fn temp_dir(&self) -> Option<Result<String, String>> {
        let number = self.temps.fetch_add(1, Ordering::Relaxed);
        let path = format!("{}/.pekit-tmp-{number}", self.own);
        let made = self
            .host_path(&path)
            .and_then(|at| fs::create_dir_all(at).map_err(|error| error.to_string()))
            .map(|()| path);
        Some(made)
    }
}
