//! Loading a workspace: finding its root, reading its files and checking
//! them through GCSL.
//!
//! # Loading
//!
//! A workspace is the directory tree under a `workspace.gcsl`, found from a
//! directory by its nearest ancestor that holds one ([`gcsl_disk::find_root`]).
//! Its files are read as `gcsl-disk` reads them for the `gcsl` command
//! ([`gcsl_disk::read_workspace`]), every nested workspace left out, and
//! checked beside GCSL's standard library ([`gcsl::workspace::check_with_standard`]),
//! rooted at `/w`, the workspace's place in the logical layout, so that
//! `fileroot.workspace` is `/w` and `fileroot.this` `/w/<directory>`.
//! The sources are kept, the standard files first, so that a diagnostic,
//! which names its file by input number, is shown against its file
//! ([`Loaded::render`], [`LoadError::render`]). A `build` and a `script` of
//! one name in one directory are an error, since `pekit run NAME` would run
//! either ([`LoadError::BuildAndScript`]).

use std::fmt::Write as _;
use std::path::{Path, PathBuf};

use gcsl::workspace::{self, Workspace};

use crate::sandbox::WORKSPACE;
use crate::target::{Target, select};
use gcsl::diagnostic::Renderer;
use gcsl::{Diagnostic, SourceFile};

/// A workspace loaded and checked (see [Loading](#loading)).
#[derive(Debug)]
pub struct Loaded {
    /// Its root, canonical.
    pub root: PathBuf,
    /// The directory it was loaded from, relative to the root, `/`
    /// separated: empty for the root itself.
    pub here: String,
    /// Every source, the standard files first, each named as it is shown,
    /// in input order: what diagnostics are shown against.
    pub sources: Vec<SourceFile>,
    /// Every source's name relative to the root, `<stdlib>/fs.gcsl` for a
    /// standard file's, in input order: what names a step's code in its
    /// receipt, wherever pekit runs from.
    pub names: Vec<String>,
    /// The checked workspace.
    pub workspace: Workspace,
}

impl Loaded {
    /// `diagnostics`, each shown against the source of its input.
    #[must_use]
    pub fn render(&self, diagnostics: &[Diagnostic]) -> String {
        render(&self.sources, diagnostics)
    }
}

/// Why a workspace could not be loaded (see [Loading](#loading)).
#[derive(Debug)]
pub enum LoadError {
    /// No directory from the one given up holds a `workspace.gcsl`.
    NoWorkspace {
        /// The directory given.
        dir: PathBuf,
    },
    /// A file or directory of the workspace could not be read.
    Read(gcsl_disk::ReadError),
    /// A `build` and a `script` of one directory have one name.
    BuildAndScript {
        /// Their qualified name.
        name: String,
    },
    /// The workspace's check failed.
    Check {
        /// Every source, as [`Loaded::sources`] has them.
        sources: Vec<SourceFile>,
        /// What the check found.
        diagnostics: Vec<Diagnostic>,
    },
}

impl LoadError {
    /// The error as the `pekit` command shows it.
    #[must_use]
    pub fn render(&self) -> String {
        match self {
            LoadError::NoWorkspace { dir } => format!(
                "error: no workspace here: neither {} nor any directory above it holds a \
                 workspace.gcsl\n",
                dir.display()
            ),
            LoadError::Read(error) => format!("error: {error}\n"),
            LoadError::BuildAndScript { name } => format!(
                "error: `{name}` is both a build and a script, so `pekit run` could run either: \
                 rename one\n"
            ),
            LoadError::Check {
                sources,
                diagnostics,
            } => render(sources, diagnostics),
        }
    }
}

/// Loads the workspace that `dir` is in (see [Loading](#loading)). Like all
/// of GCSL's pipeline, it must run on a thread with at least
/// [`gcsl::STACK_SIZE`] bytes of stack.
///
/// # Errors
///
/// That no workspace holds `dir`, that a file of it cannot be read, or
/// what its check found.
pub fn load(dir: &Path) -> Result<Loaded, LoadError> {
    let canonical = dir.canonicalize().map_err(|_| LoadError::NoWorkspace {
        dir: dir.to_path_buf(),
    })?;
    let Some(root) = gcsl_disk::find_root(&canonical) else {
        return Err(LoadError::NoWorkspace {
            dir: dir.to_path_buf(),
        });
    };
    let root = root.to_path_buf();
    let here = canonical
        .strip_prefix(&root)
        .map(|relative| relative.to_string_lossy().replace('\\', "/"))
        .unwrap_or_default();
    let read = gcsl_disk::read_workspace(&root, None).map_err(LoadError::Read)?;
    // Rooted at `/w`, as every path in a build is (see [The logical
    // layout](crate::sandbox#the-logical-layout)): so `fileroot` names the
    // workspace's directories as steps see them, on every machine.
    let (standard, checked) =
        workspace::check_with_standard(&read.relative, &read.nested, WORKSPACE);
    let mut names: Vec<String> = standard
        .iter()
        .map(|source| source.name().to_owned())
        .collect();
    names.extend(read.relative.iter().map(|source| source.name().to_owned()));
    let mut sources = standard;
    sources.extend(read.shown);
    match checked {
        Ok(workspace) => {
            if let Some(name) = build_and_script(&workspace) {
                return Err(LoadError::BuildAndScript { name });
            }
            Ok(Loaded {
                root,
                here,
                sources,
                names,
                workspace,
            })
        }
        Err(diagnostics) => Err(LoadError::Check {
            sources,
            diagnostics,
        }),
    }
}

/// The qualified name of a `build` of `workspace` that a `script` of its
/// directory has too, the first in order, if one has. A workspace whose
/// def-types cannot be listed has none here: the commands report why.
fn build_and_script(workspace: &Workspace) -> Option<String> {
    let every = |def_type: &str| -> Vec<String> {
        select(workspace, def_type, &["**"], "")
            .map(|targets| targets.iter().map(Target::qualified).collect())
            .unwrap_or_default()
    };
    let scripts = every("script");
    if scripts.is_empty() {
        return None;
    }
    every("build")
        .into_iter()
        .find(|build| scripts.binary_search(build).is_ok())
}

/// `diagnostics`, each shown against the source of its input in `sources`,
/// or with no source if it names none.
fn render(sources: &[SourceFile], diagnostics: &[Diagnostic]) -> String {
    let mut shown = String::new();
    let renderer = Renderer::plain();
    for diagnostic in diagnostics {
        let source = diagnostic
            .input
            .and_then(|input| sources.get(input as usize));
        match source {
            // Each label against its own file's source, a standard file's
            // among them.
            Some(source) => shown.push_str(&renderer.render_in(diagnostic, |input| {
                input
                    .and_then(|input| sources.get(input as usize))
                    .unwrap_or(source)
            })),
            None => {
                let _ = writeln!(shown, "error: {}", diagnostic.message);
            }
        }
    }
    shown
}
