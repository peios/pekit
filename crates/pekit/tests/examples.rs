//! Tests of the examples, runnable: each fixture under `tests/fixtures/examples`
//! built with the gcc library of `gcslib`, with the host's own toolchain and
//! with Bootlin's pinned ones.

mod building;

use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

use gcsl::eval::plain::Plain;
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
        let built = build_in(dir.path(), state.path(), target).unwrap();
        let program = file_of(&built.value, None);
        assert_eq!(run(dir.path(), program), prints, "example {n}");
        assert!(
            holds(&built_file(dir.path(), program), bootlin),
            "example {n}"
        );
        // Bootlin's `ar` records the time unless told not to, which would
        // make the library differ from one run to the next.
        if n == 2 {
            let library = dir
                .path()
                .join("pekit-out/example/greet/archive/libgreet.a");
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
         `/pekit-out/example/greeting_test/link/greeting_test` exited with status 1\npassed           \
         example/output\n1 passed, 1 failed\n",
        false,
    );
}

/// The workspace configuration of example 10: the host's C toolchain and
/// QEMU, ppk and peiso's tools at `tools/`, and the pinned Rust.
const IMAGE: &str = "import * from \"github.com/gcslib/ppk\"\n\
                     import * from \"github.com/gcslib/python\"\n\
                     import * from \"github.com/gcslib/peiso\"\n\
                     import * from \"github.com/gcslib/qemu\"\n\
                     import * from \"github.com/gcslib/rust\"\n\
                     import * from toolchain\n\
                     import fileroot\n\
                     import path\n\
                     fn built(at: String, bins: List<String>, packages: List<String>) -> Dir {\n\
                     \x20   let src = path.join(fileroot.this, \"example/{at}\")\n\
                     \x20   let ppk = {at = \"ppk\"; place = dir(path.join(fileroot.this, \"example/ppk\"))}\n\
                     \x20   = cargo_build(dir(src), vendored(file(\"{src}/Cargo.lock\")), bins = bins, packages = packages, beside = [ppk])\n\
                     }\n\
                     def Toolchain::Ppk workspace_ppk() {\n\
                     \x20   let tools = built(\"ppk\", [\"ppk\"], [])\n\
                     \x20   host_platform = ^host_platform\n\
                     \x20   platform = {}\n\
                     \x20   provides = {root = tools; runtime = dir(\"/\"); ppk = tools.file(\"ppk\")}\n\
                     }\n\
                     def Toolchain::Peiso workspace_peiso() {\n\
                     \x20   let peiso = built(\"peiso\", [\"peiso\"], [])\n\
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
    let built = build_in(dir.path(), state.path(), "iso").unwrap();
    assert!(built.ran > 9, "{}", built.ran);
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
        script.contains("-cdrom /pekit-out/example/iso/assemble/peios.iso"),
        "{script}"
    );
}

/// The workspace configuration that registers the gcc library's Bootlin
/// toolchain and the rust library's pinned Rust.
/// A git repository of two crates, `greeting` taking `words` by its path:
/// the directory it is in, its URL and its commit's name.
fn greeting_repository() -> (tempfile::TempDir, String, String) {
    // A repository of two crates, `greeting` taking `words` by its path.
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
    crate_of("words", "", "pub const HELLO: &str = \"hello from git\";\n");
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
        "import \"github.com/gcslib/rust\"\n\
         def build app() { = rust.cargo_build(dir(\"app\"), rust.vendored(file(\"app/Cargo.lock\")), bins = [\"app\"]) }\n",
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
