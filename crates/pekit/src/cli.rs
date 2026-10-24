//! The `pekit` command.
//!
//! # Commands
//!
//! `pekit build TARGET` builds the definition `TARGET` of the def-type
//! `build`, named as [Targets](crate::target#targets) says, from the
//! workspace the current directory is in. It shows the files the target
//! gave, and how many steps ran and how many were found in the store.
//! `--jobs N` runs at most `N` programs at once, by default as many as the
//! machine has processors.
//!
//! The store and the directories steps are staged in are under the cache
//! directory: `$PEKIT_CACHE_DIR` if it is set, else `pekit` under
//! `$XDG_CACHE_HOME`, else `~/.cache/pekit`. Outputs are copied to
//! `pekit-out/` at the workspace's root.
//!
//! It exits with 0 once the target is built, 1 if it could not be, and 2
//! for a command it does not understand.

use std::ffi::OsString;
use std::io::Write;
use std::path::{Component, Path, PathBuf};
use std::process::ExitCode;

use clap::{Parser, Subcommand};

use crate::engine::{Settings, build};
use crate::host::{HostSnapshot, machine};
use crate::load::load;
use crate::target::resolve;

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
    Build {
        /// The target: `hello`, or qualified by its directory, `app/hello`.
        target: String,
        /// How many programs may run at once.
        #[arg(long, short)]
        jobs: Option<usize>,
    },
}

/// Runs the command `args`, `pekit` first, from the directory `cwd`,
/// writing what it shows to `out` and its warnings and errors to `err` (see
/// [Commands](#commands)). Like all of GCSL's pipeline, it must run on a
/// thread with at least [`gcsl::STACK_SIZE`] bytes of stack.
pub fn run(
    args: impl IntoIterator<Item = OsString>,
    cwd: &Path,
    out: &mut dyn Write,
    err: &mut dyn Write,
) -> ExitCode {
    let cli = match Cli::try_parse_from(args) {
        Ok(cli) => cli,
        Err(error) => {
            let shown = error.render().to_string();
            if error.use_stderr() {
                let _ = write!(err, "{shown}");
                return ExitCode::from(2);
            }
            let _ = write!(out, "{shown}");
            return ExitCode::SUCCESS;
        }
    };
    match cli.command {
        Command::Build { target, jobs } => build_command(&target, jobs, cwd, out, err),
    }
}

/// `pekit build TARGET` (see [Commands](#commands)).
fn build_command(
    name: &str,
    jobs: Option<usize>,
    cwd: &Path,
    out: &mut dyn Write,
    err: &mut dyn Write,
) -> ExitCode {
    let mut loaded = match load(cwd) {
        Ok(loaded) => loaded,
        Err(error) => {
            let _ = write!(err, "{}", error.render());
            return ExitCode::FAILURE;
        }
    };
    let target = match resolve(&loaded.workspace, ("build", name), &loaded.here) {
        Ok(target) => target,
        Err(error) => {
            let _ = writeln!(err, "{}", error.message());
            return ExitCode::FAILURE;
        }
    };
    let cache = cache_dir();
    let host = HostSnapshot::of_host();
    if host.fingerprint.is_none() {
        let _ = writeln!(
            err,
            "warning: the host's packages cannot be identified, so no step is reused \
             from the store"
        );
    }
    let settings = Settings {
        store: cache.join("store"),
        outputs: loaded.root.join("pekit-out"),
        scratch: cache.join("scratch"),
        jobs: jobs.unwrap_or_else(|| {
            std::thread::available_parallelism().map_or(1, std::num::NonZero::get)
        }),
        platform: host.platform(&machine()),
        reuse: host.fingerprint.is_some(),
        host_tools: host.tools(),
    };
    match build(&mut loaded, &target, &settings) {
        Ok(built) => {
            for name in &built.flaky {
                let _ = writeln!(
                    err,
                    "warning: step `{name}` passed only when it was run again: it is flaky"
                );
            }
            let _ = writeln!(
                out,
                "built {}: {} {} run, {} found in the store",
                target.qualified(),
                built.ran,
                if built.ran == 1 { "step" } else { "steps" },
                built.cached
            );
            for (path, _) in built.value.files() {
                let _ = writeln!(out, "  {}", shown_path(path, &loaded.root, cwd).display());
            }
            if built.ran > 0 {
                let _ = writeln!(
                    err,
                    "note: steps ran with the host's own tools, so this build is not hermetic"
                );
            }
            ExitCode::SUCCESS
        }
        Err(error) => {
            let _ = write!(err, "{}", error.render(&loaded));
            ExitCode::FAILURE
        }
    }
}

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
