//! pekit builds Peios packages from GCSL recipes.
//!
//! A recipe workspace is a GCSL workspace whose root holds pekit's include
//! file. `pekit package` reads a package's recipe, fetches its build's
//! source, runs the build, chooses the package's files from the install
//! tree and packs them with ppk. `pekit release` does the same from a
//! committed workspace and tags the commit that made each package.

mod git;
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

use crate::recipes::{INCLUDE, INCLUDE_FILE, PackageRecipe, Recipes, WORKSPACE_FILE};

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
        #[command(flatten)]
        dirs: Dirs,
    },
    /// Build and pack packages from the committed recipes, then tag the
    /// commit that made each one
    Release {
        /// Packages to release, by name
        #[arg(value_name = "name", required_unless_present = "recipe")]
        names: Vec<String>,
        /// Release every package the recipe in <dir>, relative to the
        /// workspace's root, defines
        #[arg(long, value_name = "dir")]
        recipe: Vec<String>,
        #[command(flatten)]
        dirs: Dirs,
    },
}

/// Where a command finds recipes and stores packages.
#[derive(clap::Args)]
struct Dirs {
    /// Recipe workspace (default: the one the current directory is in)
    #[arg(long, value_name = "dir")]
    workspace: Option<PathBuf>,
    /// Store packages in <dir> (default: the workspace's
    /// pekitconf.artifact_dir, or /system/artifacts)
    #[arg(long, value_name = "dir")]
    artifact_dir: Option<PathBuf>,
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
        Command::Package { name, dirs } => {
            let identity = package(&name, dirs)?;
            println!("{identity}");
            Ok(())
        }
        Command::Release {
            names,
            recipe,
            dirs,
        } => {
            for (tag, identity) in release(names, &recipe, dirs)? {
                println!("{tag} {identity}");
            }
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

/// A checked recipe workspace, and where its packages are stored.
struct Opened {
    root: PathBuf,
    recipes: Recipes,
    artifact_dir: PathBuf,
}

/// Opens the workspace `dirs` names, or the one the current directory is
/// in, with the artifact directory `dirs` gives, or the workspace's.
fn open(dirs: Dirs) -> Result<Opened> {
    let root = match dirs.workspace {
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
    let artifact_dir = match dirs.artifact_dir {
        Some(dir) => dir,
        None => recipes.artifact_dir()?.map_or_else(
            || PathBuf::from(ppk::DEFAULT_ARTIFACT_DIR),
            |dir| root.join(dir),
        ),
    };
    Ok(Opened {
        root,
        recipes,
        artifact_dir,
    })
}

/// Builds package `name` and packs it, returning its identity.
fn package(name: &str, dirs: Dirs) -> Result<String> {
    let mut opened = open(dirs)?;
    let package = opened.recipes.package(name)?;
    // Checked before anything is built.
    let id = PackageId::new(name, &package.version, ppk::HOST_ARCH)?;
    // A build is reproducible, so building a packed version again would
    // make what is packed already.
    if let Some(identity) = ppk::packed(&id, &opened.artifact_dir)? {
        eprintln!("pekit: {id} is already packed");
        return Ok(identity.to_string());
    }
    let mut identities =
        build_and_pack(&mut opened.recipes, &[(id, package)], &opened.artifact_dir)?;
    Ok(identities.remove(0))
}

/// Builds and packs packages `names`, and every package the recipes in
/// `recipe_dirs` define, from a workspace with no uncommitted changes, then
/// tags `HEAD` for each package not tagged already. Returns each package's
/// tag and identity.
///
/// Every package is built, packed or not: ppk accepts a package only if it
/// matches what is packed already, so a release proves that `HEAD` makes
/// what was packed before. A tag is only ever added. One already made at an
/// earlier commit is kept, as the first commit that made its package.
fn release(
    names: Vec<String>,
    recipe_dirs: &[String],
    dirs: Dirs,
) -> Result<Vec<(String, String)>> {
    let mut opened = open(dirs)?;
    let head = git::clean_head(&opened.root)?;
    let mut wanted = names;
    for dir in recipe_dirs {
        wanted.extend(opened.recipes.packages_in(dir)?);
    }
    let mut packages: Vec<(PackageId, PackageRecipe)> = Vec::new();
    for name in wanted {
        if packages.iter().any(|(id, _)| id.name() == name) {
            continue;
        }
        let package = opened.recipes.package(&name)?;
        let id = PackageId::new(&name, &package.version, ppk::HOST_ARCH)?;
        git::check_tag(&git::tag(&name, &package.version))?;
        packages.push((id, package));
    }

    let identities = build_and_pack(&mut opened.recipes, &packages, &opened.artifact_dir)?;

    let mut released = Vec::new();
    let mut new = Vec::new();
    for ((id, _), identity) in packages.iter().zip(identities) {
        let tag = git::tag(id.name(), id.version());
        match git::tagged(&opened.root, &tag)? {
            Some(commit) if commit == head => {}
            Some(commit) => eprintln!("pekit: {tag} is already tagged, at {commit}"),
            None => new.push(tag.clone()),
        }
        released.push((tag, identity));
    }
    git::create_tags(&opened.root, &new, &head)?;
    for tag in &new {
        eprintln!("pekit: tagged {tag}");
    }
    Ok(released)
}

/// Builds `packages` and packs each into `artifact_dir`, returning their
/// identities in the same order. Packages that share a build run it once.
fn build_and_pack(
    recipes: &mut Recipes,
    packages: &[(PackageId, PackageRecipe)],
    artifact_dir: &Path,
) -> Result<Vec<String>> {
    let mut builds: Vec<&str> = Vec::new();
    for (_, package) in packages {
        if !builds.contains(&package.build.as_str()) {
            builds.push(&package.build);
        }
    }
    // Every recipe is read before anything is fetched or built.
    let build_recipes = builds
        .iter()
        .map(|build| recipes.build(build))
        .collect::<Result<Vec<_>>>()?;
    // Checked before the builds, which can take a long time, rather than
    // only when packing what they make.
    ppk::prepare_artifact_dir(artifact_dir).with_context(|| {
        format!(
            "cannot use the artifact directory {} (choose another with --artifact-dir)",
            artifact_dir.display()
        )
    })?;

    let cache = cache_dir()?;
    let mut identities = vec![None; packages.len()];
    for (build, recipe) in builds.iter().zip(&build_recipes) {
        let archive = source::fetch(&recipe.source, &cache)?;
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

        // Tools that stamp a time into what they make use this one instead
        // of the clock, so that building the same source gives the same
        // package.
        let epoch = source::newest_mtime(&unpacked)?;
        // SAFETY: nothing reads the environment concurrently. The main
        // thread is waiting for this one, and the threads GCSL starts to
        // copy a command's output end before the command's call returns.
        unsafe { std::env::set_var("SOURCE_DATE_EPOCH", epoch.to_string()) };

        eprintln!("pekit: building {build}");
        let tree = recipes.run_build(build, &source_path, &out_path, ppk::HOST_ARCH)?;        for (at, (id, package)) in packages.iter().enumerate() {
            if package.build != *build {
                continue;
            }
            let staged = work.path().join(format!("stage-{at}"));
            fs::create_dir(&staged).with_context(|| staged.display().to_string())?;
            stage::stage(&package.files, &tree, &staged)?;
            identities[at] = Some(ppk::pack(id, &staged, artifact_dir)?.identity);
        }
    }
    Ok(identities
        .into_iter()
        .map(|identity| identity.expect("every package's build has run"))
        .collect())
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
