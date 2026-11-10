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
fn a_target_that_fails_in_a_guard_still_ends_a_shared_call_another_waits_on() {
    // `first`'s loop splits its first iteration off inside the memoised
    // `shared`, which waits for a slow step, and `second` waits for
    // `first`'s unit to end that call. Then `first`'s second guard fails.
    // `first` fails only once its first iteration has ended `shared`, so
    // `second` finds its value, rather than waiting for a unit that will
    // never run again.
    let dir = workspace(&[(
        "app/build.gcsl",
        "#[hermetic] fn made(n: Int, pause: String) -> File {\n\
         \x20   let r = exec([\"/bin/sh\", \"-c\", \"sleep {pause}; echo {n} > out\"])\n\
         \x20   = working_dir().file(\"out\")\n\
         }\n\
         #[hermetic] fn broken() -> File {\n\
         \x20   let r = exec([\"/bin/sh\", \"-c\", \"sleep 0.05; exit 3\"])\n\
         \x20   = working_dir().file(\"out\")\n\
         }\n\
         fn shared() ->> String { = made(7, \"1\").read() }\n\
         fn keep(n: Int) -> Bool { = if n == 2 { broken().read() != \"\" } else { true } }\n\
         def build first() { = for n in [1, 2] if keep(n) { = shared() } }\n\
         def build second() { = shared() }\n",
    )]);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, _) = build_all(dir.path(), state.path(), &["first", "second"], |_| {});
    assert!(outcomes[0].is_err(), "{outcomes:?}");
    assert_eq!(
        outcomes[1],
        Ok(Plain::Str("7\n".to_owned())),
        "{outcomes:?}"
    );
}

/// Two targets, `first` and `second`, that wait for each other if `said`,
/// the body of a function of `word` that reads `/w/app/{word}`, is held to
/// program order (PEI-1671). `second`'s arguments are evaluated at once:
/// its later `inner()` runs while its earlier `outer()` waits for `first`,
/// which is in `outer()` and needs `inner()`. `a` holds `a`, and `b` `b`.
fn crossed(said: &str) -> tempfile::TempDir {
    workspace(&[
        ("app/a", "a"),
        ("app/b", "b"),
        (
            "app/build.gcsl",
            &format!(
                "import fs\n\
                 fn said(word: String) -> String {{ {said} }}\n\
                 fn inner() ->> String {{ = said(\"b\") }}\n\
                 fn outer() ->> String {{\n\
                 \x20   let a = said(\"a\")\n\
                 \x20   = \"{{a}}{{inner()}}\"\n\
                 }}\n\
                 fn both(first: String, second: String) -> String {{ = \"{{first}}+{{second}}\" }}\n\
                 def build first() {{ = outer() }}\n\
                 def build second() {{ = both(outer(), inner()) }}\n"
            ),
        ),
    ])
}

/// What `first` and `second` of [`crossed`] come to in a strict build.
fn crossed_strictly(said: &str) -> Vec<Result<Plain, String>> {
    let dir = crossed(said);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, _) = build_all(dir.path(), state.path(), &["first", "second"], |settings| {
        settings.strict = true;
    });
    outcomes
}

#[test]
fn a_strict_build_s_fs_exists_does_not_wait_for_program_order() {
    // A strict target writes nothing, so its `fs.exists` need not wait.
    let outcomes = crossed_strictly("= if fs.exists(\"/w/app/{word}\") { word } else { \"-\" }");
    let text = |text: &str| Ok(Plain::Str(text.to_owned()));
    assert_eq!(outcomes, [text("ab"), text("ab+b")]);
}

#[test]
fn a_strict_build_s_fs_read_does_not_wait_for_program_order() {
    // A strict target writes nothing, so its `fs.read` need not wait.
    let outcomes = crossed_strictly("= fs.read(\"/w/app/{word}\")");
    let text = |text: &str| Ok(Plain::Str(text.to_owned()));
    assert_eq!(outcomes, [text("ab"), text("ab+b")]);
}

#[test]
fn fs_exists_outside_a_strict_build_sees_the_target_s_earlier_writes() {
    // A call's arguments are evaluated at once, so only program order
    // makes the second's `fs.exists` wait for the file the first's program
    // makes.
    let dir = workspace(&[(
        "app/build.gcsl",
        "import fs\n\
         fn wrote() -> String {\n\
         \x20   let r = exec([\"/bin/sh\", \"-c\", \"sleep 1; touch /w/app/made\"])\n\
         \x20   = \"wrote\"\n\
         }\n\
         fn seen(wrote: String, exists: Bool) -> Bool { = exists }\n\
         def build made() { = seen(wrote(), fs.exists(\"/w/app/made\")) }\n",
    )]);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, _) = build_all(dir.path(), state.path(), &["made"], |_| {});
    assert_eq!(outcomes, [Ok(Plain::Bool(true))]);
}

#[test]
fn fs_read_outside_a_strict_build_sees_the_target_s_earlier_writes() {
    // As for `fs.exists`: only program order makes the second argument's
    // `fs.read` wait for what the first's program writes.
    let dir = workspace(&[
        ("app/made", "old"),
        (
            "app/build.gcsl",
            "import fs\n\
             fn wrote() -> String {\n\
             \x20   let r = exec([\"/bin/sh\", \"-c\", \"sleep 1; printf new > /w/app/made\"])\n\
             \x20   = \"wrote\"\n\
             }\n\
             fn seen(wrote: String, text: String) -> String { = text }\n\
             def build made() { = seen(wrote(), fs.read(\"/w/app/made\")) }\n",
        ),
    ]);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, _) = build_all(dir.path(), state.path(), &["made"], |_| {});
    assert_eq!(outcomes, [Ok(Plain::Str("new".to_owned()))]);
}

#[test]
fn a_strict_build_leaves_a_relative_fs_path_to_gcsl() {
    // GCSL's `fs` built-ins take only absolute paths, and say so.
    let outcomes = crossed_strictly("= if fs.exists(word) { word } else { \"-\" }");
    let Err(error) = &outcomes[0] else {
        panic!("{outcomes:?}");
    };
    assert!(
        error.contains("needs an absolute path, found `a`"),
        "{error}"
    );
    assert!(!error.contains("a strict build"), "{error}");
}

#[test]
fn a_strict_build_judges_a_path_once_its_dots_are_taken() {
    // `..` out of the workspace is outside it; `..` that stays in is in.
    let outside = crossed_strictly("= if fs.exists(\"/w/../etc\") { word } else { \"-\" }");
    let Err(error) = &outside[0] else {
        panic!("{outside:?}");
    };
    assert!(
        error.contains(
            "a strict build reads only the workspace and the build's outputs, and this reads `/etc`"
        ),
        "{error}"
    );
    let inside = crossed_strictly("= fs.read(\"/w/app/../app/./{word}\")");
    let text = |text: &str| Ok(Plain::Str(text.to_owned()));
    assert_eq!(inside, [text("ab"), text("ab+b")]);
}

/// Two memoised calls, `outer` calling `inner`, whose programs the targets
/// run themselves, as effects pekit does not isolate. `second`'s arguments
/// are pieces: its first waits for `first`'s run of `outer`, and its
/// second begins `inner`, whose program waits its turn behind the first.
const CROSSED: &str = "fn said(word: String) -> String {\n\
                       \x20   = exec([\"/bin/sh\", \"-c\", \"echo {word}\"]).stdout.trim()\n\
                       }\n\
                       fn inner() ->> String { = said(\"b\") }\n\
                       fn outer() ->> String {\n\
                       \x20   let a = said(\"a\")\n\
                       \x20   = \"{a}{inner()}\"\n\
                       }\n\
                       fn both(first: String, second: String) -> String { = \"{first}+{second}\" }\n\
                       def build first() { = outer() }\n\
                       def build second() { = both(outer(), inner()) }\n";

#[test]
fn targets_that_cross_over_shared_calls_both_finish() {
    // `first` begins `outer`; `second` waits for it, its later piece
    // having begun `inner`; then `first` needs `inner`. `second`'s run of
    // `inner`, which `first` waits for, no longer waits for `second`'s
    // earlier piece, rather than the two waiting for each other.
    let dir = workspace(&[("app/build.gcsl", CROSSED)]);
    let state = tempfile::tempdir().unwrap();
    let (outcomes, _) = build_all(dir.path(), state.path(), &["first", "second"], |_| {});
    assert_eq!(
        outcomes,
        [
            Ok(Plain::Str("ab".to_owned())),
            Ok(Plain::Str("ab+b".to_owned()))
        ]
    );
}

#[test]
fn targets_whose_shared_calls_need_each_other_report_the_call_that_needs_its_own_result() {
    // Each target runs one of two memoised calls that need each other.
    let dir = workspace(&[(
        "app/build.gcsl",
        "fn said(word: String) -> String {\n\
         \x20   = exec([\"/bin/sh\", \"-c\", \"echo {word}\"]).stdout.trim()\n\
         }\n\
         fn e() ->> String {\n\
         \x20   let a = said(\"e\")\n\
         \x20   = \"{a}{f()}\"\n\
         }\n\
         fn f() ->> String {\n\
         \x20   let b = said(\"f\")\n\
         \x20   = \"{b}{e()}\"\n\
         }\n\
         def build first() { = e() }\n\
         def build second() { = f() }\n",
    )]);
    let state = tempfile::tempdir().unwrap();
    let root = dir.path().to_path_buf();
    let state = state.path().to_path_buf();
    let shown = std::thread::Builder::new()
        .stack_size(gcsl::STACK_SIZE)
        .spawn(move || {
            let mut loaded = load(&root).unwrap();
            let targets = select(&loaded.workspace, "build", &["first", "second"], "").unwrap();
            let settings = settings(&root, &state, ("x86_64", "dev"));
            match build_all_observed(&mut loaded, &targets, &settings, &mut |_| {}) {
                Ok(outcomes) => panic!("{:?}", outcomes.targets),
                Err(error) => error.render(&loaded),
            }
        })
        .unwrap()
        .join()
        .unwrap();
    assert!(
        shown.contains("this call depends on its own result"),
        "{shown}"
    );
    assert!(
        shown.contains("`app/first` needs `f` here, which `app/second` is running"),
        "{shown}"
    );
    assert!(
        shown.contains("`app/second` needs `e` here, which `app/first` is running"),
        "{shown}"
    );
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
fn steps_given_two_contexts_steps_results_stay_apart_in_either_order() {
    // Each `after` is made where `case` is not bound, and given a `loud`
    // made in its own `case`: the two `after` have one identity and one
    // context, and differ only in what their `loud` gives them.
    let text = |swapped: bool| {
        let (up, down) = (
            "up = after(loud(file(\"name.txt\")))",
            "down = after(loud(file(\"name.txt\")) with { case = \"lower\" })",
        );
        let (first, second) = if swapped { (down, up) } else { (up, down) };
        format!(
            "context case: String = \"upper\"\n\
             #[hermetic] fn loud(src: File) -> File {{\n\
             \x20   let how = if ^case == \"upper\" {{ \"a-z A-Z\" }} else {{ \"A-Z a-z\" }}\n\
             \x20   let r = exec([\"/bin/sh\", \"-c\", \"tr {{how}} < {{src.path}} > out.txt\"])\n\
             \x20   working_dir().file(\"out.txt\")\n\
             }}\n\
             #[hermetic] fn after(src: File) -> File {{\n\
             \x20   let r = exec([\"/bin/sh\", \"-c\", \"cat {{src.path}} > out.txt; echo after >> out.txt\"])\n\
             \x20   working_dir().file(\"out.txt\")\n\
             }}\n\
             def build both() {{\n\
             \x20   {first}\n\
             \x20   {second}\n\
             }}\n"
        )
    };
    let dir = workspace(&[
        ("app/build.gcsl", &text(false)),
        ("app/name.txt", "Mixed\n"),
    ]);
    let state = tempfile::tempdir().unwrap();
    let read = |at: &str| fs::read_to_string(dir.path().join(at)).unwrap();
    let afters = || {
        let mut found = [
            read("pekit-out/app/both/after/out.txt"),
            read("pekit-out/app/both/after.2/out.txt"),
        ];
        found.sort();
        found
    };
    let (_, counts) = build_all(dir.path(), state.path(), &["both"], |_| {});
    assert_eq!(counts, (4, 0));
    assert_eq!(afters(), ["MIXED\nafter\n", "mixed\nafter\n"]);
    let (_, counts) = build_all(dir.path(), state.path(), &["both"], |_| {});
    assert_eq!(counts, (0, 4));
    // Met the other way round, each `after` still takes its own `loud`.
    write(dir.path(), &[("app/build.gcsl", &text(true))]);
    let fresh = tempfile::tempdir().unwrap();
    let (_, counts) = build_all(dir.path(), fresh.path(), &["both"], |_| {});
    assert_eq!(counts, (4, 0));
    assert_eq!(afters(), ["MIXED\nafter\n", "mixed\nafter\n"]);
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

#[test]
fn a_listing_of_the_workspace_leaves_out_its_history_and_nested_checkouts() {
    use std::os::unix::fs::PermissionsExt;
    let listed = "def build listed() { = (for f in dir(\".\").files() { = f.path }).join(\"\\n\") }\n\
                  def build nested() { = (for f in dir(\"worktree\").files() { = f.path }).join(\"\\n\") }\n";
    let dir = workspace(&[
        ("build.gcsl", listed),
        ("kept/a.txt", "a\n"),
        (".git/HEAD", "ref: refs/heads/main\n"),
        // A worktree, whose `.git` is a file, and a checkout, whose
        // `.git` is a directory.
        ("worktree/.git", "gitdir: /elsewhere\n"),
        ("worktree/b.txt", "b\n"),
        ("deep/checkout/.git/HEAD", "ref: refs/heads/main\n"),
        ("deep/checkout/c.txt", "c\n"),
        // Kept from the sources pekit loads, so that only a listing could
        // meet `locked`.
        (".gcslignore", "deep/checkout\n"),
    ]);
    // Never descended into: a listing that tried would fail on it.
    let locked = dir.path().join("deep/checkout/locked");
    fs::create_dir(&locked).unwrap();
    fs::set_permissions(&locked, fs::Permissions::from_mode(0o000)).unwrap();
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "listed");
    fs::set_permissions(&locked, fs::Permissions::from_mode(0o755)).unwrap();
    assert_eq!(
        built.unwrap().value,
        Plain::Str("/w/.gcslignore\n/w/build.gcsl\n/w/kept/a.txt\n/w/workspace.gcsl".to_owned())
    );
    // A nested checkout listed itself is listed whole.
    let nested = build_in(dir.path(), state.path(), "nested").unwrap();
    assert_eq!(
        nested.value,
        Plain::Str("/w/worktree/.git\n/w/worktree/b.txt".to_owned())
    );
}
