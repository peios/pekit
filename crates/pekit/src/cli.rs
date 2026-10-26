//! The `pekit` command.
//!
//! # Commands
//!
//! `pekit build TARGET...` builds the definitions `TARGET...` of the
//! def-type `build`, each named or selected by a directory glob as
//! [Targets](crate::target#targets) says, from the workspace the current
//! directory is in, as one build ([`build_all_observed`]). It shows the files
//! each target gave, and how many steps ran and how many were found in the
//! store; a target that fails, its error. `pekit test [TEST...]` runs the
//! definitions of the def-type `test` it names or selects, every one below
//! the current directory by default, as one build, and reports each:
//! `passed`, `passed (cached)` if its step was found in the store, `flaky`
//! if it passed only on a retry, or `FAILED` with its failure's message.
//! `pekit clean` removes the workspace's `pekit-out/`, and `pekit clean
//! --cache` the store and the staging directory under the cache directory
//! too, so that every step runs again; neither removes the state directory.
//! `--jobs N` runs at most `N` programs at once, by default as many as the
//! machine has processors, `--profile NAME` uses the keychain's secrets of
//! the profile `NAME`, `dev` by default ([Secrets](crate::keychain#secrets)),
//! `--trace FILE` writes a Chrome trace of the build ([The
//! trace](crate::trace#the-trace)), `--platform ARCH[-OS]` builds for that
//! platform, `--rebuild` reuses nothing from the store, and `--no-dynamic`
//! fails a target that waits for a step's result. On a terminal, a line
//! shows the steps running as it goes.
//!
//! The store and the directories steps are staged in are under the cache
//! directory: `$PEKIT_CACHE_DIR` if it is set, else `pekit` under
//! `$XDG_CACHE_HOME`, else `~/.cache/pekit`. What is this machine's alone,
//! the key secrets' identities are hashed with, is in the state directory:
//! `$PEKIT_STATE_DIR`, else `pekit` under `$XDG_STATE_HOME`, else
//! `~/.local/state/pekit`. Outputs are copied to `pekit-out/` at the
//! workspace's root.
//!
//! It exits with 0 once every target is built or every test passed, 1 if
//! one was not, and 2 for a command it does not understand.

use std::ffi::OsString;
use std::fmt::Write as _;
use std::io::Write;
use std::path::{Component, Path, PathBuf};
use std::process::ExitCode;

use clap::{Parser, Subcommand};
use gcsl::eval::plain::Plain;

use crate::evict::limit_of;
use crate::engine::{
    BuildError, Outcomes, Packing, Reuse, Settings, build_all_observed, package_all_observed,
    program_command, query_all, run_script_observed,
};
use crate::query::TargetDeps;
use crate::events::{Event, EventKind};
use crate::host::HostSnapshot;
use crate::platform::ExecutionPlatform;
use crate::layers::remove_tree;
use crate::load::{LoadError, Loaded, load};
use crate::release::Repository;
use crate::lock;
use crate::store::Store;
use crate::sandbox::HostTools;
use crate::target::{Target, TargetError, select};
use crate::trace::Trace;

/// Pekit, a build system that runs GCSL build files as an action graph.
#[derive(Debug, Parser)]
#[command(name = "pekit", version)]
struct Cli {
    /// What to do.
    #[command(subcommand)]
    command: Command,
}

/// A command (see [Commands](#commands)).
#[derive(Debug, Subcommand)]
enum Command {
    /// Build targets.
    Build(BuildArgs),
    /// Run tests.
    Test(TestArgs),
    /// Run a script, or the program a build gives.
    Run(RunArgs),
    /// Ask what targets reach, without running a step.
    #[command(subcommand)]
    Query(QueryCommand),
    /// Pack packages with the workspace's packagers.
    Package(PackageArgs),
    /// Pack packages and publish them into a store.
    Publish(PublishArgs),
    /// Remove what builds left: `pekit-out/`, and with `--cache` the store.
    Clean(CleanArgs),
    /// Pin the libraries the workspace imports in `pekit.lock`.
    Lock(LockArgs),
    /// Build a package from `HEAD` strictly, publish it, and tag it.
    Release(ReleaseArgs),
}

/// `pekit release`'s arguments (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct ReleaseArgs {
    /// The package, named as `pekit package` names one.
    #[arg(value_name = "PACKAGE")]
    package: String,
    /// The store to publish into: a path or a URL, as each packager reads
    /// it.
    #[arg(long, value_name = "STORE")]
    to: String,
    /// How many programs may run at once.
    #[arg(long, short)]
    jobs: Option<usize>,
    /// Write a Chrome trace of the build to this file.
    #[arg(long, value_name = "FILE")]
    trace: Option<PathBuf>,
}

/// `pekit lock`'s arguments (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct LockArgs {
    /// Pin every library again at its source's revision, not only those
    /// not pinned yet.
    #[arg(long)]
    update: bool,
}

/// `pekit run`'s arguments (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct RunArgs {
    /// What every command that builds takes, before the name.
    #[command(flatten)]
    shared: Shared,
    /// The script to run, or the build whose program to run, named as a
    /// target is.
    #[arg(value_name = "NAME")]
    name: String,
    /// What it is given: a script's `^script_session.args`, or the
    /// program's arguments.
    #[arg(
        trailing_var_arg = true,
        allow_hyphen_values = true,
        value_name = "ARG"
    )]
    args: Vec<String>,
}

/// `pekit query`'s questions (see [Commands](#commands)).
#[derive(Debug, clap::Subcommand)]
enum QueryCommand {
    /// The steps targets reach, and the workspace files each reads.
    Deps(DepsArgs),
    /// The targets that read a workspace file.
    Rdeps(RdepsArgs),
}

/// `pekit query deps`'s arguments (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct DepsArgs {
    /// The targets, named or selected as `pekit build` names them.
    #[arg(required = true, value_name = "TARGET")]
    targets: Vec<String>,
    /// The def-type of the targets: `build`, `test`, `package` or another.
    #[arg(long, default_value = "build")]
    kind: String,
    /// What every command that builds takes.
    #[command(flatten)]
    shared: Shared,
}

/// `pekit query rdeps`'s arguments (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct RdepsArgs {
    /// The file, a path from this directory.
    #[arg(value_name = "FILE")]
    file: PathBuf,
    /// The targets to ask of, named or selected as `pekit build` names
    /// them; every one below this directory, `**`, if none is given.
    #[arg(value_name = "TARGET")]
    targets: Vec<String>,
    /// The def-type of the targets: `build`, `test`, `package` or another.
    #[arg(long, default_value = "build")]
    kind: String,
    /// What every command that builds takes.
    #[command(flatten)]
    shared: Shared,
}

/// What a command does with the targets it builds.
#[derive(Clone, Copy, Debug)]
enum Doing<'a> {
    /// Builds them.
    Build,
    /// Packs them, or packs and publishes them.
    Pack(&'a Packing),
    /// Runs the one script given, with these words.
    Script(&'a [String]),
}

/// `pekit package`'s arguments (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct PackageArgs {
    /// The packages, named or selected as targets are; every package below
    /// this directory, `**`, if none is given.
    #[arg(value_name = "PACKAGE")]
    packages: Vec<String>,
    /// What every command that builds takes.
    #[command(flatten)]
    shared: Shared,
}

/// `pekit publish`'s arguments (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct PublishArgs {
    /// The packages, named or selected as targets are.
    #[arg(required = true, value_name = "PACKAGE")]
    packages: Vec<String>,
    /// The store to publish into: a path or a URL, as each packager reads
    /// it.
    #[arg(long, value_name = "STORE")]
    to: String,
    /// What every command that builds takes.
    #[command(flatten)]
    shared: Shared,
}

/// `pekit clean`'s arguments (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct CleanArgs {
    /// Remove the store and the staging directories too, so that every step
    /// runs again.
    #[arg(long)]
    cache: bool,
}

/// `pekit test`'s arguments (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct TestArgs {
    /// The tests, named or selected as targets are; every test below this
    /// directory, `**`, if none is given.
    #[arg(value_name = "TEST")]
    tests: Vec<String>,
    /// What every command that builds takes.
    #[command(flatten)]
    shared: Shared,
}

/// `pekit build`'s arguments (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct BuildArgs {
    /// The targets: `hello`, qualified by its directory, `app/hello`, or a
    /// directory glob, `pkgs/*` or `**`.
    #[arg(required = true, value_name = "TARGET")]
    targets: Vec<String>,
    /// What every command that builds takes.
    #[command(flatten)]
    shared: Shared,
}

/// Builds `targets` of `loaded`'s workspace once with `settings`, or does
/// with them what `doing` says, `events` seeing it as it goes.
///
/// # Errors
///
/// What [`build_all_observed`], [`package_all_observed`] or
/// [`run_script_observed`] fails with.
fn build_once(
    (loaded, targets, doing): (&mut Loaded, &[Target], Doing<'_>),
    settings: &Settings,
    events: &mut dyn FnMut(&Event),
) -> Result<Outcomes, BuildError> {
    match (doing, targets) {
        (Doing::Build, _) => build_all_observed(loaded, targets, settings, events),
        (Doing::Pack(packing), _) => {
            package_all_observed(loaded, targets, (settings, packing), events)
        }
        (Doing::Script(args), [script]) => {
            run_script_observed(loaded, script, (settings, args), events)
        }
        (Doing::Script(_), _) => Err(BuildError::Engine("a script is run alone".to_owned())),
    }
}

/// The flags every command that builds takes (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct Shared {
    /// How many programs may run at once.
    #[arg(long, short)]
    jobs: Option<usize>,
    /// Write a Chrome trace of the build to this file.
    #[arg(long, value_name = "FILE")]
    trace: Option<PathBuf>,
    /// The keychain's profile, whose secrets the build uses.
    #[arg(long, default_value = "dev")]
    profile: String,
    /// The platform to build for, `ARCH` or `ARCH-OS`: `aarch64-linux`.
    #[arg(long, value_name = "PLATFORM")]
    platform: Option<String>,
    /// Whether steps are taken from the store.
    #[command(flatten)]
    reuse: ReuseFlags,
    /// What the build allows.
    #[command(flatten)]
    allows: AllowFlags,
}

/// The shared flags of what is taken from the store (see [`Shared`]).
#[derive(Debug, clap::Args)]
struct ReuseFlags {
    /// Run every step, reusing nothing from the store.
    #[arg(long)]
    rebuild: bool,
    /// Build, then run every step again and fail if one gives other outputs
    /// from the same inputs.
    #[arg(long)]
    check_reproducible: bool,
}

/// The shared flags of what a build allows (see [`Shared`]).
#[derive(Debug, clap::Args)]
struct AllowFlags {
    /// Fail a target that waits for a step's result, a dynamic dependency.
    #[arg(long)]
    no_dynamic: bool,
    /// Build as a release does: steps borrow no host tools, and a
    /// target's own code only fetches, unpacks, asks the keychain and reads
    /// the workspace and the build's outputs.
    #[arg(long)]
    strict: bool,
}

/// Where the command writes: what it shows, its warnings and errors, and
/// whether the latter is a terminal, which shows a build's progress as it
/// goes.
pub struct Terminal<'a> {
    /// What it shows.
    pub out: &'a mut dyn Write,
    /// Its warnings, errors and progress.
    pub err: &'a mut dyn Write,
    /// Whether `err` is a terminal.
    pub live: bool,
}

/// Runs the command `args`, `pekit` first, from the directory `cwd`,
/// writing to `terminal` (see [Commands](#commands)). Like all of GCSL's
/// pipeline, it must run on a thread with at least [`gcsl::STACK_SIZE`]
/// bytes of stack.
pub fn run(
    args: impl IntoIterator<Item = OsString>,
    cwd: &Path,
    terminal: &mut Terminal<'_>,
) -> ExitCode {
    let cli = match Cli::try_parse_from(args) {
        Ok(cli) => cli,
        Err(error) => {
            let shown = error.render().to_string();
            if error.use_stderr() {
                let _ = write!(terminal.err, "{shown}");
                return ExitCode::from(2);
            }
            let _ = write!(terminal.out, "{shown}");
            return ExitCode::SUCCESS;
        }
    };
    match cli.command {
        Command::Build(args) => build_command(&args, cwd, terminal),
        Command::Test(args) => test_command(&args, cwd, terminal),
        Command::Run(args) => run_command(&args, cwd, terminal),
        Command::Query(QueryCommand::Deps(args)) => deps_command(&args, cwd, terminal),
        Command::Query(QueryCommand::Rdeps(args)) => rdeps_command(&args, cwd, terminal),
        Command::Package(args) => package_command(
            (&args.packages, &Packing::Pack),
            &args.shared,
            cwd,
            terminal,
        ),
        Command::Publish(args) => {
            let packing = Packing::Publish {
                to: args.to.clone(),
            };
            package_command((&args.packages, &packing), &args.shared, cwd, terminal)
        }
        Command::Clean(args) => clean_command(&args, cwd, terminal),
        Command::Lock(args) => lock_command(&args, cwd, terminal),
        Command::Release(args) => release_command(&args, cwd, terminal),
    }
}

/// `pekit build TARGET...` (see [Commands](#commands)).
fn build_command(args: &BuildArgs, cwd: &Path, terminal: &mut Terminal<'_>) -> ExitCode {
    let Some(mut loaded) = load_for(cwd, &args.shared, terminal) else {
        return ExitCode::FAILURE;
    };
    let names: Vec<&str> = args.targets.iter().map(String::as_str).collect();
    let targets = match select(&loaded.workspace, "build", &names, &loaded.here) {
        Ok(targets) if targets.is_empty() => {
            let _ = writeln!(
                terminal.err,
                "error: no `build` is selected by {}",
                quoted(&names)
            );
            return ExitCode::FAILURE;
        }
        Ok(targets) => targets,
        Err(error) => {
            let _ = writeln!(terminal.err, "{}", error.message());
            return ExitCode::FAILURE;
        }
    };
    let Some(settings) = settings_for(&args.shared, &loaded, terminal) else {
        return ExitCode::FAILURE;
    };
    let outcomes = match build_targets(
        (&mut loaded, &targets, Doing::Build),
        &settings,
        &args.shared,
        terminal,
    ) {
        Ok(outcomes) => outcomes,
        Err(error) => {
            let _ = write!(terminal.err, "{}", error.render(&loaded));
            return ExitCode::FAILURE;
        }
    };
    let counts = steps_counted(&outcomes);
    warn_of_eviction(&outcomes, terminal);
    let mut failed = false;
    for (target, outcome) in targets.iter().zip(&outcomes.targets) {
        match outcome {
            Ok(value) => {
                if targets.len() == 1 {
                    let _ = writeln!(terminal.out, "built {}: {counts}", target.qualified());
                } else {
                    let _ = writeln!(terminal.out, "built {}", target.qualified());
                }
                for (path, _) in value.files() {
                    let shown = shown_path(path, &loaded.root, cwd);
                    let _ = writeln!(terminal.out, "  {}", shown.display());
                }
            }
            Err(error) => {
                failed = true;
                let _ = write!(terminal.err, "{}", error.render(&loaded));
            }
        }
    }
    if targets.len() > 1 {
        let _ = writeln!(terminal.out, "{counts}");
    }
    if outcomes.ran > 0 && !settings.host_tools.bind.is_empty() {
        let _ = writeln!(
            terminal.err,
            "note: steps ran with the host's own tools, so this build is not hermetic"
        );
    }
    if failed {
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
    }
}

/// `pekit test [TEST...]` (see [Commands](#commands)).
fn test_command(args: &TestArgs, cwd: &Path, terminal: &mut Terminal<'_>) -> ExitCode {
    let Some(mut loaded) = load_for(cwd, &args.shared, terminal) else {
        return ExitCode::FAILURE;
    };
    let mut names: Vec<&str> = args.tests.iter().map(String::as_str).collect();
    if names.is_empty() {
        names.push("**");
    }
    let tests = match select(&loaded.workspace, "test", &names, &loaded.here) {
        Ok(tests) if tests.is_empty() => {
            let _ = writeln!(
                terminal.err,
                "error: no `test` is selected by {}",
                quoted(&names)
            );
            return ExitCode::FAILURE;
        }
        Ok(tests) => tests,
        Err(error) => {
            let _ = writeln!(terminal.err, "{}", error.message());
            return ExitCode::FAILURE;
        }
    };
    let Some(settings) = settings_for(&args.shared, &loaded, terminal) else {
        return ExitCode::FAILURE;
    };
    let outcomes = match build_targets(
        (&mut loaded, &tests, Doing::Build),
        &settings,
        &args.shared,
        terminal,
    ) {
        Ok(outcomes) => outcomes,
        Err(error) => {
            let _ = write!(terminal.err, "{}", error.render(&loaded));
            return ExitCode::FAILURE;
        }
    };
    let (mut passed, mut failed) = (0, 0);
    for (index, (test, outcome)) in tests.iter().zip(&outcomes.targets).enumerate() {
        let name = test.definition.path.rsplit('.').next().unwrap_or_default();
        let step = outcomes
            .steps
            .iter()
            .find(|report| report.target == index && report.name == name);
        let shown = test.qualified();
        let message = match outcome {
            Ok(result) => match test_result(result) {
                Some((true, _)) => {
                    passed += 1;
                    let label = match step {
                        Some(report) if report.flaky => "flaky",
                        Some(report) if report.cached => "passed (cached)",
                        _ => "passed",
                    };
                    let _ = writeln!(terminal.out, "{label:<16} {shown}");
                    continue;
                }
                Some((false, message)) => message.to_owned(),
                None => format!("a test that gave no result, but {result:?}"),
            },
            Err(error) => error.render(&loaded),
        };
        failed += 1;
        let _ = writeln!(terminal.out, "{:<16} {shown}", "FAILED");
        for line in message.lines() {
            let _ = writeln!(terminal.out, "    {line}");
        }
    }
    let _ = writeln!(terminal.out, "{passed} passed, {failed} failed");
    if failed > 0 {
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
    }
}

/// `pekit run NAME [ARG...]` (see [Commands](#commands)): runs the script
/// `NAME` with the words `ARG`, or builds the build `NAME` and runs the one
/// executable file it gives with the arguments `ARG`, exiting as the
/// program does.
fn run_command(args: &RunArgs, cwd: &Path, terminal: &mut Terminal<'_>) -> ExitCode {
    if args.shared.reuse.check_reproducible {
        return changes_the_world("run", terminal);
    }
    let Some(mut loaded) = load_for(cwd, &args.shared, terminal) else {
        return ExitCode::FAILURE;
    };
    let (target, script) = match runnable(&loaded, &args.name) {
        Ok(found) => found,
        Err(error) => {
            let _ = writeln!(terminal.err, "{error}");
            return ExitCode::FAILURE;
        }
    };
    let Some(settings) = settings_for(&args.shared, &loaded, terminal) else {
        return ExitCode::FAILURE;
    };
    let doing = if script {
        Doing::Script(&args.args)
    } else {
        Doing::Build
    };
    let targets = [target];
    let built = build_targets(
        (&mut loaded, &targets, doing),
        &settings,
        &args.shared,
        terminal,
    );
    let value = match built.map(|mut outcomes| outcomes.targets.remove(0)) {
        Ok(Ok(value)) => value,
        Ok(Err(error)) | Err(error) => {
            let _ = write!(terminal.err, "{}", error.render(&loaded));
            return ExitCode::FAILURE;
        }
    };
    if script {
        return ExitCode::SUCCESS;
    }
    let shown = targets[0].qualified();
    let Plain::File { path, .. } = &value else {
        let _ = writeln!(
            terminal.err,
            "error: `{shown}` gives no program to run: `pekit run` runs a build that gives one \
             executable file"
        );
        return ExitCode::FAILURE;
    };
    let ran = program_command(&loaded, &settings, (path, &args.args)).and_then(|mut command| {
        command
            .status()
            .map_err(|error| BuildError::Engine(error.to_string()))
    });
    match ran {
        Ok(status) => status
            .code()
            .and_then(|code| u8::try_from(code).ok())
            .map_or(ExitCode::FAILURE, ExitCode::from),
        Err(error) => {
            let _ = write!(terminal.err, "{}", error.render(&loaded));
            ExitCode::FAILURE
        }
    }
}

/// `pekit query deps TARGET...` (see [Commands](#commands)): what each
/// target reaches, run no step, as [`TargetDeps::shown`] shows it.
fn deps_command(args: &DepsArgs, cwd: &Path, terminal: &mut Terminal<'_>) -> ExitCode {
    let names: Vec<&str> = args.targets.iter().map(String::as_str).collect();
    let Some((loaded, queried)) = query(cwd, (&args.kind, &names), &args.shared, terminal) else {
        return ExitCode::FAILURE;
    };
    let mut failed = false;
    for deps in queried {
        match deps {
            Ok(deps) => {
                let _ = write!(terminal.out, "{}", deps.shown());
            }
            Err(error) => {
                failed = true;
                let _ = write!(terminal.err, "{}", error.render(&loaded));
            }
        }
    }
    if failed {
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
    }
}

/// `pekit query rdeps FILE [TARGET...]` (see [Commands](#commands)): each
/// target that reads `FILE`, by its qualified name, one to a line; one
/// that waits for a step's result is among them, since it might read it
/// after, and said to be.
fn rdeps_command(args: &RdepsArgs, cwd: &Path, terminal: &mut Terminal<'_>) -> ExitCode {
    let mut names: Vec<&str> = args.targets.iter().map(String::as_str).collect();
    if names.is_empty() {
        names.push("**");
    }
    let Some((loaded, queried)) = query(cwd, (&args.kind, &names), &args.shared, terminal) else {
        return ExitCode::FAILURE;
    };
    let file = absolute(cwd, &args.file);
    let Some(file) = file
        .strip_prefix(&loaded.root)
        .ok()
        .map(|relative| relative.to_string_lossy().replace('\\', "/"))
    else {
        let _ = writeln!(
            terminal.err,
            "error: `{}` is not in the workspace at `{}`",
            args.file.display(),
            loaded.root.display()
        );
        return ExitCode::FAILURE;
    };
    let mut failed = false;
    for deps in queried {
        match deps {
            Ok(deps) if deps.reads_file(&file) => {
                let _ = writeln!(terminal.out, "{}", deps.target);
            }
            Ok(deps) => {
                if let Some(at) = &deps.waits {
                    let _ = writeln!(terminal.out, "{}", deps.target);
                    let _ = writeln!(
                        terminal.err,
                        "note: `{}` waits at {at} for a step's result, so it might read `{file}` \
                         after: it is listed",
                        deps.target
                    );
                }
            }
            Err(error) => {
                failed = true;
                let _ = write!(terminal.err, "{}", error.render(&loaded));
            }
        }
    }
    if failed {
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
    }
}

/// The workspace `cwd` is in, loaded, and what each target of the def-type
/// `kind` that `names` select reaches ([`query_all`]); `None` if it cannot
/// be, said on `terminal`.
fn query(
    cwd: &Path,
    (kind, names): (&str, &[&str]),
    shared: &Shared,
    terminal: &mut Terminal<'_>,
) -> Option<(Loaded, Vec<Result<TargetDeps, BuildError>>)> {
    let mut loaded = load_for(cwd, shared, terminal)?;
    let targets = match select(&loaded.workspace, kind, names, &loaded.here) {
        Ok(targets) => targets,
        Err(error) => {
            let _ = writeln!(terminal.err, "{}", error.message());
            return None;
        }
    };
    let settings = settings_for(shared, &loaded, terminal)?;
    match query_all(&mut loaded, &targets, &settings) {
        Ok(queried) => Some((loaded, queried)),
        Err(error) => {
            let _ = write!(terminal.err, "{}", error.render(&loaded));
            None
        }
    }
}

/// `path`, from the directory `cwd` if it is relative, with every `.` and
/// `..` taken out by its text, so that a file that is not there yet is
/// named too.
fn absolute(cwd: &Path, path: &Path) -> PathBuf {
    let mut absolute = PathBuf::new();
    for part in cwd.join(path).components() {
        match part {
            std::path::Component::CurDir => {}
            std::path::Component::ParentDir => {
                absolute.pop();
            }
            other => absolute.push(other),
        }
    }
    absolute
}

/// `pekit lock [--update]` (see [Commands](#commands)): pins the libraries
/// the workspace imports, saying which it pinned anew and which it dropped.
fn lock_command(args: &LockArgs, cwd: &Path, terminal: &mut Terminal<'_>) -> ExitCode {
    let Some(root) = workspace_root(cwd, terminal) else {
        return ExitCode::FAILURE;
    };
    let cache = cache_dir();
    let store = match Store::open(&cache.join("store")) {
        Ok(store) => store,
        Err(error) => {
            let _ = writeln!(terminal.err, "error: cannot open the store: {error}");
            return ExitCode::FAILURE;
        }
    };
    match lock::lock(&root, (&cache, &store), args.update) {
        Ok(locked) => {
            for (path, commit) in &locked.pinned {
                let _ = writeln!(terminal.out, "pinned {path} at {commit}");
            }
            for path in &locked.dropped {
                let _ = writeln!(terminal.out, "dropped {path}, no longer imported");
            }
            ExitCode::SUCCESS
        }
        Err(error) => {
            let _ = write!(terminal.err, "{}", error.render());
            ExitCode::FAILURE
        }
    }
}

/// `pekit release PACKAGE --to STORE` (see [Releases](crate::release#releases)):
/// builds the package from `HEAD`, strictly, with the `release` profile,
/// each step checked to be reproducible; publishes it into `STORE`; and tags
/// `HEAD` `NAME@VERSION`, refusing a tag of that name on another commit
/// before anything is published.
fn release_command(args: &ReleaseArgs, cwd: &Path, terminal: &mut Terminal<'_>) -> ExitCode {
    let Some(root) = workspace_root(cwd, terminal) else {
        return ExitCode::FAILURE;
    };
    let fail = |terminal: &mut Terminal<'_>, message: &str| {
        let _ = writeln!(terminal.err, "error: {message}");
        ExitCode::FAILURE
    };
    let repository = match Repository::of(&root) {
        Ok(repository) => repository,
        Err(message) => return fail(terminal, &message),
    };
    match repository.unclean(&root) {
        Ok(unclean) if unclean.is_empty() => {}
        Ok(unclean) => {
            let _ = writeln!(
                terminal.err,
                "error: a release is built from `HEAD`, and the workspace differs from it: \
                 commit or remove each of"
            );
            for line in unclean {
                let _ = writeln!(terminal.err, "  {line}");
            }
            return ExitCode::FAILURE;
        }
        Err(message) => return fail(terminal, &message),
    }
    let tree = cache_dir()
        .join("scratch")
        .join(format!("release-{}", std::process::id()));
    if let Err(message) = repository.export(&tree) {
        let _ = remove_tree(&tree);
        return fail(terminal, &message);
    }
    let here = cwd
        .canonicalize()
        .ok()
        .and_then(|cwd| cwd.strip_prefix(&root).ok().map(Path::to_path_buf))
        .unwrap_or_default();
    let released = released(args, (&tree.join(here), &repository), terminal);
    let _ = remove_tree(&tree);
    released
}

/// [`release_command`]'s build, publishing and tag, in the workspace `HEAD`
/// was exported into, from its directory `at`.
fn released(
    args: &ReleaseArgs,
    (at, repository): (&Path, &Repository),
    terminal: &mut Terminal<'_>,
) -> ExitCode {
    let shared = |check_reproducible| Shared {
        jobs: args.jobs,
        trace: args.trace.clone(),
        profile: "release".to_owned(),
        platform: None,
        reuse: ReuseFlags {
            rebuild: false,
            check_reproducible,
        },
        allows: AllowFlags {
            no_dynamic: false,
            strict: true,
        },
    };
    let (checked, publishing) = (shared(true), shared(false));
    let Some(mut loaded) = load_for(at, &checked, terminal) else {
        return ExitCode::FAILURE;
    };
    let names = [args.package.as_str()];
    let package = match select(&loaded.workspace, "package", &names, &loaded.here) {
        Ok(mut packages) if packages.len() == 1 => packages.remove(0),
        Ok(_) => {
            let _ = writeln!(
                terminal.err,
                "error: `pekit release` releases one package, which `{}` does not name",
                args.package
            );
            return ExitCode::FAILURE;
        }
        Err(error) => {
            let _ = writeln!(terminal.err, "{}", error.message());
            return ExitCode::FAILURE;
        }
    };
    let Some(settings) = settings_for(&checked, &loaded, terminal) else {
        return ExitCode::FAILURE;
    };
    let packages = [package];
    let built = build_targets(
        (&mut loaded, &packages, Doing::Build),
        &settings,
        &checked,
        terminal,
    );
    let value = match built.map(|mut outcomes| outcomes.targets.remove(0)) {
        Ok(Ok(value)) => value,
        Ok(Err(error)) | Err(error) => {
            let _ = write!(terminal.err, "{}", error.render(&loaded));
            return ExitCode::FAILURE;
        }
    };
    let Some(tag) = release_tag(&value) else {
        let _ = writeln!(
            terminal.err,
            "error: `{}` gives no package's name and version",
            packages[0].qualified()
        );
        return ExitCode::FAILURE;
    };
    let tagged = match repository.tagged(&tag) {
        Ok(Some(commit)) if commit != repository.head => {
            let _ = writeln!(
                terminal.err,
                "error: the tag `{tag}` is on {commit}, not on `HEAD`, {}: a version is released \
                 once, so give this one a version of its own",
                repository.head
            );
            return ExitCode::FAILURE;
        }
        Ok(found) => found.is_some(),
        Err(message) => {
            let _ = writeln!(terminal.err, "error: {message}");
            return ExitCode::FAILURE;
        }
    };
    let publish = Settings {
        strict: false,
        ..settings.clone()
    };
    if !publish_release(
        (&mut loaded, &packages),
        (&tag, &args.to),
        (&publish, &publishing),
        terminal,
    ) {
        return ExitCode::FAILURE;
    }
    if !tagged && let Err(message) = repository.tag(&tag) {
        let _ = writeln!(terminal.err, "error: published, but not tagged: {message}");
        return ExitCode::FAILURE;
    }
    let _ = writeln!(terminal.out, "tagged {tag} at {}", repository.head);
    ExitCode::SUCCESS
}

/// The tag of the release of the package whose value is `value`, a
/// `Package`: `NAME@VERSION`; `None` for any other value.
fn release_tag(value: &Plain) -> Option<String> {
    let field = |name: &str| match value {
        Plain::Record(fields) => fields.iter().find_map(|(field, value)| match value {
            Plain::Str(text) if field == name => Some(text.clone()),
            _ => None,
        }),
        _ => None,
    };
    Some(format!("{}@{}", field("name")?, field("version")?))
}

/// Publishes the release `tag` of `packages`, the one package of `loaded`'s
/// workspace, into the store `to`, with `settings`, the strict build's but
/// allowing the target's own effects, since publishing is one, already
/// checked by that build, and its steps found in the store; `shared` for
/// its progress. Whether it was published, said on `terminal`.
fn publish_release(
    (loaded, packages): (&mut Loaded, &[Target]),
    (tag, to): (&str, &str),
    (settings, shared): (&Settings, &Shared),
    terminal: &mut Terminal<'_>,
) -> bool {
    let packing = Packing::Publish { to: to.to_owned() };
    let published = build_targets(
        (loaded, packages, Doing::Pack(&packing)),
        settings,
        shared,
        terminal,
    );
    match published.map(|mut outcomes| outcomes.targets.remove(0)) {
        Ok(Ok(Plain::List(packed))) if !packed.is_empty() => {
            for each in &packed {
                let (by, _) = packed_by(each);
                let _ = writeln!(terminal.out, "published {tag} with {by} into {to}");
            }
            true
        }
        Ok(Ok(_)) => {
            let _ = writeln!(
                terminal.err,
                "error: `{tag}` was packed by no packager: list one in `pekitconf.packagers`"
            );
            false
        }
        Ok(Err(error)) | Err(error) => {
            let _ = write!(terminal.err, "{}", error.render(loaded));
            false
        }
    }
}

/// The root of the workspace `cwd` is in, or `None`, said on `terminal`.
fn workspace_root(cwd: &Path, terminal: &mut Terminal<'_>) -> Option<PathBuf> {
    let found = cwd
        .canonicalize()
        .ok()
        .and_then(|cwd| gcsl_disk::find_root(&cwd).map(Path::to_path_buf));
    if found.is_none() {
        let _ = write!(
            terminal.err,
            "{}",
            LoadError::NoWorkspace {
                dir: cwd.to_path_buf()
            }
            .render()
        );
    }
    found
}

/// The workspace `cwd` is in, loaded as a command that builds as `shared`
/// says loads it, or `None`, said on `terminal` (see [The
/// lock](crate::lock#the-lock)): each library its lock pins that is not
/// there put back first; then, under `--strict`, failing unless its
/// libraries are those the lock pins, and otherwise warning of each that
/// differs.
fn load_for(cwd: &Path, shared: &Shared, terminal: &mut Terminal<'_>) -> Option<Loaded> {
    let root = workspace_root(cwd, terminal)?;
    let cache = cache_dir();
    let differ = Store::open(&cache.join("store"))
        .map_err(|error| lock::LockError::Io(format!("cannot open the store: {error}")))
        .and_then(|store| lock::restore(&root, (&cache, &store)));
    let differ = match differ {
        Ok(differ) => differ,
        Err(error) => {
            let _ = write!(terminal.err, "{}", error.render());
            return None;
        }
    };
    let loaded = match load(cwd) {
        Ok(loaded) => loaded,
        Err(error) => {
            let _ = write!(terminal.err, "{}", error.render());
            return None;
        }
    };
    if shared.allows.strict {
        let prefix = format!("{}/", gcsl::workspace::LIBRARIES);
        let mut libraries: Vec<String> = loaded
            .names
            .iter()
            .filter_map(|name| name.strip_prefix(prefix.as_str()))
            .filter_map(|rest| rest.rsplit_once('/').map(|(library, _)| library.to_owned()))
            .collect();
        libraries.sort();
        libraries.dedup();
        match lock::check_strict(&root, &libraries, differ) {
            Ok(problems) if problems.is_empty() => {}
            Ok(problems) => {
                let _ = write!(
                    terminal.err,
                    "{}",
                    lock::LockError::Strict(problems).render()
                );
                return None;
            }
            Err(error) => {
                let _ = write!(terminal.err, "{}", error.render());
                return None;
            }
        }
    } else {
        for difference in differ {
            let _ = writeln!(
                terminal.err,
                "warning: {difference}: remove it to have the pinned one put back, or commit \
                 the change to its repository and run `pekit lock --update`"
            );
        }
    }
    Some(loaded)
}

/// `--check-reproducible` refused for the command `pekit COMMAND`, whose
/// effects change the world and would be done twice.
fn changes_the_world(command: &str, terminal: &mut Terminal<'_>) -> ExitCode {
    let _ = writeln!(
        terminal.err,
        "error: `pekit {command}` changes the world, so it cannot be checked by doing it twice: \
         check what it builds with `pekit build --check-reproducible`"
    );
    ExitCode::FAILURE
}

/// The script, or failing one the build, that `name` names in `loaded`'s
/// workspace from its directory, with whether it is a script.
///
/// # Errors
///
/// That `name` is a glob, names neither, or names both, each as shown.
fn runnable(loaded: &Loaded, name: &str) -> Result<(Target, bool), String> {
    if name == "." || name.contains('*') {
        return Err(format!(
            "error: `pekit run` runs one script or build, which `{name}` does not name"
        ));
    }
    let found = |def_type: &str| select(&loaded.workspace, def_type, &[name], &loaded.here);
    let (script, build) = (found("script"), found("build"));
    let one = |found: &Result<Vec<Target>, TargetError>| match found {
        Ok(targets) => targets.first().cloned(),
        Err(_) => None,
    };
    match (one(&script), one(&build)) {
        (Some(script), Some(build)) => Err(format!(
            "error: `{name}` names the script `{}` and the build `{}`: name the one to run by \
             its directory",
            script.qualified(),
            build.qualified()
        )),
        (Some(script), None) => Ok((script, true)),
        (None, Some(build)) => Ok((build, false)),
        (None, None) => {
            let unexpected = [script, build]
                .into_iter()
                .filter_map(Result::err)
                .find(|error| !matches!(error, TargetError::NoDefinition { .. }));
            Err(unexpected.map_or_else(
                || format!("error: no `script` or `build` is named `{name}`"),
                |error| error.message(),
            ))
        }
    }
}

/// `pekit package [PACKAGE...]` and `pekit publish PACKAGE... --to STORE`
/// (see [Commands](#commands)): packs each package with each packager, and
/// for `publish` publishes each, showing what each packer made.
fn package_command(
    (given, packing): (&[String], &Packing),
    shared: &Shared,
    cwd: &Path,
    terminal: &mut Terminal<'_>,
) -> ExitCode {
    if shared.reuse.check_reproducible && matches!(packing, Packing::Publish { .. }) {
        return changes_the_world("publish", terminal);
    }
    let Some(mut loaded) = load_for(cwd, shared, terminal) else {
        return ExitCode::FAILURE;
    };
    let mut names: Vec<&str> = given.iter().map(String::as_str).collect();
    if names.is_empty() {
        names.push("**");
    }
    let packages = match select(&loaded.workspace, "package", &names, &loaded.here) {
        Ok(packages) if packages.is_empty() => {
            let _ = writeln!(
                terminal.err,
                "error: no `package` is selected by {}",
                quoted(&names)
            );
            return ExitCode::FAILURE;
        }
        Ok(packages) => packages,
        Err(error) => {
            let _ = writeln!(terminal.err, "{}", error.message());
            return ExitCode::FAILURE;
        }
    };
    let Some(settings) = settings_for(shared, &loaded, terminal) else {
        return ExitCode::FAILURE;
    };
    let built = build_targets(
        (&mut loaded, &packages, Doing::Pack(packing)),
        &settings,
        shared,
        terminal,
    );
    let outcomes = match built {
        Ok(outcomes) => outcomes,
        Err(error) => {
            let _ = write!(terminal.err, "{}", error.render(&loaded));
            return ExitCode::FAILURE;
        }
    };
    let verb = match packing {
        Packing::Pack => "packed",
        Packing::Publish { .. } => "published",
    };
    let mut failed = false;
    for (package, outcome) in packages.iter().zip(&outcomes.targets) {
        let shown = package.qualified();
        let packed = match outcome {
            Ok(Plain::List(packed)) if packed.is_empty() => {
                failed = true;
                let _ = writeln!(
                    terminal.err,
                    "error: `{shown}` was packed by no packager: list one in \
                     `pekitconf.packagers`"
                );
                continue;
            }
            Ok(Plain::List(packed)) => packed,
            Ok(other) => {
                failed = true;
                let _ = writeln!(terminal.err, "error: `{shown}` packed to {other:?}");
                continue;
            }
            Err(error) => {
                failed = true;
                let _ = write!(terminal.err, "{}", error.render(&loaded));
                continue;
            }
        };
        for each in packed {
            let (by, dir) = packed_by(each);
            let _ = writeln!(terminal.out, "{verb} {shown} with {by}");
            if let Some(dir) = dir {
                let shown = shown_path(dir, &loaded.root, cwd);
                let _ = writeln!(terminal.out, "  {}", shown.display());
            }
        }
    }
    let _ = writeln!(terminal.out, "{}", steps_counted(&outcomes));
    warn_of_eviction(&outcomes, terminal);
    if failed {
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
    }
}

/// The packager's name and the directory of `packed`, a `packaging.Packed`.
fn packed_by(packed: &Plain) -> (&str, Option<&str>) {
    let Plain::Record(fields) = packed else {
        return ("a packager", None);
    };
    let field = |name: &str| {
        fields
            .iter()
            .find(|(field, _)| field == name)
            .map(|(_, value)| value)
    };
    let packager = match field("packager") {
        Some(Plain::Str(name)) => name.as_str(),
        _ => "a packager",
    };
    let dir = match field("packed") {
        Some(Plain::Dir { path, .. }) => Some(path.as_str()),
        _ => None,
    };
    (packager, dir)
}

/// `pekit clean [--cache]` (see [Commands](#commands)): removes the
/// workspace's `pekit-out/`, and with `--cache` the store and the staging
/// directory under the cache directory, never the cache directory itself,
/// which can be anywhere, nor the state directory.
fn clean_command(args: &CleanArgs, cwd: &Path, terminal: &mut Terminal<'_>) -> ExitCode {
    let Some(root) = cwd
        .canonicalize()
        .ok()
        .and_then(|dir| gcsl_disk::find_root(&dir).map(Path::to_path_buf))
    else {
        let _ = writeln!(
            terminal.err,
            "error: no workspace here: neither {} nor any directory above it holds a \
             workspace.gcsl",
            cwd.display()
        );
        return ExitCode::FAILURE;
    };
    let mut doomed = vec![root.join("pekit-out")];
    if args.cache {
        let cache = cache_dir();
        doomed.extend([cache.join("store"), cache.join("scratch")]);
    }
    let mut failed = false;
    for path in doomed {
        if std::fs::symlink_metadata(&path).is_err() {
            continue;
        }
        match remove_tree(&path) {
            Ok(()) => {
                let _ = writeln!(terminal.out, "removed {}", path.display());
            }
            Err(error) => {
                failed = true;
                let _ = writeln!(
                    terminal.err,
                    "error: cannot remove {}: {error}",
                    path.display()
                );
            }
        }
    }
    if failed {
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
    }
}

/// Whether the test whose result is `result`, a `TestResult`, passed, and
/// its failure's message; `None` for a value of another shape.
fn test_result(result: &Plain) -> Option<(bool, &str)> {
    let Plain::Record(fields) = result else {
        return None;
    };
    let field = |name: &str| {
        fields
            .iter()
            .find(|(field, _)| field == name)
            .map(|(_, value)| value)
    };
    match (field("passed"), field("message")) {
        (Some(Plain::Bool(passed)), Some(Plain::Str(message))) => Some((*passed, message)),
        _ => None,
    }
}

/// `names`, each in backticks, joined as a list is.
fn quoted(names: &[&str]) -> String {
    let quoted: Vec<String> = names.iter().map(|name| format!("`{name}`")).collect();
    quoted.join(", ")
}

/// Warns on `terminal` that the store could not be kept within its limit,
/// if the eviction as `outcomes`' build ended failed: which fails no build.
fn warn_of_eviction(outcomes: &Outcomes, terminal: &mut Terminal<'_>) {
    if let Some(Err(message)) = &outcomes.evicted {
        let _ = writeln!(
            terminal.err,
            "warning: the store could not be kept within `PEKIT_CACHE_LIMIT`: {message}"
        );
    }
}

/// How many steps ran, how many of their programs were found, and how many
/// steps were found, as a build shows them.
fn steps_counted(outcomes: &Outcomes) -> String {
    let (ran, cached) = (outcomes.ran, outcomes.cached);
    let steps = if ran == 1 { "step" } else { "steps" };
    let programs = match outcomes.programs_found {
        0 => String::new(),
        1 => " (1 program found)".to_owned(),
        found => format!(" ({found} programs found)"),
    };
    format!("{ran} {steps} run{programs}, {cached} found in the store")
}

/// The settings a command builds `loaded`'s workspace with, as `shared`
/// says: the host's snapshot, warned of on `terminal` if its packages
/// cannot be identified. `None`, the error shown, for a `--platform` that
/// names none.
fn settings_for(shared: &Shared, loaded: &Loaded, terminal: &mut Terminal<'_>) -> Option<Settings> {
    let cache = cache_dir();
    let cache_limit = match limit_of(std::env::var("PEKIT_CACHE_LIMIT").ok().as_deref()) {
        Ok(limit) => limit,
        Err(message) => {
            let _ = writeln!(terminal.err, "error: {message}");
            return None;
        }
    };
    let host = HostSnapshot::of_host();
    let execution = ExecutionPlatform::of_host();
    let strict = shared.allows.strict;
    if host.fingerprint.is_none() && !strict {
        let _ = writeln!(
            terminal.err,
            "warning: the host's packages cannot be identified, so no step is reused \
             from the store"
        );
    }
    let building_for = match &shared.platform {
        None => None,
        Some(named) => {
            let (arch, os) = named.split_once('-').unwrap_or((named, &execution.os));
            if arch.is_empty() || os.is_empty() {
                let _ = writeln!(
                    terminal.err,
                    "error: `--platform {named}` names no platform: write `ARCH` or `ARCH-OS`, \
                     such as `aarch64-linux`"
                );
                return None;
            }
            Some(ExecutionPlatform {
                arch: arch.to_owned(),
                os: os.to_owned(),
            })
        }
    };
    Some(Settings {
        store: cache.join("store"),
        outputs: loaded.root.join("pekit-out"),
        scratch: cache.join("scratch"),
        jobs: shared.jobs.unwrap_or_else(|| {
            std::thread::available_parallelism().map_or(1, std::num::NonZero::get)
        }),
        // A strict build's steps see nothing of the host, so their results
        // are those of any machine of the platform, and keyed apart from
        // a build's that borrowed its tools.
        platform: if strict {
            format!("{} no-host-tools", execution.describe())
        } else {
            host.platform(&execution.describe())
        },
        execution,
        reuse: if (strict || host.fingerprint.is_some()) && !shared.reuse.rebuild {
            Reuse::Store
        } else {
            Reuse::Rerun
        },
        host_tools: if strict {
            HostTools::default()
        } else {
            host.tools()
        },
        profile: shared.profile.clone(),
        state: state_dir(),
        building_for,
        no_dynamic: shared.allows.no_dynamic,
        strict,
        cache_limit,
    })
}

/// Builds `targets` of `loaded`'s workspace with `settings`, or does with
/// them what `doing` says, showing its progress on `terminal` as it goes
/// and writing its trace where `shared` says.
///
/// # Errors
///
/// What [`build_all_observed`], [`package_all_observed`] or
/// [`run_script_observed`] fails with.
fn build_targets(
    (loaded, targets, doing): (&mut Loaded, &[Target], Doing<'_>),
    settings: &Settings,
    shared: &Shared,
    terminal: &mut Terminal<'_>,
) -> Result<Outcomes, BuildError> {
    let mut trace = Trace::new();
    let mut progress = Progress::default();
    let live = terminal.live;
    let outcomes = {
        let err = &mut *terminal.err;
        let mut events = |event: &Event| {
            trace.add(event);
            if live {
                progress.show(event, err);
            }
        };
        let first = build_once((loaded, targets, doing), settings, &mut events);
        match first {
            Ok(first) if shared.reuse.check_reproducible => {
                let again = Settings {
                    reuse: Reuse::Compare,
                    ..settings.clone()
                };
                build_once((loaded, targets, doing), &again, &mut events).and_then(|again| {
                    if again.mismatches.is_empty() {
                        Ok(first)
                    } else {
                        Err(BuildError::NotReproducible(again.mismatches))
                    }
                })
            }
            other => other,
        }
    };
    if live {
        progress.clear(terminal.err);
    }
    if let Some(path) = &shared.trace {
        let written = serde_json::to_vec(&trace.to_json())
            .map_err(|error| error.to_string())
            .and_then(|bytes| std::fs::write(path, bytes).map_err(|error| error.to_string()));
        if let Err(error) = written {
            let _ = writeln!(
                terminal.err,
                "warning: cannot write the trace to `{}`: {error}",
                path.display()
            );
        }
    }
    let outcomes = outcomes?;
    for name in &outcomes.flaky {
        let _ = writeln!(
            terminal.err,
            "warning: step `{name}` passed only when it was run again: it is flaky"
        );
    }
    Ok(outcomes)
}

/// A build's progress, as a terminal shows it: one line, rewritten at each
/// event, of the steps running and how many have finished. None is shown
/// while one of a target's own effects runs, which may be a script's
/// program that has the terminal.
#[derive(Debug, Default)]
struct Progress {
    /// The steps running, in the order they started.
    running: Vec<String>,
    /// How many steps have finished.
    finished: usize,
    /// How many were found in the store.
    found: usize,
    /// Whether a line is shown.
    shown: bool,
    /// The target's own effects queued and not yet run, by node.
    own: Vec<u32>,
}

impl Progress {
    /// Takes in `event` and shows the line again on `err`.
    fn show(&mut self, event: &Event, err: &mut dyn Write) {
        match &event.kind {
            EventKind::EffectQueued {
                node, step: None, ..
            } => {
                self.own.push(*node);
                self.clear(err);
                return;
            }
            EventKind::EffectRan {
                node, step: None, ..
            } => self.own.retain(|own| own != node),
            EventKind::Started { step, .. } => self.running.push(step.clone()),
            EventKind::Finished { step } | EventKind::Failed { step, .. } => {
                self.running.retain(|running| running != step);
                if matches!(event.kind, EventKind::Finished { .. }) {
                    self.finished += 1;
                }
            }
            EventKind::Cached { .. } => self.found += 1,
            _ => return,
        }
        if !self.own.is_empty() {
            return;
        }
        let mut line = format!("{} run, {} found", self.finished, self.found);
        if !self.running.is_empty() {
            let _ = write!(line, "; running {}", self.running.join(", "));
        }
        let line: String = line.chars().take(PROGRESS_WIDTH).collect();
        let _ = write!(err, "\r\x1b[K{line}");
        let _ = err.flush();
        self.shown = true;
    }

    /// Clears the line from `err`, if one is shown.
    fn clear(&mut self, err: &mut dyn Write) {
        if self.shown {
            let _ = write!(err, "\r\x1b[K");
            let _ = err.flush();
            self.shown = false;
        }
    }
}

/// How many characters of a progress line are shown, so that it fits on
/// one line of a narrow terminal.
const PROGRESS_WIDTH: usize = 79;

/// The cache directory (see [Commands](#commands)).
fn cache_dir() -> PathBuf {
    let var = |name: &str| std::env::var_os(name).filter(|value| !value.is_empty());
    if let Some(dir) = var("PEKIT_CACHE_DIR") {
        return PathBuf::from(dir);
    }
    if let Some(dir) = var("XDG_CACHE_HOME") {
        return PathBuf::from(dir).join("pekit");
    }
    let home = var("HOME").map_or_else(|| PathBuf::from("/"), PathBuf::from);
    home.join(".cache/pekit")
}

/// The state directory (see [Commands](#commands)).
fn state_dir() -> PathBuf {
    let var = |name: &str| std::env::var_os(name).filter(|value| !value.is_empty());
    if let Some(dir) = var("PEKIT_STATE_DIR") {
        return PathBuf::from(dir);
    }
    if let Some(dir) = var("XDG_STATE_HOME") {
        return PathBuf::from(dir).join("pekit");
    }
    let home = var("HOME").map_or_else(|| PathBuf::from("/"), PathBuf::from);
    home.join(".local/state/pekit")
}

/// The logical path `path` as someone at `cwd` finds it: on the host, under
/// the workspace at `root`, relative to `cwd`, `../pekit-out/app/hello`, if
/// the two share more than the file system's root.
fn shown_path(path: &str, root: &Path, cwd: &Path) -> PathBuf {
    let host = if let Some(rest) = path.strip_prefix("/w/") {
        root.join(rest)
    } else if let Some(rest) = path.strip_prefix("/pekit-out/") {
        root.join("pekit-out").join(rest)
    } else {
        return PathBuf::from(path);
    };
    let cwd = cwd.canonicalize().unwrap_or_else(|_| cwd.to_path_buf());
    let to: Vec<Component<'_>> = host.components().collect();
    let from: Vec<Component<'_>> = cwd.components().collect();
    let shared = to.iter().zip(&from).take_while(|(a, b)| a == b).count();
    if shared <= 1 {
        return host;
    }
    let mut shown = PathBuf::new();
    for _ in shared..from.len() {
        shown.push("..");
    }
    shown.extend(&to[shared..]);
    shown
}
