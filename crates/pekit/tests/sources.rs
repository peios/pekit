//! Tests of what a build records of its source: what it fetched but its
//! tools, and the workspace files it read (see `pekit::sources`).

mod building;
mod support;

use std::collections::BTreeSet;
use std::fs;
use std::path::Path;

use pekit::engine::{Outcomes, build_all_observed};
use pekit::load::load;
use pekit::sources::{Fetched, SourceSet};
use pekit::store::Digest;
use pekit::target::select;

use building::settings;

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// What building `names` of the workspace at `root` had as its source.
fn sources_of(root: &Path, state: &Path, names: &[&str]) -> SourceSet {
    let root = root.to_path_buf();
    let state = state.to_path_buf();
    let names: Vec<String> = names.iter().map(|name| (*name).to_owned()).collect();
    std::thread::Builder::new()
        .stack_size(gcsl::STACK_SIZE)
        .spawn(move || {
            let mut loaded = load(&root).unwrap();
            let names: Vec<&str> = names.iter().map(String::as_str).collect();
            let targets = select(&loaded.workspace, "build", &names, "").unwrap();
            let settings = settings(&root, &state, ("x86_64", "dev"));
            let Outcomes {
                targets: outcomes,
                sources,
                ..
            } = build_all_observed(&mut loaded, &targets, &settings, &mut |_| {}).unwrap();
            for outcome in outcomes {
                if let Err(error) = outcome {
                    panic!("{}", error.render(&loaded));
                }
            }
            sources
        })
        .unwrap()
        .join()
        .unwrap()
}

/// A step that shouts its input file into its output.
const SHOUT: &str = "#[hermetic] fn shout(src: File) -> File {\n\
                     \x20   let r = exec([\"/bin/sh\", \"-c\", \"tr a-z A-Z < {src.path} > out.txt\"])\n\
                     \x20   working_dir().file(\"out.txt\")\n\
                     }\n";

#[test]
fn a_build_s_source_is_what_it_fetched_and_read_but_its_tools() {
    let (source, _) = support::serve(b"source\n", "source.tar");
    let (marked, _) = support::serve(b"marked tool\n", "tool.tar");
    let (compiler, _) = support::serve(b"compiler\n", "compiler.tar");
    let pin = |bytes: &[u8]| Digest::of(bytes).hex();
    let dir = tempfile::tempdir().unwrap();
    let outside = tempfile::tempdir().unwrap();
    write(outside.path(), &[("refs", "a repository's")]);
    std::os::unix::fs::symlink(outside.path(), dir.path().join("repository")).unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                &format!(
                    "import toolchain\n\
                     import Toolchain from toolchain\n\
                     def Toolchain::C made_c() {{\n\
                     \x20   let archive = fetch(\"{compiler}\", sha256 = \"{}\")\n\
                     \x20   host_platform = {{}}\n\
                     \x20   platform = {{}}\n\
                     \x20   provides = {{root = dir(\".\"); runtime = dir(\".\"); cc = archive; \
                     ar = archive; ld = archive; sysroot = dir(\".\")}}\n\
                     }}\n\
                     pekitconf = {{toolchains = [Toolchain::C made_c]}}\n",
                    pin(b"compiler\n")
                ),
            ),
            (
                "app/build.gcsl",
                &format!(
                    "import toolchain\n\
                     import Toolchain from toolchain\n\
                     {SHOUT}\
                     def build hello() {{\n\
                     \x20   let c = toolchain.get(Toolchain::C)\n\
                     \x20   let tool = fetch(\"{marked}\", sha256 = \"{}\", tool = true)\n\
                     \x20   out = shout(fetch(\"{source}\", sha256 = \"{}\"))\n\
                     \x20   notes = file(\"notes.txt\").read()\n\
                     \x20   patches = dir(\"patches\").files().len()\n\
                     \x20   refs = dir(\"../repository\").files().len()\n\
                     \x20   tools = [c.cc.path, tool.path]\n\
                     }}\n",
                    pin(b"marked tool\n"),
                    pin(b"source\n"),
                ),
            ),
            ("app/notes.txt", "notes"),
            ("app/patches/one.patch", "a patch"),
            ("unread.txt", "never read"),
        ],
    );
    let state = tempfile::tempdir().unwrap();

    let found = sources_of(dir.path(), state.path(), &["hello"]);

    // The source's fetch, but neither the marked tool's nor the
    // toolchain's.
    assert_eq!(
        found.fetched,
        BTreeSet::from([Fetched {
            url: source,
            sha256: pin(b"source\n"),
        }])
    );
    assert!(found.git.is_empty());
    // The code, and what it read, but no file it didn't, and nothing the
    // link out of the workspace leads to.
    let files: Vec<&str> = found.files.iter().map(String::as_str).collect();
    assert_eq!(files, ["app/build.gcsl", "app/notes.txt", "workspace.gcsl"]);
    let dirs: Vec<&str> = found.dirs.iter().map(String::as_str).collect();
    assert_eq!(dirs, ["app/patches"]);
}

#[test]
fn a_build_s_source_is_the_same_whether_its_steps_ran_or_were_found() {
    let (source, _) = support::serve(b"source\n", "source.tar");
    let pin = Digest::of(b"source\n").hex();
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            (
                "app/build.gcsl",
                &format!(
                    "{SHOUT}def build hello() {{ out = shout(fetch(\"{source}\", sha256 = \"{pin}\")) }}\n"
                ),
            ),
        ],
    );
    let state = tempfile::tempdir().unwrap();
    // The step runs, and is then found in the store.
    let built = building::build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!(built.ran, 1);
    let found = sources_of(dir.path(), state.path(), &["hello"]);
    let fresh = tempfile::tempdir().unwrap();
    assert_eq!(found, sources_of(dir.path(), fresh.path(), &["hello"]));
    assert_eq!(found.fetched.len(), 1);
}

#[test]
fn a_library_s_code_is_its_whole_library_if_the_lock_pins_it_and_itself_if_not() {
    let dir = tempfile::tempdir().unwrap();
    let lock = serde_json::json!({
        "libraries": [{
            "path": "example.com/greet",
            "git": "https://example.com/greet",
            "dir": "",
            "commit": "0".repeat(40),
            "tree": "0".repeat(64),
        }],
    });
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            ("pekit.lock", &lock.to_string()),
            (
                ".gcsl/libraries/example.com/greet/greet.gcsl",
                "def greeting() -> String { = \"hello\" }\n",
            ),
            (".gcsl/libraries/example.com/greet/unread.txt", "never read"),
            (
                ".gcsl/libraries/example.com/loose/loose.gcsl",
                "def name() -> String { = \"world\" }\n",
            ),
            (".gcsl/libraries/example.com/loose/unread.txt", "never read"),
            (
                "app/build.gcsl",
                "import \"example.com/greet\"\n\
                 import \"example.com/loose\"\n\
                 def build hello() { text = \"{greet.greeting()} {loose.name()}\" }\n",
            ),
        ],
    );
    let state = tempfile::tempdir().unwrap();

    let found = sources_of(dir.path(), state.path(), &["hello"]);

    // The pinned library whole, as a build from its source restores it;
    // the unpinned one's code alone.
    let libraries: Vec<&str> = found.libraries.iter().map(String::as_str).collect();
    assert_eq!(libraries, ["example.com/greet"]);
    let dirs: Vec<&str> = found.dirs.iter().map(String::as_str).collect();
    assert_eq!(dirs, [".gcsl/libraries/example.com/greet"]);
    let files: Vec<&str> = found.files.iter().map(String::as_str).collect();
    assert_eq!(
        files,
        [
            ".gcsl/libraries/example.com/loose/loose.gcsl",
            "app/build.gcsl",
            "pekit.lock",
            "workspace.gcsl"
        ]
    );
}

#[test]
fn what_a_target_built_from_the_record_holds_is_no_source() {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            ("doc.txt", "doc\n"),
            // A file the last record named, which nothing reads now.
            ("old.txt", "old\n"),
            ("pekit.sources", "[\"old.txt\"]\n"),
            (
                "app/build.gcsl",
                "import json\n\
                 def build doc() { = file(\"../doc.txt\") }\n\
                 # Built from the record, as a recipe's source package is.\n\
                 def build listed() {\n\
                 \x20   let paths: List<String> = json.decode(file(\"../pekit.sources\").read())\n\
                 \x20   = for path in paths { = file(\"../{path}\") }\n\
                 }\n",
            ),
        ],
    );
    let state = tempfile::tempdir().unwrap();
    let source = sources_of(dir.path(), state.path(), &["app/doc", "app/listed"]);
    assert!(source.files.contains("doc.txt"), "{:?}", source.files);
    assert!(!source.files.contains("old.txt"), "{:?}", source.files);
}
