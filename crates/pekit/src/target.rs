//! Naming targets: from what someone types to a definition.
//!
//! # Targets
//!
//! A target is a definition made with a def-type: `build hello` is the
//! definition `hello` of the def-type `build`. Someone names it by its path
//! alone, `hello`, or qualified by its directory relative to the workspace's
//! root, `pekit2/examples/1/hello`, and `/hello` for one of the root. A name
//! alone means:
//!
//! - the one definition of that path in the workspace, wherever it is run
//!   from;
//! - else, of several, the one in the directory pekit is run from;
//! - else none: "`build hello` is defined in several places", listing each
//!   by its qualified name, for someone to type one.
//!
//! A qualified name always means the definition in that directory. A name
//! that matches nothing is "no `build` is named `hello`", suggesting the
//! closest names there are.

use std::fmt::Write as _;

use gcsl::workspace::{DefType, Definition, Workspace};
use gcsl::Diagnostic;

/// A definition someone named (see [Targets](#targets)).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Target {
    /// Its def-type's path, `build`.
    pub def_type: String,
    /// The definition.
    pub definition: Definition,
}

impl Target {
    /// Its qualified name, which names it whatever else is defined:
    /// `pekit2/examples/1/hello`.
    #[must_use]
    pub fn qualified(&self) -> String {
        self.definition.qualified()
    }
}

/// Why a name names no target (see [Targets](#targets)).
#[derive(Clone, Debug, PartialEq)]
pub enum TargetError {
    /// The workspace's def-types could not be listed: two of one path, say.
    DefTypes(Vec<Diagnostic>),
    /// No def-type has the path.
    NoDefType {
        /// The def-type's path asked for.
        def_type: String,
    },
    /// No definition of the def-type has the name.
    NoDefinition {
        /// The def-type's path.
        def_type: String,
        /// The name asked for.
        name: String,
        /// The closest names its definitions have, qualified where their
        /// paths repeat.
        similar: Vec<String>,
    },
    /// Several definitions have the name, none of them here.
    Ambiguous {
        /// The def-type's path.
        def_type: String,
        /// The name asked for.
        name: String,
        /// Each candidate's qualified name, sorted.
        candidates: Vec<String>,
    },
}

impl TargetError {
    /// The error as the `pekit` command shows it, with no trailing newline.
    #[must_use]
    pub fn message(&self) -> String {
        match self {
            TargetError::DefTypes(diagnostics) => diagnostics
                .iter()
                .map(|diagnostic| format!("error: {}", diagnostic.message))
                .collect::<Vec<_>>()
                .join("\n"),
            TargetError::NoDefType { def_type } => {
                format!("error: no def-type is named `{def_type}`")
            }
            TargetError::NoDefinition {
                def_type,
                name,
                similar,
            } => {
                let mut shown = format!("error: no `{def_type}` is named `{name}`");
                if !similar.is_empty() {
                    let names: Vec<String> =
                        similar.iter().map(|name| format!("`{name}`")).collect();
                    let _ = write!(shown, "\n  help: there are {}", names.join(", "));
                }
                shown
            }
            TargetError::Ambiguous {
                def_type,
                name,
                candidates,
            } => {
                let mut shown =
                    format!("error: `{def_type} {name}` is defined in several places; name one:");
                for candidate in candidates {
                    let _ = write!(shown, "\n  {candidate}");
                }
                shown
            }
        }
    }
}

/// The definition `name` names, of the def-type `def_type`, in `workspace`,
/// named from the directory `here`, relative to its root (see
/// [Targets](#targets)).
///
/// # Errors
///
/// That no def-type or definition has the name, or that it is ambiguous.
pub fn resolve(
    workspace: &Workspace,
    (def_type, name): (&str, &str),
    here: &str,
) -> Result<Target, TargetError> {
    let def_types = workspace.def_types().map_err(TargetError::DefTypes)?;
    let Some(found) = def_types.iter().find(|found| found.path == def_type) else {
        return Err(TargetError::NoDefType {
            def_type: def_type.to_owned(),
        });
    };
    let target = |definition: &Definition| Target {
        def_type: def_type.to_owned(),
        definition: definition.clone(),
    };
    if let Some((dir, path)) = name.rsplit_once('/') {
        let dir = dir.trim_start_matches('/');
        return found
            .definitions
            .iter()
            .find(|definition| definition.path == path && definition.dir == dir)
            .map(target)
            .ok_or_else(|| no_definition(found, name));
    }
    let candidates: Vec<&Definition> = found
        .definitions
        .iter()
        .filter(|definition| definition.path == name)
        .collect();
    match candidates[..] {
        [] => Err(no_definition(found, name)),
        [only] => Ok(target(only)),
        _ => {
            let local: Vec<&&Definition> = candidates
                .iter()
                .filter(|definition| definition.dir == here)
                .collect();
            if let [only] = local[..] {
                return Ok(target(only));
            }
            let mut qualified: Vec<String> = candidates
                .iter()
                .map(|definition| definition.qualified())
                .collect();
            qualified.sort();
            Err(TargetError::Ambiguous {
                def_type: def_type.to_owned(),
                name: name.to_owned(),
                candidates: qualified,
            })
        }
    }
}

/// "no `build` is named `hello`", suggesting the closest names `found`'s
/// definitions have.
fn no_definition(found: &DefType, name: &str) -> TargetError {
    let wanted = name.rsplit('/').next().unwrap_or(name);
    let mut similar: Vec<String> = found
        .definitions
        .iter()
        .filter(|definition| close(&definition.path, wanted))
        .map(Definition::qualified)
        .collect();
    similar.sort();
    similar.dedup();
    TargetError::NoDefinition {
        def_type: found.path.clone(),
        name: name.to_owned(),
        similar,
    }
}

/// Whether `a` and `b` are close enough to suggest one for the other: at
/// most a third of the longer apart by edits, and at least one.
fn close(a: &str, b: &str) -> bool {
    let longest = a.chars().count().max(b.chars().count());
    let allowed = (longest / 3).max(1);
    edits(a, b) <= allowed
}

/// The number of single-character insertions, deletions and substitutions
/// that make `a` into `b`.
fn edits(a: &str, b: &str) -> usize {
    let b: Vec<char> = b.chars().collect();
    let mut row: Vec<usize> = (0..=b.len()).collect();
    for (i, ca) in a.chars().enumerate() {
        let mut previous = row[0];
        row[0] = i + 1;
        for (j, &cb) in b.iter().enumerate() {
            let substituted = previous + usize::from(ca != cb);
            previous = row[j + 1];
            row[j + 1] = substituted.min(previous + 1).min(row[j] + 1);
        }
    }
    row[b.len()]
}
