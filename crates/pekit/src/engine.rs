//! The engine: building a target by running the steps it needs.
//!
//! # Building
//!
//! A target's invocation is evaluated in GCSL's graph mode, as one unit,
//! against one action graph (see `design/engine.md`, E1, E2 and E6). Its
//! deferred calls of functions marked `#[hermetic]` are *steps*. The engine
//! runs, repeatedly:
//!
//! - the target's unit, once it may progress: it stops for a result the
//!   engine has not given, or ends with a value whose holes are the steps it
//!   still waits for;
//! - each step the target needs whose inputs are given: from its record, if
//!   the store has one for its receipt; else in a sandbox of its own, its
//!   inputs staged, its body run as a unit of its own, each effect a job;
//! - each effect of a unit as a job on the pool, which has `jobs` threads:
//!   a step's effects in its sandbox, the target's own, which are not
//!   hermetic, under the host's view of the logical layout, one at a time,
//!   in program order, since the target's unit stops at each.
//!
//! A unit runs again only once what it stopped for is given. A step that
//! fails, by its code's own failure, is run again from its start in a fresh
//! sandbox as its `#[retry]` allows, and one that passes only then is
//! *flaky*; its `#[timeout]` spans every attempt. A step that succeeds is
//! recorded, unless its result holds a secret, and its outputs are kept by
//! their logical paths, so that the steps after it are staged with them. Once
//! the target's value has no holes, the files under `/pekit-out` it names are
//! copied out to the output directory, each a copy of its own.

use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::rc::Rc;
use std::sync::Arc;
use std::time::Instant;

use gcsl::check::{Attribute, Builtin, Constant, retry_of, timeout_of};
use gcsl::eval::Effects;
use gcsl::eval::graph::{EffectJob, Graph, GraphHost, Node, NodeId, NodeKind, NodeState, STEP_UNITS};
use gcsl::eval::plain::Plain;
use gcsl::workspace::graph::RootField;
use gcsl::workspace::{InvokeError, Progress, StepInputs};
use gcsl::{Diagnostic, Span, Value};

use crate::events::{Event, EventKind, effect_label};
use crate::fetch::IN_A_STEP;
use crate::pool::{Pool, Ran, Work};
use crate::layers::remove_tree;
use crate::keychain::{self, Keychain};
use crate::load::Loaded;
use crate::platform::{ExecutionPlatform, HOST_PLATFORM};
use crate::receipt::{ReceiptOf, receipt};
use crate::record::{Kept, StepRecord, StoreError, keep_files, stored};
use crate::sandbox::{HostTools, HostView, OUTPUTS, StepSandbox, WORKSPACE};
use crate::store::{Digest, Store, TreeEntry};
use crate::target::Target;

/// The workspace's root file, whose `pekitconf` registers its toolchains.
const WORKSPACE_FILE: &str = "workspace.gcsl";

/// The field of the workspace's root file pekit reads its configuration
/// from.
const PEKITCONF: &str = "pekitconf";

/// The context the `toolchain` module registers toolchains in.
const TOOLCHAINS: &str = "toolchains";

/// The context of the `keychain` module's profile.
const KEYCHAIN_PROFILE: &str = "keychain_profile";

/// The context of the `keychain` module's configuration.
const KEYCHAIN_KEYS: &str = "keychain_keys";

/// How many symbolic links a path is followed through, as Linux follows
/// them, before it is refused.
const MAX_LINKS: u32 = 40;

/// What a path inside an output directory is (see [`Run::inside_output`]).
enum Inside {
    /// What the store keeps of it.
    Kept(Kept),
    /// The path a symbolic link on the way leads to, the rest of the path
    /// after it.
    Link(String),
}

/// Whether the path `path` is under the directory `dir`.
fn within_dir(path: &str, dir: &str) -> bool {
    path.strip_prefix(dir)
        .is_some_and(|rest| rest.starts_with('/') || dir == "/")
}

/// The components of the path `path`, last first, so that the next is
/// popped.
fn components(path: &str) -> Vec<String> {
    path.split('/')
        .filter(|part| !part.is_empty())
        .rev()
        .map(str::to_owned)
        .collect()
}

/// The path `base` followed by the components `pending`, last first.
fn rejoined(base: &str, pending: Vec<String>) -> String {
    let mut path = base.trim_end_matches('/').to_owned();
    for part in pending.into_iter().rev() {
        path.push('/');
        path.push_str(&part);
    }
    if path.is_empty() {
        "/".to_owned()
    } else {
        path
    }
}

/// How a build runs.
#[derive(Clone, Debug)]
pub struct Settings {
    /// The content store's directory.
    pub store: PathBuf,
    /// The output directory, `pekit-out/` at the workspace's root.
    pub outputs: PathBuf,
    /// Where steps are staged and temporary directories made.
    pub scratch: PathBuf,
    /// How many effects may run at once.
    pub jobs: usize,
    /// The platform steps run on, set as `^host_platform` where the
    /// workspace declares it (see [Platforms](crate::platform#platforms)).
    pub execution: ExecutionPlatform,
    /// The platform as receipts name it: the execution platform, and the
    /// tools steps run with.
    pub platform: String,
    /// The host tools every step's sandbox borrows, which make its steps
    /// not hermetic: a host snapshot's, or a test's. None for a hermetic
    /// build.
    pub host_tools: HostTools,
    /// Whether a step found in the store is reused: not when nothing says
    /// when the tools steps run with change (see
    /// [The host snapshot](crate::host#the-host-snapshot)). Every step that
    /// succeeds is recorded either way.
    pub reuse: bool,
    /// The keychain's profile, set as `^keychain_profile` where the
    /// workspace declares it (see [Secrets](crate::keychain#secrets)).
    pub profile: String,
    /// This machine's own state, never shared as a store can be: the key
    /// secrets' identities are hashed with.
    pub state: PathBuf,
}

/// What building a target came to.
#[derive(Clone, Debug, PartialEq)]
pub struct Built {
    /// Its value, as plain data.
    pub value: Plain,
    /// How many steps ran.
    pub ran: usize,
    /// How many steps were found in the store.
    pub cached: usize,
    /// The steps that passed only on a retry, by name.
    pub flaky: Vec<String>,
}

/// Why a build failed.
#[derive(Debug)]
pub enum BuildError {
    /// The target could not be invoked, or its evaluation failed: the
    /// diagnostics, or a request's error.
    Invoke(InvokeError),
    /// What the build ran failed: a step that is not caught, say.
    Failed(Vec<Diagnostic>),
    /// Something of pekit's own failed: the store, the sandbox, a job.
    Engine(String),
}

impl BuildError {
    /// The error as the `pekit` command shows it, its diagnostics against
    /// `loaded`'s sources.
    #[must_use]
    pub fn render(&self, loaded: &Loaded) -> String {
        match self {
            BuildError::Invoke(InvokeError::Diagnostics(diagnostics))
            | BuildError::Failed(diagnostics) => loaded.render(diagnostics),
            BuildError::Invoke(InvokeError::Request(error)) => {
                format!("error: {}\n", error.message())
            }
            BuildError::Engine(message) => format!("error: {message}\n"),
        }
    }
}

/// Builds `target` of `loaded`'s workspace as `settings` say (see
/// [Building](#building)). Like all of GCSL's pipeline, it must run on a
/// thread with at least [`gcsl::STACK_SIZE`] bytes of stack.
///
/// # Errors
///
/// That the target's evaluation, or a step it needs, failed, or something of
/// pekit's own did.
pub fn build(
    loaded: &mut Loaded,
    target: &Target,
    settings: &Settings,
) -> Result<Built, BuildError> {
    build_observed(loaded, target, settings, &mut |_| {})
}

/// [`build`], telling `events` each [event](crate::events#events) as it
/// happens.
///
/// # Errors
///
/// Those of [`build`].
pub fn build_observed(
    loaded: &mut Loaded,
    target: &Target,
    settings: &Settings,
    events: &mut dyn FnMut(&Event),
) -> Result<Built, BuildError> {
    let store = Store::open(&settings.store).map_err(|error| engine(&error))?;
    std::fs::create_dir_all(&settings.scratch).map_err(|error| engine(&error))?;
    let keychain = Arc::new(Keychain::open(&settings.state).map_err(|error| {
        engine(&format!(
            "cannot open the keychain's key in `{}`: {error}",
            settings.state.display()
        ))
    })?);
    let host = Arc::new(
        HostView::new(
            &loaded.root,
            &settings.outputs,
            &target.definition.dir,
            &settings.scratch,
        )
        .with_keychain(Arc::clone(&keychain)),
    );
    let mut contexts = Vec::new();
    if loaded.workspace.declares_context(HOST_PLATFORM) {
        let platform = loaded.workspace.from_plain(&settings.execution.plain());
        contexts.push((HOST_PLATFORM.to_owned(), platform));
    }
    if loaded.workspace.declares_context(KEYCHAIN_PROFILE) {
        let profile = Value::Str(Rc::from(settings.profile.as_str()));
        contexts.push((KEYCHAIN_PROFILE.to_owned(), profile));
    }
    // The workspace's toolchains and keychain, from `workspace.gcsl`'s
    // `pekitconf`, where the module that declares each context is loaded
    // and the field is there.
    let mut fields = Vec::new();
    if let Some(file) = loaded.workspace.file(WORKSPACE_FILE) {
        for (context, path) in [
            (TOOLCHAINS, &["toolchains"] as &[&str]),
            (KEYCHAIN_KEYS, &["keychain"]),
        ] {
            if loaded.workspace.declares_context(context)
                && loaded.workspace.top_level_has(file, PEKITCONF, path)
            {
                fields.push(RootField {
                    context,
                    file,
                    field: PEKITCONF,
                    path,
                });
            }
        }
    }
    let call = loaded
        .workspace
        .prepare_call_with(
            (&target.def_type, &target.qualified()),
            &[],
            &contexts,
            &fields,
        )
        .map_err(BuildError::Invoke)?;
    let code_table: Rc<[(String, Digest)]> = loaded
        .names
        .iter()
        .zip(&loaded.sources)
        .map(|(name, source)| (name.clone(), Digest::of(source.text().as_bytes())))
        .collect();
    let mut run = Run {
        loaded,
        store,
        settings,
        host,
        graph: Graph::new(),
        outputs: HashMap::new(),
        steps: HashMap::new(),
        step_names: HashMap::new(),
        names_used: HashMap::new(),
        named: 0,
        pool: Pool::new(settings.jobs.max(1)),
        in_flight: HashMap::new(),
        events,
        began: Instant::now(),
        keychain,
        code_table,
        target_dir: target.qualified().trim_start_matches('/').to_owned(),
        ran: 0,
        cached: 0,
        flaky: Vec::new(),
    };
    let value = run.drive(&call)?;
    run.copy_out(&value)?;
    let value = run
        .loaded
        .workspace
        .to_plain(&value)
        .map_err(BuildError::Engine)?;
    Ok(Built {
        value,
        ran: run.ran,
        cached: run.cached,
        flaky: std::mem::take(&mut run.flaky),
    })
}

/// An engine error from an I/O failure.
fn engine(error: &dyn std::fmt::Display) -> BuildError {
    BuildError::Engine(error.to_string())
}

/// Whether a host takes a call of a function with `attributes` as a step:
/// if one is `hermetic`, not `hermetic(false)`.
fn hermetic(attributes: &[Attribute]) -> bool {
    attributes.iter().any(|attribute| {
        attribute.id.name() == "hermetic"
            && !attribute
                .args
                .iter()
                .any(|(name, value)| &**name == "on" && *value == Constant::Bool(false))
    })
}

/// No deferred call is a step: what runs in a step's own unit, inline.
fn inline(_: &[Attribute]) -> bool {
    false
}

/// A secret file a step is given: its path as the step sees it, and where
/// it is on the host (see [Secrets](crate::keychain#secrets)).
type SecretFile = (String, PathBuf);

/// A step the engine is running (see [Building](#building)).
struct StepRun {
    /// Its sandbox.
    sandbox: Arc<StepSandbox>,
    /// What it is given, by logical path, which each attempt is staged with.
    given: Vec<(String, Kept)>,
    /// The secret files it is given, by logical path, and where each is on
    /// the host, which each attempt has bound.
    secrets: Vec<SecretFile>,
    /// Its attempt's number, from 0.
    attempt: u32,
    /// Its receipt.
    receipt: Digest,
    /// When its first attempt began.
    started: Instant,
    /// When its attempts must end, by its `#[timeout]`.
    deadline: Option<Instant>,
    /// How many more attempts it may have.
    retries: u32,
    /// Whether an attempt of it has failed.
    failed_once: bool,
    /// The effect its unit stopped for, if it did.
    waiting: Option<NodeId>,
}

/// One build's state (see [Building](#building)).
struct Run<'a> {
    /// The workspace.
    loaded: &'a mut Loaded,
    /// The store.
    store: Store,
    /// How the build runs.
    settings: &'a Settings,
    /// The host's view, for what is not hermetic.
    host: Arc<HostView>,
    /// The action graph.
    graph: Graph,
    /// What the store keeps of each output a step made, by its logical
    /// path, and of each workspace input hashed so far.
    outputs: HashMap<String, Kept>,
    /// The steps running, by node.
    steps: HashMap<NodeId, StepRun>,
    /// Each step's name, `<function>` or `<function>.<n>`, by node.
    step_names: HashMap<NodeId, String>,
    /// How many steps of each function's name have been named.
    names_used: HashMap<String, u32>,
    /// How many of the graph's nodes have been looked at to name steps.
    named: usize,
    /// The pool effects run on.
    pool: Pool,
    /// The effects running, by node.
    in_flight: HashMap<NodeId, InFlight>,
    /// What is told each event.
    events: &'a mut dyn FnMut(&Event),
    /// When the build began.
    began: Instant,
    /// The keychain, which knows where each secret file given is.
    keychain: Arc<Keychain>,
    /// Each source's name and contents' digest, by input number: what a
    /// step's code and the functions it reads are keyed by.
    code_table: Rc<[(String, Digest)]>,
    /// The target's directory under `/pekit-out`, its qualified name.
    target_dir: String,
    /// How many steps ran.
    ran: usize,
    /// How many steps were found in the store.
    cached: usize,
    /// The steps that passed only on a retry, by name.
    flaky: Vec<String>,
}

/// A file or directory `plain`, the result of `step`, names that is
/// neither in its own output directory nor one it was given, nor in one it
/// was given: one whose path another step's file might have.
fn foreign_file<'a>(plain: &'a Plain, step: &StepRun) -> Option<&'a str> {
    let own = step.sandbox.own();
    let within = |path: &str, dir: &str| {
        path == dir
            || path
                .strip_prefix(dir)
                .is_some_and(|rest| rest.starts_with('/'))
    };
    // A fetched file and an unpacked directory are named by their contents.
    let shared = |path: &str| {
        [".fetch", ".unpack"]
            .iter()
            .any(|kind| within(path, &format!("{OUTPUTS}/{kind}")))
    };
    plain
        .files()
        .into_iter()
        .map(|(path, _)| path)
        .find(|path| {
            !within(path, own)
                && !shared(path)
                && !step.given.iter().any(|(given, kept)| match kept {
                    Kept::File(_) => path == given,
                    Kept::Dir(_) => within(path, given),
                })
        })
}

impl Drop for Run<'_> {
    /// Removes the staging directory of every step still running, as when
    /// the build fails.
    fn drop(&mut self) {
        for (&id, step) in &self.steps {
            let _ = remove_tree(&self.stage_dir(id, step.attempt));
        }
    }
}

/// An effect running, as events name it.
struct InFlight {
    /// The step it is of, by node, or none for the target's own.
    of: Option<NodeId>,
    /// The step it is of, or none for the target's own.
    step: Option<String>,
    /// What it runs ([`effect_label`]).
    effect: String,
}

/// Where the target's unit is.
enum Main {
    /// It may run.
    Runnable,
    /// It stopped for this node's result.
    Paused(NodeId),
    /// It ended with this value, whose holes are still waited for.
    Holding(Value),
}

impl Run<'_> {
    /// Runs the target's unit, the steps it needs and their effects, until
    /// its value has no holes: that value.
    fn drive(&mut self, call: &gcsl::workspace::graph::PreparedCall) -> Result<Value, BuildError> {
        let mut main = Main::Runnable;
        loop {
            if let Main::Runnable = main {
                let host = GraphHost {
                    defers: &hermetic,
                    effects: &*self.host,
                };
                main = match self
                    .loaded
                    .workspace
                    .run_prepared(call, &mut self.graph, host)
                    .map_err(BuildError::Invoke)?
                {
                    Progress::Paused(node) => Main::Paused(node),
                    Progress::Done(value) => Main::Holding(value),
                };
                self.name_new_steps();
            }
            let demands = match &main {
                Main::Paused(node) => vec![*node],
                Main::Holding(value) => {
                    let holes = self.graph.holes(value);
                    if holes.is_empty() {
                        let failures = self.graph.failures(value);
                        if failures.is_empty() {
                            return Ok(value.clone());
                        }
                        return Err(BuildError::Failed(failures));
                    }
                    holes
                }
                Main::Runnable => Vec::new(),
            };
            let mut progressed = self.start_needed(&demands)?;
            progressed |= self.advance_steps()?;
            if !self.in_flight.is_empty() {
                let Some(ran) = self.pool.next() else {
                    return Err(BuildError::Engine(
                        "the threads effects run on stopped".to_owned(),
                    ));
                };
                self.finished_effect(ran);
                progressed = true;
            }
            if let Main::Paused(node) = main
                && !self.is_waiting(node)
            {
                main = Main::Runnable;
                progressed = true;
            }
            if !progressed {
                return Err(BuildError::Engine(
                    "the build is waiting for nothing: a step it needs never became ready"
                        .to_owned(),
                ));
            }
        }
    }

    /// Whether the node `id` is still waiting for its result.
    fn is_waiting(&self, id: NodeId) -> bool {
        self.graph
            .node(id)
            .is_some_and(|node| *node.state() == NodeState::Waiting)
    }

    /// Starts every step `demands` need that is ready and not started, and
    /// runs every effect of the target's unit they need: whether it did any.
    fn start_needed(&mut self, demands: &[NodeId]) -> Result<bool, BuildError> {
        let mut started = false;
        let needed = self.graph.needed(demands);
        for id in self.graph.ready(&needed) {
            if self.steps.contains_key(&id) || self.in_flight.contains_key(&id) {
                continue;
            }
            let Some(node) = self.graph.node(id) else {
                continue;
            };
            // What needs a failed result fails with its failure, unrun.
            let failed =
                node.data()
                    .iter()
                    .find_map(|&edge| match self.graph.node(edge)?.state() {
                        NodeState::Failed(failure) => Some(failure.clone()),
                        _ => None,
                    });
            if let Some(failure) = failed {
                let is_step = node.kind() == NodeKind::Deferred;
                self.graph.resolve(id, Err(failure));
                if is_step {
                    let step = self.step_name(id);
                    self.emit(EventKind::Failed {
                        step,
                        retrying: false,
                    });
                }
                started = true;
                continue;
            }
            match node.kind() {
                NodeKind::Deferred => self.start_step(id)?,
                NodeKind::Effect(_) if node.unit() < STEP_UNITS => {
                    let host: Arc<dyn Effects + Send + Sync> = self.host.clone();
                    self.dispatch(id, host, None, None)?;
                }
                NodeKind::Effect(_) => continue,
            }
            started = true;
        }
        Ok(started)
    }

    /// Sends the effect `id`, of the step `step` or of the target's own
    /// unit, to the pool, to run under `effects` until `deadline`.
    fn dispatch(
        &mut self,
        id: NodeId,
        effects: Arc<dyn Effects + Send + Sync>,
        deadline: Option<Instant>,
        step: Option<NodeId>,
    ) -> Result<(), BuildError> {
        let job = self
            .loaded
            .workspace
            .effect_job(&self.graph, id)
            .map_err(BuildError::Engine)?;
        let running = InFlight {
            of: step,
            step: step.map(|step| self.step_name(step)),
            effect: effect_label(&job),
        };
        // A step is given its secrets, and never asks the keychain itself.
        if step.is_some()
            && matches!(
                job.builtin(),
                Builtin::KeychainString | Builtin::KeychainFile
            )
        {
            let span = self.graph.node(id).map_or(Span::new(0, 0), Node::span);
            self.finish_now(id, Err(Diagnostic::error(keychain::IN_A_STEP, span)));
            return Ok(());
        }
        // A fetch is pekit's to answer, and never a step's.
        let work = if job.builtin() == Builtin::Fetch {
            let span = self.graph.node(id).map_or(Span::new(0, 0), Node::span);
            if step.is_some() {
                self.finish_now(id, Err(Diagnostic::error(IN_A_STEP, span)));
                return Ok(());
            }
            let text = |at: usize| match job.args().get(at) {
                Some(Plain::Str(text)) => text.clone(),
                _ => String::new(),
            };
            Work::Fetch {
                url: text(0),
                sha256: text(1),
                store: self.store.clone(),
                span,
            }
        } else if job.builtin() == Builtin::ArchiveUnpack {
            let span = self.graph.node(id).map_or(Span::new(0, 0), Node::span);
            match self.unpack_work(&job, span) {
                Ok(work) => work,
                Err(message) => {
                    self.finish_now(id, Err(Diagnostic::error(message, span)));
                    return Ok(());
                }
            }
        } else {
            Work::Effect(job, effects)
        };
        self.emit(EventKind::EffectQueued {
            node: id.number(),
            step: running.step.clone(),
            effect: running.effect.clone(),
        });
        self.pool.send(id, work, deadline);
        self.in_flight.insert(id, running);
        Ok(())
    }

    /// The work of the `archive.unpack` `job`, its call at `span`: its archive
    /// as the store keeps it, a workspace file's or an output's.
    ///
    /// # Errors
    ///
    /// GCSL's own error of a `strip` below 0, or that the archive cannot be
    /// read.
    fn unpack_work(&mut self, job: &EffectJob, span: Span) -> Result<Work, String> {
        let (Some(Plain::File { path, .. }), Some(&Plain::Int(strip))) =
            (job.args().first(), job.args().get(1))
        else {
            return Err("`archive.unpack` was given what is no archive".to_owned());
        };
        let Ok(strip) = u32::try_from(strip) else {
            return Err(format!(
                "`archive.unpack` of `{path}` cannot leave off {strip} components: `strip` is 0 \
                 or more"
            ));
        };
        let Kept::File(archive) = self.input(path, false)? else {
            return Err(format!("`archive.unpack` of `{path}`: it is no file"));
        };
        Ok(Work::Unpack {
            archive,
            shown: path.clone(),
            strip,
            store: self.store.clone(),
            scratch: self.settings.scratch.clone(),
            span,
        })
    }

    /// Whether `path` is the host snapshot's (see [The host
    /// snapshot](crate::host#the-host-snapshot)): `/` or under a directory it
    /// binds, when steps borrow it; a step's input that is not staged, since
    /// every step binds it already, the receipt's platform naming the
    /// snapshot's fingerprint.
    fn is_host_path(&self, path: &str) -> bool {
        let bound = &self.settings.host_tools.bind;
        !bound.is_empty()
            && !path.starts_with(&format!("{WORKSPACE}/"))
            && !path.starts_with(&format!("{OUTPUTS}/"))
            && (path == "/" || bound.iter().any(|dir| Path::new(path).starts_with(dir)))
    }

    /// What the store keeps of `path` if it is inside a directory an output
    /// of this build is, a file a step listed of another's output, say: found
    /// through the directory's tree, a component at a time, so that a
    /// symbolic link on the way is followed from the directory it is in, as
    /// the system follows it, and `..` leaves the directory reached. A link
    /// whose target is absolute, or a `..` out of the output, leads to a path
    /// of its own, which [`Run::input`] looks up afresh. `None` if `path` is
    /// in no output, or is not in its tree.
    fn inside_output(&self, path: &str) -> Option<Inside> {
        let (top, tree) = self
            .outputs
            .iter()
            .filter_map(|(at, kept)| match kept {
                Kept::Dir(tree) => Some((at, *tree)),
                Kept::File(_) => None,
            })
            .filter(|(at, _)| {
                path.strip_prefix(at.as_str())
                    .is_some_and(|rest| rest.starts_with('/'))
            })
            .max_by_key(|(at, _)| at.len())?;
        // The components still to follow, the next last; the trees from the
        // output's down to the directory reached.
        let mut pending = components(&path[top.len()..]);
        let mut trees = vec![tree];
        let mut links = 0;
        while let Some(part) = pending.pop() {
            match part.as_str() {
                "." => continue,
                ".." if trees.len() == 1 => {
                    let above = top.rsplit_once('/').map_or("", |(above, _)| above);
                    return Some(Inside::Link(rejoined(above, pending)));
                }
                ".." => {
                    trees.pop();
                    continue;
                }
                _ => {}
            }
            let current = *trees.last()?;
            let entry = self
                .store
                .read_tree(&current)
                .ok()?
                .entries
                .into_iter()
                .find(|entry| match entry {
                    TreeEntry::File { name, .. }
                    | TreeEntry::Dir { name, .. }
                    | TreeEntry::Link { name, .. } => *name == part,
                })?;
            match entry {
                TreeEntry::Dir { tree, .. } => trees.push(tree),
                TreeEntry::File { file, .. } => {
                    return pending.is_empty().then_some(Inside::Kept(Kept::File(file)));
                }
                TreeEntry::Link { target, .. } => {
                    links += 1;
                    if links > MAX_LINKS {
                        return None;
                    }
                    if target.starts_with('/') {
                        return Some(Inside::Link(rejoined(&target, pending)));
                    }
                    pending.extend(components(&target));
                }
            }
        }
        Some(Inside::Kept(Kept::Dir(*trees.last()?)))
    }

    /// Puts what the store keeps of the output `path` under the output
    /// directory, hard-linked and read-only, in place of what an earlier
    /// build left there, so that what is not hermetic reads it mid-build, as
    /// the target's own `dir.files()` does.
    fn expose(&self, path: &str, kept: Kept) -> Result<(), BuildError> {
        let Some(at) = output_path(&self.settings.outputs, path) else {
            return Ok(());
        };
        let placed = (|| {
            if fs::symlink_metadata(&at).is_ok_and(|meta| meta.is_dir()) {
                remove_tree(&at)?;
            } else if fs::symlink_metadata(&at).is_ok() {
                fs::remove_file(&at)?;
            }
            match kept {
                Kept::File(entry) => self.store.materialise_file(&entry, &at),
                Kept::Dir(tree) => self.store.materialise_tree(&tree, &at),
            }
        })();
        placed.map_err(|error| engine(&format!("cannot write `{}`: {error}", at.display())))
    }

    /// Gives the node `id` `outcome` at once, the step that waits on it able
    /// to run again.
    fn finish_now(&mut self, id: NodeId, outcome: Result<Value, Diagnostic>) {
        self.graph.resolve(id, outcome);
        for step in self.steps.values_mut() {
            if step.waiting == Some(id) {
                step.waiting = None;
            }
        }
    }

    /// Tells the event `kind`, now.
    fn emit(&mut self, kind: EventKind) {
        let at = self.began.elapsed();
        (self.events)(&Event { at, kind });
    }

    /// Names every step the graph has that is not named, in the order the
    /// target's code met them, which is the same in every build whatever
    /// order steps finish in: its function's name, and its number among the
    /// target's steps of that name after the first, `compile.2`.
    fn name_new_steps(&mut self) {
        let mut found = Vec::new();
        for (id, node) in self.graph.nodes().skip(self.named) {
            if node.kind() != NodeKind::Deferred {
                continue;
            }
            let function = node
                .closure()
                .and_then(|closure| closure.code.find_function(closure.function))
                .and_then(|code| code.name_text.as_deref())
                .unwrap_or("step")
                .to_owned();
            found.push((id, function));
        }
        self.named = self.graph.len();
        for (id, function) in found {
            let count = self.names_used.entry(function.clone()).or_insert(0);
            *count += 1;
            let name = if *count == 1 {
                function
            } else {
                format!("{function}.{count}")
            };
            self.step_names.insert(id, name.clone());
            self.emit(EventKind::Discovered { step: name });
        }
    }

    /// The name of the step `id` ([`Run::name_new_steps`]).
    fn step_name(&self, id: NodeId) -> String {
        self.step_names
            .get(&id)
            .cloned()
            .unwrap_or_else(|| format!("step-{}", id.number()))
    }

    /// The secret files `inputs` hold, in their arguments, captures or
    /// context, at any depth, each by its path as a step sees it and where
    /// it is on the host: bound into the step's sandbox, never staged, since
    /// staging stores (see [Secrets](crate::keychain#secrets)). A
    /// keychain's file is where the keychain found it, and a workspace
    /// file where it is in the workspace.
    ///
    /// # Errors
    ///
    /// A secret's file that is neither.
    fn secret_files(&self, inputs: &StepInputs) -> Result<Vec<SecretFile>, String> {
        let mut found: Vec<SecretFile> = Vec::new();
        let mut pending: Vec<&Plain> = inputs
            .args
            .iter()
            .chain(&inputs.captures)
            .chain(inputs.context.iter().map(|(_, value)| value))
            .collect();
        while let Some(plain) = pending.pop() {
            let value = match plain {
                Plain::Secret { value, .. } => value,
                Plain::Record(fields) => {
                    pending.extend(fields.iter().map(|(_, value)| value));
                    continue;
                }
                Plain::List(items) => {
                    pending.extend(items);
                    continue;
                }
                Plain::Variant {
                    payload: Some(payload),
                    ..
                } => {
                    pending.push(payload);
                    continue;
                }
                Plain::Closure { captures, .. } => {
                    pending.extend(captures);
                    continue;
                }
                _ => continue,
            };
            for (path, _) in value.files() {
                if found.iter().any(|(known, _)| known == path) {
                    continue;
                }
                let host = match self.keychain.host_file(path) {
                    Some(host) => host,
                    None => match path
                        .strip_prefix(WORKSPACE)
                        .and_then(|rest| rest.strip_prefix('/'))
                    {
                        Some(rest) => self.loaded.root.join(rest),
                        None => {
                            return Err(format!(
                                "`{path}` is a secret's file a step cannot be given: only the \
                                 keychain's and the workspace's are"
                            ));
                        }
                    },
                };
                found.push((path.to_owned(), host));
            }
        }
        Ok(found)
    }

    /// Starts the step `id`: from its record if the store has one for its
    /// receipt, else in a sandbox of its own, its inputs staged.
    fn start_step(&mut self, id: NodeId) -> Result<(), BuildError> {
        let inputs = self
            .loaded
            .workspace
            .step_inputs(&self.graph, id)
            .map_err(BuildError::Engine)?;
        let code: Vec<(String, Digest)> = inputs
            .code_files
            .iter()
            .filter_map(|&file| self.code_table.get(file as usize).cloned())
            .collect();
        let name = self.step_name(id);
        let own = format!("{}/{name}", self.target_dir);
        let own_path = format!("{OUTPUTS}/{own}");
        let mut given: Vec<(String, Kept)> = Vec::new();
        let table = Rc::clone(&self.code_table);
        let code_of = move |input: u32| table.get(input as usize).cloned();
        let of = ReceiptOf {
            code: &code,
            args: &inputs.args,
            captures: &inputs.captures,
            context: &inputs.context,
            own: &own_path,
            code_of: &code_of,
            platform: &self.settings.platform,
        };
        let receipt = {
            let mut contents = |path: &str, dir: bool| -> Result<Digest, String> {
                // The host snapshot's own, which every step binds already,
                // keyed by its path on top of the snapshot's fingerprint.
                if self.is_host_path(path) {
                    return Ok(Digest::of(format!("host-snapshot:{path}").as_bytes()));
                }
                let kept = self.input(path, dir)?;
                given.push((path.to_owned(), kept));
                Ok(match kept {
                    Kept::File(entry) => entry.digest,
                    Kept::Dir(tree) => tree,
                })
            };
            receipt(of, &mut contents).map_err(BuildError::Engine)?
        };
        let found = if self.settings.reuse {
            self.store
                .record::<StepRecord>(&receipt)
                .map_err(|error| engine(&error))?
        } else {
            None
        };
        if let Some(record) = found {
            for (path, kept) in record.result.files() {
                self.outputs.insert(path.to_owned(), kept);
            }
            let value = self.loaded.workspace.from_plain(&record.result.plain());
            self.graph.resolve(id, Ok(value));
            self.cached += 1;
            self.emit(EventKind::Cached { step: name });
            return Ok(());
        }
        let (retries, timeout) = (retry_of(&inputs.attributes), timeout_of(&inputs.attributes));
        let secrets = self.secret_files(&inputs).map_err(BuildError::Engine)?;
        let sandbox = self.fresh_sandbox(id, &own, 0, (&given, &secrets))?;
        let started = Instant::now();
        self.steps.insert(
            id,
            StepRun {
                sandbox,
                given,
                secrets,
                attempt: 0,
                receipt,
                started,
                deadline: timeout.map(|limit| started + limit),
                retries,
                failed_once: false,
                waiting: None,
            },
        );
        self.emit(EventKind::Started {
            step: name,
            attempt: 0,
        });
        Ok(())
    }

    /// A new sandbox for the attempt `attempt` of the step `id`, whose own
    /// output directory is `own`, `<target>/<step>` under `/pekit-out`,
    /// staged with `given`; nothing left behind if it cannot be made.
    fn fresh_sandbox(
        &self,
        id: NodeId,
        own: &str,
        attempt: u32,
        (given, secrets): (&[(String, Kept)], &[SecretFile]),
    ) -> Result<Arc<StepSandbox>, BuildError> {
        let stage = self.stage_dir(id, attempt);
        if stage.exists() {
            remove_tree(&stage).map_err(|error| engine(&error))?;
        }
        let staged = (|| {
            let sandbox = StepSandbox::new(&stage, own)
                .map_err(|error| engine(&error))?
                .with_host(&self.settings.host_tools)
                .with_secrets(secrets.to_vec());
            // Each directory before what is in it, which it stages: so a
            // file of a staged directory is as the directory's tree has it, a
            // symbolic link a link, not the file it leads to.
            let mut ordered: Vec<&(String, Kept)> = given.iter().collect();
            ordered.sort_by(|a, b| a.0.cmp(&b.0));
            let mut dirs: Vec<&str> = Vec::new();
            for (path, kept) in ordered {
                if dirs.iter().any(|dir| within_dir(path, dir)) {
                    continue;
                }
                sandbox
                    .stage(&self.store, path, *kept)
                    .map_err(|error| engine(&format!("cannot stage `{path}`: {error}")))?;
                if matches!(kept, Kept::Dir(_)) {
                    dirs.push(path);
                }
            }
            Ok(Arc::new(sandbox))
        })();
        if staged.is_err() {
            let _ = remove_tree(&stage);
        }
        staged
    }

    /// Where the attempt `attempt` of the step `id` is staged.
    fn stage_dir(&self, id: NodeId, attempt: u32) -> PathBuf {
        self.settings.scratch.join(format!(
            "step-{}-{}-{attempt}",
            std::process::id(),
            id.number()
        ))
    }

    /// Removes the staging directory of the step `step`, the node `id`, once
    /// it is done with.
    fn unstage(&self, id: NodeId, step: &StepRun) -> Result<(), BuildError> {
        let stage = self.stage_dir(id, step.attempt);
        if stage.exists() {
            remove_tree(&stage).map_err(|error| engine(&error))?;
        }
        Ok(())
    }

    /// What the store keeps of the input at `path`, a file or, if `dir`, a
    /// directory: an output of a step of this build, or a workspace file's,
    /// put into the store. Never what an earlier build left in the output
    /// directory, which might be stale.
    fn input(&mut self, path: &str, dir: bool) -> Result<Kept, String> {
        self.input_through(path, dir, 0)
    }

    /// As [`Run::input`], `path` reached through `links` symbolic links that
    /// led to paths of their own.
    fn input_through(&mut self, path: &str, dir: bool, links: u32) -> Result<Kept, String> {
        if let Some(kept) = self.outputs.get(path) {
            return Ok(*kept);
        }
        match self.inside_output(path) {
            Some(Inside::Kept(kept)) => {
                self.outputs.insert(path.to_owned(), kept);
                return Ok(kept);
            }
            Some(Inside::Link(to)) if links < MAX_LINKS => {
                let kept = self
                    .input_through(&to, dir, links + 1)
                    .map_err(|error| format!("`{path}` is a symbolic link to `{to}`: {error}"))?;
                self.outputs.insert(path.to_owned(), kept);
                return Ok(kept);
            }
            Some(Inside::Link(_)) => {
                return Err(format!(
                    "`{path}` is reached through too many symbolic links"
                ));
            }
            None => {}
        }
        let Some(rest) = path
            .strip_prefix(WORKSPACE)
            .and_then(|rest| rest.strip_prefix('/'))
        else {
            if path == OUTPUTS || path.starts_with(&format!("{OUTPUTS}/")) {
                return Err(format!("`{path}` is no output of any step of this build"));
            }
            return Err(format!(
                "`{path}` is outside the workspace: a hermetic step is given only the \
                 workspace's files and other steps' outputs"
            ));
        };
        let host = self.loaded.root.join(rest);
        let kept = if dir {
            Kept::Dir(
                self.store
                    .put_tree(&host)
                    .map_err(|error| format!("cannot read `{path}`: {error}"))?,
            )
        } else {
            Kept::File(
                self.store
                    .put_file(&host)
                    .map_err(|error| format!("cannot read `{path}`: {error}"))?,
            )
        };
        self.outputs.insert(path.to_owned(), kept);
        Ok(kept)
    }

    /// Runs every step's unit that may progress, until each stops for an
    /// effect, which goes to the pool, or ends: whether any did.
    fn advance_steps(&mut self) -> Result<bool, BuildError> {
        let runnable: Vec<NodeId> = self
            .steps
            .iter()
            .filter(|(_, step)| step.waiting.is_none())
            .map(|(&id, _)| id)
            .collect();
        let advanced = !runnable.is_empty();
        for id in runnable {
            self.advance_step(id)?;
        }
        Ok(advanced)
    }

    /// Runs the step `id`'s unit once.
    fn advance_step(&mut self, id: NodeId) -> Result<(), BuildError> {
        let Some(step) = self.steps.get(&id) else {
            return Ok(());
        };
        let sandbox = Arc::clone(&step.sandbox);
        let deadline = step.deadline;
        let host = GraphHost {
            defers: &inline,
            effects: &*sandbox,
        };
        match self.loaded.workspace.run_step(&mut self.graph, id, host) {
            Ok(Progress::Paused(effect)) => {
                if let Some(step) = self.steps.get_mut(&id) {
                    step.waiting = Some(effect);
                }
                let effects: Arc<dyn Effects + Send + Sync> = sandbox;
                self.dispatch(effect, effects, deadline, Some(id))
            }
            Ok(Progress::Done(value)) => self.succeeded(id, &value),
            Err(failure) => self.failed(id, failure),
        }
    }

    /// Records the step `id`, which gave `value`, and gives it to the graph.
    fn succeeded(&mut self, id: NodeId, value: &Value) -> Result<(), BuildError> {
        let Some(step) = self.steps.remove(&id) else {
            return Ok(());
        };
        let outcome = self.record(id, &step, value);
        self.unstage(id, &step)?;
        outcome
    }

    /// Records the step `step`, the node `id`, which gave `value`, and gives
    /// it to the graph, its outputs kept.
    fn record(&mut self, id: NodeId, step: &StepRun, value: &Value) -> Result<(), BuildError> {
        let plain = self
            .loaded
            .workspace
            .to_plain(value)
            .map_err(BuildError::Engine)?;
        if let Some(path) = foreign_file(&plain, step) {
            let message = format!(
                "the step's result names `{path}`, which is neither in its output directory \
                 `{}` nor given to it: a step's files must be one or the other, so that no \
                 other step's have its paths",
                step.sandbox.own()
            );
            self.graph
                .resolve(id, Err(Diagnostic::error(message, Span::new(0, 0))));
            let step = self.step_name(id);
            self.emit(EventKind::Failed {
                step,
                retrying: false,
            });
            return Ok(());
        }
        let host = |path: &str| step.sandbox.host_path(path).unwrap_or_default();
        // Its files are kept whatever it holds: later steps are given them.
        let kept = match keep_files(&plain, &self.store, &host) {
            Ok(kept) => kept,
            Err(StoreError::Io { path, error }) => {
                let message =
                    format!("the step's result names `{path}`, which it did not make: {error}");
                self.graph
                    .resolve(id, Err(Diagnostic::error(message, Span::new(0, 0))));
                let step = self.step_name(id);
                self.emit(EventKind::Failed {
                    step,
                    retrying: false,
                });
                return Ok(());
            }
            Err(other) => return Err(BuildError::Engine(format!("{other:?}"))),
        };
        for (path, entry) in &kept {
            self.outputs.insert(path.clone(), *entry);
            self.expose(path, *entry)?;
        }
        match stored(&plain, &kept) {
            Ok(result) => {
                let record = StepRecord {
                    receipt: step.receipt,
                    result,
                    duration_ms: u64::try_from(step.started.elapsed().as_millis())
                        .unwrap_or(u64::MAX),
                };
                self.store
                    .put_record(&step.receipt, &record)
                    .map_err(|error| engine(&error))?;
            }
            // A secret is never stored: the step runs every time.
            Err(StoreError::Secret) => {}
            Err(other) => return Err(BuildError::Engine(format!("{other:?}"))),
        }
        if step.failed_once {
            let name = self.step_names.get(&id).cloned().unwrap_or_default();
            self.flaky.push(name);
        }
        self.ran += 1;
        self.graph.resolve(id, Ok(value.clone()));
        let step = self.step_name(id);
        self.emit(EventKind::Finished { step });
        Ok(())
    }

    /// The step `id` failed with `failure`: run again afresh if its
    /// `#[retry]` allows and its time has not run out, else its failure.
    fn failed(&mut self, id: NodeId, failure: Diagnostic) -> Result<(), BuildError> {
        let retry = self.steps.get(&id).is_some_and(|step| {
            step.retries > 0
                && step
                    .deadline
                    .is_none_or(|deadline| Instant::now() < deadline)
        });
        let name = self.step_name(id);
        self.emit(EventKind::Failed {
            step: name.clone(),
            retrying: retry,
        });
        if !retry {
            if let Some(step) = self.steps.remove(&id) {
                self.unstage(id, &step)?;
            }
            self.graph.resolve(id, Err(failure));
            return Ok(());
        }
        let Some(step) = self.steps.remove(&id) else {
            return Ok(());
        };
        self.unstage(id, &step)?;
        let own = format!("{}/{name}", self.target_dir);
        let attempt = step.attempt + 1;
        let sandbox = self.fresh_sandbox(id, &own, attempt, (&step.given, &step.secrets))?;
        self.graph.forget_unit(STEP_UNITS + id.number());
        self.steps.insert(
            id,
            StepRun {
                sandbox,
                attempt,
                retries: step.retries - 1,
                failed_once: true,
                waiting: None,
                ..step
            },
        );
        self.emit(EventKind::Started {
            step: name,
            attempt,
        });
        Ok(())
    }

    /// The effect `ran` finished: gives it to the graph, the step that waits
    /// on it able to run again.
    fn finished_effect(&mut self, ran: Ran) {
        let node = ran.node;
        let of = self.in_flight.get(&node).and_then(|running| running.of);
        if let Some(running) = self.in_flight.remove(&node) {
            let at = ran.ended.saturating_duration_since(self.began);
            (self.events)(&Event {
                at,
                kind: EventKind::EffectRan {
                    node: node.number(),
                    step: running.step,
                    effect: running.effect,
                    worker: ran.worker,
                    began: ran.began.saturating_duration_since(self.began),
                    ok: ran.outcome.is_ok(),
                },
            });
        }
        let mut outcome = ran.outcome;
        for (path, kept) in ran.kept {
            // What a step unpacked is in its own sandbox; what the target
            // fetched or unpacked is under the output directory.
            let placed = match of.and_then(|step| self.steps.get(&step)) {
                Some(step) => step
                    .sandbox
                    .stage(&self.store, &path, kept)
                    .map_err(|error| engine(&error)),
                None => self.expose(&path, kept),
            };
            if let Err(BuildError::Engine(message)) = placed {
                outcome = Err(Diagnostic::error(message, Span::new(0, 0)));
            }
            self.outputs.insert(path, kept);
        }
        let value = outcome.map(|plain| self.loaded.workspace.from_plain(&plain));
        self.finish_now(node, value);
    }

    /// Copies every output under `/pekit-out` that `value` names out to the
    /// output directory, each file a copy of its own.
    fn copy_out(&self, value: &Value) -> Result<(), BuildError> {
        let plain = self
            .loaded
            .workspace
            .to_plain(value)
            .map_err(BuildError::Engine)?;
        for (path, _) in plain.files() {
            let Some(at) = output_path(&self.settings.outputs, path) else {
                continue;
            };
            // What no step made, the target's own effects wrote there.
            let Some(kept) = self.outputs.get(path) else {
                continue;
            };
            let _ = std::fs::remove_file(&at);
            let _ = std::fs::remove_dir_all(&at);
            let copied = match kept {
                Kept::File(entry) => self.store.copy_file(entry, &at),
                Kept::Dir(tree) => self.store.copy_tree(tree, &at),
            };
            copied.map_err(|error| engine(&format!("cannot write `{}`: {error}", at.display())))?;
        }
        Ok(())
    }
}

/// The path under the output directory a logical output path is at.
#[must_use]
pub fn output_path(outputs: &Path, path: &str) -> Option<PathBuf> {
    let rest = path.strip_prefix(OUTPUTS)?.strip_prefix('/')?;
    Some(outputs.join(rest))
}
