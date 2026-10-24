//! The `pekit` command.
//!
//! # Commands
//!
//! `pekit build TARGET` builds the definition `TARGET` of the def-type
//! `build`, named as [Targets](crate::target#targets) says, from the
//! workspace the current directory is in. It shows the files the target
//! gave, and how many steps ran and how many were found in the store.
//! `--jobs N` runs at most `N` programs at once, by default as many as the
//! machine has processors, and `--trace FILE` writes a Chrome trace of the
//! build ([The trace](crate::trace#the-trace)). On a terminal, a line shows
//! the steps running as it goes.
//!
//! The store and the directories steps are staged in are under the cache
//! directory: `$PEKIT_CACHE_DIR` if it is set, else `pekit` under
//! `$XDG_CACHE_HOME`, else `~/.cache/pekit`. Outputs are copied to
//! `pekit-out/` at the workspace's root.
//!
//! It exits with 0 once the target is built, 1 if it could not be, and 2
//! for a command it does not understand.

use std::ffi::OsString;
use std::fmt::Write as _;
use std::io::Write;
use std::path::{Component, Path, PathBuf};
use std::process::ExitCode;

use clap::{Parser, Subcommand};

use crate::engine::{Settings, build_observed};
use crate::events::{Event, EventKind};
use crate::host::{HostSnapshot, machine};
use crate::load::load;
use crate::target::resolve;
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
    /// Build a target.
    Build(BuildArgs),
}

/// `pekit build`'s arguments (see [Commands](#commands)).
#[derive(Debug, clap::Args)]
struct BuildArgs {
    /// The target: `hello`, or qualified by its directory, `app/hello`.
    target: String,
    /// How many programs may run at once.
    #[arg(long, short)]
    jobs: Option<usize>,
    /// Write a Chrome trace of the build to this file.
    #[arg(long, value_name = "FILE")]
    trace: Option<PathBuf>,
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
    }
}

/// `pekit build TARGET` (see [Commands](#commands)).
fn build_command(args: &BuildArgs, cwd: &Path, terminal: &mut Terminal<'_>) -> ExitCode {
    let mut loaded = match load(cwd) {
        Ok(loaded) => loaded,
        Err(error) => {
            let _ = write!(terminal.err, "{}", error.render());
            return ExitCode::FAILURE;
        }
    };
    let target = match resolve(&loaded.workspace, ("build", &args.target), &loaded.here) {
        Ok(target) => target,
        Err(error) => {
            let _ = writeln!(terminal.err, "{}", error.message());
            return ExitCode::FAILURE;
        }
    };
    let cache = cache_dir();
    let host = HostSnapshot::of_host();
    if host.fingerprint.is_none() {
        let _ = writeln!(
            terminal.err,
            "warning: the host's packages cannot be identified, so no step is reused \
             from the store"
        );
    }
    let settings = Settings {
        store: cache.join("store"),
        outputs: loaded.root.join("pekit-out"),
        scratch: cache.join("scratch"),
        jobs: args.jobs.unwrap_or_else(|| {
            std::thread::available_parallelism().map_or(1, std::num::NonZero::get)
        }),
        platform: host.platform(&machine()),
        reuse: host.fingerprint.is_some(),
        host_tools: host.tools(),
    };
    let mut trace = Trace::new();
    let mut progress = Progress::default();
    let live = terminal.live;
    let built = {
        let err = &mut *terminal.err;
        let mut events = |event: &Event| {
            trace.add(event);
            if live {
                progress.show(event, err);
            }
        };
        build_observed(&mut loaded, &target, &settings, &mut events)
    };
    if live {
        progress.clear(terminal.err);
    }
    if let Some(path) = &args.trace {
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
    match built {
        Ok(built) => {
            for name in &built.flaky {
                let _ = writeln!(
                    terminal.err,
                    "warning: step `{name}` passed only when it was run again: it is flaky"
                );
            }
            let _ = writeln!(
                terminal.out,
                "built {}: {} {} run, {} found in the store",
                target.qualified(),
                built.ran,
                if built.ran == 1 { "step" } else { "steps" },
                built.cached
            );
            for (path, _) in built.value.files() {
                let shown = shown_path(path, &loaded.root, cwd);
                let _ = writeln!(terminal.out, "  {}", shown.display());
            }
            if built.ran > 0 {
                let _ = writeln!(
                    terminal.err,
                    "note: steps ran with the host's own tools, so this build is not hermetic"
                );
            }
            ExitCode::SUCCESS
        }
        Err(error) => {
            let _ = write!(terminal.err, "{}", error.render(&loaded));
            ExitCode::FAILURE
        }
    }
}

/// A build's progress, as a terminal shows it: one line, rewritten at each
/// event, of the steps running and how many have finished.
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
}

impl Progress {
    /// Takes in `event` and shows the line again on `err`.
    fn show(&mut self, event: &Event, err: &mut dyn Write) {
        match &event.kind {
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
