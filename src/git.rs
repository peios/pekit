//! The Git repository a recipe workspace is in: what a release checks before
//! building, and the tags it makes after. Each released package is tagged
//! `name@version` at the commit that made it, and a tag is only ever added.

use std::io::Write;
use std::path::Path;
use std::process::{Command, Output, Stdio};

use anyhow::{Context, Result, bail};

/// The tag a release gives version `version` of package `name`.
pub fn tag(name: &str, version: &str) -> String {
    format!("{name}@{version}")
}

/// The commit `HEAD` names in the repository `root` is in, refusing a work
/// tree with any change under `root`, untracked files included: pekit reads
/// every GCSL file of the workspace, so a tag must name all of them.
pub fn clean_head(root: &Path) -> Result<String> {
    let status = git(
        root,
        &["status", "--porcelain", "--untracked-files=all", "--", "."],
    )?;
    if !status.is_empty() {
        bail!(
            "{} has changes that are not committed:\n{}",
            root.display(),
            status.trim_end()
        );
    }
    Ok(git(root, &["rev-parse", "--verify", "HEAD^{commit}"])?
        .trim()
        .to_owned())
}

/// Refuses `tag` unless Git accepts it as a tag's name. A version can hold
/// characters, such as `~`, that a tag cannot.
pub fn check_tag(tag: &str) -> Result<()> {
    let output = run(Command::new("git").args(["check-ref-format", &format!("refs/tags/{tag}")]))?;
    if !output.status.success() {
        bail!("{tag} cannot be a Git tag's name");
    }
    Ok(())
}

/// The commit `tag` names in the repository `root` is in, or `None` if there
/// is no such tag.
pub fn tagged(root: &Path, tag: &str) -> Result<Option<String>> {
    let spec = format!("refs/tags/{tag}^{{commit}}");
    let output = run(Command::new("git").arg("-C").arg(root).args([
        "rev-parse",
        "--quiet",
        "--verify",
        &spec,
    ]))?;
    if !output.status.success() {
        return Ok(None);
    }
    let commit = String::from_utf8(output.stdout).context("git's output is not UTF-8")?;
    Ok(Some(commit.trim().to_owned()))
}

/// Creates each of `tags` at `commit`, all of them or, if any already
/// exists, none.
pub fn create_tags(root: &Path, tags: &[String], commit: &str) -> Result<()> {
    if tags.is_empty() {
        return Ok(());
    }
    let mut child = Command::new("git")
        .arg("-C")
        .arg(root)
        .args(["update-ref", "--stdin"])
        .stdin(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .context("cannot run git")?;
    let mut stdin = child.stdin.take().expect("stdin is piped");
    for tag in tags {
        writeln!(stdin, "create refs/tags/{tag} {commit}").context("cannot write to git")?;
    }
    drop(stdin);
    let output = child.wait_with_output().context("cannot run git")?;
    if !output.status.success() {
        bail!(
            "cannot create the tags: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }
    Ok(())
}

/// Runs git in `dir` with `args`, returning its standard output.
fn git(dir: &Path, args: &[&str]) -> Result<String> {
    let output = run(Command::new("git").arg("-C").arg(dir).args(args))?;
    if !output.status.success() {
        bail!(
            "git {}: {}",
            args.join(" "),
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }
    String::from_utf8(output.stdout).context("git's output is not UTF-8")
}

fn run(command: &mut Command) -> Result<Output> {
    command.output().context("cannot run git")
}
