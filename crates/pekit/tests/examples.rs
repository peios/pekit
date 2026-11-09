//! Tests of the examples, runnable: each fixture under `tests/fixtures/examples`
//! built with the gcc library of `gcslib`, with the host's own toolchain and
//! with Bootlin's pinned ones.

mod building;

use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

use gcsl::eval::plain::Plain;
use pekit::events::EventKind;
use pekit::exposure::Exposing;
use pekit::store::Store;

use building::{Options, build_in, observed_with};

/// The repository's root.
fn repository() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../..")
}

/// Copies the tree at `from` to `to`, its files' contents alone, but a
/// git repository's `.git`.
fn copy_tree(from: &Path, to: &Path) {
    fs::create_dir_all(to).unwrap();
    for entry in fs::read_dir(from).unwrap() {
        let entry = entry.unwrap();
        if entry.file_name() == ".git" {
            continue;
        }
        let at = to.join(entry.file_name());
        if entry.file_type().unwrap().is_dir() {
            copy_tree(&entry.path(), &at);
        } else {
            fs::copy(entry.path(), at).unwrap();
        }
    }
}

/// The gcslib libraries published each in a repository of its own.
const PUBLISHED: [&str; 5] = ["buildenv", "clang", "gcc", "python", "rust"];

/// Copies every gcslib library to `to`, a directory of libraries, each at
/// its path: those not yet published from this repository's `gcslib`, and
/// each published one from its clone in `$PEKIT_TEST_GCSLIB`, else the
/// Peios workspace's `gcslib`.
fn libraries(to: &Path) {
    copy_tree(&repository().join("gcslib"), to);
    let clones = std::env::var_os("PEKIT_TEST_GCSLIB")
        .map_or_else(|| repository().join("../gcslib"), PathBuf::from);
    for name in PUBLISHED {
        let from = clones.join(name);
        assert!(
            from.join("README.md").is_file(),
            "the gcslib library `{name}` is not at `{}`: clone \
             https://github.com/gcslib/{name} there, or set $PEKIT_TEST_GCSLIB",
            from.display()
        );
        copy_tree(&from, &to.join("github.com/gcslib").join(name));
    }
}

/// A workspace of example `n`'s fixture, in its directory `example`, the
/// gcslib libraries among its libraries, its `workspace.gcsl` of `config`.
fn example(n: u32, config: &str) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    let fixture = repository().join(format!("tests/fixtures/examples/{n}"));
    copy_tree(&fixture, &dir.path().join("example"));
    libraries(&dir.path().join(".gcsl/libraries"));
    fs::write(dir.path().join("workspace.gcsl"), config).unwrap();
    dir
}

/// The path of the file `value` is, or of the one its field `field` is.
fn file_of<'a>(value: &'a Plain, field: Option<&str>) -> &'a str {
    let value = match (value, field) {
        (Plain::Record(fields), Some(field)) => fields
            .iter()
            .find_map(|(name, value)| (name == field).then_some(value))
            .unwrap_or_else(|| panic!("no `{field}` in {value:?}")),
        _ => value,
    };
    let Plain::File { path, .. } = value else {
        panic!("not a file: {value:?}");
    };
    path
}

/// The file at the logical path `path` of a build of the workspace `root`.
fn built_file(root: &Path, path: &str) -> PathBuf {
    root.join(path.trim_start_matches('/'))
}

/// What the program at the logical path `path` of a build of the workspace
/// `root` prints.
fn run(root: &Path, path: &str) -> String {
    let ran = Command::new(built_file(root, path)).output().unwrap();
    assert!(ran.status.success(), "{ran:?}");
    String::from_utf8(ran.stdout).unwrap()
}

#[test]
fn example_1_builds_one_program_with_the_host_s_toolchain() {
    let dir = example(1, "");
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((built.ran, built.cached), (2, 0));
    assert_eq!(
        run(dir.path(), file_of(&built.value, None)),
        "Hello from pekit2\n"
    );
}

#[test]
fn example_2_reruns_what_a_change_reaches_and_no_more() {
    let dir = example(2, "");
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!((built.ran, built.cached), (4, 0));
    assert_eq!(
        run(dir.path(), file_of(&built.value, None)),
        "Hello, pekit2\n"
    );
    let edit = |path: &str, from: &str, to: &str| {
        let path = dir.path().join("example").join(path);
        let text = fs::read_to_string(&path).unwrap();
        assert!(text.contains(from), "{text}");
        fs::write(&path, text.replacen(from, to, 1)).unwrap();
    };
    // `main.c`: its compile and the link, not the library's.
    edit("main.c", "\"pekit2\"", "\"you\"");
    let again = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!((again.ran, again.cached), (2, 2));
    assert_eq!(run(dir.path(), file_of(&again.value, None)), "Hello, you\n");
    // The private header: the library's compile, its archive and the link;
    // never the program's compile, which is not given it.
    edit("lib/internal.h", "\"Hello\"", "\"Hi\"");
    let again = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!((again.ran, again.cached), (3, 1));
    assert_eq!(run(dir.path(), file_of(&again.value, None)), "Hi, you\n");
    // A comment in the library's code: its compile alone, whose object is
    // the same, so nothing after it runs.
    edit(
        "lib/greet.c",
        "#include <stdio.h>",
        "#include <stdio.h>\n/* a comment */",
    );
    let again = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!((again.ran, again.cached), (1, 3));
    // The public header, which both compiles are given: both run, and their
    // objects are the same.
    edit(
        "lib/greet.h",
        "#define GREET_H",
        "#define GREET_H\n/* a comment */",
    );
    let again = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!((again.ran, again.cached), (2, 2));
}

#[test]
fn example_3_runs_a_tool_the_build_made() {
    let dir = example(3, "");
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "app").unwrap();
    // The tool's compile and link, its run, and the program's two compiles
    // and link.
    assert_eq!((built.ran, built.cached), (6, 0));
    assert_eq!(
        run(dir.path(), file_of(&built.value, None)),
        "Hello from a file that was turned into code during the build.\n"
    );
}

/// What example 4's program prints.
const ZLIB: &str = "zlib 1.3.1: 56 bytes -> 19 bytes -> \"pekit2 pekit2 pekit2 pekit2 pekit2 \
                    pekit2 pekit2 pekit2\"\n";

#[test]
fn configure_make_gives_each_word_as_one_argument() {
    use std::os::unix::fs::PermissionsExt as _;
    let dir = example(4, HOST);
    // A build system that records the arguments its `configure` was given.
    let configure = dir.path().join("example/args/configure");
    fs::create_dir_all(configure.parent().unwrap()).unwrap();
    fs::write(
        &configure,
        "#!/bin/sh\n\
         for a in \"$@\"; do printf '%s\\n' \"$a\"; done > args.txt\n\
         prefix=${1#--prefix=}\n\
         printf 'all:\\n\\ninstall:\\n\\tmkdir -p %s && cp args.txt %s/\\n' \"$prefix\" \"$prefix\" > Makefile\n",
    )
    .unwrap();
    fs::set_permissions(&configure, fs::Permissions::from_mode(0o755)).unwrap();
    fs::write(
        dir.path().join("example/args.gcsl"),
        "import \"github.com/gcslib/buildenv\"\n\
         def build args() {\n\
         \x20   = buildenv.configure_make(dir(\"args\"), configure = [\"--with=a b\", \"it's\", \"$HOME\"])\n\
         }\n",
    )
    .unwrap();
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "args").unwrap();
    let Plain::Dir { path, .. } = &built.value else {
        panic!("not a directory: {:?}", built.value);
    };
    let args = fs::read_to_string(built_file(dir.path(), &format!("{path}/args.txt"))).unwrap();
    let words: Vec<&str> = args.lines().skip(1).collect();
    assert_eq!(words, ["--with=a b", "it's", "$HOME"]);
}

#[test]
fn configure_make_gives_every_file_of_a_source_one_time() {
    use std::os::unix::fs::PermissionsExt as _;
    let dir = example(4, HOST);
    // A build system that records how many times its source's files have,
    // to the nanosecond, but the one it writes: a copy gives each its own.
    let src = dir.path().join("example/times");
    for n in 0..64 {
        fs::create_dir_all(src.join(format!("d{}", n % 4))).unwrap();
        fs::write(src.join(format!("d{}/f{n}", n % 4)), format!("{n}\n")).unwrap();
    }
    let configure = src.join("configure");
    fs::write(
        &configure,
        "#!/bin/sh\n\
         find . -type f ! -name times.txt -printf '%T@\\n' | sort -u | wc -l > times.txt\n\
         prefix=${1#--prefix=}\n\
         printf 'all:\\n\\ninstall:\\n\\tmkdir -p %s && cp times.txt %s/\\n' \"$prefix\" \"$prefix\" > Makefile\n",
    )
    .unwrap();
    fs::set_permissions(&configure, fs::Permissions::from_mode(0o755)).unwrap();
    fs::write(
        dir.path().join("example/times.gcsl"),
        "import \"github.com/gcslib/buildenv\"\n\
         def build times() { = buildenv.configure_make(dir(\"times\")) }\n",
    )
    .unwrap();
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "times").unwrap();
    let Plain::Dir { path, .. } = &built.value else {
        panic!("not a directory: {:?}", built.value);
    };
    let times = fs::read_to_string(built_file(dir.path(), &format!("{path}/times.txt"))).unwrap();
    assert_eq!(times.trim(), "1");
}

#[test]
fn example_4_builds_zlib_with_its_own_build_system_and_the_host_s_tools() {
    let Some(state) = seeded(&["zlib-1.3.1.tar.gz"]) else {
        eprintln!("skipped: zlib's source is not on this machine");
        return;
    };
    let dir = example(4, HOST);
    let built = build_in(dir.path(), state.path(), "app").unwrap();
    // zlib's build, the program's compile, and the link.
    assert_eq!((built.ran, built.cached), (3, 0));
    assert_eq!(run(dir.path(), file_of(&built.value, None)), ZLIB);
    // The program changed: zlib's build is found in the store.
    let main = dir.path().join("example/main.c");
    let text = fs::read_to_string(&main).unwrap();
    fs::write(&main, text.replacen("pekit2", "pekit3", 1)).unwrap();
    let again = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!((again.ran, again.cached), (2, 1));
}

/// The workspace configuration of example 5: the host's C toolchain and
/// the ppk tool at `tools/ppk`, and ppk its packager.
const PPK: &str = "import * from \"github.com/gcslib/ppk\"\n\
                   import * from toolchain\n\
                   import fileroot\n\
                   import path\n\
                   def Toolchain::Ppk workspace_ppk() {\n\
                   \x20   let tools = dir(path.join(fileroot.this, \"tools\"))\n\
                   \x20   host_platform = ^host_platform\n\
                   \x20   platform = {}\n\
                   \x20   provides = {root = tools; runtime = dir(\"/\"); ppk = tools.file(\"ppk\")}\n\
                   }\n\
                   pekitconf = {\n\
                   \x20   toolchains = [Toolchain::C system_c, Toolchain::Ppk workspace_ppk]\n\
                   \x20   packagers = [packager()]\n\
                   }\n";

/// The `ppk` on this process's `PATH`, if there is one.
fn ppk_tool() -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    std::env::split_paths(&path)
        .map(|dir| dir.join("ppk"))
        .find(|candidate| candidate.is_file())
}

#[test]
fn example_5_packs_two_packages_and_repacks_what_a_change_reaches() {
    let Some(tool) = ppk_tool() else {
        eprintln!("skipped: no `ppk` on PATH");
        return;
    };
    let dir = example(5, PPK);
    fs::create_dir_all(dir.path().join("tools")).unwrap();
    fs::copy(tool, dir.path().join("tools/ppk")).unwrap();
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "packages").unwrap();
    // The library's compile and archive, the program's compile and link,
    // and the two packs.
    assert_eq!((built.ran, built.cached), (6, 0));
    let greet = built_file(dir.path(), file_of_dir(&built.value, "greet"));
    // The identity packing by hand gives. Example 5 records one, but ppk now
    // derives the program's requirements, such as its glibc floor, so the
    // identity depends on the C library it was linked against.
    let identity = packed_by_hand(dir.path(), state.path());
    assert!(greet.join(format!("manifests/sha256/{identity}")).is_file());
    assert!(greet.join("refs/dev.example.greet/1.0-1/x86_64").is_file());
    let dev = built_file(dir.path(), file_of_dir(&built.value, "greet_dev"));
    assert!(
        dev.join("refs/dev.example.greet-dev/1.0-1/x86_64")
            .is_file()
    );
    let edit = |path: &str, from: &str, to: &str| {
        let path = dir.path().join("example").join(path);
        let text = fs::read_to_string(&path).unwrap();
        assert!(text.contains(from), "{text}");
        fs::write(&path, text.replacen(from, to, 1)).unwrap();
    };
    // `main.c`: the program's compile and link, and its pack; not the
    // library's steps, nor the developers' package's pack.
    edit("main.c", "\"pekit2\"", "\"you\"");
    let again = build_in(dir.path(), state.path(), "packages").unwrap();
    assert_eq!((again.ran, again.cached), (3, 3));
    // The version alone: both packs, and nothing else.
    edit("package.gcsl", "\"1.0-1\"", "\"1.0-2\"");
    let again = build_in(dir.path(), state.path(), "packages").unwrap();
    assert_eq!((again.ran, again.cached), (2, 4));
    let greet = built_file(dir.path(), file_of_dir(&again.value, "greet"));
    assert!(greet.join("refs/dev.example.greet/1.0-2/x86_64").is_file());
}

#[test]
fn example_5_s_packages_are_packed_and_published_by_their_packager() {
    let Some(tool) = ppk_tool() else {
        eprintln!("skipped: no `ppk` on PATH");
        return;
    };
    let dir = example(5, PPK);
    fs::create_dir_all(dir.path().join("tools")).unwrap();
    fs::copy(tool, dir.path().join("tools/ppk")).unwrap();
    let cache = tempfile::tempdir().unwrap();
    // Every package, each packed by ppk.
    let (out, success) = pekit(dir.path(), cache.path(), &["package"]);
    assert!(success, "{out}");
    assert_eq!(
        out,
        "packed example/dev.example.greet with ppk\n\
         \x20 pekit-out/example/dev.example.greet/pack/artifacts\n\
         packed example/dev.example.greet-dev with ppk\n\
         \x20 pekit-out/example/dev.example.greet-dev/pack/artifacts\n\
         6 steps run, 0 found in the store\n"
    );
    let packed = dir
        .path()
        .join("pekit-out/example/dev.example.greet/pack/artifacts");
    assert!(packed.join("refs/dev.example.greet/1.0-1/x86_64").is_file());
    // Published into two stores, packed once: publishing is no step, so it
    // is done each time, and the packing is found.
    let stores = tempfile::tempdir().unwrap();
    for store in ["one", "two"] {
        let to = stores.path().join(store);
        let to = format!("--to={}", to.display());
        let (out, success) = pekit(
            dir.path(),
            cache.path(),
            &["publish", "dev.example.greet", &to],
        );
        assert!(success, "{out}");
        assert!(
            out.starts_with("published example/dev.example.greet with ppk\n")
                && out.ends_with("0 steps run, 5 found in the store\n"),
            "{out}"
        );
        let published = stores.path().join(store);
        assert!(
            published
                .join("refs/dev.example.greet/1.0-1/x86_64")
                .is_file()
        );
        assert!(!published.join("refs/dev.example.greet-dev").exists());
    }
}

/// Example 5 with `ppk` written in the developers' package's body before
/// its closing brace.
fn example_5_with_ppk(ppk: &str) -> Option<tempfile::TempDir> {
    let Some(tool) = ppk_tool() else {
        eprintln!("skipped: no `ppk` on PATH");
        return None;
    };
    let dir = example(5, PPK);
    fs::create_dir_all(dir.path().join("tools")).unwrap();
    fs::copy(tool, dir.path().join("tools/ppk")).unwrap();
    let path = dir.path().join("example/package.gcsl");
    let text = fs::read_to_string(&path).unwrap();
    let from = "\"lib/libgreet.a\" = g.archive}\n";
    assert!(text.contains(from), "{text}");
    fs::write(&path, text.replacen(from, &format!("{from}{ppk}\n"), 1)).unwrap();
    Some(dir)
}

#[test]
fn a_package_s_ppk_field_is_its_manifest_s_metadata() {
    let Some(dir) = example_5_with_ppk(
        "    ppk = {\n\
         \x20       description = \"The greet library, for developers\"\n\
         \x20       license_class = \"free\"\n\
         \x20       dependencies = {\"dev.example.greet\" = \"= 1.0-1\"; \"dev.example.any\" = \"\"}\n\
         \x20       requires = {\"libc.so.6\" = \"\"}\n\
         \x20       provides = {greeting = \"1\"}\n\
         \x20       claims = {\"/usr/lib/libgreet-current.a\" = \"libgreet.a\"}\n\
         \x20   }",
    ) else {
        return;
    };
    let cache = tempfile::tempdir().unwrap();
    let (out, success) = pekit(dir.path(), cache.path(), &["package"]);
    assert!(success, "{out}");
    let manifests = dir
        .path()
        .join("pekit-out/example/dev.example.greet-dev/pack/artifacts/manifests/sha256");
    let manifest = fs::read_dir(&manifests)
        .unwrap()
        .map(|entry| fs::read_to_string(entry.unwrap().path()).unwrap())
        .collect::<Vec<_>>();
    let [manifest] = &manifest[..] else {
        panic!("not one manifest: {manifest:?}");
    };
    for part in [
        "description = \"The greet library, for developers\"\n",
        "license_class = \"free\"\n",
        "[claims]\n\"/usr/lib/libgreet-current.a\" = \"libgreet.a\"\n",
        "[dependencies]\n\"dev.example.any\" = \"\"\n\"dev.example.greet\" = \"= 1.0-1\"\n",
        "[provides]\n\"greeting\" = \"1\"\n",
        "[requires]\n\"libc.so.6\" = \"\"\n",
    ] {
        assert!(manifest.contains(part), "no {part:?} in {manifest}");
    }
    // The program's package has no `ppk`, so it packs as `ppk pack` packs it
    // with no options.
    let greet = dir
        .path()
        .join("pekit-out/example/dev.example.greet/pack/artifacts");
    let identity = packed_by_hand(dir.path(), cache.path());
    assert!(greet.join(format!("manifests/sha256/{identity}")).is_file());
}

/// The identity of example 5's program packed by hand, as
/// `dev.example.greet@1.0-1`, with `tools/ppk` and no options.
fn packed_by_hand(dir: &Path, state: &Path) -> String {
    let built = build_in(dir, state, "app").unwrap();
    let files = tempfile::tempdir().unwrap();
    fs::create_dir_all(files.path().join("bin")).unwrap();
    fs::copy(
        built_file(dir, file_of(&built.value, None)),
        files.path().join("bin/greet"),
    )
    .unwrap();
    let artifacts = tempfile::tempdir().unwrap();
    let ran = Command::new(dir.join("tools/ppk"))
        .args(["pack", "dev.example.greet@1.0-1?arch=x86_64"])
        .arg(files.path())
        .arg("--artifact-dir")
        .arg(artifacts.path())
        .output()
        .unwrap();
    assert!(ran.status.success(), "{ran:?}");
    let out = String::from_utf8(ran.stdout).unwrap();
    out.trim().strip_prefix("sha256:").unwrap().to_owned()
}

#[test]
fn a_ppk_field_ppk_has_no_form_for_is_an_error_naming_it() {
    let Some(dir) =
        example_5_with_ppk("    ppk = {dependency = {\"dev.example.greet\" = \"= 1.0-1\"}}")
    else {
        return;
    };
    let cache = tempfile::tempdir().unwrap();
    let ran = Command::new(env!("CARGO_BIN_EXE_pekit"))
        .args(["package"])
        .current_dir(dir.path())
        .env("PEKIT_CACHE_DIR", cache.path())
        .env("PEKIT_STATE_DIR", cache.path().join("state"))
        .output()
        .unwrap();
    assert!(!ran.status.success());
    let err = String::from_utf8(ran.stderr).unwrap();
    assert!(
        err.contains("dev.example.greet-dev: ppk has no field `dependency`"),
        "{err}"
    );
}

#[test]
fn a_workspace_with_no_packager_packs_nothing() {
    let dir = example(5, "");
    let cache = tempfile::tempdir().unwrap();
    let ran = Command::new(env!("CARGO_BIN_EXE_pekit"))
        .args(["package"])
        .current_dir(dir.path())
        .env("PEKIT_CACHE_DIR", cache.path())
        .env("PEKIT_STATE_DIR", cache.path().join("state"))
        .output()
        .unwrap();
    assert!(!ran.status.success());
    let err = String::from_utf8(ran.stderr).unwrap();
    assert!(err.contains("packed by no packager"), "{err}");
}

/// The path of the directory `value`'s field `field` is.
fn file_of_dir<'a>(value: &'a Plain, field: &str) -> &'a str {
    let Plain::Record(fields) = value else {
        panic!("not a record: {value:?}");
    };
    match fields.iter().find(|(name, _)| name == field) {
        Some((_, Plain::Dir { path, .. })) => path,
        _ => panic!("no directory `{field}` in {value:?}"),
    }
}

/// What the Python archive at the logical path `path` of a build of the
/// workspace `root` prints, run by the host's `python3`.
fn run_python(root: &Path, path: &str) -> String {
    let ran = Command::new("python3")
        .arg(built_file(root, path))
        .output()
        .unwrap();
    assert!(ran.status.success(), "{ran:?}");
    String::from_utf8(ran.stdout).unwrap()
}

#[test]
fn example_8_reruns_the_checks_a_change_reaches_and_the_bundle() {
    let dir = example(8, HOST);
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "app").unwrap();
    // Each file's check, and the bundle.
    assert_eq!((built.ran, built.cached), (4, 0));
    let program = file_of(&built.value, None).to_owned();
    assert_eq!(run_python(dir.path(), &program), "Hello, pekit2\n");
    // A program of its own too, which its `#!` line runs.
    assert_eq!(run(dir.path(), &program), "Hello, pekit2\n");
    let first = fs::read(built_file(dir.path(), &program)).unwrap();
    let edit = |path: &str, from: &str, to: &str| {
        let path = dir.path().join("example").join(path);
        let text = fs::read_to_string(&path).unwrap();
        fs::write(&path, text.replacen(from, to, 1)).unwrap();
    };
    // `words`, which `app` reaches through `greet`: its check and the
    // bundle, not the others' checks.
    edit("words/words.py", "\"Hello\"", "\"Hi\"");
    let again = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!((again.ran, again.cached), (2, 2));
    assert_eq!(run_python(dir.path(), &program), "Hi, pekit2\n");
    // The program: its check and the bundle.
    edit("app/__main__.py", "\"pekit2\"", "\"you\"");
    let again = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!((again.ran, again.cached), (2, 2));
    assert_eq!(run_python(dir.path(), &program), "Hi, you\n");
    // The same sources give the same archive, built again from nothing.
    edit("words/words.py", "\"Hi\"", "\"Hello\"");
    edit("app/__main__.py", "\"you\"", "\"pekit2\"");
    let fresh = tempfile::tempdir().unwrap();
    build_in(dir.path(), fresh.path(), "app").unwrap();
    assert_eq!(fs::read(built_file(dir.path(), &program)).unwrap(), first);
}

#[test]
fn example_6_builds_for_the_host_alone_with_the_host_s_toolchain() {
    let dir = example(6, "");
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!(
        run(dir.path(), file_of(&built.value, None)),
        "Hello from a file that was turned into code during the build.\n"
    );
    // The host's toolchain builds for the host, never for aarch64.
    let error = build_in(dir.path(), state.path(), "all").unwrap_err();
    assert!(
        error.contains(
            "no toolchain of kind `Toolchain::C` runs on `x86_64-linux` and builds for \
             `aarch64-linux`"
        ),
        "{error}"
    );
}

#[test]
fn example_9_compiles_files_found_only_once_their_generator_has_run() {
    let dir = example(9, "");
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "app").unwrap();
    // The generator's compile and link, its run, the four generated files'
    // compiles, the program's own, and the link.
    assert_eq!((built.ran, built.cached), (9, 0));
    assert_eq!(
        run(dir.path(), file_of(&built.value, None)),
        "Hello, pekit2\nBuilt from files nobody listed in advance\nGoodbye\n"
    );
}

/// The workspace configuration that registers the gcc library's Bootlin
/// toolchains and the buildenv library's pinned environment.
const PINNED: &str = "import * from \"github.com/gcslib/gcc\"\n\
                      import * from \"github.com/gcslib/buildenv\"\n\
                      import * from \"github.com/gcslib/python\"\n\
                      import Toolchain from toolchain\n\
                      pekitconf = {toolchains = [Toolchain::C gcc_x86_64, Toolchain::C \
                      gcc_x86_64_to_aarch64, Toolchain::BuildEnv busybox_x86_64, \
                      Toolchain::Python python_3_14]}\n";

/// The workspace configuration that registers the host's own C toolchain
/// and tools.
const HOST: &str = "import * from \"github.com/gcslib/buildenv\"\n\
                    import * from \"github.com/gcslib/python\"\n\
                    import * from toolchain\n\
                    pekitconf = {toolchains = [Toolchain::C system_c, Toolchain::BuildEnv \
                    host_tools, Toolchain::Python system_python]}\n";

/// Each file the libraries and examples fetch, by its name in the
/// directory of [`downloads`], with its pin.
const DOWNLOADS: [(&str, &str); 8] = [
    (
        "libclang-18.1.1-py2.py3-none-manylinux2010_x86_64.whl",
        "c533091d8a3bbf7460a00cb6c1a71da93bffe148f172c7d03b1c31fbf8aa2a0b",
    ),
    (
        "x86-64--glibc--stable-2026.08-1.tar.xz",
        "cde893afab04ac7dcd15c46aac214ff550441b982536124c88a71146a0eeedd3",
    ),
    (
        "x86-64--musl--stable-2026.08-1.tar.xz",
        "78d3a4683d6ac47b5ee73bd5bce210b55eb93dff1b137c61298af97eb0d2b5a6",
    ),
    (
        "aarch64--glibc--stable-2026.08-1.tar.xz",
        "0213efac9b5577f20d58de9431960a191347ffc2257b27ffe7250522bf1f7867",
    ),
    (
        "busybox-1.35.0-x86_64-linux-musl",
        "6e123e7f3202a8c1e9b1f94d8941580a25135382b99e8d3e34fb858bba311348",
    ),
    (
        "make-4.4.1.tar.gz",
        "dd16fb1d67bfab79a72f5e8390735c49e3e8e70b4945a15ab1f81ddb78658fb3",
    ),
    (
        "zlib-1.3.1.tar.gz",
        "9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23",
    ),
    (
        "cpython-3.14.8+20261009-x86_64-unknown-linux-gnu-install_only.tar.gz",
        "83f9cb480b702548c592443f86209cf0fc03448d90692df257f095c01791dccc",
    ),
];

/// The directory the files the libraries and examples fetch are in, so
/// that a test downloads nothing: `$PEKIT_TEST_DOWNLOADS`, else the Peios
/// workspace's `local/pekit2-data`.
fn downloads() -> PathBuf {
    std::env::var_os("PEKIT_TEST_DOWNLOADS")
        .map_or_else(|| repository().join("../local/pekit2-data"), PathBuf::from)
}

/// A new state directory whose store holds the files of [`DOWNLOADS`]
/// named `names`, as a fetch would leave them; `None` if one is missing.
fn seeded(names: &[&str]) -> Option<tempfile::TempDir> {
    let state = tempfile::tempdir().unwrap();
    let store = Store::open(&state.path().join("store")).unwrap();
    // The git repositories the builds fetch commits of, as pekit keeps
    // them, so that nothing is fetched.
    let repositories = downloads().join("git");
    if repositories.is_dir() {
        copy_tree(&repositories, &state.path().join("store/git"));
    }
    for name in names {
        let (_, pin) = DOWNLOADS.iter().find(|(file, _)| file == name)?;
        let path = downloads().join(name);
        if !path.exists() {
            return None;
        }
        assert_eq!(store.put_file(&path).unwrap().digest.hex(), *pin, "{name}");
    }
    Some(state)
}

/// The machine an ELF file at `path` is for, by its header's `e_machine`.
fn elf_machine(path: &Path) -> u16 {
    let bytes = fs::read(path).unwrap();
    assert_eq!(bytes.get(..4), Some(&b"\x7fELF"[..]));
    u16::from_le_bytes([bytes[18], bytes[19]])
}

/// `text` with each step's identity, `.steps/<function>/<identity>`, as
/// `.steps/<function>/*`: it is a digest of what the step is, the platform
/// among it.
fn masked(text: &str) -> String {
    let mut out = String::new();
    let mut rest = text;
    while let Some(at) = rest.find(".steps/") {
        let (before, after) = rest.split_at(at + ".steps/".len());
        out.push_str(before);
        let function = after.find('/').map_or(after.len(), |end| end + 1);
        out.push_str(&after[..function]);
        let after = &after[function..];
        let digits = after.chars().take_while(char::is_ascii_hexdigit).count();
        out.push('*');
        rest = &after[digits..];
    }
    out.push_str(rest);
    out
}

/// The one directory of the steps of `function` under `dir`'s
/// `pekit-out/.steps`.
fn step_dir(dir: &Path, function: &str) -> PathBuf {
    let found: Vec<PathBuf> = fs::read_dir(dir.join("pekit-out/.steps").join(function))
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .collect();
    assert_eq!(found.len(), 1, "{found:?}");
    found.into_iter().next().unwrap()
}

/// Whether the file at `path` holds `text`, such as its compiler's name.
fn holds(path: &Path, text: &str) -> bool {
    fs::read(path)
        .unwrap()
        .windows(text.len())
        .any(|window| window == text.as_bytes())
}

/// The time each member's header of the `ar` archive at `path` records,
/// as written: `0` for each of an archive made deterministically.
fn member_times(path: &Path) -> Vec<String> {
    let bytes = fs::read(path).unwrap();
    assert_eq!(bytes.get(..8), Some(&b"!<arch>\n"[..]));
    let mut times = Vec::new();
    let mut at = 8;
    // Each header is 60 bytes: a name of 16, a time of 12, an owner of 6,
    // a group of 6, a mode of 8 and a size of 10, then `` `\n``; each
    // member's contents follow, padded to an even length.
    while let Some(header) = bytes.get(at..at + 60) {
        let field =
            |from: usize, to: usize| String::from_utf8_lossy(&header[from..to]).trim().to_owned();
        times.push(field(16, 28));
        let size: usize = field(48, 58).parse().unwrap();
        at += 60 + size + size % 2;
    }
    times
}

/// ELF's numbers for x86-64 and for aarch64.
const X86_64: u16 = 0x3e;
const AARCH64: u16 = 0xb7;

#[test]
fn the_examples_build_with_bootlin_s_pinned_toolchains() {
    // One store, given what the builds fetch as a fetch would, so that each
    // toolchain is unpacked once for every example, and nothing is
    // downloaded.
    let names: Vec<&str> = DOWNLOADS.iter().map(|(name, _)| *name).collect();
    let Some(state) = seeded(&names) else {
        eprintln!("skipped: what the builds fetch is not on this machine");
        return;
    };
    let bootlin = "toolchains.bootlin.com";
    for (n, target, prints) in [
        (1, "hello", "Hello from pekit2\n"),
        (2, "app", "Hello, pekit2\n"),
        (
            3,
            "app",
            "Hello from a file that was turned into code during the build.\n",
        ),
        (4, "app", ZLIB),
        (
            9,
            "app",
            "Hello, pekit2\nBuilt from files nobody listed in advance\nGoodbye\n",
        ),
    ] {
        let dir = example(n, PINNED);
        // Every output written, as `pekit run` writes them, so that a
        // step's library can be looked at.
        let options = Options {
            exposing: Exposing::All,
            ..Options::default()
        };
        let built = observed_with(dir.path(), state.path(), target, options)
            .0
            .unwrap();
        let program = file_of(&built.value, None);
        assert_eq!(run(dir.path(), program), prints, "example {n}");
        assert!(
            holds(&built_file(dir.path(), program), bootlin),
            "example {n}"
        );
        // Bootlin's `ar` records the time unless told not to, which would
        // make the library differ from one run to the next.
        if n == 2 {
            let library = step_dir(dir.path(), "archive").join("libgreet.a");
            let times = member_times(&library);
            assert!(
                !times.is_empty() && times.iter().all(|time| time == "0"),
                "{times:?}"
            );
        }
    }
    // Example 8, with the pinned interpreter.
    let dir = example(8, PINNED);
    let built = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!(
        run_python(dir.path(), file_of(&built.value, None)),
        "Hello, pekit2\n"
    );
    // Example 6, for both platforms, its tool built once, for the host.
    let dir = example(6, PINNED);
    let built = build_in(dir.path(), state.path(), "all").unwrap();
    let x86_64 = built_file(dir.path(), file_of(&built.value, Some("x86_64")));
    let aarch64 = built_file(dir.path(), file_of(&built.value, Some("aarch64")));
    assert_eq!(elf_machine(&x86_64), X86_64);
    assert_eq!(elf_machine(&aarch64), AARCH64);
    assert!(holds(&aarch64, bootlin));
    assert_eq!(
        run(dir.path(), file_of(&built.value, Some("x86_64"))),
        "Hello from a file that was turned into code during the build.\n"
    );
}

#[test]
fn the_pinned_toolchains_build_strictly_with_nothing_of_the_host() {
    let names: Vec<&str> = DOWNLOADS.iter().map(|(name, _)| *name).collect();
    let Some(state) = seeded(&names) else {
        eprintln!("skipped: what the builds fetch is not on this machine");
        return;
    };
    let strict = Options {
        strict: true,
        ..Options::default()
    };
    for (n, target, prints) in [(1, "hello", "Hello from pekit2\n"), (4, "app", ZLIB)] {
        let dir = example(n, PINNED);
        let (built, _) = observed_with(dir.path(), state.path(), target, strict);
        let built = built.unwrap();
        assert_eq!(
            run(dir.path(), file_of(&built.value, None)),
            prints,
            "example {n}"
        );
    }
    let dir = example(8, PINNED);
    let built = observed_with(dir.path(), state.path(), "app", strict)
        .0
        .unwrap();
    assert_eq!(
        run_python(dir.path(), file_of(&built.value, None)),
        "Hello, pekit2\n"
    );
    // The host's own toolchain is refused: its definition looks for the
    // host's `cc`.
    let dir = example(1, HOST);
    let (built, _) = observed_with(dir.path(), state.path(), "hello", strict);
    let error = built.unwrap_err();
    assert!(
        error.contains("a strict build refuses a target's own `which`"),
        "{error}"
    );
}

/// `pekit` run with `args` in the workspace `dir`, its cache and state in
/// `cache`: what it printed, and whether it succeeded.
fn pekit(dir: &Path, cache: &Path, args: &[&str]) -> (String, bool) {
    let ran = Command::new(env!("CARGO_BIN_EXE_pekit"))
        .args(args)
        .current_dir(dir)
        .env("PEKIT_CACHE_DIR", cache)
        .env("PEKIT_STATE_DIR", cache.join("state"))
        .output()
        .unwrap();
    (String::from_utf8(ran.stdout).unwrap(), ran.status.success())
}

#[test]
fn example_7_tests_are_cached_steps_rerun_as_a_change_reaches_them() {
    let dir = example(7, "");
    let cache = tempfile::tempdir().unwrap();
    let tests_print = |expected: &str, passes: bool| {
        let (out, success) = pekit(dir.path(), cache.path(), &["test"]);
        assert_eq!(masked(&out), expected);
        assert_eq!(success, passes, "{out}");
    };
    let edit = |path: &str, from: &str, to: &str| {
        let path = dir.path().join("example").join(path);
        let text = fs::read_to_string(&path).unwrap();
        assert!(text.contains(from), "{text}");
        fs::write(&path, text.replacen(from, to, 1)).unwrap();
    };
    tests_print(
        "passed           example/greeting\npassed           example/output\n2 passed, 0 failed\n",
        true,
    );
    // Nothing changed: both found in the store.
    tests_print(
        "passed (cached)  example/greeting\npassed (cached)  example/output\n2 passed, 0 failed\n",
        true,
    );
    // The program: the golden test, not the unit test.
    edit("main.c", "\"pekit2\"", "\"you\"");
    edit("test/expected_output.txt", "pekit2", "you");
    tests_print(
        "passed (cached)  example/greeting\npassed           example/output\n2 passed, 0 failed\n",
        true,
    );
    // What the golden test expects: it alone, and it fails.
    edit("test/expected_output.txt", "Hello", "Hi");
    tests_print(
        "passed (cached)  example/greeting\nFAILED           example/output\n    assertion \
         failed: `ran.stdout == expected.read()`\n1 passed, 1 failed\n",
        false,
    );
    edit("test/expected_output.txt", "Hi", "Hello");
    // A comment in `main.c`, the same program: the golden test is found.
    edit("main.c", "int main", "/* a comment */\nint main");
    tests_print(
        "passed (cached)  example/greeting\npassed (cached)  example/output\n2 passed, 0 failed\n",
        true,
    );
    // The library: both tests run, the unit test failing as its program
    // does, the golden test passing with what it now expects.
    edit("lib/internal.h", "\"Hello\"", "\"Hi\"");
    edit("test/expected_output.txt", "Hello", "Hi");
    tests_print(
        "FAILED           example/greeting\n    \
         `/pekit-out/.steps/link/*/greeting_test` exited with status 1\n    \
         its standard error: greeting() returned \"Hi\", expected \"Hello\"\npassed           \
         example/output\n1 passed, 1 failed\n",
        false,
    );
}

/// The workspace configuration of example 10: the host's C toolchain and
/// QEMU, ppk and peiso's tools at `tools/`, and the pinned Rust. ppk is the
/// one its package takes, `build ppk()`, so that it is compiled once.
const IMAGE: &str = "import * from \"github.com/gcslib/ppk\"\n\
                     import Toolchain::* from \"github.com/gcslib/python\"\n\
                     import * from \"github.com/gcslib/peiso\"\n\
                     import * from \"github.com/gcslib/qemu\"\n\
                     import * from \"github.com/gcslib/rust\"\n\
                     import * from toolchain\n\
                     import build ppk from example\n\
                     import fileroot\n\
                     import path\n\
                     fn built(at: String, bins: List<String>, packages: List<String>, crates: List<CrateSettings> = []) -> Dir {\n\
                     \x20   let src = path.join(fileroot.this, \"example/{at}\")\n\
                     \x20   let ppk = {at = \"ppk\"; place = dir(path.join(fileroot.this, \"example/ppk\"))}\n\
                     \x20   = cargo_build(dir(src), vendored(file(\"{src}/Cargo.lock\")), bins = bins, packages = packages, beside = [ppk], crates = crates)\n\
                     }\n\
                     def Toolchain::Ppk workspace_ppk() {\n\
                     \x20   let tools = build ppk()\n\
                     \x20   host_platform = ^host_platform\n\
                     \x20   platform = {}\n\
                     \x20   provides = {root = tools; runtime = dir(\"/\"); ppk = tools.file(\"ppk\")}\n\
                     }\n\
                     def Toolchain::Peiso workspace_peiso() {\n\
                     \x20   let uapi = \"example/ppk/vendor/kernel/security/pkm/kacs/peios_uapi.rs\"\n\
                     \x20   let peiso = built(\"peiso\", [\"peiso\"], [], crates = [{package = \"peios-uapi\"; \
                     beside = [{at = \"../../../security/pkm/kacs/peios_uapi.rs\"; place = file(path.join(fileroot.this, uapi))}]}])\n\
                     \x20   let utils = built(\"peiosutils\", [\"mkirf\", \"mkuki\"], [\"pu_mkirf\", \"pu_mkuki\"])\n\
                     \x20   host_platform = ^host_platform\n\
                     \x20   platform = {}\n\
                     \x20   provides = {root = peiso; runtime = dir(\"/\"); peiso = peiso.file(\"peiso\"); \
                     mkirf = utils.file(\"mkirf\"); mkuki = utils.file(\"mkuki\")}\n\
                     }\n\
                     pekitconf = {toolchains = [Toolchain::C system_c, Toolchain::Ppk workspace_ppk, \
                     Toolchain::Peiso workspace_peiso, Toolchain::Qemu system_qemu, \
                     Toolchain::Rust rust_1_98, Toolchain::Python python_3_14]}\n";

/// Whether the host has what example 10 takes of it: QEMU, its firmware,
/// and the tools peiso makes an ISO with. ppk, peiso and peiosutils' `mkirf`
/// and `mkuki` are built from their sources.
fn image_host() -> bool {
    [
        "/usr/bin/qemu-system-x86_64",
        "/usr/share/OVMF/OVMF_CODE_4M.fd",
        "/usr/share/OVMF/OVMF_VARS_4M.fd",
        "/usr/bin/xorriso",
        "/usr/sbin/mkfs.vfat",
        "/usr/bin/mcopy",
    ]
    .iter()
    .all(|path| Path::new(path).is_file())
}

#[test]
#[ignore = "opt-in, as it takes about six minutes: `tools/check --all`, or `cargo test --test \
            examples -- --ignored`"]
fn example_10_boots_an_iso_of_packages_from_a_repository_and_built_here() {
    let sources = ["ppk", "peiso", "peiosutils", "prelude"]
        .map(|name| (name, repository().join("..").join(name)));
    let packages = downloads().join("peios-repository");
    let rust = rust_downloads();
    let names: Vec<&str> = DOWNLOADS.iter().map(|(name, _)| *name).collect();
    let ready = packages.is_dir()
        && rust.len() > 4
        && image_host()
        && sources
            .iter()
            .all(|(_, source)| source.join("Cargo.lock").is_file());
    let Some(state) = seeded(&names).filter(|_| ready) else {
        eprintln!(
            "skipped: example 10's host tools, its repository, Rust or a source are not here"
        );
        return;
    };
    let store = Store::open(&state.path().join("store")).unwrap();
    for path in &rust {
        store.put_file(path).unwrap();
    }
    let dir = example(10, IMAGE);
    // ppk, peiso and peiosutils as their committed trees, as a release
    // would build them.
    for (name, source) in &sources {
        committed(source, &dir.path().join("example").join(name));
    }
    copy_tree(&packages, &dir.path().join("example/repository"));
    // The kernel and busybox from the repository, ppk built from its source
    // and the packages built here, each packed, and the ISO.
    let (built, events) = observed_with(dir.path(), state.path(), "iso", Options::default());
    let built = built.unwrap();
    assert!(built.ran > 9, "{}", built.ran);
    // Each Rust build once: ppk, libpeios, prelude, peiso and peiosutils.
    // The ppk toolchain packs with the ppk its package takes.
    let compiled: Vec<&str> = events
        .iter()
        .filter_map(|event| match &event.kind {
            EventKind::Started { step, attempt: 0 } if step.starts_with("cargo_build/") => {
                Some(step.as_str())
            }
            _ => None,
        })
        .collect();
    assert_eq!(compiled.len(), 5, "{compiled:?}");
    let ppk = build_in(dir.path(), state.path(), "ppk").unwrap();
    assert_eq!(ppk.ran, 0, "the ISO's ppk is the one built alone");
    assert!(built_file(dir.path(), file_of(&built.value, None)).is_file());
    // The boot test boots it to busybox's prompt.
    let cache = state.path();
    let (out, passed) = pekit(dir.path(), cache, &["test"]);
    assert!(passed, "{out}");
    assert_eq!(out, "passed           example/boots\n1 passed, 0 failed\n");
    let edit = |path: &str, from: &str, to: &str| {
        let path = dir.path().join("example").join(path);
        let text = fs::read_to_string(&path).unwrap();
        assert!(text.contains(from), "{text}");
        fs::write(&path, text.replacen(from, to, 1)).unwrap();
    };
    // A change to the package built here: its step, its packing and the
    // ISO, and none of the repository's packages.
    edit("motd.txt", "welcome", "hello");
    let again = build_in(dir.path(), state.path(), "iso").unwrap();
    assert_eq!(again.ran, 3, "{}", again.cached);
    // A change to the kernel's command line alone: the ISO, and no package.
    edit("build.gcsl", "\"console=ttyS0\"", "\"console=ttyS0 quiet\"");
    let again = build_in(dir.path(), state.path(), "iso").unwrap();
    assert_eq!(again.ran, 1, "{}", again.cached);
    // Each reaches the boot test, which boots the new ISO.
    let (out, passed) = pekit(dir.path(), cache, &["test"]);
    assert!(passed, "{out}");
    assert_eq!(out, "passed           example/boots\n1 passed, 0 failed\n");
    let (rdeps, _) = pekit(
        dir.path(),
        cache,
        &["query", "rdeps", "example/motd.txt", "--kind", "test"],
    );
    assert_eq!(rdeps, "example/boots\n");
    // The launcher `pekit run boot` runs.
    let launcher = build_in(dir.path(), state.path(), "boot").unwrap();
    let script =
        fs::read_to_string(built_file(dir.path(), file_of(&launcher.value, None))).unwrap();
    assert!(
        masked(&script).contains("-cdrom /pekit-out/.steps/assemble/*/peios.iso"),
        "{script}"
    );
}

/// The workspace configuration that registers the gcc library's Bootlin
/// toolchain and the rust library's pinned Rust.
/// A git repository of two crates, `greeting` taking `words` by its path:
/// the directory it is in, its URL and its commit's name.
fn greeting_repository() -> (tempfile::TempDir, String, String) {
    repository_of_greeting("pub const HELLO: &str = \"hello from git\";\n", &[])
}

/// A git repository of two crates, `greeting` taking `words`, whose
/// `src/lib.rs` is `words`, by its path, and `files` besides, each a path
/// and its text: the directory it is in, its URL and its commit's name.
fn repository_of_greeting(
    words: &str,
    files: &[(&str, &str)],
) -> (tempfile::TempDir, String, String) {
    let origin = tempfile::tempdir().unwrap();
    let crate_of = |name: &str, deps: &str, code: &str| {
        let at = origin.path().join(name);
        fs::create_dir_all(at.join("src")).unwrap();
        fs::write(
            at.join("Cargo.toml"),
            format!("[package]\nname = \"{name}\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\n{deps}"),
        )
        .unwrap();
        fs::write(at.join("src/lib.rs"), code).unwrap();
    };
    crate_of("words", "", words);
    for (path, text) in files {
        let at = origin.path().join(path);
        fs::create_dir_all(at.parent().unwrap()).unwrap();
        fs::write(at, text).unwrap();
    }
    crate_of(
        "greeting",
        "words = { path = \"../words\" }\n",
        "pub fn hello() -> &'static str { words::HELLO }\n",
    );
    let git = |args: &[&str]| {
        let ran = Command::new("git")
            .arg("-C")
            .arg(origin.path())
            .args([
                "-c",
                "user.name=t",
                "-c",
                "user.email=t@t",
                "-c",
                "commit.gpgsign=false",
            ])
            .args(args)
            .output()
            .unwrap();
        assert!(
            ran.status.success(),
            "{}",
            String::from_utf8_lossy(&ran.stderr)
        );
        String::from_utf8_lossy(&ran.stdout).trim().to_owned()
    };
    git(&["init", "--quiet"]);
    git(&["add", "."]);
    git(&["commit", "--quiet", "-m", "one"]);
    let commit = git(&["rev-parse", "HEAD"]);
    let url = format!("file://{}", origin.path().display());
    (origin, url, commit)
}

/// A workspace at `dir` whose `example/app` is a program taking `greeting`
/// from `url` at `commit`, with its lock as Cargo writes it, built by the
/// rust library's `app`.
fn greeting_app(dir: &Path, (url, commit): (&str, &str)) {
    greeting_app_built(
        dir,
        (url, commit),
        "def build app() { = rust.cargo_build(dir(\"app\"), rust.vendored(file(\"app/Cargo.lock\")), bins = [\"app\"]) }\n",
    );
}

/// [`greeting_app`], with `build` the definitions of `example/build.gcsl`,
/// which imports the rust library.
fn greeting_app_built(dir: &Path, (url, commit): (&str, &str), build: &str) {
    // A program taking `greeting` by the commit, and its lock, as Cargo
    // writes it.
    let app = dir.join("example/app");
    fs::create_dir_all(app.join("src")).unwrap();
    fs::write(
        app.join("Cargo.toml"),
        format!(
            "[package]\nname = \"app\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n\
             [dependencies]\ngreeting = {{ git = \"{url}\", rev = \"{commit}\" }}\n"
        ),
    )
    .unwrap();
    fs::write(
        app.join("src/main.rs"),
        "fn main() { println!(\"{}\", greeting::hello()); }\n",
    )
    .unwrap();
    let source = format!("git+{url}?rev={commit}#{commit}");
    fs::write(
        app.join("Cargo.lock"),
        format!(
            "# This file is automatically @generated by Cargo.\n\
             # It is not intended for manual editing.\nversion = 4\n\n\
             [[package]]\nname = \"app\"\nversion = \"0.1.0\"\ndependencies = [\n \"greeting\",\n]\n\n\
             [[package]]\nname = \"greeting\"\nversion = \"0.1.0\"\nsource = \"{source}\"\n\
             dependencies = [\n \"words\",\n]\n\n\
             [[package]]\nname = \"words\"\nversion = \"0.1.0\"\nsource = \"{source}\"\n"
        ),
    )
    .unwrap();
    fs::write(
        dir.join("example/build.gcsl"),
        format!("import \"github.com/gcslib/rust\"\n{build}"),
    )
    .unwrap();
    libraries(&dir.join(".gcsl/libraries"));
    fs::write(dir.join("workspace.gcsl"), RUST).unwrap();
}

#[test]
fn the_rust_library_builds_with_crates_from_a_git_repository() {
    let rust = rust_downloads();
    let names: Vec<&str> = DOWNLOADS.iter().map(|(name, _)| *name).collect();
    let Some(state) = seeded(&names).filter(|_| rust.len() > 4) else {
        eprintln!("skipped: Rust or the crates are not on this machine");
        return;
    };
    let store = Store::open(&state.path().join("store")).unwrap();
    for path in &rust {
        store.put_file(path).unwrap();
    }
    let (origin, url, commit) = greeting_repository();
    let dir = tempfile::tempdir().unwrap();
    greeting_app(dir.path(), (&url, &commit));
    let built = build_in(dir.path(), state.path(), "app").unwrap();
    let Plain::Dir { path, .. } = &built.value else {
        panic!("not a directory: {:?}", built.value);
    };
    let program = built_file(dir.path(), &format!("{path}/app"));
    let ran = Command::new(&program).output().unwrap();
    assert_eq!(String::from_utf8_lossy(&ran.stdout), "hello from git\n");
    // And strictly, the repository gone: found in the store.
    drop(origin);
    let strict = Options {
        strict: true,
        ..Options::default()
    };
    observed_with(dir.path(), state.path(), "app", strict)
        .0
        .unwrap();
}

#[test]
fn a_git_crate_is_given_what_it_reads_beside_its_directory() {
    let rust = rust_downloads();
    let names: Vec<&str> = DOWNLOADS.iter().map(|(name, _)| *name).collect();
    let Some(state) = seeded(&names).filter(|_| rust.len() > 4) else {
        eprintln!("skipped: Rust or the crates are not on this machine");
        return;
    };
    let store = Store::open(&state.path().join("store")).unwrap();
    for path in &rust {
        store.put_file(path).unwrap();
    }
    // `words` reads a file of its repository outside its own directory,
    // which vendoring leaves behind.
    let (_origin, url, commit) = repository_of_greeting(
        "pub const HELLO: &str = include_str!(\"../../shared/hello.txt\");\n",
        &[("shared/hello.txt", "hello from beside")],
    );
    let built = |beside: &str| {
        let dir = tempfile::tempdir().unwrap();
        greeting_app_built(
            dir.path(),
            (&url, &commit),
            &format!(
                "def build app() {{\n\
                 \x20   let ws = rust.workspace(dir(\"app\"), rust.vendored(file(\"app/Cargo.lock\")), crates = [{{package = \"words\"; beside = [{beside}]}}])\n\
                 \x20   = rust.bin(ws, \"app\")\n\
                 }}\n"
            ),
        );
        let result = build_in(dir.path(), state.path(), "app");
        (dir, result)
    };
    let (_, without) = built("");
    let error = without.unwrap_err();
    assert!(error.contains("shared/hello.txt"), "{error}");
    let (dir, with) = built(&format!(
        "{{at = \"../shared/hello.txt\"; place = fetch_git(\"{url}\", \"{commit}\").file(\"shared/hello.txt\")}}"
    ));
    let built = with.unwrap();
    let Plain::File { path, .. } = &built.value else {
        panic!("not a file: {:?}", built.value);
    };
    let program = built_file(dir.path(), path);
    let ran = Command::new(&program).output().unwrap();
    assert_eq!(String::from_utf8_lossy(&ran.stdout), "hello from beside\n");
}

#[test]
fn a_git_crate_must_be_at_the_commit_its_repository_s_library_is_pinned_at() {
    let (_origin, url, commit) = greeting_repository();
    let dir = tempfile::tempdir().unwrap();
    greeting_app(dir.path(), (&url, &commit));
    // The lock pins a library from the crate's repository at another
    // commit; the library is there, so nothing is fetched to restore it.
    let other = "0".repeat(40);
    fs::create_dir_all(dir.path().join(".gcsl/libraries/example.org/greeting")).unwrap();
    fs::write(
        dir.path().join("pekit.lock"),
        format!(
            "{{\"libraries\": [{{\"path\": \"example.org/greeting\", \"git\": \"{url}.git\", \
             \"dir\": \"\", \"commit\": \"{other}\", \"tree\": \"\"}}]}}\n"
        ),
    )
    .unwrap();
    let state = tempfile::tempdir().unwrap();
    let error = build_in(dir.path(), state.path(), "app").unwrap_err();
    assert!(
        error.contains(&format!(
            "`Cargo.lock` pins `greeting` at `{commit}` of `{url}`, but `pekit.lock` pins the \
             library `example.org/greeting` from the same repository at `{other}`"
        )),
        "{error}"
    );
}

/// The lock of a workspace whose member `app` takes `cc`, with what `cc`
/// needs, from crates.io, and the member `helper` to build, as Cargo writes
/// it.
const CC_LOCK: &str = "# This file is automatically @generated by Cargo.\n\
                       # It is not intended for manual editing.\nversion = 4\n\n\
                       [[package]]\nname = \"app\"\nversion = \"0.1.0\"\ndependencies = [\n \"cc\",\n \"helper\",\n]\n\n\
                       [[package]]\nname = \"cc\"\nversion = \"1.2.59\"\n\
                       source = \"registry+https://github.com/rust-lang/crates.io-index\"\n\
                       checksum = \"b7a4d3ec6524d28a329fc53654bbadc9bdd7b0431f5d65f1a56ffb28a1ee5283\"\n\
                       dependencies = [\n \"find-msvc-tools\",\n \"shlex\",\n]\n\n\
                       [[package]]\nname = \"find-msvc-tools\"\nversion = \"0.1.9\"\n\
                       source = \"registry+https://github.com/rust-lang/crates.io-index\"\n\
                       checksum = \"5baebc0774151f905a1a2cc41989300b1e6fbb29aff0ceffa1064fdd3088d582\"\n\n\
                       [[package]]\nname = \"helper\"\nversion = \"0.1.0\"\n\n\
                       [[package]]\nname = \"shlex\"\nversion = \"1.3.0\"\n\
                       source = \"registry+https://github.com/rust-lang/crates.io-index\"\n\
                       checksum = \"0fda2ff0d084019ba4d7c6f371c95d8fd75ce3524c3cb8fb653a3023f6323e64\"\n";

/// A state directory seeded with the Rust parts, the crates and the
/// Bootlin toolchain, as [`seeded`] makes one; `None` if any is missing.
fn seeded_for_rust() -> Option<tempfile::TempDir> {
    let rust = rust_downloads();
    let names: Vec<&str> = DOWNLOADS.iter().map(|(name, _)| *name).collect();
    let state = seeded(&names).filter(|_| rust.len() > 4)?;
    let store = Store::open(&state.path().join("store")).unwrap();
    for path in &rust {
        store.put_file(path).unwrap();
    }
    Some(state)
}

/// A pekit workspace in `dir` with the Rust toolchains, its
/// `example/build.gcsl` of `build`, which imports the rust library, and
/// `files` under `example/ws`, a Cargo workspace.
fn cargo_workspace(dir: &Path, build: &str, files: &[(&str, &str)]) {
    for (path, text) in files {
        let at = dir.join("example/ws").join(path);
        fs::create_dir_all(at.parent().unwrap()).unwrap();
        fs::write(at, text).unwrap();
    }
    fs::write(
        dir.join("example/build.gcsl"),
        format!("import \"github.com/gcslib/rust\"\n{build}"),
    )
    .unwrap();
    libraries(&dir.join(".gcsl/libraries"));
    fs::write(dir.join("workspace.gcsl"), RUST).unwrap();
}

#[test]
fn the_rust_library_plans_without_nested_checkouts_and_builds_c_plus_plus() {
    use std::os::unix::fs::PermissionsExt;
    let Some(state) = seeded_for_rust() else {
        eprintln!("skipped: Rust or the crates are not on this machine");
        return;
    };
    let dir = tempfile::tempdir().unwrap();
    let broken = "this is no manifest [\n";
    cargo_workspace(
        dir.path(),
        "def build app() {\n\
         \x20   let ws = rust.workspace(dir(\"ws\"), rust.vendored(file(\"ws/Cargo.lock\")), \
         ignore = [\"target\", \"pekit-out\", \"crates/generated\"])\n\
         \x20   = rust.bin(ws, \"app\")\n\
         }\n",
        &[
            (
                "Cargo.toml",
                "[workspace]\nmembers = [\"crates/*\"]\nexclude = [\"fuzz\"]\nresolver = \"2\"\n",
            ),
            ("Cargo.lock", CC_LOCK),
            (
                "crates/app/Cargo.toml",
                "[package]\nname = \"app\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n\
                 [build-dependencies]\ncc = \"=1.2.59\"\nhelper = { path = \"../helper\" }\n",
            ),
            (
                "crates/app/build.rs",
                "fn main() {\n\
                 \x20   cc::Build::new().cpp(true).file(\"src/greet.cpp\").compile(\"greet\");\n\
                 \x20   println!(\"cargo:rustc-env=HELPER_SAYS={}\", helper::greeting().trim());\n\
                 }\n",
            ),
            // A build dependency that reads its own sources when the build
            // script runs, as `luau0-src` reads the C++ it builds.
            (
                "crates/helper/Cargo.toml",
                "[package]\nname = \"helper\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
            ),
            (
                "crates/helper/src/lib.rs",
                "pub fn greeting() -> String {\n\
                 \x20   std::fs::read_to_string(concat!(env!(\"CARGO_MANIFEST_DIR\"), \"/data.txt\")).unwrap()\n\
                 }\n",
            ),
            ("crates/helper/data.txt", "from helper\n"),
            (
                "crates/app/src/greet.cpp",
                "#include <string>\n\
                 extern \"C\" unsigned long greeting_length() { return std::string(\"hello from C++\").size(); }\n",
            ),
            (
                "crates/app/src/main.rs",
                "extern \"C\" { fn greeting_length() -> std::ffi::c_ulong; }\n\
                 fn main() { println!(\"{} {}\", env!(\"HELPER_SAYS\"), unsafe { greeting_length() }); }\n",
            ),
            // An agent's worktree, matched by `crates/*`, whose manifest
            // Cargo cannot read: planning must never see it.
            (
                "crates/agent/.git",
                "gitdir: /elsewhere/.git/worktrees/agent\n",
            ),
            ("crates/agent/Cargo.toml", broken),
            // An output named in `ignore`, matched by `crates/*` too.
            ("crates/generated/Cargo.toml", broken),
            // Cargo's own outputs, ignored by default.
            ("target/package/app-0.1.0/Cargo.toml", broken),
            // A workspace of its own that the main one excludes, with
            // crates its lock has and the main lock lacks.
            (
                "fuzz/Cargo.toml",
                "[package]\nname = \"app-fuzz\"\nversion = \"0.0.0\"\nedition = \"2021\"\n\n\
                 [dependencies]\nlibfuzzer-sys = \"0.4\"\napp = { path = \"../crates/app\" }\n\n\
                 [workspace]\nmembers = [\".\"]\n",
            ),
            (
                "fuzz/Cargo.lock",
                "version = 4\n\n[[package]]\nname = \"app-fuzz\"\nversion = \"0.0.0\"\n",
            ),
            ("fuzz/fuzz_targets/parse.rs", "#![no_main]\n"),
        ],
    );
    // Never listed: a listing that met it would fail on it. Kept from the
    // sources pekit loads, which would meet it first.
    let locked = dir.path().join("example/ws/crates/agent/locked");
    fs::create_dir(&locked).unwrap();
    fs::set_permissions(&locked, fs::Permissions::from_mode(0o000)).unwrap();
    fs::write(dir.path().join(".gcslignore"), "example/ws/crates/agent\n").unwrap();
    let built = build_in(dir.path(), state.path(), "app");
    fs::set_permissions(&locked, fs::Permissions::from_mode(0o755)).unwrap();
    let built = built.unwrap();
    let program = built_file(dir.path(), file_of(&built.value, None));
    let ran = Command::new(&program).output().unwrap();
    assert!(ran.status.success(), "{ran:?}");
    assert_eq!(String::from_utf8_lossy(&ran.stdout), "from helper 14\n");
    // Built again, nothing runs; nor after the worktree and the outputs
    // change, which planning never reads.
    let again = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!(again.ran, 0);
    for path in [
        "crates/agent/src/lib.rs",
        "crates/generated/new.rs",
        "target/debug/app",
    ] {
        let at = dir.path().join("example/ws").join(path);
        fs::create_dir_all(at.parent().unwrap()).unwrap();
        fs::write(at, "").unwrap();
    }
    let changed = build_in(dir.path(), state.path(), "app").unwrap();
    assert_eq!(changed.ran, 0);
}

const RUST: &str = "import * from \"github.com/gcslib/gcc\"\n\
                    import * from \"github.com/gcslib/rust\"\n\
                    import Toolchain from toolchain\n\
                    pekitconf = {toolchains = [Toolchain::C gcc_x86_64, Toolchain::Rust rust_1_98]}\n";

/// The Rust parts and the crates `rust.vendored` fetches, by their paths in
/// [`downloads`]' `rust/`: what the rust library's builds need, besides the
/// Bootlin toolchain.
fn rust_downloads() -> Vec<PathBuf> {
    let dir = downloads().join("rust");
    let mut found: Vec<PathBuf> = fs::read_dir(&dir)
        .into_iter()
        .flatten()
        .flatten()
        .map(|entry| entry.path())
        .filter(|path| path.extension().is_some_and(|extension| extension == "xz"))
        .collect();
    found.extend(
        fs::read_dir(dir.join("crates"))
            .into_iter()
            .flatten()
            .flatten()
            .map(|entry| entry.path()),
    );
    found
}

/// The committed tree of the git repository at `source`, at `HEAD`,
/// written into `into`, as a release would build it.
fn committed(source: &Path, into: &Path) {
    fs::create_dir_all(into).unwrap();
    let archive = Command::new("git")
        .args(["-C", &source.to_string_lossy(), "archive", "HEAD"])
        .output()
        .unwrap();
    assert!(archive.status.success());
    tar::Archive::new(&archive.stdout[..]).unpack(into).unwrap();
}

#[test]
fn the_rust_library_builds_ppk_from_its_source_and_its_lock() {
    let source = repository().join("../ppk");
    let rust = rust_downloads();
    let names: Vec<&str> = DOWNLOADS.iter().map(|(name, _)| *name).collect();
    let Some(state) =
        seeded(&names).filter(|_| rust.len() > 4 && source.join("Cargo.lock").is_file())
    else {
        eprintln!("skipped: Rust, the crates or ppk's source are not on this machine");
        return;
    };
    let store = Store::open(&state.path().join("store")).unwrap();
    for path in &rust {
        store.put_file(path).unwrap();
    }
    let dir = tempfile::tempdir().unwrap();
    copy_tree(
        &repository().join("tests/fixtures/rust"),
        &dir.path().join("example"),
    );
    libraries(&dir.path().join(".gcsl/libraries"));
    fs::write(dir.path().join("workspace.gcsl"), RUST).unwrap();
    // ppk's committed tree, as a release would build it.
    committed(&source, &dir.path().join("example/src"));
    let built = build_in(dir.path(), state.path(), "ppk").unwrap();
    let Plain::Dir { path, .. } = &built.value else {
        panic!("not a directory: {:?}", built.value);
    };
    let ppk = built_file(dir.path(), &format!("{path}/ppk"));
    let version = Command::new(&ppk).arg("--version").output().unwrap();
    assert_eq!(String::from_utf8_lossy(&version.stdout), "ppk 0.1.0\n");
    // A static program, which needs nothing of the system it runs on.
    let header = fs::read(&ppk).unwrap();
    assert!(
        !header
            .windows(b"ld-linux".len())
            .any(|window| window == b"ld-linux")
    );
    // Built again, nothing runs.
    let again = build_in(dir.path(), state.path(), "ppk").unwrap();
    assert_eq!(again.ran, 0);
    // And strictly, with nothing of the host: the same program.
    let strict = Options {
        strict: true,
        ..Options::default()
    };
    let (built, _) = observed_with(dir.path(), state.path(), "ppk", strict);
    let built = built.unwrap();
    let Plain::Dir { path, .. } = &built.value else {
        panic!("not a directory: {:?}", built.value);
    };
    let strictly = fs::read(built_file(dir.path(), &format!("{path}/ppk"))).unwrap();
    assert_eq!(strictly, header, "a strict build gives the same program");
    // For glibc, given the target alone: linked against the system's C
    // library, with the registered C toolchain for the crates' C.
    let built = build_in(dir.path(), state.path(), "ppk_glibc").unwrap();
    let Plain::Dir { path, .. } = &built.value else {
        panic!("not a directory: {:?}", built.value);
    };
    let ppk = built_file(dir.path(), &format!("{path}/ppk"));
    let version = Command::new(&ppk).arg("--version").output().unwrap();
    assert_eq!(String::from_utf8_lossy(&version.stdout), "ppk 0.1.0\n");
    assert!(holds(&ppk, "ld-linux-x86-64.so.2"));
}

#[test]
fn the_rust_library_builds_ppk_one_crate_at_a_time() {
    let source = repository().join("../ppk");
    let rust = rust_downloads();
    let names: Vec<&str> = DOWNLOADS.iter().map(|(name, _)| *name).collect();
    let Some(state) =
        seeded(&names).filter(|_| rust.len() > 4 && source.join("Cargo.lock").is_file())
    else {
        eprintln!("skipped: Rust, the crates or ppk's source are not on this machine");
        return;
    };
    let store = Store::open(&state.path().join("store")).unwrap();
    for path in &rust {
        store.put_file(path).unwrap();
    }
    let dir = tempfile::tempdir().unwrap();
    copy_tree(
        &repository().join("tests/fixtures/rust"),
        &dir.path().join("example"),
    );
    libraries(&dir.path().join(".gcsl/libraries"));
    fs::write(dir.path().join("workspace.gcsl"), RUST).unwrap();
    committed(&source, &dir.path().join("example/src"));
    let built = build_in(dir.path(), state.path(), "ppk_crates").unwrap();
    let ppk = built_file(dir.path(), file_of(&built.value, None));
    let version = Command::new(&ppk).arg("--version").output().unwrap();
    assert_eq!(String::from_utf8_lossy(&version.stdout), "ppk 0.1.0\n");
    assert!(holds(&ppk, "ld-linux-x86-64.so.2"));
    // Each crate a step: far more than Cargo's one.
    assert!(built.ran > 50, "{}", built.ran);
    // Built again, nothing runs.
    let again = build_in(dir.path(), state.path(), "ppk_crates").unwrap();
    assert_eq!(again.ran, 0);
    // And strictly, with nothing of the host: the same program.
    let strict = Options {
        strict: true,
        ..Options::default()
    };
    let (strictly, _) = observed_with(dir.path(), state.path(), "ppk_crates", strict);
    let strictly = strictly.unwrap();
    assert_eq!(
        fs::read(built_file(dir.path(), file_of(&strictly.value, None))).unwrap(),
        fs::read(&ppk).unwrap(),
        "a strict build gives the same program"
    );
    // A change to ppk's own library compiles it, and the program that links
    // it, again: none of the crates it uses, nor Cargo's plan, which reads
    // no source.
    let lib = dir.path().join("example/src/src/lib.rs");
    let text = fs::read_to_string(&lib).unwrap();
    fs::write(&lib, format!("{text}// changed\n")).unwrap();
    let changed = build_in(dir.path(), state.path(), "ppk_crates").unwrap();
    assert_eq!(changed.ran, 2);
    let version = Command::new(built_file(dir.path(), file_of(&changed.value, None)))
        .arg("--version")
        .output()
        .unwrap();
    assert_eq!(String::from_utf8_lossy(&version.stdout), "ppk 0.1.0\n");
}

/// A workspace with the Rust toolchains, whose keychain's `module-signing`
/// key is the test key, and whose `example/signed` signs `example/modules`,
/// where it writes two modules, which it gives by their paths in the tree.
fn modsig_workspace(dir: &Path) -> Vec<(&'static str, Vec<u8>)> {
    libraries(&dir.join(".gcsl/libraries"));
    let key = "tests/fixtures/modsig/test-key.pem";
    fs::create_dir_all(dir.join("keys")).unwrap();
    fs::copy(repository().join(key), dir.join("keys/test.pem")).unwrap();
    fs::write(
        dir.join("workspace.gcsl"),
        RUST.replace(
            "rust_1_98]}",
            "rust_1_98]; keychain = [\
             {profile = \"dev\"; name = \"module-signing\"; file = \"keys/test.pem\"}]}",
        ),
    )
    .unwrap();
    let made = vec![
        ("1.0/kernel/a.ko.zst", b"\x7fELF module a".to_vec()),
        ("1.0/kernel/sub/b.ko.zst", b"\x7fELF module b".repeat(1000)),
    ];
    let tree = dir.join("example/modules");
    for (path, module) in &made {
        let at = tree.join(path);
        fs::create_dir_all(at.parent().unwrap()).unwrap();
        fs::write(at, zstd::encode_all(module.as_slice(), 3).unwrap()).unwrap();
    }
    fs::write(tree.join("1.0/modules.dep"), "kernel/a.ko.zst:\n").unwrap();
    fs::write(
        dir.join("example/build.gcsl"),
        "import \"github.com/gcslib/modsig\"\n\
         def build signed() { = modsig.sign(dir(\"modules\")) }\n",
    )
    .unwrap();
    made
}

/// The tree of the directory `value`, a build's value in `root`: its
/// files by their paths, with their contents.
fn signed_tree(root: &Path, value: &Plain) -> Vec<(PathBuf, Vec<u8>)> {
    fn walk(dir: &Path, within: &Path, found: &mut Vec<(PathBuf, Vec<u8>)>) {
        for entry in fs::read_dir(dir).unwrap() {
            let entry = entry.unwrap();
            let path = within.join(entry.file_name());
            if entry.path().is_dir() {
                walk(&entry.path(), &path, found);
            } else {
                found.push((path, fs::read(entry.path()).unwrap()));
            }
        }
    }
    let Plain::Dir { path, .. } = value else {
        panic!("not a directory: {value:?}");
    };
    let mut found = Vec::new();
    walk(&built_file(root, path), Path::new(""), &mut found);
    found.sort();
    found
}

/// Whether any file under `dir` holds `text`.
fn anywhere(dir: &Path, text: &str) -> bool {
    fs::read_dir(dir).unwrap().flatten().any(|entry| {
        let path = entry.path();
        if entry.file_type().unwrap().is_dir() {
            anywhere(&path, text)
        } else {
            entry.file_type().unwrap().is_file() && holds(&path, text)
        }
    })
}

#[test]
#[ignore = "opt-in, as it takes about five minutes: `tools/check --all`, or `cargo test --test \
            examples -- --ignored`"]
fn the_modsig_library_signs_after_the_build_with_a_key_of_the_keychain_s() {
    let rust = rust_downloads();
    let names: Vec<&str> = DOWNLOADS.iter().map(|(name, _)| *name).collect();
    let crates_here = rust.iter().any(|path| path.ends_with("ml-dsa-0.1.2.crate"));
    let Some(state) = seeded(&names).filter(|_| crates_here) else {
        eprintln!("skipped: Rust or the crates of modsig's tool are not on this machine");
        return;
    };
    let store = Store::open(&state.path().join("store")).unwrap();
    for path in &rust {
        store.put_file(path).unwrap();
    }
    let dir = tempfile::tempdir().unwrap();
    let made = modsig_workspace(dir.path());
    let built = build_in(dir.path(), state.path(), "signed").unwrap();
    let tree = signed_tree(dir.path(), &built.value);
    assert_eq!(
        tree.len(),
        3,
        "{:?}",
        tree.iter().map(|t| &t.0).collect::<Vec<_>>()
    );
    for (path, module) in &made {
        let (_, file) = tree.iter().find(|(at, _)| at.ends_with(path)).unwrap();
        let signed = zstd::decode_all(file.as_slice()).unwrap();
        assert!(signed.starts_with(module));
        assert!(signed.ends_with(b"~Module signature appended~\n"));
    }
    let (_, index) = tree
        .iter()
        .find(|(at, _)| at.ends_with("modules.dep"))
        .unwrap();
    assert_eq!(index, b"kernel/a.ko.zst:\n");

    // Built again, nothing runs: it is kept under the key's identity.
    let again = build_in(dir.path(), state.path(), "signed").unwrap();
    assert_eq!(again.ran, 0);

    // Neither the store nor what a build exposes holds the key.
    let key = fs::read_to_string(dir.path().join("keys/test.pem")).unwrap();
    let line = key.lines().nth(1).unwrap();
    assert!(!anywhere(&state.path().join("store"), line));
    assert!(!anywhere(&dir.path().join("pekit-out"), line));

    // Run again, it gives what it gave; and on a machine with a keychain
    // key of its own it runs again, and gives the same bytes.
    let check = Options {
        reuse: pekit::engine::Reuse::Compare,
        ..Options::default()
    };
    let (checked, _) = observed_with(dir.path(), state.path(), "signed", check);
    checked.unwrap();
    fs::remove_file(state.path().join("state/keychain-key")).unwrap();
    let other = build_in(dir.path(), state.path(), "signed").unwrap();
    assert_eq!(other.ran, 1, "only the signing step");
    assert_eq!(signed_tree(dir.path(), &other.value), tree);

    // Strictly, with nothing of the host: the same bytes.
    let strict = Options {
        strict: true,
        ..Options::default()
    };
    let (strictly, _) = observed_with(dir.path(), state.path(), "signed", strict);
    assert_eq!(signed_tree(dir.path(), &strictly.unwrap().value), tree);

    // A module that can't be signed fails with the reason, in the step.
    let plain = dir.path().join("example/modules/1.0/kernel/c.ko");
    fs::write(&plain, "\x7fELF module c").unwrap();
    let failed = build_in(dir.path(), state.path(), "signed").unwrap_err();
    let printed = format!("{failed:?}");
    assert!(
        printed.contains("1.0/kernel/c.ko: a module that is not compressed as .ko.zst"),
        "{printed}"
    );
    fs::remove_file(plain).unwrap();

    // With no key, the build says so, by the secret's name.
    fs::write(dir.path().join("workspace.gcsl"), RUST).unwrap();
    let failed = build_in(dir.path(), state.path(), "signed").unwrap_err();
    let printed = format!("{failed:?}");
    assert!(printed.contains("no secret `module-signing`"), "{printed}");
}
