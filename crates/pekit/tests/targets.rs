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
                     \x20   working_dir().file(\"out.txt\")\n\
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
fn a_target_that_ends_a_shared_call_another_waits_on_is_progress() {
    // `second` begins the memoised `shared`, which waits for a slow step.
    // `first` needs it only after a quick step, so waits for `second`'s
    // unit; then, in the round `second` ends `shared`, `first` runs before
    // it and waits again, though nothing waits for anything any more.
    let dir = workspace(&[(
        "app/build.gcsl",
        "#[hermetic] fn made(n: Int, pause: String) -> File {\n\
         \x20   let r = exec([\"/bin/sh\", \"-c\", \"sleep {pause}; echo {n} > out\"])\n\
         \x20   = working_dir().file(\"out\")\n\
         }\n\
         fn shared() ->> String { = made(7, \"1\").read() }\n\
         def build first() { = if made(1, \"0.05\").read() != \"\" { shared() } else { \"\" } }\n\
         def build second() { = shared() }\n",
    )]);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, _) = build_all(dir.path(), state.path(), &["first", "second"], |_| {});
    let shared = Plain::Str("7\n".to_owned());
    assert_eq!(outcomes, [Ok(shared.clone()), Ok(shared)]);
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
        selected(root, "build", &["a/**"], ""),
        ["a/b/three", "a/one", "a/two"]
    );
    // A glob's last part is the name, as a qualified name's is: every one
    // of a directory, some of them, or every one a level below.
    assert_eq!(selected(root, "build", &["a/*"], ""), ["a/one", "a/two"]);
    assert_eq!(selected(root, "build", &["a/t*"], ""), ["a/two"]);
    assert_eq!(selected(root, "build", &["*"], ""), ["/root"]);
    assert_eq!(selected(root, "build", &["/*"], "a"), ["/root"]);
    assert_eq!(
        selected(root, "build", &["*/*"], ""),
        ["a/one", "a/two", "c/four"]
    );
    assert_eq!(selected(root, "build", &["**/th*"], ""), ["a/b/three"]);
    // From a directory, and its own alone; a name and a glob, once each.
    assert_eq!(selected(root, "build", &["."], "a"), ["a/one", "a/two"]);
    assert_eq!(selected(root, "build", &["*"], "a"), ["a/one", "a/two"]);
    assert_eq!(selected(root, "build", &["../c/*"], "a"), ["c/four"]);
    assert_eq!(
        selected(root, "build", &["one", "a/*", "a/b/."], ""),
        ["a/b/three", "a/one", "a/two"]
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
fn several_targets_are_one_build_a_build_they_share_built_once() {
    let text = format!(
        "{SHOUT}def build common() {{ = shout(file(\"name.txt\")) }}\n\
         def build first() {{ out = build common() }}\n\
         def build second() {{ out = build common() }}\n\
         def build own() {{ out = shout(file(\"name.txt\")) }}\n\
         def build broken() {{ = panic(\"no\") }}\n"
    );
    let dir = workspace(&[("app/build.gcsl", &text), ("app/name.txt", "n\n")]);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, counts) = build_all(
        dir.path(),
        state.path(),
        &["first", "second", "own", "broken"],
        |_| {},
    );
    // `broken` fails alone; `first` and `second` share `common`'s step, and
    // `own`, which makes the same call itself, shares it too: a step is
    // what it is, whichever definition makes it.
    assert_eq!(counts, (1, 0));
    // In the order `select` gives, by qualified name.
    let [broken, first, own, second] = &outcomes[..] else {
        panic!("{outcomes:?}");
    };
    assert_eq!(first, second);
    let shown = format!("{first:?} {own:?}");
    let step = shown
        .split("/pekit-out/.steps/shout/")
        .nth(1)
        .and_then(|rest| rest.split('/').next())
        .unwrap_or_else(|| panic!("{shown}"));
    assert_eq!(
        shown
            .matches(&format!("/pekit-out/.steps/shout/{step}/out.txt"))
            .count(),
        2,
        "{shown}"
    );
    let error = broken.as_ref().unwrap_err();
    assert!(error.contains("error: no"), "{error}");
    // Each target's view of it, under its own name.
    for target in ["first", "second", "own"] {
        let out = dir
            .path()
            .join(format!("pekit-out/app/{target}/shout/out.txt"));
        assert_eq!(fs::read_to_string(out).unwrap(), "N\n");
    }
    // `common` built alone is the step its callers built.
    let (_, counts) = build_all(dir.path(), state.path(), &["common"], |_| {});
    assert_eq!(counts, (0, 1));
}

#[test]
fn one_call_made_in_two_contexts_is_two_steps_at_two_paths() {
    // `loud` reads the context `case`: made in each of its values, the same
    // call is two steps, which the contexts alone tell apart.
    let text = "context case: String = \"upper\"\n\
                #[hermetic] fn loud(src: File) -> File {\n\
                \x20   let how = if ^case == \"upper\" { \"a-z A-Z\" } else { \"A-Z a-z\" }\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"tr {how} < {src.path} > out.txt\"])\n\
                \x20   working_dir().file(\"out.txt\")\n\
                }\n\
                def build both() {\n\
                \x20   up = loud(file(\"name.txt\"))\n\
                \x20   down = loud(file(\"name.txt\")) with { case = \"lower\" }\n\
                }\n";
    let dir = workspace(&[("app/build.gcsl", text), ("app/name.txt", "Mixed\n")]);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, counts) = build_all(dir.path(), state.path(), &["both"], |_| {});
    assert_eq!(counts, (2, 0));
    let shown = format!("{:?}", outcomes[0]);
    let paths: Vec<&str> = shown
        .split("path: \"")
        .skip(1)
        .filter_map(|rest| rest.split('"').next())
        .collect();
    assert_eq!(paths.len(), 2, "{shown}");
    assert_ne!(paths[0], paths[1]);
    assert!(
        paths
            .iter()
            .all(|path| path.starts_with("/pekit-out/.steps/loud/")),
        "{shown}"
    );
    // Each where its target's view puts it, the second step of the function
    // after the first.
    let read = |at: &str| fs::read_to_string(dir.path().join(at)).unwrap();
    let mut views = [
        read("pekit-out/app/both/loud/out.txt"),
        read("pekit-out/app/both/loud.2/out.txt"),
    ];
    views.sort();
    assert_eq!(views, ["MIXED\n", "mixed\n"]);
    // Built again, each is found where it was.
    let (again, counts) = build_all(dir.path(), state.path(), &["both"], |_| {});
    assert_eq!(counts, (0, 2));
    assert_eq!(format!("{:?}", again[0]), shown);
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

#[test]
fn a_definition_built_for_another_reads_its_own_directory_s_files() {
    let lib = format!("{SHOUT}def build lib() {{ = shout(file(\"lib.txt\")).read() }}\n");
    let dir = workspace(&[
        ("build.gcsl", &lib),
        ("lib.txt", "from lib\n"),
        ("app/build.gcsl", "def build app() { = build lib() }\n"),
        ("app/lib.txt", "from app\n"),
    ]);
    let state = tempfile::tempdir().unwrap();
    let alone = build_in(dir.path(), state.path(), "lib").unwrap();
    assert_eq!(alone.value, Plain::Str("FROM LIB\n".to_owned()));
    // Built for `app`, in another directory, `lib` reads its own file, so
    // its step is the one it was, found in the store; and the step's own
    // `out.txt` is in the step's directory, not the definition's.
    let through = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!(through.value, Plain::Str("FROM LIB\n".to_owned()));
    assert_eq!((through.ran, through.cached), (0, 1));
}
