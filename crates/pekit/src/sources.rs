//! A package's corresponding source: what its build fetched and read, which
//! the sources file records for each recipe (see [Sources](#sources)).
//!
//! # Sources
//!
//! A build's source is everything it was built from that isn't a tool:
//!
//! - each file it fetched, by its URL and SHA-256, save those fetched with
//!   `fetch(..., tool = true)`, a compiler's archive or a build
//!   environment's programs;
//! - each git tree it fetched, by its URL and commit;
//! - each workspace file and directory its code, its own effects and its
//!   steps read, or its targets' values hold, with `workspace.gcsl` and
//!   `pekit.lock`, which loading the workspace takes, save a path that
//!   leads out of the workspace, such as a link to a repository of
//!   packages to take build roots from;
//! - each library whose code it ran: one the lock pins whole, by its
//!   import path and its directory, as a build restores it whole, and one
//!   it pins none of by the files of its code.
//!
//! What is fetched while a toolchain is made, in GCSL's `^making_tool`,
//! is a tool, as is what the toolchain is made from.
//!
//! `pekit sources` builds each recipe's packages, a recipe being the
//! directory their definitions are in, and records what each build's source
//! was in the sources file, `pekit.sources`, beside `pekit.lock`, with the
//! names of the recipe's packages. A package that holds a recipe's source
//! is built from that record alone, so it is the same wherever it is built,
//! and the record shows in review when a recipe's source changes.
//! `pekit publish --strict` refuses to publish a recipe's packages when its
//! record is not what its build's source and its packages are.

use std::collections::BTreeSet;
use std::fs;
use std::path::Path;

use serde::{Deserialize, Serialize};

/// The sources file, at the workspace's root.
pub const SOURCES_FILE: &str = "pekit.sources";

/// A file a build fetched.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize, Deserialize)]
pub struct Fetched {
    /// Where it was fetched from.
    pub url: String,
    /// Its SHA-256, in lowercase hexadecimal.
    pub sha256: String,
}

/// A git tree a build fetched.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize, Deserialize)]
pub struct GitTree {
    /// The repository.
    pub url: String,
    /// The commit, in full.
    pub commit: String,
}

/// What one build's source was (see [Sources](#sources)).
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct SourceSet {
    /// The files it fetched, sorted.
    #[serde(default)]
    pub fetched: BTreeSet<Fetched>,
    /// The git trees it fetched, sorted.
    #[serde(default)]
    pub git: BTreeSet<GitTree>,
    /// The libraries whose code it ran, by their import paths, sorted: each
    /// one the lock pins, its directory among `dirs`.
    #[serde(default)]
    pub libraries: BTreeSet<String>,
    /// The workspace files it read, by their paths relative to the
    /// workspace's root, sorted.
    #[serde(default)]
    pub files: BTreeSet<String>,
    /// The workspace directories it read, each standing for everything in
    /// it, by their paths relative to the workspace's root, sorted.
    #[serde(default)]
    pub dirs: BTreeSet<String>,
}

impl SourceSet {
    /// Adds the workspace paths `read`, each a directory's if `is_dir` says
    /// so and a file's otherwise, to `dirs` and `files`: but none inside a
    /// directory read too, and none of `left_out` or inside one.
    pub fn add_reads(
        &mut self,
        read: BTreeSet<String>,
        left_out: &BTreeSet<String>,
        is_dir: impl Fn(&str) -> bool,
    ) {
        let within = |path: &str, dir: &str| {
            dir.is_empty() || path == dir || path.starts_with(&format!("{dir}/"))
        };
        let (dirs, files): (BTreeSet<String>, BTreeSet<String>) =
            read.into_iter().partition(|path| is_dir(path));
        let kept = |path: &&String| {
            !left_out.iter().any(|dir| within(path, dir))
                && !dirs.iter().any(|dir| dir != *path && within(path, dir))
        };
        self.files.extend(files.iter().filter(kept).cloned());
        self.dirs.extend(dirs.iter().filter(kept).cloned());
    }
}

/// One recipe's record.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Recipe {
    /// The recipe: the directory its packages' definitions are in, relative
    /// to the workspace's root.
    pub recipe: String,
    /// The names of its packages, sorted: those a package of its source is
    /// the source of.
    #[serde(default)]
    pub packages: Vec<String>,
    /// What its build's source was.
    #[serde(flatten)]
    pub source: SourceSet,
}

/// The sources file (see [Sources](#sources)).
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Sources {
    /// Each recipe's record, sorted by recipe.
    #[serde(default)]
    pub recipes: Vec<Recipe>,
}

impl Sources {
    /// The sources file of the workspace at `root`, empty if it has none.
    ///
    /// # Errors
    ///
    /// That it cannot be read or is not a sources file.
    pub fn read(root: &Path) -> Result<Self, String> {
        let path = root.join(SOURCES_FILE);
        let text = match fs::read_to_string(&path) {
            Ok(text) => text,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Ok(Self::default());
            }
            Err(error) => return Err(format!("{}: {error}", path.display())),
        };
        serde_json::from_str(&text).map_err(|error| format!("{}: {error}", path.display()))
    }

    /// Writes the sources file of the workspace at `root`, replacing it
    /// whole.
    ///
    /// # Errors
    ///
    /// That it cannot be written.
    pub fn write(&self, root: &Path) -> Result<(), String> {
        let path = root.join(SOURCES_FILE);
        let mut text = serde_json::to_string_pretty(self).map_err(|error| error.to_string())?;
        text.push('\n');
        let temp = root.join(format!(".{SOURCES_FILE}.{}", crate::sweep::token()));
        fs::write(&temp, text).map_err(|error| format!("{}: {error}", temp.display()))?;
        fs::rename(&temp, &path).map_err(|error| format!("{}: {error}", path.display()))
    }

    /// The record of `recipe`, if it has one.
    #[must_use]
    pub fn get(&self, recipe: &str) -> Option<&Recipe> {
        self.recipes.iter().find(|each| each.recipe == recipe)
    }

    /// Records `record`, its packages sorted, replacing what was recorded of
    /// its recipe.
    pub fn set(&mut self, mut record: Recipe) {
        record.packages.sort();
        record.packages.dedup();
        self.recipes.retain(|each| each.recipe != record.recipe);
        self.recipes.push(record);
        self.recipes.sort_by(|a, b| a.recipe.cmp(&b.recipe));
    }
}

/// The recipe of a target named `qualified`, `/org.gnu.bash/org.gnu.bash`
/// say: the directory its definition is in, `org.gnu.bash`, or empty for
/// the workspace's root.
#[must_use]
pub fn recipe_of(qualified: &str) -> String {
    let path = qualified.trim_start_matches('/');
    path.rsplit_once('/')
        .map_or_else(String::new, |(dir, _)| dir.to_owned())
}

/// The name of a target named `qualified`: its definition's own name, the
/// part after its recipe.
#[must_use]
pub fn name_of(qualified: &str) -> String {
    let path = qualified.trim_start_matches('/');
    path.rsplit_once('/')
        .map_or(path, |(_, name)| name)
        .to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn paths(each: &[&str]) -> BTreeSet<String> {
        each.iter().map(|&path| path.to_owned()).collect()
    }

    fn record(recipe: &str, files: &[&str]) -> Recipe {
        Recipe {
            recipe: recipe.to_owned(),
            packages: vec![format!("{recipe}.b"), format!("{recipe}.a")],
            source: SourceSet {
                files: paths(files),
                ..SourceSet::default()
            },
        }
    }

    #[test]
    fn a_recipe_is_its_targets_directory_and_a_name_the_rest() {
        assert_eq!(recipe_of("/org.gnu.bash/org.gnu.bash"), "org.gnu.bash");
        assert_eq!(recipe_of("a/b/c"), "a/b");
        assert_eq!(recipe_of("/top"), "");
        assert_eq!(
            name_of("/org.gnu.bash/org.gnu.bash-doc"),
            "org.gnu.bash-doc"
        );
        assert_eq!(name_of("/top"), "top");
    }

    #[test]
    fn a_path_in_a_directory_read_or_left_out_goes() {
        let mut set = SourceSet::default();
        let dirs = paths(&["org.gnu.bash", "org.gnu.bash/patches", "repository"]);
        set.add_reads(
            paths(&[
                "org.gnu.bash",
                "org.gnu.bash/build.gcsl",
                "org.gnu.bash/patches",
                "org.gnu.bash-doc/x",
                "repository",
                "repository/refs",
                "recipe.gcsl",
            ]),
            &paths(&["repository"]),
            |path| dirs.contains(path),
        );
        assert_eq!(set.dirs, paths(&["org.gnu.bash"]));
        assert_eq!(set.files, paths(&["org.gnu.bash-doc/x", "recipe.gcsl"]));
    }

    #[test]
    fn the_file_reads_back_with_recipes_and_packages_sorted() {
        let root = tempfile::tempdir().unwrap();
        assert_eq!(Sources::read(root.path()).unwrap(), Sources::default());
        let mut sources = Sources::default();
        sources.set(record("b", &["b/x"]));
        sources.set(record("a", &["a/x"]));
        sources.set(record("b", &["b/y"]));
        sources.write(root.path()).unwrap();
        let read = Sources::read(root.path()).unwrap();
        assert_eq!(read, sources);
        let names: Vec<&str> = read
            .recipes
            .iter()
            .map(|each| each.recipe.as_str())
            .collect();
        assert_eq!(names, ["a", "b"]);
        let b = read.get("b").unwrap();
        assert_eq!(b.packages, ["b.a", "b.b"]);
        assert_eq!(b.source.files, paths(&["b/y"]));
    }
}
