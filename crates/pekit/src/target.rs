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
//!
//! A command takes any number of names ([`select`]), and a *glob* among
//! them, a pattern with `*` in it, selects the definitions of the def-type
//! it matches, relative to the directory pekit is run from. It reads as a
//! qualified name does, its last part the definition's name and the parts
//! before it the directory's: `app/*` is every definition of `app`,
//! `app/test_*` those whose names begin `test_`, `*` every one of the
//! directory itself, and `pkgs/*/*` every one a directory of `pkgs` holds.
//! In a part, `*` matches any run of characters. `**` matches any number of
//! directories, none among them, and as the last part matches every name
//! too, so `**` alone is every definition below, and `pkgs/**` every one of
//! `pkgs` and below. `.` is the directory itself, and every name in it.
//! What several names select is selected once, the targets sorted by their
//! qualified names.

use std::fmt::Write as _;

use gcsl::workspace::{DefType, Definition, Workspace};
use gcsl::Diagnostic;

/// The def-types GCSL's prelude declares for pekit, which a workspace has
/// whether or not it defines anything with them.
const PRELUDE_DEF_TYPES: [&str; 4] = ["build", "package", "script", "test"];

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
        // The prelude's are listed only once something is defined with them.
        if PRELUDE_DEF_TYPES.contains(&def_type) {
            return Err(TargetError::NoDefinition {
                def_type: def_type.to_owned(),
                name: name.to_owned(),
                similar: Vec::new(),
            });
        }
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

/// The definitions of the def-type `def_type` in `workspace` that `names`
/// select, each a name or a glob, named from the directory
/// `here`, relative to its root (see [Targets](#targets)): sorted by
/// qualified name, each once. A glob that selects nothing selects nothing,
/// and none selects a library's definitions, which are in no directory of
/// the workspace.
///
/// # Errors
///
/// What [`resolve`] finds wrong with a name, or that the workspace's
/// def-types cannot be listed.
pub fn select(
    workspace: &Workspace,
    def_type: &str,
    names: &[&str],
    here: &str,
) -> Result<Vec<Target>, TargetError> {
    let mut targets: Vec<Target> = Vec::new();
    for name in names {
        if !is_glob(name) {
            targets.push(resolve(workspace, (def_type, name), here)?);
            continue;
        }
        let def_types = workspace.def_types().map_err(TargetError::DefTypes)?;
        let Some(found) = def_types.iter().find(|found| found.path == def_type) else {
            if PRELUDE_DEF_TYPES.contains(&def_type) {
                continue;
            }
            return Err(TargetError::NoDefType {
                def_type: def_type.to_owned(),
            });
        };
        let (pattern, wanted) = glob_of(here, name);
        targets.extend(
            found
                .definitions
                .iter()
                .filter(|definition| {
                    let dir: Vec<&str> = definition
                        .dir
                        .split('/')
                        .filter(|part| !part.is_empty())
                        .collect();
                    // A library's directory is none of the workspace's, so
                    // no glob of the workspace's matches its definitions.
                    dir.first() != Some(&gcsl::workspace::LIBRARIES)
                        && matches_glob(&pattern, &dir)
                        && matches_segment(wanted, &definition.path)
                })
                .map(|definition| Target {
                    def_type: def_type.to_owned(),
                    definition: definition.clone(),
                }),
        );
    }
    targets.sort_by_key(Target::qualified);
    targets.dedup();
    Ok(targets)
}

/// Whether `name` is a glob: a directory, `.` or ending `/.`, or a pattern
/// with `*` in it.
#[must_use]
pub fn is_glob(name: &str) -> bool {
    name == "." || name.ends_with("/.") || name.contains('*')
}

/// The glob `name`, from the directory `here`, as the segments of the
/// directories it matches and the glob of the names it matches there (see
/// [Targets](#targets)): its last part the name, as a qualified name's is,
/// but for `**` and `.`, which are directories, each of whose names it
/// matches.
fn glob_of<'a>(here: &'a str, name: &'a str) -> (Vec<&'a str>, &'a str) {
    let (dir, last) = match name.rsplit_once('/') {
        Some(("", last)) => ("/", last),
        Some((dir, last)) => (dir, last),
        None => (".", name),
    };
    match last {
        "**" => (glob_segments(here, name), "*"),
        "." => (glob_segments(here, dir), "*"),
        _ => (glob_segments(here, dir), last),
    }
}

/// The segments of the directory glob `name`, from the directory `here`:
/// `here`'s own, then the glob's, `.` dropped and `..` leaving the one
/// before.
fn glob_segments<'a>(here: &'a str, name: &'a str) -> Vec<&'a str> {
    let mut segments: Vec<&str> = Vec::new();
    let start = if name.starts_with('/') { "" } else { here };
    for part in start.split('/').chain(name.split('/')) {
        match part {
            "" | "." => {}
            ".." => {
                segments.pop();
            }
            _ => segments.push(part),
        }
    }
    segments
}

/// Whether the directory of the segments `dir` matches the glob of the
/// segments `pattern`: `**` any number of segments, a segment with `*` in it
/// any one whose name its parts begin, end and are in, in order, and any
/// other the same segment.
fn matches_glob(pattern: &[&str], dir: &[&str]) -> bool {
    match pattern.split_first() {
        None => dir.is_empty(),
        Some((&"**", rest)) => (0..=dir.len()).any(|skip| matches_glob(rest, &dir[skip..])),
        Some((first, rest)) => match dir.split_first() {
            Some((segment, others)) => {
                matches_segment(first, segment) && matches_glob(rest, others)
            }
            None => false,
        },
    }
}

/// Whether the directory's or definition's name `segment` matches the glob
/// segment `pattern`, whose `*`s each match any run of characters.
fn matches_segment(pattern: &str, segment: &str) -> bool {
    let mut parts = pattern.split('*');
    let first = parts.next().unwrap_or_default();
    let Some(mut rest) = segment.strip_prefix(first) else {
        return false;
    };
    let parts: Vec<&str> = parts.collect();
    let Some((last, middle)) = parts.split_last() else {
        return rest.is_empty();
    };
    for part in middle {
        match rest.find(part) {
            Some(at) => rest = &rest[at + part.len()..],
            None => return false,
        }
    }
    rest.ends_with(last)
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
