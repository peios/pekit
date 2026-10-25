//! Tests of building several targets as one build, of selecting them by
//! names and directory globs, and of the flags every command shares.

mod building;

use std::fs;
use std::path::Path;

use gcsl::eval::plain::Plain;
use pekit::engine::{Outcomes, Reuse, Settings, build_all_observed};
use pekit::load::load;
use pekit::platform::ExecutionPlatform;
use pekit::target::{Target, select};

use building::{Options, build_in, observed_with, settings};

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// A step that shouts its input file into its output.
const SHOUT: &str = "#[hermetic] fn shout(src: File) -> File {\n\
                     \x20   let r = exec([\"/bin/sh\", \"-c\", \"tr a-z A-Z < {src.path} > out.txt\"])\n\
                     \x20   file(\"out.txt\")\n\
                     }\n";

/// A workspace of `files`, and its `workspace.gcsl`, empty.
fn workspace(files: &[(&str, &str)]) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(dir.path(), &[("workspace.gcsl", "")]);
    write(dir.path(), files);
    dir
}

/// The targets of `def_type` that `names` select in the workspace at
/// `root`, from its directory `here`, by their qualified names.
fn selected(root: &Path, def_type: &str, names: &[&str], here: &str) -> Vec<String> {
    let root = root.to_path_buf();
    let def_type = def_type.to_owned();
    let names: Vec<String> = names.iter().map(|name| (*name).to_owned()).collect();
    let here = here.to_owned();
    std::thread::Builder::new()
        .stack_size(gcsl::STACK_SIZE)
        .spawn(move || {
            let loaded = load(&root).unwrap();
            let names: Vec<&str> = names.iter().map(String::as_str).collect();
            select(&loaded.workspace, &def_type, &names, &here)
                .map_err(|error| error.message())
                .unwrap()
                .iter()
                .map(Target::qualified)
                .collect()
        })
        .unwrap()
        .join()
        .unwrap()
}

/// Builds the targets `names` select of the workspace at `root`, as
/// `adjust` changes the settings: what each came to, its error as shown.
fn build_all(
    root: &Path,
    state: &Path,
    names: &[&str],
    adjust: fn(&mut Settings),
) -> (Vec<Result<Plain, String>>, (usize, usize)) {
    let root = root.to_path_buf();
    let state = state.to_path_buf();
    let names: Vec<String> = names.iter().map(|name| (*name).to_owned()).collect();
    std::thread::Builder::new()
        .stack_size(gcsl::STACK_SIZE)
        .spawn(move || {
            let mut loaded = load(&root).unwrap();
            let names: Vec<&str> = names.iter().map(String::as_str).collect();
            let targets = select(&loaded.workspace, "build", &names, "").unwrap();
            let mut settings = settings(&root, &state, ("x86_64", "dev"));
            adjust(&mut settings);
            let Outcomes {
                targets: outcomes,
                ran,
                cached,
                ..
            } = build_all_observed(&mut loaded, &targets, &settings, &mut |_| {}).unwrap();
            let shown = outcomes
                .into_iter()
                .map(|outcome| outcome.map_err(|error| error.render(&loaded)))
                .collect();
            (shown, (ran, cached))
        })
        .unwrap()
        .join()
        .unwrap()
}

#[test]
fn names_and_globs_select_targets_once_each_sorted() {
    let dir = workspace(&[
        (
            "a/build.gcsl",
            "def build one() { = 1 }\ndef build two() { = 2 }",
        ),
        ("a/b/build.gcsl", "def build three() { = 3 }"),
        ("c/build.gcsl", "def build four() { = 4 }"),
        ("build.gcsl", "def build root() { = 0 }"),
    ]);
    let root = dir.path();
    assert_eq!(
        selected(root, "build", &["**"], ""),
        ["/root", "a/b/three", "a/one", "a/two", "c/four"]
    );
    assert_eq!(
        selected(root, "build", &["*"], ""),
        ["a/one", "a/two", "c/four"]
    );
    assert_eq!(
        selected(root, "build", &["a/**"], ""),
        ["a/b/three", "a/one", "a/two"]
    );
    // From a directory, and its own alone; a name and a glob, once each.
    assert_eq!(selected(root, "build", &["."], "a"), ["a/one", "a/two"]);
    assert_eq!(
        selected(root, "build", &["one", "a/*", "**/b"], ""),
        ["a/b/three", "a/one"]
    );
    // A glob that selects nothing is nothing, even of a def-type nothing
    // defines yet.
    assert_eq!(
        selected(root, "build", &["nowhere/*"], ""),
        Vec::<String>::new()
    );
    assert_eq!(selected(root, "package", &["**"], ""), Vec::<String>::new());
}

#[test]
fn several_targets_are_one_build_their_shared_step_run_once() {
    let text = format!(
        "{SHOUT}def build first() {{ out = shout(file(\"name.txt\")) }}\n\
         def build second() {{ out = shout(file(\"name.txt\")) }}\n\
         def build broken() {{ = panic(\"no\") }}\n"
    );
    let dir = workspace(&[("app/build.gcsl", &text), ("app/name.txt", "n\n")]);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, counts) = build_all(
        dir.path(),
        state.path(),
        &["first", "second", "broken"],
        |_| {},
    );
    // `broken` fails alone; `first` and `second` share one step, of
    // `first`, which met it first.
    assert_eq!(counts, (1, 0));
    // In the order `select` gives, by qualified name.
    let [broken, first, second] = &outcomes[..] else {
        panic!("{outcomes:?}");
    };
    assert_eq!(first, second);
    let shown = format!("{first:?}");
    assert!(
        shown.contains("/pekit-out/app/first/shout/out.txt"),
        "{shown}"
    );
    let error = broken.as_ref().unwrap_err();
    assert!(error.contains("error: no"), "{error}");
    let out = dir.path().join("pekit-out/app/first/shout/out.txt");
    assert_eq!(fs::read_to_string(out).unwrap(), "N\n");
}

#[test]
fn no_dynamic_fails_a_target_that_waits_for_a_step() {
    let text = format!(
        "{SHOUT}def build waits() {{ = shout(file(\"name.txt\")).read() }}\n\
         def build static() {{ = shout(file(\"name.txt\")) }}\n"
    );
    let dir = workspace(&[("app/build.gcsl", &text), ("app/name.txt", "n\n")]);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, _) = build_all(dir.path(), state.path(), &["waits", "static"], |settings| {
        settings.no_dynamic = true;
    });
    // `static`, then `waits`, by qualified name.
    assert!(outcomes[0].is_ok(), "{outcomes:?}");
    let error = outcomes[1].as_ref().unwrap_err();
    assert!(
        error.contains("the target waits here for a step's result, a dynamic dependency"),
        "{error}"
    );
    // Without it, the dynamic one builds.
    let (outcomes, _) = build_all(dir.path(), state.path(), &["waits"], |_| {});
    assert_eq!(outcomes[0], Ok(Plain::Str("N\n".to_owned())));
}

#[test]
fn platform_sets_what_is_built_for_and_rebuild_reruns_every_step() {
    let text = format!(
        "{SHOUT}def build arch() {{ = ^platform.arch }}\n\
         def build out() {{ = shout(file(\"name.txt\")) }}\n"
    );
    let dir = workspace(&[("app/build.gcsl", &text), ("app/name.txt", "n\n")]);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, _) = build_all(dir.path(), state.path(), &["arch"], |_| {});
    assert_eq!(outcomes[0], Ok(Plain::Str("x86_64".to_owned())));
    let (outcomes, _) = build_all(dir.path(), state.path(), &["arch"], |settings| {
        settings.building_for = Some(ExecutionPlatform {
            arch: "aarch64".to_owned(),
            os: "linux".to_owned(),
        });
    });
    assert_eq!(outcomes[0], Ok(Plain::Str("aarch64".to_owned())));
    // Built, found, and rebuilt.
    let (_, counts) = build_all(dir.path(), state.path(), &["out"], |_| {});
    assert_eq!(counts, (1, 0));
    let (_, counts) = build_all(dir.path(), state.path(), &["out"], |_| {});
    assert_eq!(counts, (0, 1));
    let (_, counts) = build_all(dir.path(), state.path(), &["out"], |settings| {
        settings.reuse = Reuse::Rerun;
    });
    assert_eq!(counts, (1, 0));
}

#[test]
fn a_target_built_alone_is_what_it_is_among_others() {
    let text = format!(
        "{SHOUT}def build first() {{ out = shout(file(\"name.txt\")) }}\n\
         def build second() {{ = 2 }}\n"
    );
    let dir = workspace(&[("app/build.gcsl", &text), ("app/name.txt", "n\n")]);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, _) = build_all(dir.path(), state.path(), &["first", "second"], |_| {});
    let alone = build_in(dir.path(), state.path(), "first").unwrap();
    assert_eq!(outcomes[0], Ok(alone.value.clone()));
    // Found in the store the second time, as any build's are.
    assert_eq!((alone.ran, alone.cached), (0, 1));
    let (again, events) = observed_with(dir.path(), state.path(), "second", Options::default());
    assert_eq!(again.unwrap().value, Plain::Int(2));
    assert!(
        events
            .iter()
            .all(|event| !format!("{event:?}").contains("Started"))
    );
}
