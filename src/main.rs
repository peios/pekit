//! pekit builds Peios packages from GCSL recipes.
//!
//! A recipe workspace is a GCSL workspace whose root holds pekit's include
//! file. `pekit package` reads a package's recipe, fetches its build's
//! source, runs the build, chooses the package's files from the install
//! tree and packs them with ppk. `pekit release` does the same from a
//! committed workspace and tags the commit that made each package.

mod cache;
mod git;
mod recipes;
mod source;
mod stage;
mod sync;

use std::fs::{self, OpenOptions};
use std::io;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::thread;

use anyhow::{Context, Result, anyhow, bail};
use clap::{Parser, Subcommand};
use ppk::PackageId;

use crate::cache::{KeyParts, WorkDir};
use crate::recipes::{
    BuildRecipe, INCLUDE, INCLUDE_FILE, PackageRecipe, Recipes, Source, WORKSPACE_FILE,
};
use crate::sync::Tree;

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
        /// Packages to build, by name
        #[arg(value_name = "name", required = true)]
        names: Vec<String>,
        /// Build afresh: ignore cached builds, and empty each build's work
        /// directory first. Packed versions are still not built again
        #[arg(long)]
        rebuild: bool,
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
        Command::Package {
            names,
            rebuild,
            dirs,
        } => {
            for (package, identity) in package(names, rebuild, dirs)? {
                println!("{package} {identity}");
            }
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

/// Builds packages `names` and packs them, returning each one's
/// `name@version` and identity. A package whose version is packed already
/// is not built again, and packages that share a build run it once. Builds
/// use the cache, afresh if `rebuild`.
fn package(names: Vec<String>, rebuild: bool, dirs: Dirs) -> Result<Vec<(String, String)>> {
    let mut opened = open(dirs)?;
    // Each package, with its identity if it is packed already.
    let mut wanted: Vec<(PackageId, Option<String>)> = Vec::new();
    let mut unpacked = Vec::new();
    for name in names {
        if wanted.iter().any(|(id, _)| id.name() == name) {
            continue;
        }
        let package = opened.recipes.package(&name)?;
        // Checked before anything is built.
        let id = PackageId::new(&name, &package.version, ppk::HOST_ARCH)?;
        // A build is reproducible, so building a packed version again would
        // make what is packed already.
        let packed = ppk::packed(&id, &opened.artifact_dir)?;
        match packed {
            Some(_) => eprintln!("pekit: {id} is already packed"),
            None => unpacked.push((id.clone(), package)),
        }
        wanted.push((id, packed.map(|identity| identity.to_string())));
    }
    let mut built = build_and_pack(
        &opened.root,
        &mut opened.recipes,
        &unpacked,
        &opened.artifact_dir,
        Builds::Cached { rebuild },
    )?
    .into_iter();
    Ok(wanted
        .into_iter()
        .map(|(id, packed)| {
            let identity = packed.unwrap_or_else(|| built.next().expect("one for each built"));
            (format!("{}@{}", id.name(), id.version()), identity)
        })
        .collect())
}

/// Builds and packs packages `names`, and every package the recipes in
/// `recipe_dirs` define, from a workspace with no uncommitted changes, then
/// tags `HEAD` for each package not tagged already. Returns each package's
/// tag and identity.
///
/// Every package is built, packed or not, and afresh, without the cache:
/// ppk accepts a package only if it matches what is packed already, so a
/// release proves that `HEAD` makes what was packed before, from its source
/// alone. A tag is only ever added. One already made at an
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
    for (id, package) in &packages {
        let build = opened.recipes.build(&package.build)?;
        if let Source::Local { .. } = build.source {
            bail!(
                "{}: its source, {}, is a local path, which a tag cannot pin; release it from a fetched source",
                id.name(),
                build.source_name
            );
        }
    }

    let identities = build_and_pack(
        &opened.root,
        &mut opened.recipes,
        &packages,
        &opened.artifact_dir,
        Builds::Clean,
    )?;

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

/// How builds are run.
#[derive(Clone, Copy)]
enum Builds {
    /// With the cache: a build whose key has a kept install tree does not
    /// run, and one that runs uses its kept work directory. If `rebuild`,
    /// the build runs, from an empty work directory, and its tree replaces
    /// any kept.
    Cached { rebuild: bool },
    /// Afresh, in a new work directory, keeping nothing.
    Clean,
}

/// Builds `packages` from the workspace at `root` and packs each into
/// `artifact_dir`, returning their identities in the same order. Packages
/// that share a build run it once.
fn build_and_pack(
    root: &Path,
    recipes: &mut Recipes,
    packages: &[(PackageId, PackageRecipe)],
    artifact_dir: &Path,
    builds_as: Builds,
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
        let work = match builds_as {
            Builds::Cached { rebuild } => {
                WorkDir::kept(&cache, root, build, ppk::HOST_ARCH, rebuild)?
            }
            Builds::Clean => WorkDir::new(&cache)?,
        };
        let tree = build_tree(root, recipes, build, recipe, &work, &cache, builds_as)?;
        for (at, (id, package)) in packages.iter().enumerate() {
            if package.build != *build {
                continue;
            }
            let work_dir = cache.join("work");
            let staged = tempfile::Builder::new()
                .prefix("stage-")
                .tempdir_in(&work_dir)
                .with_context(|| work_dir.display().to_string())?;
            stage::stage(&package.files, &tree, staged.path())?;
            identities[at] = Some(ppk::pack(id, staged.path(), artifact_dir)?.identity);
        }
    }
    Ok(identities
        .into_iter()
        .map(|identity| identity.expect("every package's build has run"))
        .collect())
}

/// The install tree build `build`, whose recipe is `recipe`, gives, running
/// it in `work` unless the cache keeps its tree.
fn build_tree(
    root: &Path,
    recipes: &mut Recipes,
    build: &str,
    recipe: &BuildRecipe,
    work: &WorkDir,
    cache: &Path,
    builds_as: Builds,
) -> Result<PathBuf> {
    let previous = work.record()?;
    // A local source is read before anything else, since its content is
    // part of the key; a fetched one's SHA-256 says what it is already.
    let local = match &recipe.source {
        Source::Local { path } => {
            let dir = root.join(path);
            let files = source::local_files(&dir)?;
            Some(Tree::scan(
                &dir,
                files,
                &format!("local {path}"),
                &previous,
            )?)
        }
        Source::Fetched { .. } => None,
    };
    let key = match builds_as {
        Builds::Cached { rebuild } => {
            let source = match (&recipe.source, &local) {
                (Source::Fetched { sha256, .. }, _) => format!("fetched {sha256}"),
                (_, Some(tree)) => format!("local {} {}", tree.digest(), tree.newest()),
                (Source::Local { .. }, None) => unreachable!("a local source is read"),
            };
            let key = cache::key(&KeyParts {
                build,
                arch: ppk::HOST_ARCH,
                recipe: &recipes.build_files(build)?,
                source: &source,
            });
            if !rebuild && let Some(tree) = cache::kept(cache, &key) {
                eprintln!("pekit: {build} is unchanged since it was last built");
                return Ok(tree);
            }
            Some(key)
        }
        Builds::Clean => None,
    };

    // Tools that stamp a time into what they make use the source's newest
    // instead of the clock, so that building the same source gives the same
    // package.
    let (tree, epoch) = match (&recipe.source, local) {
        (Source::Fetched { url, sha256 }, _) => {
            let archive = source::fetch(url, sha256, cache)?;
            let unpacked = work.unpacked()?;
            let top = source::unpack(&archive, &unpacked)?;
            let files = source::files(&top)?;
            let tree = Tree::scan(&top, files, &format!("fetched {sha256}"), &previous)?;
            (tree, source::newest_mtime(&unpacked)?)
        }
        (_, Some(tree)) => {
            let epoch = tree.newest();
            (tree, epoch)
        }
        (Source::Local { .. }, None) => unreachable!("a local source is read"),
    };
    let source_path = work.source();
    let synced = tree.sync(&source_path, &previous)?;
    work.write_record(&synced)?;
    let out_path = work.out()?;

    // SAFETY: nothing reads the environment concurrently. The main thread is
    // waiting for this one, and the threads GCSL starts to copy a command's
    // output end before the command's call returns.
    unsafe { std::env::set_var("SOURCE_DATE_EPOCH", epoch.to_string()) };

    eprintln!("pekit: building {build}");
    let tree = recipes.run_build(build, &source_path, &out_path, ppk::HOST_ARCH)?;
    match key {
        Some(key) => cache::keep(cache, &key, &tree, work),
        None => Ok(tree),
    }
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
