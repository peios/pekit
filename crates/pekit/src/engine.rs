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
//! A unit runs again only once what it stopped for is given, carrying on
//! from where it stopped. A target or a step that stopped for another
//! unit's run, which computes a memoised call or a `lazy` it needs, runs
//! again once anything else in the build has run, a step, an effect, or a
//! unit that ended a memoised call (`Workspace::memo_ended`), as the other
//! may have; if nothing else can, the units wait for each other, and the
//! build fails. A step that
//! fails, by its code's own failure, is run again from its start in a fresh
//! sandbox as its `#[retry]` allows, and one that passes only then is
//! *flaky*; its `#[timeout]` spans every attempt. A step that succeeds is
//! recorded, unless its result holds a secret, and its outputs are kept by
//! their logical paths, so that the steps after it are staged with them. Once
//! the target's value has no holes, the files under `/pekit-out` it names are
//! copied out to the output directory, each a copy of its own, and linked
//! under the target's name.
//!
//! Several targets are one build ([`build_all_observed`]): each is a unit of
//! its own, against the one graph, so that a step two of them reach runs
//! once and their steps run at once. A step is reported as the target's
//! whose unit met it first, or met the step that met it; its directory is
//! named by what it is, `/pekit-out/.steps/<function>/<identity>`, the same
//! whichever targets reach it (see [Identities](crate::receipt#identities)),
//! and each target's results are linked under its own name too
//! ([`target_view`]). A target that fails, in its evaluation or by a step
//! it needs, fails alone. Under
//! `--no-dynamic`, a target whose unit stops for a step's result, a dynamic
//! dependency, fails there.

use std::collections::HashMap;
use std::fmt::Write as _;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::rc::Rc;
use std::sync::Arc;
use std::time::Instant;

use gcsl::check::{Attribute, Builtin, Constant, retry_of, timeout_of};
use gcsl::eval::Effects;
use gcsl::eval::graph::{
    EffectJob, Graph, GraphHost, Node, NodeId, NodeKind, NodeState, STEP_UNITS, StepReads,
};
use gcsl::eval::plain::Plain;
use gcsl::workspace::graph::{PreparedCall, RootField, RootWrap};
use gcsl::workspace::{Given, InvokeError, Progress, StepInputs};
use gcsl::{Diagnostic, Span, Value};

use crate::events::{Event, EventKind, effect_label};
use crate::evict::{Evicted, evict};
use crate::exposure::{Exposing, Exposures, OutputRead, within};
use crate::fetch::IN_A_STEP;
use crate::git_fetch;
use crate::pool::{Done, Pool, Ran, Work};
use crate::layers::remove_tree;
use crate::keychain::{self, Keychain};
use crate::load::Loaded;
use crate::platform::{ExecutionPlatform, HOST_PLATFORM, PLATFORM};
use crate::query::{StepDeps, TargetDeps, in_workspace};
use crate::receipt::{IdentityOf, ReceiptOf, identity, receipt};
use crate::record::{Kept, Manifest, StepRecord, StoreError, Stored, differences, manifest_key, stored};
use crate::remote::Remote;
use crate::scratch::Scratch;
use crate::sandbox::{HostTools, HostView, OUTPUTS, Pending, StepSandbox, WORKSPACE};
use crate::store::{Digest, Seen, Store, TreeEntry, clock_beside};
use crate::sweep::Sweeper;
use crate::target::Target;

mod programs;

use programs::{Before, Programs};

/// The workspace's root file, whose `pekitconf` registers its toolchains.
const WORKSPACE_FILE: &str = "workspace.gcsl";

/// The field of the workspace's root file pekit reads its configuration
/// from.
const PEKITCONF: &str = "pekitconf";

/// The context the `toolchain` module registers toolchains in.
const TOOLCHAINS: &str = "toolchains";

/// The context of the `keychain` module's profile.
const KEYCHAIN_PROFILE: &str = "keychain_profile";

/// The context the `libraries` module says what the lock pins in.
const PINNED_LIBRARIES: &str = "pinned_libraries";

/// The context of the `keychain` module's configuration.
const KEYCHAIN_KEYS: &str = "keychain_keys";

/// The context the `packaging` module lists the packagers in.
const PACKAGERS: &str = "packagers";

/// The directory under `/pekit-out` that steps' directories are in, each
/// `<function>/<identity>`.
pub const STEPS: &str = ".steps";

/// How many hex digits of its digest name a step's identity: 64 bits.
const IDENTITY_DIGITS: usize = 16;

/// Why a strict build refuses `job`, an effect of a target's own code, if it
/// does (see [`Settings::strict`]): it runs a program, writes, or reads a
/// path outside the workspace and the build's outputs, any of which could
/// make a release depend on the machine that built it.
fn strict_refusal(job: &EffectJob) -> Option<String> {
    let builtin = job.builtin();
    match builtin {
        Builtin::Fetch
        | Builtin::FetchGit
        | Builtin::ArchiveUnpack
        | Builtin::KeychainString
        | Builtin::KeychainFile => None,
        Builtin::FsExists
        | Builtin::FsRead
        | Builtin::File
        | Builtin::FileRead
        | Builtin::Dir
        | Builtin::DirFiles => {
            let path = match job.args().first() {
                Some(Plain::Str(path) | Plain::File { path, .. } | Plain::Dir { path, .. }) => {
                    path.as_str()
                }
                _ => "",
            };
            let inside = |root: &str| {
                path == root
                    || path
                        .strip_prefix(root)
                        .is_some_and(|rest| rest.starts_with('/'))
            };
            (!inside(WORKSPACE) && !inside(OUTPUTS)).then(|| {
                format!(
                    "a strict build reads only the workspace and the build's outputs, and this \
                     reads `{path}`"
                )
            })
        }
        _ => Some(format!(
            "a strict build refuses a target's own `{}`: a release's targets run programs and \
             write files only in steps",
            builtin.name()
        )),
    }
}

/// The context only a run of a script binds (see the prelude's `script`).
const SCRIPT_SESSION: &str = "script_session";

/// The packaging module's file, which packing gives each package to.
const PACKAGING_FILE: &str = "<stdlib>/packaging.gcsl";

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
    /// What becomes of a step whose receipt has a record ([`Reuse`]).
    pub reuse: Reuse,
    /// The keychain's profile, set as `^keychain_profile` where the
    /// workspace declares it (see [Secrets](crate::keychain#secrets)).
    pub profile: String,
    /// This machine's own state, never shared as a store can be: the key
    /// secrets' identities are hashed with.
    pub state: PathBuf,
    /// The platform built for, set as `^platform` where the workspace
    /// declares it; `None` for its default, the platform steps run on.
    pub building_for: Option<ExecutionPlatform>,
    /// Whether a target that waits for a step's result, a dynamic
    /// dependency, fails where it would wait.
    pub no_dynamic: bool,
    /// Whether the build is strict, as a release's is: a target's own
    /// effects may fetch, unpack, ask the keychain and read the workspace
    /// and the build's outputs, and nothing else (see [Building](#building)).
    /// The host tools a step borrows are the caller's to leave out.
    pub strict: bool,
    /// How large the store may be, in bytes, as the build ends: none if it
    /// may be as large as it likes (see [Eviction](crate::evict#eviction)).
    pub cache_limit: Option<u64>,
    /// Whether GCSL profiles the build's evaluation (see
    /// [Profiling](gcsl::eval::profile#profiling)), which
    /// [`gcsl::workspace::Workspace::profile_report`] then reports.
    pub profile_gcsl: bool,
    /// Which outputs of the build are put under the output directory (see
    /// [Exposure](crate::exposure#exposure)).
    pub exposing: Exposing,
    /// The remote cache, if there is one (see [The remote
    /// cache](crate::remote#the-remote-cache)): a step missed in the store
    /// is looked for in it, and, if it uploads, a step recorded is written
    /// to it; both only where steps are taken from the store
    /// ([`Reuse::Store`]).
    pub remote: Option<Arc<Remote>>,
}

/// What becomes of a step whose receipt has a record (see
/// [Building](#building)).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Reuse {
    /// Its record is its result, and it is not run.
    Store,
    /// It runs, and is recorded, as every step that succeeds is: when
    /// nothing says when the tools steps run with change (see [The host
    /// snapshot](crate::host#the-host-snapshot)), or a build asks to run
    /// every step.
    Rerun,
    /// It runs, and its result is compared with its record's, not
    /// recorded: one that differs is one of [`Outcomes::mismatches`], and
    /// the record stays as it was.
    Compare,
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
    /// How many of those were found in the remote cache first.
    pub found_remotely: usize,
    /// How many programs the steps that ran found in the store (see
    /// `design/additive.md`, F1).
    pub programs_found: usize,
    /// The steps that passed only on a retry, by name.
    pub flaky: Vec<String>,
}

/// What building several targets came to: each target's value, as plain
/// data, or why it failed, in the order the targets were given, and the
/// steps of them all.
#[derive(Debug)]
pub struct Outcomes {
    /// Each target's value or failure.
    pub targets: Vec<Result<Plain, BuildError>>,
    /// How many steps ran.
    pub ran: usize,
    /// How many steps were found in the store.
    pub cached: usize,
    /// How many of those were found in the remote cache first.
    pub found_remotely: usize,
    /// How many programs the steps that ran found in the store (see
    /// `design/additive.md`, F1).
    pub programs_found: usize,
    /// The steps that passed only on a retry, by name.
    pub flaky: Vec<String>,
    /// Why the remote cache could not be used, if it could not, as it was
    /// first found: which fails no build.
    pub remote_failed: Option<String>,
    /// What became of each step that was found in the store or passed.
    pub steps: Vec<StepReport>,
    /// Each step that, with [`Reuse::Compare`], gave another result than
    /// its record holds for the same receipt.
    pub mismatches: Vec<Mismatch>,
    /// What the store's eviction did as the build ended, if it was asked
    /// to: nothing, if it was not over its limit or another process had
    /// it; or why it failed, which fails no build.
    pub evicted: Option<Result<Option<Evicted>, String>>,
}

/// A step that gave another result than its record holds for the same
/// inputs: one that is not reproducible.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Mismatch {
    /// The step, by its directory under `/pekit-out` and name.
    pub step: String,
    /// What differs ([`differences`]).
    pub differences: Vec<String>,
}

/// What became of one step of a build that was found in the store or
/// passed (see [Building](#building)).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct StepReport {
    /// The target it was of, by its place among the targets given.
    pub target: usize,
    /// Its name, `<function>/<identity>`, as its directory under
    /// `/pekit-out/.steps` has it.
    pub name: String,
    /// Its function's name, a definition's last segment for a definition's
    /// body.
    pub function: String,
    /// Whether it was found in the store.
    pub cached: bool,
    /// Whether it passed only on a retry.
    pub flaky: bool,
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
    /// The target waited for a step's result, a dynamic dependency, which
    /// the build forbids: where it waited.
    Dynamic(Diagnostic),
    /// Built again from the same inputs, these steps gave other results.
    NotReproducible(Vec<Mismatch>),
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
            BuildError::Dynamic(diagnostic) => loaded.render(std::slice::from_ref(diagnostic)),
            BuildError::NotReproducible(mismatches) => {
                let mut shown = format!(
                    "error: {} not reproducible: run again with the same inputs, {} other \
                     outputs\n",
                    match mismatches.len() {
                        1 => "a step is".to_owned(),
                        many => format!("{many} steps are"),
                    },
                    if mismatches.len() == 1 {
                        "it gave"
                    } else {
                        "they gave"
                    },
                );
                for mismatch in mismatches {
                    let _ = writeln!(shown, "  {}", mismatch.step);
                    for difference in &mismatch.differences {
                        let _ = writeln!(shown, "    {difference}");
                    }
                }
                shown
            }
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
    let mut outcomes = build_all_observed(loaded, std::slice::from_ref(target), settings, events)?;
    let value = outcomes
        .targets
        .pop()
        .unwrap_or_else(|| Err(engine(&"a build of one target gave no outcome")))?;
    Ok(Built {
        value,
        ran: outcomes.ran,
        cached: outcomes.cached,
        found_remotely: outcomes.found_remotely,
        programs_found: outcomes.programs_found,
        flaky: outcomes.flaky,
    })
}

/// Builds `targets` of `loaded`'s workspace as one build, one graph, as
/// `settings` say, telling `events` each [event](crate::events#events) as
/// it happens (see [Building](#building)): a step two of them need runs
/// once, and their steps run at once. Each target comes to its value, or
/// fails apart from the others.
///
/// # Errors
///
/// That something of pekit's own failed, as no target's does.
pub fn build_all_observed(
    loaded: &mut Loaded,
    targets: &[Target],
    settings: &Settings,
    events: &mut dyn FnMut(&Event),
) -> Result<Outcomes, BuildError> {
    build_roots(loaded, targets, settings, (None, Vec::new()), events)
}

/// What packing does with each package (see [Building](#building)): packs
/// it with each packager, or packs it and publishes it into the store
/// `to`, a path or a URL as the packager reads it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Packing {
    /// Pack, the packaging module's `pack`.
    Pack,
    /// Pack and publish, the packaging module's `publish`.
    Publish {
        /// The store.
        to: String,
    },
}

/// Packs each of `targets`, definitions of `package`, of `loaded`'s
/// workspace with each packager its configuration lists, as one build, as
/// `packing` says: each target's value given to the packaging module's
/// `pack`, or `publish` with the store, the packagers `^packagers`, from
/// `pekitconf`. The store is an argument, not a root context, so that the
/// packing steps are those of any store. Each comes to a list of what each
/// packager packed (see [`build_all_observed`]).
///
/// # Errors
///
/// That the workspace loads no packaging module, so has no packager, or
/// those of [`build_all_observed`].
pub fn package_all_observed(
    loaded: &mut Loaded,
    targets: &[Target],
    (settings, packing): (&Settings, &Packing),
    events: &mut dyn FnMut(&Event),
) -> Result<Outcomes, BuildError> {
    let Some(file) = loaded.workspace.file(PACKAGING_FILE) else {
        return Err(BuildError::Engine(
            "this workspace has no packager: list one in `pekitconf.packagers`, from a \
             packager library such as gcslib's `ppk`"
                .to_owned(),
        ));
    };
    let (function, args) = match packing {
        Packing::Pack => ("pack", Vec::new()),
        Packing::Publish { to } => ("publish", vec![Given::Str(to.clone())]),
    };
    let wrap = RootWrap {
        file,
        function,
        args: &args,
    };
    build_roots(loaded, targets, settings, (Some(wrap), Vec::new()), events)
}

/// Runs the script `script` of `loaded`'s workspace, a definition of
/// `script`, as one build, as [`build_all_observed`] builds a target: in
/// the root context `^script_session` too, its `args` the words `args`.
/// Its own effects run on the host's view of the workspace, in program
/// order, outside any step, and what it builds is built as any build is.
///
/// # Errors
///
/// That the workspace's standard library declares no scripts, or those of
/// [`build_all_observed`].
pub fn run_script_observed(
    loaded: &mut Loaded,
    script: &Target,
    (settings, args): (&Settings, &[String]),
    events: &mut dyn FnMut(&Event),
) -> Result<Outcomes, BuildError> {
    if !loaded.workspace.declares_context(SCRIPT_SESSION) {
        return Err(BuildError::Engine(format!(
            "this workspace's GCSL declares no `{SCRIPT_SESSION}`, so it runs no scripts"
        )));
    }
    let words = args.iter().map(|word| Plain::Str(word.clone())).collect();
    let session = Plain::Record(vec![("args".to_owned(), Plain::List(words))]);
    let contexts = vec![(SCRIPT_SESSION.to_owned(), session)];
    build_roots(
        loaded,
        std::slice::from_ref(script),
        settings,
        (None, contexts),
        events,
    )
}

/// The command that runs the program at `path`, as a build names it,
/// `/pekit-out/app/link/app` say, with the arguments `args`, on the host's
/// view of `loaded`'s workspace that a script's effects run on, in the
/// directory pekit runs in, with its terminal and its environment.
///
/// # Errors
///
/// That `path` is no file of the view, or is not executable, or that the
/// view cannot be made.
pub fn program_command(
    loaded: &Loaded,
    settings: &Settings,
    (path, args): (&str, &[String]),
) -> Result<std::process::Command, BuildError> {
    use std::os::unix::fs::PermissionsExt as _;
    let host = HostView::new(&loaded.root, &settings.outputs, "", &settings.scratch)
        .with_libraries(loaded.libraries.clone());
    let on_host = host.host_path(path).map_err(BuildError::Engine)?;
    let executable = fs::metadata(&on_host)
        .is_ok_and(|meta| meta.is_file() && meta.permissions().mode() & 0o111 != 0);
    if !executable {
        return Err(BuildError::Engine(format!(
            "`{path}` is no executable file, so it cannot be run"
        )));
    }
    let mut command = std::process::Command::new(path);
    command.args(args);
    host.command(command).map_err(BuildError::Engine)
}

/// [`build_all_observed`], each target's call given to `wrap` if it is
/// given, in the root contexts and `contexts` too.
fn build_roots(
    loaded: &mut Loaded,
    targets: &[Target],
    settings: &Settings,
    (wrap, contexts): (Option<RootWrap<'_>>, Vec<(String, Plain)>),
    events: &mut dyn FnMut(&Event),
) -> Result<Outcomes, BuildError> {
    let mut run = new_run(loaded, targets, settings, (wrap, contexts), events)?;
    run.drive()?;
    run.finish_uploads()?;
    if settings.exposing == Exposing::All {
        run.expose_waiting()?;
    }
    let mut outcomes = Vec::with_capacity(run.roots.len());
    for index in 0..run.roots.len() {
        let done = run.roots.get_mut(index).and_then(|root| root.done.take());
        let outcome = match done {
            Some(Ok(value)) => run.copy_out(&value, index).and_then(|()| {
                run.loaded
                    .workspace
                    .to_plain(&value)
                    .map_err(BuildError::Engine)
            }),
            Some(Err(error)) => Err(error),
            None => Err(engine(&"a target the build never finished")),
        };
        outcomes.push(outcome);
    }
    run.sweeper.finish().map_err(|error| {
        engine(&format!(
            "cannot remove a step's staging directory: {error}"
        ))
    })?;
    // A stamp cache that can't be kept is a workspace read again next time.
    let _ = keep_workspace_stamps(&run.store, &run.loaded.root, &run.workspace_seen.0);
    // What a killed build left that can't be removed is left for the next.
    let _ = crate::sweep::sweep_stale(&run.store, &settings.scratch);
    let evicted = settings.cache_limit.map(|limit| {
        evict(&run.store, &run.store.used(), limit).map_err(|error| error.to_string())
    });
    Ok(Outcomes {
        evicted,
        targets: outcomes,
        ran: run.ran,
        cached: run.cached,
        found_remotely: run.found_remotely,
        programs_found: run.programs_found,
        remote_failed: run.remote_failed.take(),
        flaky: std::mem::take(&mut run.flaky),
        steps: std::mem::take(&mut run.reports),
        mismatches: std::mem::take(&mut run.mismatches),
    })
}

/// What each of `targets` of `loaded`'s workspace reaches, evaluated as
/// [`build_all_observed`] builds them but running no step (see
/// [Queries](crate::query#queries)): its own effects run, a fetch's or a
/// read's, and a target that waits for a step's result stops there. Each
/// comes to what it reaches, or the failure of its evaluation.
///
/// # Errors
///
/// What [`build_all_observed`] fails with of pekit's own.
pub fn query_all(
    loaded: &mut Loaded,
    targets: &[Target],
    settings: &Settings,
) -> Result<Vec<Result<TargetDeps, BuildError>>, BuildError> {
    let mut events = |_: &Event| {};
    let mut run = new_run(loaded, targets, settings, (None, Vec::new()), &mut events)?;
    run.query = true;
    run.drive()?;
    let mut found = Vec::with_capacity(targets.len());
    for (index, target) in targets.iter().enumerate() {
        let done = run.roots.get_mut(index).and_then(|root| root.done.take());
        found.push(match done {
            Some(Ok(_)) => Ok(run.deps_of(index, target.qualified())),
            Some(Err(error)) => Err(error),
            None => Err(engine(&"a target the query never finished")),
        });
    }
    Ok(found)
}

/// A run of `targets` of `loaded`'s workspace, as [`build_roots`] makes it,
/// not yet driven.
///
/// # Errors
///
/// That the store, its scratch or the keychain's key cannot be opened.
fn new_run<'a>(
    loaded: &'a mut Loaded,
    targets: &[Target],
    settings: &'a Settings,
    (wrap, contexts): (Option<RootWrap<'_>>, Vec<(String, Plain)>),
    events: &'a mut dyn FnMut(&Event),
) -> Result<Run<'a>, BuildError> {
    loaded.ready().map_err(BuildError::Engine)?;
    if settings.profile_gcsl {
        loaded.workspace.start_profiling();
    }
    let store = Store::open(&settings.store).map_err(|error| engine(&error))?;
    let workspace_seen = (
        workspace_stamps(&store, &loaded.root),
        clock_beside(&loaded.root.join(".pekit")).map_err(|error| engine(&error))?,
    );
    std::fs::create_dir_all(&settings.scratch).map_err(|error| engine(&error))?;
    let keychain = Arc::new(Keychain::open(&settings.state).map_err(|error| {
        engine(&format!(
            "cannot open the keychain's key in `{}`: {error}",
            settings.state.display()
        ))
    })?);
    let roots = prepare_roots(loaded, targets, (settings, &keychain), (wrap, contexts));
    let code_table: Rc<[(String, Digest)]> = loaded
        .names
        .iter()
        .zip(&loaded.sources)
        .map(|(name, source)| (name.clone(), Digest::of(source.text().as_bytes())))
        .collect();
    let unit_roots = roots
        .iter()
        .enumerate()
        .filter_map(|(index, root)| Some((root.call.as_ref()?.unit(), index)))
        .collect();
    Ok(Run {
        loaded,
        store,
        settings,
        roots,
        unit_roots,
        query: false,
        exposures: Exposures::default(),
        graph: Graph::new(),
        outputs: HashMap::new(),
        steps: HashMap::new(),
        step_names: HashMap::new(),
        step_roots: HashMap::new(),
        named_steps: HashMap::new(),
        named: 0,
        pool: Pool::new(settings.jobs.max(1)),
        in_flight: HashMap::new(),
        events,
        began: Instant::now(),
        keychain,
        code_table,
        ran: 0,
        cached: 0,
        programs_found: 0,
        workspace_seen,
        flaky: Vec::new(),
        reports: Vec::new(),
        mismatches: Vec::new(),
        background: 0,
        looking: HashMap::new(),
        found_remotely: 0,
        remote_failed: None,
        uploads: 0,
        sweeper: Sweeper::new(
            settings
                .scratch
                .join(format!("swept-{}", crate::sweep::token())),
        ),
    })
}

/// A root of each of `targets` of `loaded`'s workspace, as `settings` say,
/// its own effects under a host view of its directory that asks `keychain`
/// for secrets: its prepared call, or why it could not be prepared. The
/// contexts `extra` are made values of the workspace here, readied for this
/// build, never of one an earlier build used (see
/// [Loading](crate::load#loading)).
fn prepare_roots(
    loaded: &mut Loaded,
    targets: &[Target],
    (settings, keychain): (&Settings, &Arc<Keychain>),
    (wrap, extra): (Option<RootWrap<'_>>, Vec<(String, Plain)>),
) -> Vec<Root> {
    let mut contexts = root_contexts(loaded, settings);
    contexts.extend(
        extra
            .into_iter()
            .map(|(name, plain)| (name, loaded.workspace.from_plain(&plain))),
    );
    let fields = root_fields(loaded);
    targets
        .iter()
        .map(|target| {
            let host = Arc::new(
                HostView::new(
                    &loaded.root,
                    &settings.outputs,
                    &target.definition.dir,
                    &settings.scratch,
                )
                .with_keychain(Arc::clone(keychain))
                .with_libraries(loaded.libraries.clone()),
            );
            let call = loaded.workspace.prepare_call_wrapped(
                (&target.def_type, &target.qualified()),
                &[],
                &contexts,
                (&fields, wrap),
            );
            let dir = target.qualified().trim_start_matches('/').to_owned();
            match call {
                Ok(call) => Root {
                    call: Some(call),
                    dir,
                    host,
                    main: Main::Runnable,
                    done: None,
                    stopped_at: Vec::new(),
                    waits: None,
                },
                Err(error) => Root {
                    call: None,
                    dir,
                    host,
                    main: Main::Runnable,
                    done: Some(Err(BuildError::Invoke(error))),
                    stopped_at: Vec::new(),
                    waits: None,
                },
            }
        })
        .collect()
}

/// The root contexts every target of `loaded`'s workspace is evaluated in,
/// as `settings` say, where the workspace declares each: `^host_platform`,
/// `^platform` if the build is for another, `^keychain_profile`, and
/// `^pinned_libraries`, each library `pekit.lock` pins.
fn root_contexts(loaded: &mut Loaded, settings: &Settings) -> Vec<(String, Value)> {
    let mut contexts = Vec::new();
    if loaded.workspace.declares_context(HOST_PLATFORM) {
        let platform = loaded.workspace.from_plain(&settings.execution.plain());
        contexts.push((HOST_PLATFORM.to_owned(), platform));
    }
    if let Some(building_for) = &settings.building_for
        && loaded.workspace.declares_context(PLATFORM)
    {
        let platform = loaded.workspace.from_plain(&building_for.plain());
        contexts.push((PLATFORM.to_owned(), platform));
    }
    if loaded.workspace.declares_context(KEYCHAIN_PROFILE) {
        let profile = Value::Str(Rc::from(settings.profile.as_str()));
        contexts.push((KEYCHAIN_PROFILE.to_owned(), profile));
    }
    if loaded.workspace.declares_context(PINNED_LIBRARIES) {
        let text = |text: &str| Plain::Str(text.to_owned());
        let pins = loaded
            .pinned
            .iter()
            .map(|pin| {
                Plain::Record(vec![
                    ("commit".to_owned(), text(&pin.commit)),
                    ("dir".to_owned(), text(&pin.dir)),
                    ("git".to_owned(), text(&pin.git)),
                    ("path".to_owned(), text(&pin.path)),
                ])
            })
            .collect();
        let pins = loaded.workspace.from_plain(&Plain::List(pins));
        contexts.push((PINNED_LIBRARIES.to_owned(), pins));
    }
    contexts
}

/// The root contexts set from `workspace.gcsl`'s `pekitconf`: the
/// toolchains and the keychain, where the module that declares each
/// context is loaded and the field is there.
fn root_fields(loaded: &Loaded) -> Vec<RootField<'static>> {
    let mut fields = Vec::new();
    if let Some(file) = loaded.workspace.file(WORKSPACE_FILE) {
        for (context, path) in [
            (TOOLCHAINS, &["toolchains"] as &[&str]),
            (KEYCHAIN_KEYS, &["keychain"]),
            (PACKAGERS, &["packagers"]),
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
    fields
}

/// Where the store keeps the stamps of the workspace at `root`'s files.
fn stamps_path(store: &Store, root: &Path) -> PathBuf {
    let name = Digest::of(root.as_os_str().as_encoded_bytes()).hex();
    store.root().join("seen").join(format!("{name}.json"))
}

/// The stamps of the workspace at `root`'s files as the last build kept
/// them, or none.
fn workspace_stamps(store: &Store, root: &Path) -> Seen {
    fs::read(stamps_path(store, root))
        .ok()
        .and_then(|bytes| serde_json::from_slice(&bytes).ok())
        .unwrap_or_default()
}

/// Keeps `seen`, the stamps of the workspace at `root`'s files, but of those
/// that are no longer there, for the next build.
///
/// # Errors
///
/// That the store cannot be written.
fn keep_workspace_stamps(store: &Store, root: &Path, seen: &Seen) -> io::Result<()> {
    let mut kept = seen.clone();
    kept.retain_present();
    let path = stamps_path(store, root);
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let temp = path.with_extension(format!("tmp-{}", crate::sweep::token()));
    fs::write(&temp, serde_json::to_vec(&kept).map_err(io::Error::other)?)?;
    fs::rename(&temp, &path)
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

/// The keys of the contexts the step `step`'s last run read, sorted: each it
/// read, and, if it read every one its context binds, each of those.
fn read_keys(step: &StepRun) -> Vec<String> {
    let mut keys: Vec<String> = step.reads.keys.iter().map(|(key, _)| key.clone()).collect();
    if step.reads.all {
        keys.extend(
            step.keyed
                .contexts
                .iter()
                .map(|context| context.key.clone()),
        );
    }
    keys.sort();
    keys.dedup();
    keys
}

/// The devices a step of a function with `attributes` is bound, by its
/// `#[device(NAME)]`s: `/dev/kvm` for `kvm`, where this machine has it, and
/// nothing where it doesn't, since a device changes how fast a step runs,
/// never what it gives, and so keys nothing.
///
/// # Errors
///
/// A device pekit doesn't know.
fn devices_of(attributes: &[Attribute]) -> Result<Vec<PathBuf>, String> {
    let mut devices = Vec::new();
    for attribute in attributes
        .iter()
        .filter(|attribute| attribute.id.name() == "device")
    {
        let name = attribute
            .args
            .iter()
            .find_map(|(field, value)| match value {
                Constant::Str(name) if &**field == "name" => Some(name.to_string()),
                _ => None,
            });
        let path = match name.as_deref() {
            Some("kvm") => PathBuf::from("/dev/kvm"),
            other => {
                return Err(format!(
                    "pekit gives a step no device `{}`: the one it knows is `kvm`",
                    other.unwrap_or_default()
                ));
            }
        };
        if path.exists() && !devices.contains(&path) {
            devices.push(path);
        }
    }
    Ok(devices)
}

/// Whether a step of `attributes` is marked `#[scratch]`.
fn wants_scratch(attributes: &[Attribute]) -> bool {
    attributes
        .iter()
        .any(|attribute| attribute.id.name() == "scratch")
}

/// No deferred call is a step: what runs in a step's own unit, inline.
fn inline(_: &[Attribute]) -> bool {
    false
}

/// A secret file a step is given: its path as the step sees it, and where
/// it is on the host (see [Secrets](crate::keychain#secrets)).
type SecretFile = (String, PathBuf);

/// What a step's sandbox is made with, for each attempt.
struct Staging<'a> {
    /// Its secret files, to bind.
    secrets: &'a [SecretFile],
    /// Its devices, to bind.
    devices: &'a [PathBuf],
    /// Its scratch directory, on the host, to bind, if it has one.
    scratch: Option<&'a Path>,
}

/// Where a step the engine is running is: its unit runs only in between
/// the engine's own work for it, which is done on the pool.
#[derive(Clone, Copy, Debug)]
enum Phase {
    /// Its sandbox is being staged, since then.
    Staging(Instant),
    /// Its unit runs.
    Running,
    /// The files of its result are being kept in the store.
    Keeping,
}

/// A step the engine is running (see [Building](#building)).
struct StepRun {
    /// Its sandbox.
    sandbox: Arc<StepSandbox>,
    /// Where it is.
    phase: Phase,
    /// Its result, once it has one, while its files are kept.
    result: Option<Value>,
    /// What it is given, by logical path, which each attempt is staged with.
    given: Vec<(String, Kept)>,
    /// The devices bound into its sandbox, by its `#[device]`s.
    devices: Vec<PathBuf>,
    /// The secret files it is given, by logical path, and where each is on
    /// the host, which each attempt has bound.
    secrets: Vec<SecretFile>,
    /// Its attempt's number, from 0.
    attempt: u32,
    /// What keys it but the contexts it reads.
    keyed: Keyed,
    /// Its base, its receipt with no context, which its manifest is kept by.
    base: Digest,
    /// What its unit's last run read of its context.
    reads: StepReads,
    /// When its first attempt began.
    started: Instant,
    /// When its attempts must end, by its `#[timeout]`.
    deadline: Option<Instant>,
    /// How many more attempts it may have.
    retries: u32,
    /// Whether an attempt of it has failed.
    failed_once: bool,
    /// The effects its unit stopped for, if it did: it runs again once one
    /// of them is given.
    waiting: Vec<NodeId>,
    /// The unit whose run it stopped for, if it did, which computes a shared
    /// result it needs.
    waiting_unit: Option<u32>,
    /// Its scratch directory, if it is marked `#[scratch]`, held for as long
    /// as it runs (see [Scratch directories](crate::scratch#scratch-directories)).
    scratch: Option<Scratch>,
    /// What its attempt's programs are keyed by (see `design/additive.md`,
    /// F1).
    programs: Programs,
}

/// What a step is keyed by but the contexts it reads (see
/// `design/engine.md`, R2).
#[derive(Debug)]
struct Keyed {
    /// Its inputs, as GCSL gives them.
    inputs: StepInputs,
    /// The path and contents' digest of each file whose source makes its
    /// code.
    code: Vec<(String, Digest)>,
    /// Its own output directory, logically.
    own: String,
    /// Each context it can read, as keying and staging it need it.
    contexts: Vec<ContextInput>,
}

/// A context a step can read (see `design/engine.md`, R3): its key, its value,
/// and what the step is given once it reads it.
#[derive(Clone, Debug)]
struct ContextInput {
    /// Its declaration's key's text.
    key: String,
    /// Its value.
    plain: Plain,
    /// What it gives the step once the step reads it.
    pending: Pending,
}

/// One build's state (see [Building](#building)).
struct Run<'a> {
    /// The workspace.
    loaded: &'a mut Loaded,
    /// The store.
    store: Store,
    /// How the build runs.
    settings: &'a Settings,
    /// The targets the build evaluates.
    roots: Vec<Root>,
    /// The target each unit is of, by unit: a target's own, and each
    /// step's, so that a step met in one is of the same target.
    unit_roots: HashMap<u32, usize>,
    /// Whether it is a query's, which runs no step (see
    /// [Queries](crate::query#queries)).
    query: bool,
    /// The action graph.
    graph: Graph,
    /// What the store keeps of each output a step made, by its logical
    /// path, and of each workspace input hashed so far.
    outputs: HashMap<String, Kept>,
    /// The steps running, by node.
    steps: HashMap<NodeId, StepRun>,
    /// Each step's name, `<function>/<identity>`, by node: its directory
    /// under `/pekit-out/.steps`.
    step_names: HashMap<u32, String>,
    /// The target each step is of, by node, its number among the roots.
    step_roots: HashMap<NodeId, usize>,
    /// The step each name is of, so that two steps of one identity but for
    /// the workspace's configuration are told apart.
    named_steps: HashMap<String, NodeId>,
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
    /// How many steps ran.
    ran: usize,
    /// How many steps were found in the store.
    cached: usize,
    /// How many programs the steps that ran found in the store.
    programs_found: usize,
    /// What each workspace file was when it was last put into the store, kept
    /// between builds, and when this build began, by the workspace's file
    /// system's clock (see `design/additive.md`, F4).
    workspace_seen: (Seen, (i64, i64)),
    /// The steps that passed only on a retry, by name.
    flaky: Vec<String>,
    /// What became of each step that was found in the store or passed.
    reports: Vec<StepReport>,
    /// The steps that gave another result than their records hold.
    mismatches: Vec<Mismatch>,
    /// The outputs yet to be put under the output directory (see
    /// [Exposure](crate::exposure#exposure)).
    exposures: Exposures,
    /// What removes each step's staging directory once it is done (see
    /// [Sweeping](crate::sweep#sweeping)).
    sweeper: Sweeper,
    /// How many of the engine's own jobs for steps are on the pool:
    /// stagings, keepings and lookups in the remote cache.
    background: usize,
    /// The steps being looked for in the remote cache, by node.
    looking: HashMap<NodeId, Looking>,
    /// How many steps were found in the remote cache.
    found_remotely: usize,
    /// Why the remote cache could not be used, as first found.
    remote_failed: Option<String>,
    /// How many uploads to the remote cache are on the pool, which the
    /// build's end waits for.
    uploads: usize,
}

/// A step being looked for in the remote cache (see [The remote
/// cache](crate::remote#the-remote-cache)): what it is, as
/// [`Run::start_step`] found it, and whether its manifest has been asked
/// for, so that its records are asked for next.
struct Looking {
    /// The step, as its receipt is made.
    keyed: Keyed,
    /// Its base receipt.
    base: Digest,
    /// What it is given, to stage.
    given: Vec<(String, Kept)>,
    /// The devices bound into its sandbox.
    devices: Vec<PathBuf>,
    /// Whether its records, not its manifest, were asked for.
    records: bool,
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
    // What it was given: its arguments' and captures' files, and those of
    // the contexts it read.
    let staged = step.sandbox.staged();
    plain
        .files()
        .into_iter()
        .map(|(path, _)| path)
        .find(|path| {
            !within(path, own)
                && !shared(path)
                && !step
                    .given
                    .iter()
                    .chain(&staged)
                    .any(|(given, kept)| match kept {
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
    /// The built-in and arguments to fold into its step's view once it has
    /// run, if it is one its step's programs see (see `design/additive.md`,
    /// F1).
    fold: Option<(Builtin, Vec<Plain>)>,
}

/// A target the build evaluates (see [Building](#building)).
struct Root {
    /// Its prepared call; `None` if it could not be prepared, which `done`
    /// says.
    call: Option<PreparedCall>,
    /// Its directory under `/pekit-out`, its qualified name.
    dir: String,
    /// The view its own effects run in, its working directory its
    /// directory.
    host: Arc<HostView>,
    /// Where its unit is.
    main: Main,
    /// What it came to, once it is done.
    done: Option<Result<Value, BuildError>>,
    /// In a query, what it needed when it was done: the holes of its value,
    /// or the step's result it stopped for.
    stopped_at: Vec<NodeId>,
    /// In a query, the step whose result it stopped for, if it did.
    waits: Option<NodeId>,
}

/// Where a target's unit is.
enum Main {
    /// It may run.
    Runnable,
    /// It stopped for these nodes' results, its tasks each waiting for one.
    Paused(Vec<NodeId>),
    /// It stopped for the run of another unit, which computes a shared
    /// result it needs: it may run again once anything else has run.
    Waiting,
    /// It ended with this value, whose holes are still waited for.
    Holding(Value),
}

impl Run<'_> {
    /// Runs every target's unit, the steps they need and their effects,
    /// until each target's value has no holes, or it failed: what each came
    /// to is its root's.
    fn drive(&mut self) -> Result<(), BuildError> {
        loop {
            // The memoised calls ended before this round's runs: a round
            // that ends one has moved on what a waiting unit waits for,
            // though no step or effect ran.
            let ended = self.loaded.workspace.memo_ended();
            self.run_roots();
            self.name_new_steps();
            let demands = self.root_demands();
            if self.roots.iter().all(|root| root.done.is_some()) {
                return Ok(());
            }
            let mut progressed = self.start_needed(&demands)?;
            progressed |= self.advance_steps()?;
            if !self.in_flight.is_empty() || self.background > 0 {
                let Some(ran) = self.pool.next() else {
                    return Err(BuildError::Engine(
                        "the threads effects run on stopped".to_owned(),
                    ));
                };
                self.finished(ran)?;
                // Every other job that has finished too, so that a unit
                // waiting for several effects runs again once for all of
                // them.
                while let Some(ran) = self.pool.ready() {
                    self.finished(ran)?;
                }
                progressed = true;
            }
            progressed |= self.loaded.workspace.memo_ended() != ended;
            // A unit waiting for another's shared result runs again once
            // anything else has run, the other unit perhaps; if nothing else
            // can, and no shared result was finished, the units wait for
            // each other.
            let waiting = self.wake_waiting(progressed);
            if waiting && !progressed {
                return Err(BuildError::Engine(
                    "the build's targets and steps wait for each other's shared results".to_owned(),
                ));
            }
            for index in 0..self.roots.len() {
                let given = match self.roots.get(index) {
                    Some(Root {
                        main: Main::Paused(nodes),
                        done: None,
                        ..
                    }) => nodes.iter().any(|&node| !self.is_waiting(node)),
                    _ => continue,
                };
                if given {
                    if let Some(root) = self.roots.get_mut(index) {
                        root.main = Main::Runnable;
                    }
                    progressed = true;
                }
            }
            if !progressed {
                return Err(BuildError::Engine(
                    "the build is waiting for nothing: a step it needs never became ready"
                        .to_owned(),
                ));
            }
        }
    }

    /// Lets every target and step that stopped for another unit's run run
    /// again, if anything ran this round, `progressed`. Returns whether any
    /// was waiting so.
    fn wake_waiting(&mut self, progressed: bool) -> bool {
        let mut waiting = false;
        for root in &mut self.roots {
            if root.done.is_none() && matches!(root.main, Main::Waiting) {
                waiting = true;
                if progressed {
                    root.main = Main::Runnable;
                }
            }
        }
        for step in self.steps.values_mut() {
            if step.waiting_unit.is_some() {
                waiting = true;
                if progressed {
                    step.waiting_unit = None;
                }
            }
        }
        waiting
    }

    /// Runs the unit of each target that may run once: until it stops for a
    /// node's result, which, for a step's under `--no-dynamic`, fails it, or
    /// ends with a value, or fails.
    fn run_roots(&mut self) {
        let no_dynamic = self.settings.no_dynamic;
        let query = self.query;
        let mut evaluated = Vec::new();
        for root in &mut self.roots {
            if root.done.is_some() || !matches!(root.main, Main::Runnable) {
                continue;
            }
            let Some(call) = &root.call else {
                continue;
            };
            let host = GraphHost {
                defers: &hermetic,
                effects: &*root.host,
            };
            let began = self.began.elapsed();
            let ran = self
                .loaded
                .workspace
                .run_prepared(call, &mut self.graph, host);
            evaluated.push(Event {
                at: self.began.elapsed(),
                kind: EventKind::Evaluated {
                    unit: root.dir.clone(),
                    step: false,
                    began,
                },
            });
            match ran {
                Ok(Progress::Paused(nodes)) => {
                    let step = nodes.iter().copied().find_map(|node| {
                        self.graph
                            .node(node)
                            .filter(|found| found.kind() == NodeKind::Deferred)
                            .map(|found| (node, found))
                    });
                    match step {
                        Some((node, _)) if query => {
                            root.stopped_at.clone_from(&nodes);
                            root.waits = Some(node);
                            // Its value is never read: what it reached is.
                            root.done = Some(Ok(Value::Str(Rc::from(""))));
                        }
                        Some((_, found)) if no_dynamic => {
                            root.done = Some(Err(BuildError::Dynamic(
                                Diagnostic::error(
                                    "the target waits here for a step's result, a dynamic \
                                     dependency, which `--no-dynamic` forbids",
                                    found.span(),
                                )
                                .in_input(found.input()),
                            )));
                        }
                        _ => root.main = Main::Paused(nodes),
                    }
                }
                Ok(Progress::Waiting(_)) => root.main = Main::Waiting,
                Ok(Progress::Done(value)) => root.main = Main::Holding(value),
                Err(error) => root.done = Some(Err(BuildError::Invoke(error))),
            }
        }
        for event in &evaluated {
            (self.events)(event);
        }
    }

    /// The nodes each target that is not done waits for: the one its unit
    /// stopped for, or the holes of the value it ended with. A target whose
    /// value has no holes is done: its value, or its failures.
    fn root_demands(&mut self) -> Vec<NodeId> {
        let mut demands = Vec::new();
        for root in &mut self.roots {
            if root.done.is_some() {
                continue;
            }
            match &root.main {
                Main::Paused(nodes) => demands.extend(nodes.iter().copied()),
                Main::Holding(value) => {
                    let holes = self.graph.holes(value);
                    if self.query {
                        root.done = Some(Ok(value.clone()));
                        root.stopped_at = holes;
                        continue;
                    }
                    if holes.is_empty() {
                        let failures = self.graph.failures(value);
                        root.done = Some(if failures.is_empty() {
                            Ok(value.clone())
                        } else {
                            Err(BuildError::Failed(failures))
                        });
                    }
                    demands.extend(holes);
                }
                Main::Runnable | Main::Waiting => {}
            }
        }
        demands
    }

    /// Reports the step `id` as found in the store or passed, whether it was
    /// found, and whether it passed only on a retry, as `(cached, flaky)`.
    fn report(&mut self, id: NodeId, (cached, flaky): (bool, bool)) {
        let name = self.step_name(id);
        let function = name.split('/').next().unwrap_or_default().to_owned();
        self.reports.push(StepReport {
            target: self.step_roots.get(&id).copied().unwrap_or(0),
            name,
            function,
            cached,
            flaky,
        });
    }

    /// The target the unit `unit` is of, by its number among the roots: a
    /// target's own unit's, or a step's, as its step's target.
    fn root_of(&self, unit: u32) -> usize {
        self.unit_roots.get(&unit).copied().unwrap_or(0)
    }

    /// What the target of the root `index`, named `target`, reached, once a
    /// query's run is done with it (see [Queries](crate::query#queries)).
    fn deps_of(&self, index: usize, target: String) -> TargetDeps {
        let workspace = &self.loaded.workspace;
        let names = &self.loaded.names;
        let shown = |files: Vec<u32>| -> Vec<String> {
            let mut shown: Vec<String> = files
                .into_iter()
                .filter_map(|file| names.get(file as usize))
                .filter(|name| !name.starts_with('<'))
                .cloned()
                .collect();
            shown.sort();
            shown.dedup();
            shown
        };
        let Some(root) = self.roots.get(index) else {
            return TargetDeps {
                target,
                code: Vec::new(),
                reads: Vec::new(),
                steps: Vec::new(),
                waits: None,
            };
        };
        let call = root.call.as_ref();
        let code = call.map_or_else(Vec::new, |call| {
            call.files()
                .iter()
                .flat_map(|&file| workspace.files_needed(file))
                .collect()
        });
        let reached = self.graph.needed(&root.stopped_at);
        let steps = reached
            .iter()
            .filter(|&&id| {
                self.graph
                    .node(id)
                    .is_some_and(|node| node.kind() == NodeKind::Deferred)
            })
            .map(|&id| StepDeps {
                name: self.own_dir(id),
                code: shown(workspace.node_code_files(&self.graph, id)),
                reads: self.node_reads(id),
                after: self.steps_after(id),
            })
            .collect();
        let waits = root.waits.and_then(|id| {
            let node = self.graph.node(id)?;
            // Where its own code met it, which another target's step need
            // not have been made.
            let (file, span) = call
                .and_then(|call| self.graph.met_at(call.unit(), id))
                .unwrap_or((node.input(), node.span()));
            let source = self.loaded.sources.get(file as usize)?;
            let line = source
                .text()
                .get(..span.start as usize)?
                .matches('\n')
                .count()
                + 1;
            Some(format!("{}:{line}", names.get(file as usize)?))
        });
        TargetDeps {
            target,
            code: shown(code),
            reads: call.map_or_else(Vec::new, |call| self.own_reads(call.unit())),
            steps,
            waits,
        }
    }

    /// The workspace files and directories the node `id`, a step, is given:
    /// those its arguments, captures and context hold, by their paths
    /// relative to the root, sorted.
    fn node_reads(&self, id: NodeId) -> Vec<String> {
        let Some(node) = self.graph.node(id) else {
            return Vec::new();
        };
        let mut values: Vec<&Value> = node.args().iter().collect();
        if let Some(closure) = node.closure() {
            values.extend(closure.captures.iter());
        }
        let context = node.context();
        values.extend(context.iter().map(|(_, value)| value));
        let mut reads: Vec<String> = values
            .into_iter()
            .flat_map(|value| value.files().into_iter().chain(value.dirs()))
            .filter_map(|file| in_workspace(&file.path))
            .collect();
        reads.sort();
        reads.dedup();
        reads
    }

    /// The steps whose results the node `id` is given, by name, sorted.
    fn steps_after(&self, id: NodeId) -> Vec<String> {
        let Some(node) = self.graph.node(id) else {
            return Vec::new();
        };
        let mut after: Vec<String> = self
            .graph
            .needed(node.data())
            .into_iter()
            .filter(|&edge| {
                edge != id
                    && self
                        .graph
                        .node(edge)
                        .is_some_and(|found| found.kind() == NodeKind::Deferred)
            })
            .map(|edge| self.own_dir(edge))
            .collect();
        after.sort();
        after.dedup();
        after
    }

    /// The workspace files and directories the effects of the unit `unit`,
    /// a target's own, read: each that reads a path's, and the files and
    /// directories any other is given, by their paths relative to the root,
    /// sorted.
    fn own_reads(&self, unit: u32) -> Vec<String> {
        let mut reads = Vec::new();
        for (id, node) in self.graph.nodes() {
            if node.unit() != unit || node.kind() == NodeKind::Deferred {
                continue;
            }
            let Ok(job) = self.loaded.workspace.effect_job(&self.graph, id) else {
                continue;
            };
            for arg in job.args() {
                if let Plain::Str(path) = arg {
                    reads.extend(in_workspace(path));
                }
                reads.extend(
                    arg.files()
                        .into_iter()
                        .filter_map(|(path, _)| in_workspace(path)),
                );
            }
        }
        reads.sort();
        reads.dedup();
        reads
    }

    /// The directory under `/pekit-out` of the step `id`,
    /// `.steps/<function>/<identity>` ([`Run::name_new_steps`]).
    fn own_dir(&self, id: NodeId) -> String {
        format!("{STEPS}/{}", self.step_name(id))
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
            if self.steps.contains_key(&id)
                || self.in_flight.contains_key(&id)
                || self.looking.contains_key(&id)
            {
                continue;
            }
            let Some(node) = self.graph.node(id) else {
                continue;
            };
            if self.query && node.kind() == NodeKind::Deferred {
                continue;
            }
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
                    let root = self.root_of(node.unit());
                    let Some(view) = self.roots.get(root).map(|found| Arc::clone(&found.host))
                    else {
                        continue;
                    };
                    let host: Arc<dyn Effects + Send + Sync> = view;
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
        let mut running = InFlight {
            of: step,
            step: step.map(|step| self.step_name(step)),
            effect: effect_label(&job),
            fold: None,
        };
        let span = self.graph.node(id).map_or(Span::new(0, 0), Node::span);
        // A step is given its secrets, and never asks the keychain itself.
        if step.is_some()
            && matches!(
                job.builtin(),
                Builtin::KeychainString | Builtin::KeychainFile
            )
        {
            self.finish_now(id, Err(Diagnostic::error(keychain::IN_A_STEP, span)));
            return Ok(());
        }
        if step.is_none()
            && self.settings.strict
            && let Some(refused) = strict_refusal(&job)
        {
            self.finish_now(id, Err(Diagnostic::error(refused, span)));
            return Ok(());
        }
        let mut program = None;
        if let Some(of) = step {
            match self.program_before(of, &job, span) {
                Before::Program(work) => {
                    program = self
                        .steps
                        .get(&of)
                        .map(|run| (Arc::clone(&run.sandbox), work));
                }
                Before::Fold(builtin, args) => running.fold = Some((builtin, args)),
                Before::Plain => {}
            }
        }
        let own = if step.is_none() {
            self.before_target_effect(&job, &*effects, span)
        } else {
            Ok(None)
        };
        let own = own.and_then(|answered| match answered {
            Some(work) => Ok(Some(work)),
            None => self.own_work(&job, step, span),
        });
        let work = match own {
            Ok(Some(work)) => work,
            Ok(None) => match program {
                Some((sandbox, work)) => Work::Program(job, sandbox, work),
                None => Work::Effect(job, effects),
            },
            Err(BuildError::Engine(message)) => {
                self.finish_now(id, Err(Diagnostic::error(message, span)));
                return Ok(());
            }
            Err(other) => return Err(other),
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

    /// The work of `job`, of the step `step` or of the target's own unit,
    /// its call at `span`, if it is pekit's own to answer, a fetch or an
    /// unpacking: none if it is not.
    ///
    /// # Errors
    ///
    /// Why it fails at once: a step fetches, or an unpacking's archive
    /// cannot be read.
    fn own_work(
        &mut self,
        job: &EffectJob,
        step: Option<NodeId>,
        span: Span,
    ) -> Result<Option<Work>, BuildError> {
        let text = |at: usize| match job.args().get(at) {
            Some(Plain::Str(text)) => text.clone(),
            _ => String::new(),
        };
        match job.builtin() {
            // A fetch is pekit's to answer, and never a step's.
            Builtin::Fetch if step.is_some() => Err(engine(&IN_A_STEP)),
            Builtin::FetchGit if step.is_some() => Err(engine(&git_fetch::IN_A_STEP)),
            Builtin::Fetch => Ok(Some(Work::Fetch {
                url: text(0),
                sha256: text(1),
                store: self.store.clone(),
                span,
            })),
            Builtin::FetchGit => Ok(Some(Work::FetchGit {
                url: text(0),
                commit: text(1),
                store: self.store.clone(),
                scratch: self.settings.scratch.clone(),
                span,
            })),
            Builtin::ArchiveUnpack => self
                .unpack_work(job, span)
                .map(Some)
                .map_err(|message| engine(&message)),
            _ => Ok(None),
        }
    }

    /// Readies the output directory for `job`, an effect of a target's own
    /// code under `effects`, its call at `span` (see
    /// [Exposure](crate::exposure#exposure)): the work that answers it from
    /// the store if it reads an output and may be, else none, every waiting
    /// exposure made first if it may read the output directory.
    ///
    /// # Errors
    ///
    /// That an exposure cannot be written.
    fn before_target_effect(
        &mut self,
        job: &EffectJob,
        effects: &dyn Effects,
        span: Span,
    ) -> Result<Option<Work>, BuildError> {
        match job.builtin() {
            // Answered from the store, or reading nothing.
            Builtin::Fetch
            | Builtin::FetchGit
            | Builtin::ArchiveUnpack
            | Builtin::KeychainString
            | Builtin::File
            | Builtin::Dir => Ok(None),
            Builtin::FileRead | Builtin::DirFiles => {
                let path = match job.args().first() {
                    Some(Plain::File { path, .. } | Plain::Dir { path, .. }) => path.as_str(),
                    _ => "",
                };
                if let Some(read) = self.output_read(job.builtin(), path) {
                    return Ok(Some(Work::Read {
                        read,
                        path: path.to_owned(),
                        store: self.store.clone(),
                        span,
                    }));
                }
                if self.reaches_outputs(path, effects) {
                    self.expose_waiting()?;
                }
                Ok(None)
            }
            Builtin::FsExists | Builtin::FsRead | Builtin::KeychainFile => {
                self.expose_waiting()?;
                Ok(None)
            }
            // A program, or a write, may read or change anything there.
            _ => {
                self.expose_waiting()?;
                self.exposures.to_disk();
                Ok(None)
            }
        }
    }

    /// What `builtin`, `file_read` or `dir_files`, of `path` reads from the
    /// store, if it may be answered from there: while no target's own
    /// effect has changed the output directory, of a file or a directory,
    /// as `builtin` reads, inside an output of the build.
    fn output_read(&self, builtin: Builtin, path: &str) -> Option<OutputRead> {
        if !self.exposures.answers_from_store() || !within(path, OUTPUTS) {
            return None;
        }
        match (builtin, self.output_kept(path, 0)?) {
            (Builtin::FileRead, Kept::File(entry)) => Some(OutputRead::Text(entry)),
            (Builtin::DirFiles, Kept::Dir(tree)) => Some(OutputRead::Files(tree)),
            _ => None,
        }
    }

    /// What the store keeps of `path`, under `/pekit-out`, if it is an output
    /// of the build or inside one, reached through `links` symbolic links
    /// that led to paths of their own: none if it leads anywhere else.
    fn output_kept(&self, path: &str, links: u32) -> Option<Kept> {
        if let Some(kept) = self.outputs.get(path) {
            return Some(*kept);
        }
        match self.inside_output(path)? {
            Inside::Kept(kept) => Some(kept),
            Inside::Link(to) if links < MAX_LINKS && within(&to, OUTPUTS) => {
                self.output_kept(&to, links + 1)
            }
            Inside::Link(_) => None,
        }
    }

    /// Whether `path`, as GCSL code under `effects` names it, is in the
    /// output directory or leads into it, so that reading it reads what
    /// was exposed: if it can't be told, it may.
    fn reaches_outputs(&self, path: &str, effects: &dyn Effects) -> bool {
        if within(path, OUTPUTS) {
            return true;
        }
        let Ok(host) = effects.host_path(path) else {
            return true;
        };
        let outputs = &self.settings.outputs;
        let outputs = fs::canonicalize(outputs).unwrap_or_else(|_| outputs.clone());
        fs::canonicalize(host).map_or(true, |real| real.starts_with(outputs))
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

    /// Notes that the output `path` is `kept`, to be put under the output
    /// directory once something may read it there (see
    /// [Exposure](crate::exposure#exposure)).
    fn expose(&mut self, path: &str, kept: Kept) {
        self.exposures.defer(path.to_owned(), kept);
    }

    /// Makes every exposure that waits (see
    /// [Exposure](crate::exposure#exposure)).
    ///
    /// # Errors
    ///
    /// That one cannot be written.
    fn expose_waiting(&mut self) -> Result<(), BuildError> {
        for (path, kept) in self.exposures.take_all() {
            self.write_exposure(&path, kept)?;
        }
        Ok(())
    }

    /// Puts what the store keeps of the output `path` under the output
    /// directory, hard-linked and read-only, in place of what an earlier
    /// build left there, so that what is not hermetic reads it.
    fn write_exposure(&self, path: &str, kept: Kept) -> Result<(), BuildError> {
        let Some(at) = output_path(&self.settings.outputs, path) else {
            return Ok(());
        };
        // Left as it is if it is that already, as a build that changed
        // nothing finds each unpacked toolchain.
        let already = match kept {
            Kept::Dir(tree) => self.store.is_materialised(&tree, &at),
            Kept::File(entry) => self.store.is_linked(&entry, &at),
        };
        if already {
            return Ok(());
        }
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
            if step.waiting.contains(&id) {
                step.waiting.clear();
            }
        }
    }

    /// Tells the event `kind`, now.
    fn emit(&mut self, kind: EventKind) {
        let at = self.began.elapsed();
        (self.events)(&Event { at, kind });
    }

    /// Names every step the graph has that is not named, in the order the
    /// target's code met them: its function's name and its identity, which
    /// is the same in every build that makes it, whichever targets reach it
    /// (see [Identities](crate::receipt#identities)). Two steps whose
    /// identities are one, which only the contexts they were made in tell
    /// apart, are told apart by the digest of those contexts too, the later
    /// met.
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
            found.push((id, function, self.root_of(node.unit())));
        }
        self.named = self.graph.len();
        for (id, function, root) in found {
            self.step_roots.insert(id, root);
            self.unit_roots.insert(STEP_UNITS + id.number(), root);
            let mut name = format!("{function}/{}", self.identity_of(id, false));
            if self
                .named_steps
                .get(&name)
                .is_some_and(|&other| other != id)
            {
                name = format!("{name}-{}", self.identity_of(id, true));
            }
            self.named_steps.insert(name.clone(), id);
            self.step_names.insert(id.number(), name.clone());
            self.emit(EventKind::Discovered { step: name });
        }
    }

    /// The identity of the step `id`, as the start of its digest's hex (see
    /// [Identities](crate::receipt#identities)), every context it was made
    /// in naming it if `every`: its node's number if it cannot be had, which
    /// starting it then fails on.
    fn identity_of(&self, id: NodeId, every: bool) -> String {
        let made = self
            .loaded
            .workspace
            .step_identity(&self.graph, id)
            .and_then(|inputs| {
                let callee = inputs.callee.ok_or("a step with no callee")?;
                let context: Vec<(String, Plain)> =
                    inputs.context.into_iter().filter(|_| every).collect();
                let table = Rc::clone(&self.code_table);
                let code_name =
                    move |input: u32| table.get(input as usize).map(|(name, _)| name.clone());
                let step_of = |node: u32| self.step_names.get(&node).cloned();
                identity(IdentityOf {
                    callee: &callee,
                    args: &inputs.args,
                    context: &context,
                    platform: &self.settings.platform,
                    code_name: &code_name,
                    step_of: &step_of,
                })
            });
        match made {
            Ok(digest) => digest.hex().chars().take(IDENTITY_DIGITS).collect(),
            Err(_) => format!("node-{}", id.number()),
        }
    }

    /// The name of the step `id` ([`Run::name_new_steps`]).
    fn step_name(&self, id: NodeId) -> String {
        self.step_names
            .get(&id.number())
            .cloned()
            .unwrap_or_else(|| format!("step-{}", id.number()))
    }

    /// The secret files `values`, a step's arguments and captures, or a
    /// context's value, hold, at any depth, each by its path as a step sees
    /// it and where it is on the host: bound into the step's sandbox, never
    /// staged, since staging stores (see [Secrets](crate::keychain#secrets)).
    /// A keychain's file is where the keychain found it, and a workspace
    /// file where it is in the workspace.
    ///
    /// # Errors
    ///
    /// A secret's file that is neither.
    fn secret_files<'p>(
        &self,
        values: impl Iterator<Item = &'p Plain>,
    ) -> Result<Vec<SecretFile>, String> {
        let mut found: Vec<SecretFile> = Vec::new();
        let mut pending: Vec<&Plain> = values.collect();
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
        let devices = match devices_of(&inputs.attributes) {
            Ok(devices) => devices,
            Err(message) => {
                let span = self.graph.node(id).map_or(Span::new(0, 0), Node::span);
                self.finish_now(id, Err(Diagnostic::error(message, span)));
                return Ok(());
            }
        };
        let code: Vec<(String, Digest)> = inputs
            .code_files
            .iter()
            .filter_map(|&file| self.code_table.get(file as usize).cloned())
            .collect();
        let own = self.own_dir(id);
        let own_path = format!("{OUTPUTS}/{own}");
        let keyed = Keyed {
            code,
            own: own_path,
            contexts: self.contexts_of(&inputs).map_err(BuildError::Engine)?,
            inputs,
        };
        let mut given: Vec<(String, Kept)> = Vec::new();
        let base = self.receipt_of(&keyed, &[], Some(&mut given))?;
        // A record whose contents were evicted is no hit: the step runs
        // again and puts them back.
        let found = if self.settings.reuse == Reuse::Store {
            self.looked_up(&keyed, &base)?
                .filter(|record| self.holds_all(&record.result))
        } else {
            None
        };
        if let Some(record) = found {
            self.found_step(id, &record);
            return Ok(());
        }
        let looking = Looking {
            keyed,
            base,
            given,
            devices,
            records: false,
        };
        // Missed here, it is looked for in the remote cache, its manifest
        // first, on the pool.
        if self.settings.reuse == Reuse::Store
            && let Some(remote) = &self.settings.remote
        {
            let work = Work::Remote {
                remote: Arc::clone(remote),
                keys: vec![manifest_key(&looking.base)],
                manifest: true,
                store: self.store.clone(),
            };
            self.pool.send(id, work, None);
            self.background += 1;
            self.looking.insert(id, looking);
            return Ok(());
        }
        self.run_step(id, looking)
    }

    /// The step `id` found in the store, as `record` holds it.
    fn found_step(&mut self, id: NodeId, record: &StepRecord) {
        // Its outputs kept as a step's that ran are, since a target's own
        // code may read them (see [Exposure](crate::exposure#exposure)).
        for (path, kept) in record.result.files() {
            self.outputs.insert(path.to_owned(), kept);
            self.expose(path, kept);
        }
        // The store holds its long strings: `holds_all` found them, and no
        // eviction runs while the build has the store.
        let value = match record.result.plain(&self.store) {
            Ok(plain) => Ok(self.loaded.workspace.from_plain(&plain)),
            Err(error) => Err(Diagnostic::error(
                format!("the step's record names a string the store can't give: {error}"),
                Span::new(0, 0),
            )),
        };
        self.graph.resolve(id, value);
        self.cached += 1;
        self.report(id, (true, false));
        let step = self.step_name(id);
        self.emit(EventKind::Cached { step });
    }

    /// What the remote cache gave the step `id`, `found`, whether it found
    /// one of the records asked for, or why it could not be asked: its
    /// records are asked for next, once its manifest has been; then it is
    /// found in the store, or, missed, it runs.
    fn looked_remotely(
        &mut self,
        id: NodeId,
        found: Result<bool, String>,
    ) -> Result<(), BuildError> {
        let Some(mut looking) = self.looking.remove(&id) else {
            return Ok(());
        };
        if let Err(message) = found {
            self.remote_failed.get_or_insert(message);
        }
        if looking.records {
            let found = self
                .looked_up(&looking.keyed, &looking.base)?
                .filter(|record| self.holds_all(&record.result));
            if let Some(record) = found {
                self.found_remotely += 1;
                self.found_step(id, &record);
                return Ok(());
            }
        } else if let Some(remote) = &self.settings.remote {
            let receipts = self.receipts_by_manifest(&looking.keyed, &looking.base)?;
            if !receipts.is_empty() {
                let work = Work::Remote {
                    remote: Arc::clone(remote),
                    keys: receipts,
                    manifest: false,
                    store: self.store.clone(),
                };
                self.pool.send(id, work, None);
                self.background += 1;
                looking.records = true;
                self.looking.insert(id, looking);
                return Ok(());
            }
        }
        self.run_step(id, looking)
    }

    /// Runs the step `id`, `looking` as [`Run::start_step`] found it, in a
    /// sandbox of its own, its inputs staged.
    fn run_step(&mut self, id: NodeId, looking: Looking) -> Result<(), BuildError> {
        let Looking {
            keyed,
            base,
            given,
            devices,
            ..
        } = looking;
        let name = self.step_name(id);
        let own = self.own_dir(id);
        let inputs = &keyed.inputs;
        let (retries, timeout) = (retry_of(&inputs.attributes), timeout_of(&inputs.attributes));
        let secrets = self
            .secret_files(inputs.args.iter().chain(&inputs.captures))
            .map_err(BuildError::Engine)?;
        let scratch = self.scratch_of(id, (&name, &own), &inputs.attributes)?;
        let sandbox = self.fresh_sandbox(
            id,
            &own,
            0,
            &Staging {
                secrets: &secrets,
                devices: &devices,
                scratch: scratch.as_ref().map(Scratch::dir),
            },
            &keyed,
        )?;
        self.stage(id, &sandbox, &given);
        let started = Instant::now();
        let programs = Programs::starting((&self.settings.platform, &own), &given);
        self.steps.insert(
            id,
            StepRun {
                sandbox,
                phase: Phase::Staging(started),
                result: None,
                programs,
                given,
                secrets,
                devices,
                scratch,
                attempt: 0,
                keyed,
                base,
                reads: StepReads::default(),
                started,
                deadline: timeout.map(|limit| started + limit),
                retries,
                failed_once: false,
                waiting: Vec::new(),
                waiting_unit: None,
            },
        );
        self.emit(EventKind::Started {
            step: name,
            attempt: 0,
        });
        Ok(())
    }

    /// Whether the store holds every file and directory `stored` names, and
    /// every long string it holds as a blob.
    fn holds_all(&self, stored: &Stored) -> bool {
        stored.files().into_iter().all(|(_, kept)| match kept {
            Kept::File(entry) => self.store.has(&entry.digest),
            Kept::Dir(tree) => self.store.holds_tree(&tree),
        }) && stored.texts().iter().all(|text| self.store.has(text))
    }

    /// The scratch directory of the step `id`, named `name`, whose own
    /// output directory is `own`, if `attributes` mark it `#[scratch]` (see
    /// [Scratch directories](crate::scratch#scratch-directories)).
    fn scratch_of(
        &self,
        id: NodeId,
        (name, own): (&str, &str),
        attributes: &[Attribute],
    ) -> Result<Option<Scratch>, BuildError> {
        if !wants_scratch(attributes) {
            return Ok(None);
        }
        let fresh =
            self.settings
                .scratch
                .join(format!("kept-{}-{}", crate::sweep::token(), id.number()));
        let platform = &self.settings.platform;
        let scratch = Scratch::of(
            self.store.root(),
            (platform, own),
            &fresh,
            self.settings.strict,
        );
        if let Ok(scratch) = &scratch
            && let Some(name) = scratch.name()
        {
            self.store.note_scratch(name);
        }
        scratch.map(Some).map_err(|error| {
            engine(&format!(
                "cannot give `{name}` its scratch directory: {error}"
            ))
        })
    }

    /// A new sandbox for the attempt `attempt` of the step `id`, whose own
    /// output directory is `own`, `<target>/<step>` under `/pekit-out`, to
    /// be staged as `staging` says ([`Run::stage`]); nothing left behind if
    /// it cannot be made.
    fn fresh_sandbox(
        &self,
        id: NodeId,
        own: &str,
        attempt: u32,
        staging: &Staging<'_>,
        keyed: &Keyed,
    ) -> Result<Arc<StepSandbox>, BuildError> {
        let stage = self.stage_dir(id, attempt);
        if stage.exists() {
            remove_tree(&stage).map_err(|error| engine(&error))?;
        }
        let made = StepSandbox::new(&stage, own).map_err(|error| engine(&error));
        if made.is_err() {
            let _ = remove_tree(&stage);
        }
        Ok(Arc::new(
            made?
                .with_host(&self.settings.host_tools)
                .with_secrets(staging.secrets.to_vec())
                .with_devices(staging.devices.to_vec())
                .with_scratch(staging.scratch.map(Path::to_path_buf))
                .with_contexts(
                    keyed
                        .contexts
                        .iter()
                        .map(|context| (context.key.clone(), context.pending.clone()))
                        .collect(),
                    self.store.clone(),
                ),
        ))
    }

    /// Stages what the step `id` is given, `given`, in its sandbox
    /// `sandbox`, on the pool: it runs once that is done
    /// ([`Run::staged`]).
    fn stage(&mut self, id: NodeId, sandbox: &Arc<StepSandbox>, given: &[(String, Kept)]) {
        let work = Work::Stage {
            sandbox: Arc::clone(sandbox),
            given: given.to_vec(),
            store: self.store.clone(),
        };
        self.pool.send(id, work, None);
        self.background += 1;
    }

    /// The step `id`'s sandbox is staged, or could not be, `staged`: it
    /// runs from now, the time staging took none of its own.
    fn staged(&mut self, id: NodeId, staged: Result<(), String>) -> Result<(), BuildError> {
        staged.map_err(BuildError::Engine)?;
        if let Some(step) = self.steps.get_mut(&id)
            && let Phase::Staging(since) = step.phase
        {
            let waited = since.elapsed();
            if step.attempt == 0 {
                step.started += waited;
            }
            step.deadline = step.deadline.map(|deadline| deadline + waited);
            step.phase = Phase::Running;
        }
        Ok(())
    }

    /// Where the attempt `attempt` of the step `id` is staged.
    fn stage_dir(&self, id: NodeId, attempt: u32) -> PathBuf {
        self.settings.scratch.join(format!(
            "step-{}-{}-{attempt}",
            crate::sweep::token(),
            id.number()
        ))
    }

    /// Removes the staging directory of the step `step`, the node `id`, once
    /// it is done with.
    fn unstage(&mut self, id: NodeId, step: &StepRun) -> Result<(), BuildError> {
        let stage = self.stage_dir(id, step.attempt);
        self.sweeper.remove(&stage).map_err(|error| engine(&error))
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
        // Read only if it changed since a build last put it in the store.
        let host = self.loaded.libraries.host(&self.loaded.root, rest);
        // The build's outputs are no part of the workspace.
        let outputs = self.settings.outputs.as_path();
        if host.starts_with(outputs) {
            return Err(format!(
                "`{path}` is in the build's output directory, which is no part of the \
                 workspace: name an output at its path under `{OUTPUTS}`"
            ));
        }
        let (seen, now) = &mut self.workspace_seen;
        let unreadable = |error: io::Error| format!("cannot read `{path}`: {error}");
        let kept = if dir {
            let never = |_: &Path, _: &fs::Metadata| false;
            let tree = self
                .store
                .put_tree_seen_at(&host, (seen, *now), &never)
                .map_err(unreadable)?;
            Kept::Dir(tree.ok_or_else(|| format!("cannot read `{path}`"))?)
        } else {
            Kept::File(
                self.store
                    .put_file_seen_at(&host, (seen, *now))
                    .map_err(unreadable)?,
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
            .filter(|(_, step)| {
                matches!(step.phase, Phase::Running)
                    && step.waiting.is_empty()
                    && step.waiting_unit.is_none()
            })
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
        let began = self.began.elapsed();
        let ran = self.loaded.workspace.run_step(&mut self.graph, id, host);
        let unit = self.step_name(id);
        self.emit(EventKind::Evaluated {
            unit,
            step: true,
            began,
        });
        match ran {
            Ok((Progress::Paused(waits), reads)) => {
                if let Some(step) = self.steps.get_mut(&id) {
                    step.waiting.clone_from(&waits);
                    step.reads = reads;
                }
                let effects: Arc<dyn Effects + Send + Sync> = sandbox;
                for effect in waits {
                    // One its unit waited for before is running already.
                    if !self.in_flight.contains_key(&effect) && self.is_waiting(effect) {
                        self.dispatch(effect, Arc::clone(&effects), deadline, Some(id))?;
                    }
                }
                Ok(())
            }
            Ok((Progress::Waiting(unit), reads)) => {
                if let Some(step) = self.steps.get_mut(&id) {
                    step.waiting_unit = Some(unit);
                    step.reads = reads;
                }
                Ok(())
            }
            Ok((Progress::Done(value), reads)) => {
                if let Some(step) = self.steps.get_mut(&id) {
                    step.reads = reads;
                }
                self.succeeded(id, &value)
            }
            Err(failure) => self.failed(id, failure),
        }
    }

    /// The step `id` gave `value`: its result's files are kept in the store,
    /// on the pool, and it is recorded once they are ([`Run::kept`]),
    /// unless it names a file that is not its own.
    fn succeeded(&mut self, id: NodeId, value: &Value) -> Result<(), BuildError> {
        let plain = self
            .loaded
            .workspace
            .to_plain(value)
            .map_err(BuildError::Engine)?;
        let Some(step) = self.steps.get_mut(&id) else {
            return Ok(());
        };
        if let Some(path) = foreign_file(&plain, step) {
            let message = format!(
                "the step's result names `{path}`, which is neither in its output directory \
                 `{}` nor given to it: a step's files must be one or the other, so that no \
                 other step's have its paths",
                step.sandbox.own()
            );
            self.graph
                .resolve(id, Err(Diagnostic::error(message, Span::new(0, 0))));
            let name = self.step_name(id);
            self.emit(EventKind::Failed {
                step: name,
                retrying: false,
            });
            if let Some(step) = self.steps.remove(&id) {
                self.unstage(id, &step)?;
            }
            return Ok(());
        }
        step.phase = Phase::Keeping;
        step.result = Some(value.clone());
        let work = Work::Keep {
            plain,
            sandbox: Arc::clone(&step.sandbox),
            store: self.store.clone(),
        };
        self.pool.send(id, work, None);
        self.background += 1;
        Ok(())
    }

    /// The files of the result `plain` of the step `id` are kept, or could
    /// not be, `kept`: it is recorded, and its staging directory removed.
    fn kept(
        &mut self,
        id: NodeId,
        plain: &Plain,
        kept: Result<HashMap<String, Kept>, StoreError>,
    ) -> Result<(), BuildError> {
        let Some(mut step) = self.steps.remove(&id) else {
            return Ok(());
        };
        let value = step.result.take().unwrap_or(Value::Empty);
        let outcome = self.record(id, &step, (&value, plain), kept);
        self.unstage(id, &step)?;
        outcome
    }

    /// Records the step `step`, the node `id`, which gave `value`, `plain`
    /// as plain data, its files kept, `kept`, and gives it to the graph.
    fn record(
        &mut self,
        id: NodeId,
        step: &StepRun,
        (value, plain): (&Value, &Plain),
        kept: Result<HashMap<String, Kept>, StoreError>,
    ) -> Result<(), BuildError> {
        // Its files are kept whatever it holds: later steps are given them.
        let kept = match kept {
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
            self.expose(path, *entry);
        }
        match stored(plain, &kept, &self.store) {
            Ok(result) => self.keep_record(id, step, result)?,
            // A secret is never stored: the step runs every time.
            Err(StoreError::Secret) => {}
            Err(other) => return Err(BuildError::Engine(format!("{other:?}"))),
        }
        if step.failed_once {
            let name = self
                .step_names
                .get(&id.number())
                .cloned()
                .unwrap_or_default();
            self.flaky.push(name);
        }
        self.report(id, (false, step.failed_once));
        self.ran += 1;
        self.graph.resolve(id, Ok(value.clone()));
        let step = self.step_name(id);
        self.emit(EventKind::Finished { step });
        Ok(())
    }

    /// Records `result`, what the step `step`, the node `id`, gave, by its
    /// receipt; or, with [`Reuse::Compare`] and a record there already,
    /// compares it with that record's, noting a step that gave another.
    fn keep_record(
        &mut self,
        id: NodeId,
        step: &StepRun,
        result: Stored,
    ) -> Result<(), BuildError> {
        let receipt = self.read_receipt(step)?;
        if self.settings.reuse == Reuse::Compare
            && let Some(first) = self
                .store
                .record::<StepRecord>(&receipt)
                .map_err(|error| engine(&error))?
        {
            if first.result != result {
                let differences = differences(&self.store, &first.result, &result);
                let step = self.own_dir(id);
                self.mismatches.push(Mismatch { step, differences });
            }
            return Ok(());
        }
        let record = StepRecord {
            receipt,
            result,
            duration_ms: u64::try_from(step.started.elapsed().as_millis()).unwrap_or(u64::MAX),
        };
        self.store
            .put_record(&receipt, &record)
            .map_err(|error| engine(&error))?;
        self.remember_reads(step)?;
        // Recorded, it is written to the remote cache, on the pool, which
        // the build's end waits for.
        if self.settings.reuse == Reuse::Store
            && let Some(remote) = self
                .settings
                .remote
                .as_ref()
                .filter(|remote| remote.uploads())
        {
            let work = Work::Upload {
                remote: Arc::clone(remote),
                keys: vec![receipt],
                manifest: Some(manifest_key(&step.base)),
                store: self.store.clone(),
            };
            self.pool.send(id, work, None);
            self.uploads += 1;
        }
        Ok(())
    }

    /// An upload to the remote cache ended, `uploaded`: a failed one is
    /// noted, and fails no build.
    fn uploaded(&mut self, uploaded: Result<(), String>) {
        self.uploads -= 1;
        if let Err(message) = uploaded {
            self.remote_failed.get_or_insert(message);
        }
    }

    /// Waits for every upload to the remote cache still on the pool, once
    /// the build's targets are done; anything else that ends meanwhile the
    /// build no longer needs.
    fn finish_uploads(&mut self) -> Result<(), BuildError> {
        while self.uploads > 0 {
            let Some(ran) = self.pool.next() else {
                return Err(BuildError::Engine(
                    "the threads effects run on stopped".to_owned(),
                ));
            };
            if let Done::Uploaded(uploaded) = ran.done {
                self.uploaded(uploaded);
            }
        }
        Ok(())
    }

    /// Adds the set of contexts the step `step`'s last run read to the
    /// manifest of its base, merged with what is there, so that a build
    /// recording another set beside it loses neither.
    fn remember_reads(&self, step: &StepRun) -> Result<(), BuildError> {
        let keys = read_keys(step);
        let at = manifest_key(&step.base);
        let mut manifest = self
            .store
            .record::<Manifest>(&at)
            .map_err(|error| engine(&error))?
            .unwrap_or_default();
        if manifest.sets.contains(&keys) {
            return Ok(());
        }
        manifest.sets.push(keys);
        self.store
            .put_record(&at, &manifest)
            .map_err(|error| engine(&error))
    }

    /// The receipt of the step `step` by the contexts its last run read: its
    /// base and each's value now (see `design/engine.md`, R2).
    ///
    /// # Errors
    ///
    /// That it read a context it cannot read, which would be GCSL's error,
    /// or that a file a value names cannot be hashed.
    fn read_receipt(&mut self, step: &StepRun) -> Result<Digest, BuildError> {
        for (key, bound) in &step.reads.keys {
            let readable = step
                .keyed
                .contexts
                .iter()
                .any(|context| &context.key == key);
            if *bound != readable {
                return Err(BuildError::Engine(format!(
                    "a step read the context `{key}`, which it cannot read so: GCSL's needs \
                     and its reads disagree"
                )));
            }
        }
        self.receipt_of(&step.keyed, &read_keys(step), None)
    }

    /// The step `keyed` found in the store, by each set of contexts its
    /// manifest has for its base `base`, its receipt by each made of the
    /// values its context gives them now, the first found.
    fn looked_up(
        &mut self,
        keyed: &Keyed,
        base: &Digest,
    ) -> Result<Option<StepRecord>, BuildError> {
        for receipt in self.receipts_by_manifest(keyed, base)? {
            let found = self
                .store
                .record::<StepRecord>(&receipt)
                .map_err(|error| engine(&error))?;
            if found.is_some() {
                return Ok(found);
            }
        }
        Ok(None)
    }

    /// The receipts the step `keyed` is looked up by: one for each set of
    /// contexts the store's manifest has for its base `base`, made of the
    /// values its context gives them now, in the manifest's order.
    fn receipts_by_manifest(
        &mut self,
        keyed: &Keyed,
        base: &Digest,
    ) -> Result<Vec<Digest>, BuildError> {
        let manifest = self
            .store
            .record::<Manifest>(&manifest_key(base))
            .map_err(|error| engine(&error))?;
        manifest
            .map(|manifest| manifest.sets)
            .unwrap_or_default()
            .iter()
            .map(|keys| self.receipt_of(keyed, keys, None))
            .collect()
    }

    /// The receipt of the step `keyed` by the contexts `keys`, each with the
    /// value its context gives it, or unbound; its base if `keys` is empty.
    /// Each file a value names is hashed as an input, and, if `given` is,
    /// added to it to be staged.
    fn receipt_of(
        &mut self,
        keyed: &Keyed,
        keys: &[String],
        mut given: Option<&mut Vec<(String, Kept)>>,
    ) -> Result<Digest, BuildError> {
        let context: Vec<(String, Option<Plain>)> = keys
            .iter()
            .map(|key| {
                let value = keyed.contexts.iter().find(|context| &context.key == key);
                (key.clone(), value.map(|context| context.plain.clone()))
            })
            .collect();
        let table = Rc::clone(&self.code_table);
        let code_of = move |input: u32| table.get(input as usize).cloned();
        let of = ReceiptOf {
            code: &keyed.code,
            args: &keyed.inputs.args,
            captures: &keyed.inputs.captures,
            context: &context,
            own: &keyed.own,
            code_of: &code_of,
            platform: &self.settings.platform,
        };
        let mut contents = |path: &str, dir: bool| -> Result<Digest, String> {
            // The host snapshot's own, which every step binds already,
            // keyed by its path on top of the snapshot's fingerprint.
            if self.is_host_path(path) {
                return Ok(Digest::of(format!("host-snapshot:{path}").as_bytes()));
            }
            let kept = self.input(path, dir)?;
            if let Some(given) = given.as_deref_mut() {
                given.push((path.to_owned(), kept));
            }
            Ok(match kept {
                Kept::File(entry) => entry.digest,
                Kept::Dir(tree) => tree,
            })
        };
        receipt(of, &mut contents).map_err(BuildError::Engine)
    }

    /// Each context `inputs` says its step can read, with what it gives the
    /// step once the step reads it: the files and directories its value
    /// names, but the host snapshot's, which every step binds, each as the
    /// store keeps it, directories first; and its secret files.
    fn contexts_of(&mut self, inputs: &StepInputs) -> Result<Vec<ContextInput>, String> {
        let mut found = Vec::with_capacity(inputs.context.len());
        for (key, plain) in &inputs.context {
            let mut placed: Vec<(&str, bool)> = plain.files();
            placed.sort_unstable();
            placed.dedup();
            let mut files = Vec::new();
            for (path, dir) in placed {
                if !self.is_host_path(path) {
                    files.push((path.to_owned(), self.input(path, dir)?));
                }
            }
            let secrets = self.secret_files(std::iter::once(plain))?;
            found.push(ContextInput {
                key: key.clone(),
                plain: plain.clone(),
                pending: Pending { files, secrets },
            });
        }
        Ok(found)
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
        let own = self.own_dir(id);
        let attempt = step.attempt + 1;
        let sandbox = self.fresh_sandbox(
            id,
            &own,
            attempt,
            &Staging {
                secrets: &step.secrets,
                devices: &step.devices,
                scratch: step.scratch.as_ref().map(Scratch::dir),
            },
            &step.keyed,
        )?;
        self.stage(id, &sandbox, &step.given);
        self.graph.forget_unit(STEP_UNITS + id.number());
        let programs = Programs::starting((&self.settings.platform, &own), &step.given);
        self.steps.insert(
            id,
            StepRun {
                sandbox,
                phase: Phase::Staging(Instant::now()),
                result: None,
                programs,
                attempt,
                retries: step.retries - 1,
                failed_once: true,
                waiting: Vec::new(),
                waiting_unit: None,
                ..step
            },
        );
        self.emit(EventKind::Started {
            step: name,
            attempt,
        });
        Ok(())
    }

    /// The job `ran` finished: the engine's own work for a step, or an
    /// effect.
    fn finished(&mut self, mut ran: Ran) -> Result<(), BuildError> {
        let worked = |work: &str| Event {
            at: ran.ended.saturating_duration_since(self.began),
            kind: EventKind::Worked {
                step: self.step_name(ran.node),
                work: work.to_owned(),
                worker: ran.worker,
                began: ran.began.saturating_duration_since(self.began),
            },
        };
        match std::mem::replace(&mut ran.done, Done::Effect) {
            Done::Staged(staged) => {
                let event = worked("stage");
                (self.events)(&event);
                self.background -= 1;
                self.staged(ran.node, staged)
            }
            Done::Kept(kept) => {
                let event = worked("keep");
                (self.events)(&event);
                self.background -= 1;
                let plain = ran.outcome.unwrap_or(Plain::Empty);
                self.kept(ran.node, &plain, kept)
            }
            Done::Remote(found) => {
                let event = worked("remote");
                (self.events)(&event);
                self.background -= 1;
                self.looked_remotely(ran.node, found)
            }
            Done::Uploaded(uploaded) => {
                let event = worked("upload");
                (self.events)(&event);
                self.uploaded(uploaded);
                Ok(())
            }
            done => {
                ran.done = done;
                self.finished_effect(ran);
                Ok(())
            }
        }
    }

    /// The effect `ran` finished: gives it to the graph, the step that waits
    /// on it able to run again.
    fn finished_effect(&mut self, ran: Ran) {
        let node = ran.node;
        let of = self.in_flight.get(&node).and_then(|running| running.of);
        let mut fold = None;
        if let Some(mut running) = self.in_flight.remove(&node) {
            fold = running.fold.take();
            let at = ran.ended.saturating_duration_since(self.began);
            let found = matches!(&ran.done, Done::Program(program) if program.found);
            let kind = match running.step {
                Some(step) if found => EventKind::EffectFound {
                    node: node.number(),
                    step,
                    effect: running.effect,
                },
                step => EventKind::EffectRan {
                    node: node.number(),
                    step,
                    effect: running.effect,
                    worker: ran.worker,
                    began: ran.began.saturating_duration_since(self.began),
                    ok: ran.outcome.is_ok(),
                },
            };
            (self.events)(&Event { at, kind });
        }
        let mut outcome = ran.outcome;
        for (path, kept) in ran.kept.iter().cloned() {
            // What a step unpacked is in its own sandbox; what the target
            // fetched or unpacked is under the output directory.
            let placed = if let Some(step) = of.and_then(|step| self.steps.get(&step)) {
                step.sandbox
                    .stage(&self.store, &path, kept)
                    .map_err(|error| engine(&error))
            } else {
                self.expose(&path, kept);
                Ok(())
            };
            if let Err(BuildError::Engine(message)) = placed {
                outcome = Err(Diagnostic::error(message, Span::new(0, 0)));
            }
            self.outputs.insert(path, kept);
        }
        if let Some(step) = of {
            if let Some((builtin, args)) = &fold {
                self.program_folded(step, (*builtin, args), &ran.kept);
            }
            if let Done::Program(program) = ran.done {
                self.program_ran(step, program, &outcome);
            }
        }
        let value = outcome.map(|plain| self.loaded.workspace.from_plain(&plain));
        self.finish_now(node, value);
    }

    /// Copies every output under `/pekit-out` that `value`, the value of the
    /// root `index`, names out to the output directory, each file a copy of
    /// its own, and makes its target's view of those its steps made (see
    /// [`target_view`]).
    fn copy_out(&mut self, value: &Value, index: usize) -> Result<(), BuildError> {
        let plain = self
            .loaded
            .workspace
            .to_plain(value)
            .map_err(BuildError::Engine)?;
        let files = plain.files();
        for &(path, _) in &files {
            let Some(at) = output_path(&self.settings.outputs, path) else {
                continue;
            };
            // A result inside an output is copied over it once it is
            // there; whatever is waiting to be exposed in its place is not.
            for (above, kept) in self.exposures.take_above(path) {
                self.write_exposure(&above, kept)?;
            }
            self.exposures.forget_within(path);
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
        let Some(root) = self.roots.get(index) else {
            return Ok(());
        };
        let paths: Vec<&str> = files.iter().map(|&(path, _)| path).collect();
        for (path, view) in target_view(&root.dir, &paths) {
            let (Some(from), Some(at)) = (
                output_path(&self.settings.outputs, &path),
                output_path(&self.settings.outputs, &view),
            ) else {
                continue;
            };
            link_view(&from, &at)
                .map_err(|error| engine(&format!("cannot write `{}`: {error}", at.display())))?;
        }
        Ok(())
    }
}

/// The view of the outputs a target's value names that steps made, each
/// under `/pekit-out/.steps/<function>/<identity>/`: each at the same path
/// under `/pekit-out/<target>/<function>/`, the steps of one function told
/// apart in the order the value first names them, `<function>.2` the second,
/// so that what a build made is found by its target's name. Each output's
/// logical path, with its view's.
#[must_use]
pub fn target_view(target: &str, paths: &[&str]) -> Vec<(String, String)> {
    let prefix = format!("{OUTPUTS}/{STEPS}/");
    let mut named: Vec<(String, String)> = Vec::new();
    let mut counts: HashMap<String, u32> = HashMap::new();
    let mut views = Vec::new();
    for &path in paths {
        let Some(rest) = path.strip_prefix(&prefix) else {
            continue;
        };
        let mut parts = rest.splitn(3, '/');
        let (Some(function), Some(identity)) = (parts.next(), parts.next()) else {
            continue;
        };
        let step = format!("{function}/{identity}");
        let known = named
            .iter()
            .find(|(known, _)| *known == step)
            .map(|(_, shown)| shown.clone());
        let shown = known.unwrap_or_else(|| {
            let count = counts.entry(function.to_owned()).or_insert(0);
            *count += 1;
            let shown = if *count == 1 {
                function.to_owned()
            } else {
                format!("{function}.{count}")
            };
            named.push((step, shown.clone()));
            shown
        });
        let view = match parts.next() {
            Some(inner) => format!("{OUTPUTS}/{target}/{shown}/{inner}"),
            None => format!("{OUTPUTS}/{target}/{shown}"),
        };
        if !views
            .iter()
            .any(|(known, _): &(String, String)| *known == path)
        {
            views.push((path.to_owned(), view));
        }
    }
    views
}

/// Makes `at` a symbolic link to `from`, by a relative path, whatever was
/// there before.
fn link_view(from: &Path, at: &Path) -> io::Result<()> {
    if let Ok(meta) = fs::symlink_metadata(at) {
        if meta.is_dir() {
            fs::remove_dir_all(at)?;
        } else {
            fs::remove_file(at)?;
        }
    }
    let parent = at.parent().unwrap_or_else(|| Path::new("."));
    fs::create_dir_all(parent)?;
    std::os::unix::fs::symlink(relative_to(from, parent), at)
}

/// The path from the directory `base` to `to`, both absolute.
fn relative_to(to: &Path, base: &Path) -> PathBuf {
    let to: Vec<_> = to.components().collect();
    let from: Vec<_> = base.components().collect();
    let shared = to.iter().zip(&from).take_while(|(a, b)| a == b).count();
    let mut path = PathBuf::new();
    for _ in shared..from.len() {
        path.push("..");
    }
    path.extend(&to[shared..]);
    path
}

/// The path under the output directory a logical output path is at.
#[must_use]
pub fn output_path(outputs: &Path, path: &str) -> Option<PathBuf> {
    let rest = path.strip_prefix(OUTPUTS)?.strip_prefix('/')?;
    Some(outputs.join(rest))
}
