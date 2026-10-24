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
/// toolchains.
const PINNED: &str = "import * from \"github.com/gcslib/gcc\"\n\
                      import Toolchain from toolchain\n\
                      pekitconf = {toolchains = [Toolchain::C gcc_x86_64, Toolchain::C \
                      gcc_x86_64_to_aarch64]}\n";

/// Bootlin's archives of the toolchains the gcc library pins, each with its
/// pin: from `$PEKIT_TEST_TOOLCHAINS`, else the Peios workspace's
/// `local/pekit2-data`; `None` if either is missing.
fn bootlin_archives() -> Option<[(PathBuf, &'static str); 2]> {
    let dir = std::env::var_os("PEKIT_TEST_TOOLCHAINS")
        .map_or_else(|| repository().join("../local/pekit2-data"), PathBuf::from);
    let archives = [
        (
            dir.join("x86-64--glibc--stable-2026.08-1.tar.xz"),
            "cde893afab04ac7dcd15c46aac214ff550441b982536124c88a71146a0eeedd3",
        ),
        (
            dir.join("aarch64--glibc--stable-2026.08-1.tar.xz"),
            "0213efac9b5577f20d58de9431960a191347ffc2257b27ffe7250522bf1f7867",
        ),
    ];
    archives
        .iter()
        .all(|(path, _)| path.exists())
        .then_some(archives)
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
    let Some(archives) = bootlin_archives() else {
        eprintln!("skipped: Bootlin's toolchain archives are not on this machine");
        return;
    };
    // One store, given the archives as a fetch would, so that each is
    // unpacked once for every example, and nothing is downloaded.
    let state = tempfile::tempdir().unwrap();
    let store = Store::open(&state.path().join("store")).unwrap();
    for (path, pin) in &archives {
        assert_eq!(store.put_file(path).unwrap().digest.hex(), *pin);
    }
    let bootlin = "toolchains.bootlin.com";
    for (n, target, prints) in [
        (1, "hello", "Hello from pekit2\n"),
        (2, "app", "Hello, pekit2\n"),
        (
            3,
            "app",
            "Hello from a file that was turned into code during the build.\n",
        ),
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
