//! Queries: what a target's evaluation reaches, found without running a
//! step.
//!
//! # Queries
//!
//! `pekit query deps` evaluates targets as a build does, their own effects
//! run (fetches, unpacks and reads, which the evaluation needs), but no
//! step: what each reaches is the steps its value, or the result it stopped
//! for, needs ([`crate::engine::query_all`]). Each target and step is
//! reported with the workspace files it reads: its code's, those it is
//! given as arguments, captures or context, and, for the target, those its
//! own effects read. A step's outputs are known only once it runs, so a
//! target that waits for one stops there, and what it reaches after is
//! unknown ([`TargetDeps::waits`]), never built to find out.
//!
//! `pekit query rdeps FILE` is the question CI asks: of the targets
//! selected, those that read the file ([`TargetDeps::reads_file`]). A
//! GCSL file is read by every target whose code is in its directory or
//! below, since its definitions are seen there, a change to it able to
//! change what a name there means, whether or not anything names it yet.
//! A target whose evaluation stopped for a step's result might read it
//! after, so it is among them too: unknown is never taken for no.

use std::fmt::Write as _;

/// What a target's evaluation reaches (see [Queries](#queries)).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TargetDeps {
    /// The target, by its qualified name.
    pub target: String,
    /// The workspace files its call's code needs, by their paths relative
    /// to the root, sorted.
    pub code: Vec<String>,
    /// The workspace files and directories its own effects read, sorted.
    pub reads: Vec<String>,
    /// Each step it reaches, in the order its code met them.
    pub steps: Vec<StepDeps>,
    /// Where it waits for a step's result, if it does, as `FILE:LINE`:
    /// what it reaches after is unknown.
    pub waits: Option<String>,
}

/// A step a target reaches (see [Queries](#queries)).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct StepDeps {
    /// The step, by its directory under `/pekit-out` and name.
    pub name: String,
    /// The workspace files its code needs, sorted.
    pub code: Vec<String>,
    /// The workspace files and directories it is given, sorted.
    pub reads: Vec<String>,
    /// The steps whose results it is given, by name, sorted.
    pub after: Vec<String>,
}

impl TargetDeps {
    /// Whether the target reads the workspace file `file`, a path relative
    /// to the root: one its code or a step's needs, or one it or a step reads
    /// or is given, a directory holding it among them; or a GCSL file of the
    /// directory of a file of that code or above (see [Queries](#queries)).
    #[must_use]
    pub fn reads_file(&self, file: &str) -> bool {
        if let Some(name) = file.strip_suffix(".gcsl") {
            let dir = name.rsplit_once('/').map_or("", |(dir, _)| dir);
            let below = |code: &String| {
                dir.is_empty()
                    || code
                        .strip_prefix(dir)
                        .is_some_and(|rest| rest.starts_with('/'))
            };
            let code = self
                .code
                .iter()
                .chain(self.steps.iter().flat_map(|step| &step.code));
            if code.into_iter().any(below) {
                return true;
            }
        }
        let holds = |read: &String| {
            read == file
                || file
                    .strip_prefix(read.as_str())
                    .is_some_and(|rest| rest.starts_with('/'))
                || read.is_empty()
        };
        self.code.iter().chain(&self.reads).any(holds)
            || self
                .steps
                .iter()
                .any(|step| step.code.iter().chain(&step.reads).any(holds))
    }

    /// The target as `pekit query deps` shows it.
    #[must_use]
    pub fn shown(&self) -> String {
        let mut shown = format!("{}\n", self.target);
        let list = |shown: &mut String, indent: &str, label: &str, items: &[String]| {
            if !items.is_empty() {
                let _ = writeln!(shown, "{indent}{label}: {}", items.join(", "));
            }
        };
        list(&mut shown, "  ", "code", &self.code);
        list(&mut shown, "  ", "reads", &self.reads);
        for step in &self.steps {
            let _ = writeln!(shown, "  step {}", step.name);
            list(&mut shown, "    ", "code", &step.code);
            list(&mut shown, "    ", "reads", &step.reads);
            list(&mut shown, "    ", "after", &step.after);
        }
        if let Some(at) = &self.waits {
            let _ = writeln!(
                shown,
                "  unknown: it waits at {at} for a step's result, so what it reaches after is \
                 known only once that step runs"
            );
        }
        shown
    }
}

/// The path relative to the workspace's root of `path`, a logical path, if
/// it is in the workspace, `/w`: `app/main.c` of `/w/app/main.c`, and the
/// empty path of `/w` itself.
#[must_use]
pub fn in_workspace(path: &str) -> Option<String> {
    let rest = path.strip_prefix(crate::sandbox::WORKSPACE)?;
    if rest.is_empty() {
        return Some(String::new());
    }
    rest.strip_prefix('/').map(str::to_owned)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn target(code: &[&str], reads: &[&str], step_reads: &[&str]) -> TargetDeps {
        let owned = |items: &[&str]| items.iter().map(|item| (*item).to_owned()).collect();
        TargetDeps {
            target: "app/app".to_owned(),
            code: owned(code),
            reads: owned(reads),
            steps: vec![StepDeps {
                name: "app/app/cc".to_owned(),
                code: Vec::new(),
                reads: owned(step_reads),
                after: Vec::new(),
            }],
            waits: None,
        }
    }

    #[test]
    fn a_target_reads_a_file_it_or_a_step_reads_or_a_directory_holds() {
        let deps = target(
            &["app/build.gcsl"],
            &["data"],
            &["app/main.c", "vendor/zlib"],
        );
        for file in [
            "app/build.gcsl",
            "data/x.txt",
            "app/main.c",
            "vendor/zlib/z.c",
        ] {
            assert!(deps.reads_file(file), "{file}");
        }
        for file in [
            "app/build.gcsl.orig",
            "vendor/zlibs/z.c",
            "app/other.c",
            "dat",
        ] {
            assert!(!deps.reads_file(file), "{file}");
        }
        // The whole workspace, as a directory, holds every file.
        assert!(target(&[], &[""], &[]).reads_file("any/file"));
    }

    #[test]
    fn a_gcsl_file_is_read_by_every_target_whose_code_is_below_it() {
        let deps = target(&["app/sub/build.gcsl"], &[], &[]);
        for file in ["workspace.gcsl", "app/defs.gcsl", "app/sub/new.gcsl"] {
            assert!(deps.reads_file(file), "{file}");
        }
        for file in [
            "lib/build.gcsl",
            "app/sub/deeper/x.gcsl",
            "app/sub/notes.txt",
        ] {
            assert!(!deps.reads_file(file), "{file}");
        }
    }

    #[test]
    fn only_workspace_paths_are_the_workspace_s() {
        assert_eq!(in_workspace("/w/app/main.c"), Some("app/main.c".to_owned()));
        assert_eq!(in_workspace("/w"), Some(String::new()));
        assert_eq!(in_workspace("/wx/a"), None);
        assert_eq!(in_workspace("/pekit-out/app/x"), None);
    }
}
