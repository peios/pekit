//! pekit builds Peios packages from GCSL recipes.
//!
//! A recipe workspace is a GCSL workspace whose root holds pekit's include
//! file. `pekit package` reads a package's recipe, fetches its build's
//! source, runs the build, chooses the package's files from the install
//! tree and packs them with ppk.

mod recipes;
mod source;
mod stage;

use std::fs::{self, OpenOptions};
use std::io;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::thread;

use anyhow::{Context, Result, anyhow, bail};
use clap::{Parser, Subcommand};
use ppk::PackageId;

use crate::recipes::{INCLUDE, INCLUDE_FILE, Recipes, WORKSPACE_FILE};

/// Build Peios packages from GCSL recipes
#[derive(Parser)]
#[command(name = "pekit", version, arg_required_else_help = true)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Write pekit's declarations into a recipe workspace
    Init {
        /// Workspace directory (default: the current directory)
        #[arg(value_name = "dir")]
        dir: Option<PathBuf>,
    },
    /// Build a package from its recipe and pack it into the artifact directory
    Package {
        /// Package to build, by name
        #[arg(value_name = "name")]
        name: String,
        /// Recipe workspace (default: the one the current directory is in)
        #[arg(long, value_name = "dir")]
        workspace: Option<PathBuf>,
        /// Store packages in <dir> (default: /system/artifacts)
        #[arg(
            long,
            value_name = "dir",
            default_value = ppk::DEFAULT_ARTIFACT_DIR,
            hide_default_value = true,
        )]
        artifact_dir: PathBuf,
    },
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    // GCSL evaluates recursively and needs a deeper stack than the main
    // thread's.
    let ran = thread::Builder::new()
        .stack_size(gcsl::STACK_SIZE)
        .spawn(move || run(cli))
        .map_err(anyhow::Error::from)
        .and_then(|thread| {
            thread
                .join()
                .unwrap_or_else(|_| Err(anyhow!("internal error: pekit panicked")))
        });
    match ran {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("pekit: {error:#}");
            ExitCode::FAILURE
        }
    }
}

fn run(cli: Cli) -> Result<()> {
    match cli.command {
        Command::Init { dir } => init(&dir.unwrap_or_else(|| PathBuf::from("."))),
        Command::Package {
            name,
            workspace,
            artifact_dir,
        } => {
            let identity = package(&name, workspace, &artifact_dir)?;
            println!("{identity}");
            Ok(())
        }
    }
}

/// Writes pekit's include file into the workspace at `dir`, replacing any
/// there, and an empty `workspace.gcsl` if there is none.
fn init(dir: &Path) -> Result<()> {
    let include = dir.join(INCLUDE_FILE);
    fs::write(&include, INCLUDE).with_context(|| include.display().to_string())?;
    let marker = dir.join(WORKSPACE_FILE);
    match OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&marker)
    {
        Ok(_) => Ok(()),
        Err(error) if error.kind() == io::ErrorKind::AlreadyExists => Ok(()),
        Err(error) => Err(error).with_context(|| marker.display().to_string()),
    }
}

/// Builds package `name` from the recipes of `workspace`, or of the
/// workspace the current directory is in, and packs it into `artifact_dir`,
/// returning its identity.
fn package(name: &str, workspace: Option<PathBuf>, artifact_dir: &Path) -> Result<String> {
    let root = match workspace {
        Some(root) => root,
        None => {
            let cwd = std::env::current_dir().context("cannot read the current directory")?;
            gcsl_disk::find_root(&cwd)
                .map(Path::to_path_buf)
                .ok_or_else(|| anyhow!("not in a recipe workspace: no {WORKSPACE_FILE} above"))?
        }
    };
    let root = fs::canonicalize(&root).with_context(|| root.display().to_string())?;
    let mut recipes = Recipes::open(&root)?;
    let package = recipes.package(name)?;
    // Checked before anything is built.
    let id = PackageId::new(name, &package.version, ppk::HOST_ARCH)?;
    // A build is reproducible, so building a packed version again would
    // make what is packed already.
    if let Some(identity) = ppk::packed(&id, artifact_dir)? {
        eprintln!("pekit: {id} is already packed");
        return Ok(identity.to_string());
    }
    let build = recipes.build(&package.build)?;

    let cache = cache_dir()?;
    let archive = source::fetch(&build.source, &cache)?;
    let work_dir = cache.join("work");
    fs::create_dir_all(&work_dir).with_context(|| work_dir.display().to_string())?;
    let work = tempfile::Builder::new()
        .prefix("build-")
        .tempdir_in(&work_dir)
        .with_context(|| work_dir.display().to_string())?;
    let unpacked = work.path().join("source");
    let source_path = source::unpack(&archive, &unpacked)?;
    let out_path = work.path().join("out");
    fs::create_dir(&out_path).with_context(|| out_path.display().to_string())?;

    // Tools that stamp a time into what they make use this one instead of
    // the clock, so that building the same source gives the same package.
    let epoch = source::newest_mtime(&unpacked)?;
    // SAFETY: nothing reads the environment concurrently. The main thread
    // is waiting for this one, and the threads GCSL starts to copy a
    // command's output end before the command's call returns.
    unsafe { std::env::set_var("SOURCE_DATE_EPOCH", epoch.to_string()) };

    eprintln!("pekit: building {}", package.build);
    let tree = recipes.run_build(&package.build, &source_path, &out_path, ppk::HOST_ARCH)?;
    let staged = work.path().join("stage");
    fs::create_dir(&staged).with_context(|| staged.display().to_string())?;
    stage::stage(&package.files, &tree, &staged)?;
    let packed = ppk::pack(&id, &staged, artifact_dir)?;
    Ok(packed.identity)
}

/// pekit's cache: `$XDG_CACHE_HOME/peios/pekit`, or `~/.cache/peios/pekit`.
fn cache_dir() -> Result<PathBuf> {
    let base = match std::env::var_os("XDG_CACHE_HOME").filter(|dir| !dir.is_empty()) {
        Some(dir) => PathBuf::from(dir),
        None => match std::env::var_os("HOME").filter(|dir| !dir.is_empty()) {
            Some(home) => PathBuf::from(home).join(".cache"),
            None => bail!("no cache directory: neither XDG_CACHE_HOME nor HOME is set"),
        },
    };
    Ok(base.join("peios").join("pekit"))
}
