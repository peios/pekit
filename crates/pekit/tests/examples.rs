//! Tests of the examples, runnable: each fixture under `tests/fixtures/examples`
//! built with the gcc library of `gcslib`, with the host's own toolchain and
//! with Bootlin's pinned ones.

mod building;

use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

use gcsl::eval::plain::Plain;
use pekit::store::Store;

use building::build_in;

/// The repository's root.
fn repository() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../..")
}

/// Copies the tree at `from` to `to`, its files' contents alone.
fn copy_tree(from: &Path, to: &Path) {
    fs::create_dir_all(to).unwrap();
    for entry in fs::read_dir(from).unwrap() {
        let entry = entry.unwrap();
        let at = to.join(entry.file_name());
        if entry.file_type().unwrap().is_dir() {
            copy_tree(&entry.path(), &at);
        } else {
            fs::copy(entry.path(), at).unwrap();
        }
    }
}

/// A workspace of example `n`'s fixture, in its directory `example`, the
/// gcc library among its libraries, its `workspace.gcsl` of `config`.
fn example(n: u32, config: &str) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    let fixture = repository().join(format!("tests/fixtures/examples/{n}"));
    copy_tree(&fixture, &dir.path().join("example"));
    copy_tree(
        &repository().join("gcslib"),
        &dir.path().join(".gcsl/libraries"),
    );
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
    // The identity packing by hand gives, as example 5 records it.
    assert!(
        greet
            .join(
                "manifests/sha256/7b27eb467e0a79eb6d231a9464bb6f627f8352e8a35ea78fc432c63d213fdeca"
            )
            .is_file()
    );
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
const DOWNLOADS: [(&str, &str); 6] = [
    (
        "x86-64--glibc--stable-2026.08-1.tar.xz",
        "cde893afab04ac7dcd15c46aac214ff550441b982536124c88a71146a0eeedd3",
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

/// Whether the file at `path` holds `text`, such as its compiler's name.
fn holds(path: &Path, text: &str) -> bool {
    fs::read(path)
        .unwrap()
        .windows(text.len())
        .any(|window| window == text.as_bytes())
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
        let built = build_in(dir.path(), state.path(), target).unwrap();
        let program = file_of(&built.value, None);
        assert_eq!(run(dir.path(), program), prints, "example {n}");
        assert!(
            holds(&built_file(dir.path(), program), bootlin),
            "example {n}"
        );
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
        assert_eq!(out, expected);
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
         `/pekit-out/example/greeting/link/greeting_test` exited with status 1\npassed           \
         example/output\n1 passed, 1 failed\n",
        false,
    );
}
