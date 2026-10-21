//! The recipe workspace: pekit's include file, the checked GCSL workspace and
//! what pekit reads from it.
//!
//! Recipes describe and pekit acts: a definition is invoked by name through
//! GCSL's host API, and what comes back is plain data, but for references
//! such as a package's `build = build busybox`, which
//! `Workspace::definition_of` turns back into names.

use std::fs;
use std::io::{self, IsTerminal, Write};
use std::path::{Path, PathBuf};

use anyhow::{Context, Result, anyhow, bail};
use gcsl::diagnostic::Renderer;
use gcsl::workspace::{self, Given, InvokeError, Referenced, Workspace};
use gcsl::{Diagnostic, Outcome, Severity, SourceFile, Value};

/// pekit's declarations of the types and def-types recipes use, which
/// `pekit init` writes into a workspace.
pub const INCLUDE: &str = include_str!("pekit-include.gcsl");

/// The name of the include file, at the workspace's root, where every recipe
/// directory sees its declarations without importing them.
pub const INCLUDE_FILE: &str = "pekit-include.gcsl";

/// The file that marks a GCSL workspace's root.
pub const WORKSPACE_FILE: &str = "workspace.gcsl";

/// The recipe format this pekit reads, which each recipe file gives as
/// `pekit = 1`.
const FORMAT: i64 = 1;

/// The field of `workspace.gcsl` that holds pekit's settings. A root file's
/// fields are the whole workspace's, so recipe files cannot use it.
const CONFIG: &str = "pekitconf";

/// A checked recipe workspace.
pub struct Recipes {
    workspace: Workspace,
    /// Every source, by input number, the standard files first: what
    /// diagnostics are rendered against.
    sources: Vec<SourceFile>,
}

/// What a package's recipe says.
pub struct PackageRecipe {
    /// The version the package is made with: the recipe file's `version`
    /// and `revision`, `1.37.0-1`.
    pub version: String,
    /// The build it takes its files from.
    pub build: String,
    /// Each path in /usr with the path in the install tree it comes from, in
    /// the order the recipe gives them.
    pub files: Vec<(String, String)>,
}

/// What a build's recipe says before it runs.
pub struct BuildRecipe {
    /// The name of its source.
    pub source_name: String,
    /// The source pekit gives it.
    pub source: Source,
}

/// A build's source.
pub enum Source {
    /// An archive pekit downloads, checks and unpacks.
    Fetched {
        url: String,
        /// The SHA-256 of what the URL serves, in lower-case hex.
        sha256: String,
    },
    /// A source tree on this machine, as the recipe writes it: relative to
    /// the workspace's root unless absolute.
    Local { path: String },
}

impl Recipes {
    /// Checks the workspace at `root`, refusing it unless its include file is
    /// pekit's. Its warnings are shown on standard error.
    pub fn open(root: &Path) -> Result<Self> {
        check_include(root)?;
        let read = gcsl_disk::read_workspace(root, None).map_err(|error| anyhow!("{error}"))?;
        let (mut sources, checked) =
            workspace::check_with_standard(&read.relative, &read.nested, &root.to_string_lossy());
        sources.extend(read.shown);
        match checked {
            Ok(workspace) => {
                show(&sources, workspace.warnings());
                Ok(Recipes { workspace, sources })
            }
            Err(diagnostics) => {
                show(&sources, &diagnostics);
                bail!("{}: the recipes have errors", root.display())
            }
        }
    }

    /// The artifact directory `workspace.gcsl` sets as
    /// `pekitconf.artifact_dir`, as written, or `None` if it sets no
    /// `pekitconf`.
    pub fn artifact_dir(&mut self) -> Result<Option<String>> {
        let file = self
            .workspace
            .file(WORKSPACE_FILE)
            .ok_or_else(|| anyhow!("the workspace has no {WORKSPACE_FILE}"))?;
        let value = self
            .workspace
            .run(file)
            .map(|outcome| outcome.value)
            .map_err(|diagnostics| {
                self.failed(&diagnostics, &format!("{WORKSPACE_FILE} failed"))
            })?;
        let Ok(config) = field(&value, CONFIG) else {
            return Ok(None);
        };
        let dir = field(config, "artifact_dir")
            .and_then(string)
            .with_context(|| format!("{WORKSPACE_FILE}: {CONFIG}"))?;
        if dir.is_empty() {
            bail!("{WORKSPACE_FILE}: {CONFIG}.artifact_dir is empty");
        }
        Ok(Some(dir.to_owned()))
    }

    /// The packages the recipe in directory `dir`, relative to the
    /// workspace's root, defines, in the order it defines them.
    pub fn packages_in(&self, dir: &str) -> Result<Vec<String>> {
        let def_types = self
            .workspace
            .def_types()
            .map_err(|diagnostics| self.failed(&diagnostics, "the recipes are ambiguous"))?;
        let files = &self.workspace.scan().files;
        let in_dir = |file: u32| {
            files
                .get(file as usize)
                .is_some_and(|file| Path::new(&file.path).parent() == Some(Path::new(dir)))
        };
        let packages: Vec<String> = def_types
            .iter()
            .filter(|found| found.path == "package")
            .flat_map(|found| &found.definitions)
            .filter(|definition| in_dir(definition.file))
            .map(|definition| definition.path.clone())
            .collect();
        if packages.is_empty() {
            bail!("no recipe in {dir} defines a package");
        }
        Ok(packages)
    }

    /// The recipe of package `name`.
    pub fn package(&mut self, name: &str) -> Result<PackageRecipe> {
        let file = self.definition_file("package", name)?;
        let version = self.version(file)?;
        let package = self.invoke("package", name, &[])?;
        let build = match self.referenced(field(&package, "build")?)? {
            Referenced { def_type, path } if def_type == "build" => path,
            Referenced { def_type, path } => {
                bail!("package {name}: build is {def_type} {path}, not a build")
            }
        };
        let files = match field(&package, "files")? {
            Value::Empty => Vec::new(),
            Value::Record(record) => {
                let paths = record.layout.texts.iter().zip(record.values.iter());
                paths
                    .map(|(to, from)| Ok((to.to_string(), string(from)?.to_owned())))
                    .collect::<Result<_>>()?
            }
            other => bail!("package {name}: files is {other}, not a record"),
        };
        if !list(field(&package, "dependencies")?)?.is_empty() {
            bail!("package {name}: runtime dependencies are not supported yet");
        }
        Ok(PackageRecipe {
            version,
            build,
            files,
        })
    }

    /// The recipe of build `name`, read from its meta without running it.
    pub fn build(&mut self, name: &str) -> Result<BuildRecipe> {
        let dependencies = self.meta_field("build", name, "dependencies")?;
        if !list(&dependencies)?.is_empty() {
            bail!("build {name}: build dependencies are not supported yet");
        }
        let source = self.meta_field("build", name, "source")?;
        let source = match self.referenced(&source)? {
            Referenced { def_type, path } if def_type == "source" => path,
            Referenced { def_type, path } => {
                bail!("build {name}: source is {def_type} {path}, not a source")
            }
        };
        let value = self.invoke("source", &source, &[])?;
        if let Ok(path) = field(&value, "local_path") {
            let path = string(path)?.to_owned();
            if path.is_empty() {
                bail!("source {source}: local_path is empty");
            }
            return Ok(BuildRecipe {
                source_name: source,
                source: Source::Local { path },
            });
        }
        let url = string(field(&value, "url")?)?.to_owned();
        let sha256 = string(field(&value, "sha256")?)?.to_owned();
        if sha256.len() != 64
            || !sha256
                .bytes()
                .all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
        {
            bail!("source {source}: sha256 is not 64 lower-case hex digits: {sha256}");
        }
        Ok(BuildRecipe {
            source_name: source,
            source: Source::Fetched { url, sha256 },
        })
    }

    /// Runs build `name` with `source_path` unpacked and `out_path` empty,
    /// for `arch`, and returns the install tree it gives.
    pub fn run_build(
        &mut self,
        name: &str,
        source_path: &Path,
        out_path: &Path,
        arch: &str,
    ) -> Result<PathBuf> {
        let given = |name: &str, path: &Path| -> Result<(String, Given)> {
            let path = path
                .to_str()
                .ok_or_else(|| anyhow!("{}: not valid UTF-8", path.display()))?;
            Ok((name.to_owned(), Given::Str(path.to_owned())))
        };
        let args = [
            given("source_path", source_path)?,
            given("out_path", out_path)?,
            ("arch".to_owned(), Given::Str(arch.to_owned())),
        ];
        let tree = self.invoke("build", name, &args)?;
        Ok(PathBuf::from(string(&tree)?))
    }

    /// The input number of the file that defines `def_type path`.
    fn definition_file(&self, def_type: &str, path: &str) -> Result<u32> {
        let def_types = self
            .workspace
            .def_types()
            .map_err(|diagnostics| self.failed(&diagnostics, "the recipes are ambiguous"))?;
        def_types
            .iter()
            .filter(|found| found.path == def_type)
            .flat_map(|found| &found.definitions)
            .find(|definition| definition.path == path)
            .map(|definition| definition.file)
            .ok_or_else(|| anyhow!("no recipe defines {def_type} {path}"))
    }

    /// The version a recipe file gives its packages, checking that it is
    /// written for this pekit.
    fn version(&mut self, file: u32) -> Result<String> {
        let name = self.source_name(file);
        let value = self
            .workspace
            .run(file)
            .map(|outcome| outcome.value)
            .map_err(|diagnostics| self.failed(&diagnostics, &format!("{name} failed")))?;
        match field(&value, "pekit") {
            Ok(Value::Int(FORMAT)) => {}
            Ok(other) => bail!("{name}: pekit is {other}, but this pekit reads {FORMAT}"),
            Err(_) => bail!("{name}: no `pekit = {FORMAT}`"),
        }
        let version = string(field(&value, "version").with_context(|| name.clone())?)?;
        let revision = string(field(&value, "revision").with_context(|| name.clone())?)?;
        Ok(format!("{version}-{revision}"))
    }

    /// The definition `value`, a reference, names.
    fn referenced(&self, value: &Value) -> Result<Referenced> {
        self.workspace
            .definition_of(value)
            .ok_or_else(|| anyhow!("{value} is not a reference to a definition"))
    }

    fn invoke(&mut self, def_type: &str, path: &str, args: &[(String, Given)]) -> Result<Value> {
        let invoked = self.workspace.invoke(def_type, path, args);
        self.outcome(invoked, def_type, path)
    }

    fn meta_field(&mut self, def_type: &str, path: &str, field: &str) -> Result<Value> {
        match self.workspace.meta_field(def_type, path, field) {
            Ok(Some(outcome)) => Ok(outcome.value),
            Ok(None) => bail!("{def_type} {path} has no {field}"),
            Err(error) => self.outcome(Err(error), def_type, path),
        }
    }

    /// The value of an invocation of `def_type path`, or why it failed, its
    /// diagnostics shown.
    fn outcome(
        &self,
        outcome: Result<Outcome, InvokeError>,
        def_type: &str,
        path: &str,
    ) -> Result<Value> {
        match outcome {
            Ok(outcome) => Ok(outcome.value),
            Err(InvokeError::Diagnostics(diagnostics)) => {
                Err(self.failed(&diagnostics, &format!("{def_type} {path} failed")))
            }
            Err(InvokeError::Request(request)) => {
                let mut message = request.message();
                for note in request.notes() {
                    message.push_str("; ");
                    message.push_str(&note);
                }
                Err(anyhow!(message))
            }
        }
    }

    /// Shows `diagnostics` and gives `message` as the error, or the
    /// diagnostics' own message if there is a single error.
    fn failed(&self, diagnostics: &[Diagnostic], message: &str) -> anyhow::Error {
        show(&self.sources, diagnostics);
        let errors = diagnostics
            .iter()
            .filter(|diagnostic| diagnostic.severity == Severity::Error)
            .count();
        match errors {
            1 => anyhow!("{message}"),
            n => anyhow!("{message} with {n} errors"),
        }
    }

    fn source_name(&self, file: u32) -> String {
        self.sources
            .get(file as usize)
            .map_or_else(|| format!("file {file}"), |source| source.name().to_owned())
    }
}

/// Refuses the workspace at `root` unless its include file is pekit's.
fn check_include(root: &Path) -> Result<()> {
    let path = root.join(INCLUDE_FILE);
    let text = match fs::read_to_string(&path) {
        Ok(text) => text,
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            bail!("{}: not found; run `pekit init`", path.display())
        }
        Err(error) => return Err(error).with_context(|| path.display().to_string()),
    };
    if text != INCLUDE {
        bail!("{}: not this pekit's; run `pekit init`", path.display());
    }
    Ok(())
}

/// Renders `diagnostics` to standard error, each against the source of the
/// input it names, with colour when standard error is a terminal and
/// `NO_COLOR` is not set.
fn show(sources: &[SourceFile], diagnostics: &[Diagnostic]) {
    let Some(fallback) = sources.last() else {
        return;
    };
    let no_color = std::env::var_os("NO_COLOR").is_some_and(|value| !value.is_empty());
    let renderer = if io::stderr().is_terminal() && !no_color {
        Renderer::styled()
    } else {
        Renderer::plain()
    };
    let source_of = |input: Option<u32>| {
        input
            .and_then(|input| sources.get(input as usize))
            .unwrap_or(fallback)
    };
    let text = diagnostics
        .iter()
        .map(|diagnostic| renderer.render_in(diagnostic, source_of))
        .collect::<Vec<_>>()
        .join("\n");
    // A failure to write to stderr is ignored: there is nowhere left to
    // report it.
    let _ = io::stderr().lock().write_all(text.as_bytes());
}

/// The field `name` of the record `value`.
fn field<'v>(value: &'v Value, name: &str) -> Result<&'v Value> {
    if let Value::Record(record) = value
        && let Some(at) = record.layout.texts.iter().position(|text| &**text == name)
        && let Some(found) = record.values.get(at)
    {
        return Ok(found);
    }
    bail!("{value} has no field {name}")
}

fn string(value: &Value) -> Result<&str> {
    match value {
        Value::Str(text) => Ok(text),
        other => bail!("{other} is not a string"),
    }
}

fn list(value: &Value) -> Result<&[Value]> {
    match value {
        Value::List(list) => Ok(list.values()),
        other => bail!("{other} is not a list"),
    }
}
