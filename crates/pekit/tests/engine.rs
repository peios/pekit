//! Tests of building targets: steps run in their sandboxes, recorded, and
//! found in the store again.

use std::fs;
use std::path::{Path, PathBuf};

use pekit::engine::{BuildError, Built, Settings, build_observed};
use pekit::events::{Event, EventKind};
use pekit::load::load;
use pekit::platform::ExecutionPlatform;
use pekit::sandbox::HostTools;
use pekit::target::resolve;

/// The fixture module that declares what pekit's def-types and attributes
/// will be declared by (see `design/engine.md`, E9).
const STEPS: &str = "#[attribute] #[defers] type hermetic = {on: Bool = true}\n\
                     #[def_type] def build(name: String, f: fn() -> {out: File}) -> {out: File} { f() }\n";

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// The host tools a test's shell needs, which a real step never borrows.
fn host_tools() -> HostTools {
    HostTools {
        bind: ["/usr", "/bin", "/lib", "/lib64"]
            .iter()
            .map(PathBuf::from)
            .filter(|dir| dir.exists())
            .collect(),
        ..HostTools::default()
    }
}

/// A workspace whose `app` has `build.gcsl` of `text` and the files `files`.
fn workspace(text: &str, files: &[(&str, &str)]) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            ("workspace.gcsl", ""),
            ("steps.gcsl", STEPS),
            ("app/build.gcsl", text),
        ],
    );
    write(&dir.path().join("app"), files);
    dir
}

/// Builds `build <name>` of the workspace at `root`, its store and scratch
/// beside it in `state`: what it came to, or its error as shown.
fn build_in(root: &Path, state: &Path, name: &str) -> Result<Built, String> {
    observed_in(root, state, name).0
}

/// [`build_in`], with the events the build told.
fn observed_in(root: &Path, state: &Path, name: &str) -> (Result<Built, String>, Vec<Event>) {
    observed_on(root, state, name, "x86_64")
}

/// [`observed_in`], the execution platform's architecture `arch`.
fn observed_on(
    root: &Path,
    state: &Path,
    name: &str,
    arch: &str,
) -> (Result<Built, String>, Vec<Event>) {
    let arch = arch.to_owned();
    let root = root.to_path_buf();
    let state = state.to_path_buf();
    let name = name.to_owned();
    std::thread::Builder::new()
        .stack_size(gcsl::STACK_SIZE)
        .spawn(move || {
            let mut events = Vec::new();
            let mut loaded = match load(&root) {
                Ok(loaded) => loaded,
                Err(error) => return (Err(error.render()), events),
            };
            let target = match resolve(&loaded.workspace, ("build", &name), "") {
                Ok(target) => target,
                Err(error) => return (Err(error.message()), events),
            };
            let settings = Settings {
                store: state.join("store"),
                outputs: root.join("pekit-out"),
                scratch: state.join("scratch"),
                jobs: 4,
                platform: format!("{arch}-linux"),
                execution: ExecutionPlatform {
                    arch,
                    os: "linux".to_owned(),
                },
                host_tools: host_tools(),
                reuse: true,
            };
            let built = build_observed(&mut loaded, &target, &settings, &mut |event| {
                events.push(event.clone());
            });
            (
                built.map_err(|error: BuildError| error.render(&loaded)),
                events,
            )
        })
        .unwrap()
        .join()
        .unwrap()
}

/// A step that shouts its input file into its output.
const SHOUT: &str = "#[hermetic] fn shout(src: File) -> File {\n\
                     \x20   let r = exec([\"/bin/sh\", \"-c\", \"tr a-z A-Z < {src.path} > out.txt\"])\n\
                     \x20   file(\"out.txt\")\n\
                     }\n";

#[test]
fn a_step_runs_once_and_is_then_found_in_the_store() {
    let text = format!("{SHOUT}def build hello() {{ out = shout(file(\"name.txt\")) }}\n");
    let dir = workspace(&text, &[("name.txt", "world\n")]);
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((built.ran, built.cached), (1, 0));
    let out = dir.path().join("pekit-out/app/hello/shout/out.txt");
    assert_eq!(fs::read_to_string(&out).unwrap(), "WORLD\n");
    // Again: found by its receipt, and copied out again.
    fs::remove_dir_all(dir.path().join("pekit-out")).unwrap();
    let again = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((again.ran, again.cached), (0, 1));
    assert_eq!(again.value, built.value);
    assert_eq!(fs::read_to_string(&out).unwrap(), "WORLD\n");
    // Its input changed: run again.
    fs::write(dir.path().join("app/name.txt"), "there\n").unwrap();
    let changed = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((changed.ran, changed.cached), (1, 0));
    assert_eq!(fs::read_to_string(&out).unwrap(), "THERE\n");
    // No staging directory is left behind.
    let left: Vec<_> = fs::read_dir(state.path().join("scratch"))
        .unwrap()
        .flatten()
        .filter(|entry| entry.file_name().to_string_lossy().starts_with("step-"))
        .collect();
    assert!(left.is_empty(), "{left:?}");
}

#[test]
fn a_step_is_given_an_earlier_step_s_output() {
    let text = format!(
        "{SHOUT}#[hermetic] fn twice(src: File) -> File {{\n\
         \x20   let r = exec([\"/bin/sh\", \"-c\", \"cat {{src.path}} {{src.path}} > out.txt\"])\n\
         \x20   file(\"out.txt\")\n\
         }}\n\
         def build hello() {{ out = twice(shout(file(\"name.txt\"))) }}\n"
    );
    let dir = workspace(&text, &[("name.txt", "hi\n")]);
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!(built.ran, 2);
    let out = dir.path().join("pekit-out/app/hello/twice/out.txt");
    assert_eq!(fs::read_to_string(out).unwrap(), "HI\nHI\n");
}

#[test]
fn a_step_that_fails_fails_the_build_with_its_own_error() {
    let text = "#[hermetic] #[retry(2)] fn broken() -> File {\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"sleep 0.3; echo nope >&2; exit 3\"])\n\
                \x20   file(\"out.txt\")\n\
                }\n\
                def build hello() { out = broken() }\n";
    let dir = workspace(text, &[]);
    let state = tempfile::tempdir().unwrap();
    let started = std::time::Instant::now();
    let error = build_in(dir.path(), state.path(), "hello").unwrap_err();
    // Its own error, at its call, with what it printed.
    assert!(
        error.starts_with("error: `/bin/sh` exited with status 3\n"),
        "{error}"
    );
    assert!(error.contains("app/build.gcsl:2:13"), "{error}");
    assert!(error.contains("its standard error: nope"), "{error}");
    // Run three times, each afresh.
    assert!(
        started.elapsed() >= std::time::Duration::from_millis(900),
        "{error}"
    );
}

#[test]
fn a_step_s_timeout_stops_it() {
    let text = "#[hermetic] #[timeout(1s)] fn slow() -> File {\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"sleep 20\"])\n\
                \x20   file(\"out.txt\")\n\
                }\n\
                def build hello() { out = slow() }\n";
    let dir = workspace(text, &[]);
    let state = tempfile::tempdir().unwrap();
    let started = std::time::Instant::now();
    let error = build_in(dir.path(), state.path(), "hello").unwrap_err();
    assert!(started.elapsed().as_secs() < 10, "{error}");
    assert!(error.starts_with("error: the step timed out\n"), "{error}");
}

/// A step that joins two files into its output.
const JOIN: &str = "#[hermetic] fn join(a: File, b: File) -> File {\n\
                    \x20   let r = exec([\"/bin/sh\", \"-c\", \"cat {a.path} {b.path} > out.txt\"])\n\
                    \x20   file(\"out.txt\")\n\
                    }\n";

#[test]
fn steps_of_one_function_keep_their_names_whichever_is_cached() {
    let text = format!(
        "{SHOUT}{JOIN}def build hello() {{ out = join(shout(file(\"a.txt\")), shout(file(\"b.txt\"))) }}\n"
    );
    let dir = workspace(&text, &[("a.txt", "a\n"), ("b.txt", "b\n")]);
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((built.ran, built.cached), (3, 0));
    let out = dir.path().join("pekit-out/app/hello/join/out.txt");
    assert_eq!(fs::read_to_string(&out).unwrap(), "A\nB\n");
    // The second changed: the first found, the second run, each under its
    // own name.
    fs::write(dir.path().join("app/b.txt"), "c\n").unwrap();
    let again = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((again.ran, again.cached), (2, 1));
    assert_eq!(fs::read_to_string(&out).unwrap(), "A\nC\n");
}

#[test]
fn a_step_whose_result_holds_a_secret_runs_each_time_and_keeps_its_files() {
    let text = format!(
        "{SHOUT}#[hermetic] fn sealed(src: File) -> {{out: File, key: Secret<String>}} {{\n\
         \x20   let r = exec([\"/bin/sh\", \"-c\", \"cp {{src.path}} out.txt\"])\n\
         \x20   {{out = file(\"out.txt\"); key = secret(\"k3y\", \"test key\")}}\n\
         }}\n\
         def build hello() {{ out = shout(sealed(file(\"name.txt\")).out) }}\n"
    );
    let dir = workspace(&text, &[("name.txt", "hidden\n")]);
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((built.ran, built.cached), (2, 0));
    let out = dir.path().join("pekit-out/app/hello/shout/out.txt");
    assert_eq!(fs::read_to_string(&out).unwrap(), "HIDDEN\n");
    // Never recorded, so run again; the step after it is found.
    let again = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((again.ran, again.cached), (1, 1));
    assert_eq!(fs::read_to_string(&out).unwrap(), "HIDDEN\n");
}

/// The events `events` told, but their times, and effects but their nodes,
/// threads and times.
fn kinds(events: &[Event]) -> Vec<String> {
    events
        .iter()
        .map(|event| match &event.kind {
            EventKind::EffectQueued { step, effect, .. } => {
                format!("queued {effect} of {step:?}")
            }
            EventKind::EffectRan {
                step, effect, ok, ..
            } => format!("ran {effect} of {step:?}, ok {ok}"),
            other => format!("{other:?}"),
        })
        .collect()
}

#[test]
fn a_build_tells_each_step_s_life_and_each_program_it_runs() {
    let text = format!(
        "{SHOUT}#[hermetic] fn copy(src: File) -> File {{\n\
         \x20   let r = exec([\"cp\", src.path, \"out.txt\"])\n\
         \x20   file(\"out.txt\")\n\
         }}\n\
         def build hello() {{ out = copy(shout(file(\"name.txt\"))) }}\n"
    );
    let dir = workspace(&text, &[("name.txt", "hi\n")]);
    let state = tempfile::tempdir().unwrap();
    let (built, events) = observed_in(dir.path(), state.path(), "hello");
    built.unwrap();
    assert_eq!(
        kinds(&events),
        [
            "Discovered { step: \"shout\" }",
            "Discovered { step: \"copy\" }",
            "Started { step: \"shout\", attempt: 0 }",
            "queued exec sh of Some(\"shout\")",
            "ran exec sh of Some(\"shout\"), ok true",
            "Finished { step: \"shout\" }",
            "Started { step: \"copy\", attempt: 0 }",
            "queued exec cp of Some(\"copy\")",
            "ran exec cp of Some(\"copy\"), ok true",
            "Finished { step: \"copy\" }",
        ]
    );
    // Times only grow, and a program ran within its own span.
    assert!(events.windows(2).all(|pair| pair[0].at <= pair[1].at));
    for event in &events {
        if let EventKind::EffectRan { began, .. } = event.kind {
            assert!(began <= event.at);
        }
    }
    // Again: each found in the store.
    let (again, events) = observed_in(dir.path(), state.path(), "hello");
    again.unwrap();
    assert_eq!(
        kinds(&events),
        [
            "Discovered { step: \"shout\" }",
            "Discovered { step: \"copy\" }",
            "Cached { step: \"shout\" }",
            "Cached { step: \"copy\" }",
        ]
    );
}

#[test]
fn a_target_is_evaluated_on_its_execution_platform() {
    let text = "type Platform = {arch: String, os: String}\n\
                context host_platform: Platform = {arch = \"none\"; os = \"none\"}\n\
                context platform: Platform = {...^host_platform}\n\
                #[hermetic] fn arch(a: String) -> File {\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"echo {a} > out.txt\"])\n\
                \x20   file(\"out.txt\")\n\
                }\n\
                def build hello() { out = arch(\"{^host_platform.arch} for {^platform.arch}\") }\n";
    let dir = workspace(text, &[]);
    let state = tempfile::tempdir().unwrap();
    observed_on(dir.path(), state.path(), "hello", "aarch64")
        .0
        .unwrap();
    let out = dir.path().join("pekit-out/app/hello/arch/out.txt");
    assert_eq!(fs::read_to_string(&out).unwrap(), "aarch64 for aarch64\n");
    // On another, another step.
    let other = observed_on(dir.path(), state.path(), "hello", "riscv64")
        .0
        .unwrap();
    assert_eq!((other.ran, other.cached), (1, 0));
    assert_eq!(fs::read_to_string(&out).unwrap(), "riscv64 for riscv64\n");
}

#[test]
fn a_step_s_result_names_only_its_own_files_or_those_it_was_given() {
    let text = "#[hermetic] fn stray() -> File {\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"echo x > /tmp/x\"])\n\
                \x20   file(\"/tmp/x\")\n\
                }\n\
                def build hello() { out = stray() }\n";
    let dir = workspace(text, &[]);
    let state = tempfile::tempdir().unwrap();
    let error = build_in(dir.path(), state.path(), "hello").unwrap_err();
    assert!(
        error.contains("names `/tmp/x`, which is neither in its output directory"),
        "{error}"
    );
    // A file it was given, passed on, is its to name.
    let text = "#[hermetic] fn pass(src: File) -> File { src }\n\
                def build hello() { out = pass(file(\"name.txt\")) }\n";
    let dir = workspace(text, &[("name.txt", "n\n")]);
    let built = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!(built.ran, 1);
}

#[test]
fn steps_run_commands_in_sandboxes_of_their_own_directories() {
    let busybox = Path::new("/usr/bin/busybox");
    if !busybox.exists() {
        eprintln!("skipped: no static busybox on this host");
        return;
    }
    let text = "#[hermetic] fn inside(root: Dir, word: String) -> File {\n\
                \x20   let s = root.sandbox()\n\
                \x20   let r = s.exec(\"/bin/sh\", \"-c\", \"echo {word} > /made.txt\")\n\
                \x20   s.file(\"/made.txt\")\n\
                }\n\
                #[hermetic] fn join(a: File, b: File) -> File {\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"cat {a.path} {b.path} > out.txt\"])\n\
                \x20   file(\"out.txt\")\n\
                }\n\
                def build hello() {\n\
                \x20   out = join(inside(dir(\"root\"), \"one\"), inside(dir(\"root\"), \"two\"))\n\
                }\n";
    let dir = workspace(text, &[]);
    let bin = dir.path().join("app/root/bin");
    fs::create_dir_all(&bin).unwrap();
    fs::copy(busybox, bin.join("busybox")).unwrap();
    std::os::unix::fs::symlink("busybox", bin.join("sh")).unwrap();
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!(built.ran, 3);
    let out = dir.path().join("pekit-out/app/hello/join/out.txt");
    assert_eq!(fs::read_to_string(out).unwrap(), "one\ntwo\n");
}

#[test]
fn a_step_that_needs_a_failed_step_s_result_fails_with_its_error_unrun() {
    let text = format!(
        "{SHOUT}{JOIN}#[hermetic] fn broken() -> File {{\n\
         \x20   let r = exec([\"/bin/sh\", \"-c\", \"exit 4\"])\n\
         \x20   file(\"out.txt\")\n\
         }}\n\
         def build hello() {{ out = join(broken(), shout(file(\"name.txt\"))) }}\n"
    );
    let dir = workspace(&text, &[("name.txt", "n\n")]);
    let state = tempfile::tempdir().unwrap();
    let (built, events) = observed_in(dir.path(), state.path(), "hello");
    let error = built.unwrap_err();
    assert!(
        error.starts_with("error: `/bin/sh` exited with status 4"),
        "{error}"
    );
    // `join` never started.
    assert!(
        !events
            .iter()
            .any(|event| matches!(&event.kind, EventKind::Started { step, .. } if step == "join")),
        "{events:?}"
    );
}

#[test]
fn a_step_s_compositions_link_its_inputs_and_no_program_changes_them() {
    let text = "import fs\n\
                #[hermetic] fn probe(src: File) -> File {\n\
                \x20   let d = fs.compose_dir({\"in/x\" = src})\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"stat -c %h {d.path}/in/x > out.txt; \
                (echo y > {d.path}/new || echo refused) >> out.txt 2>/dev/null; \
                (echo y >> {d.path}/in/x || echo refused) >> out.txt 2>/dev/null\"])\n\
                \x20   file(\"out.txt\")\n\
                }\n\
                def build hello() { out = probe(file(\"x.txt\")) }\n";
    let dir = workspace(text, &[("x.txt", "x\n")]);
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "hello").unwrap();
    let out = dir.path().join("pekit-out/app/hello/probe/out.txt");
    let lines: Vec<String> = fs::read_to_string(out)
        .unwrap()
        .lines()
        .map(str::to_owned)
        .collect();
    // Linked, not copied: the store's blob, its staged input and the
    // composition's entry are one file.
    assert!(lines[0].parse::<u32>().unwrap() >= 3, "{lines:?}");
    assert_eq!(lines[1..], ["refused", "refused"]);
}

#[test]
fn a_step_s_sandbox_is_an_overlay_read_upper_over_lower() {
    let busybox = Path::new("/usr/bin/busybox");
    if !busybox.exists() {
        eprintln!("skipped: no static busybox on this host");
        return;
    }
    let text = "#[hermetic] fn inside(root: Dir) -> {made: File, bin: Dir, sub: Dir} {\n\
                \x20   let s = root.sandbox()\n\
                \x20   let a = s.exec(\"/bin/sh\", \"-c\", \"echo one > /made.txt; echo new > /bin/extra\")\n\
                \x20   let b = s.exec(\"/bin/sh\", \"-c\", \"echo two >> /made.txt; mv /bin/old /old; mkdir /sub; echo s > /sub/f\")\n\
                \x20   made = s.file(\"/made.txt\")\n\
                \x20   bin = s.dir(\"/bin\")\n\
                \x20   sub = s.dir(\"/sub\")\n\
                }\n\
                #[hermetic] fn pick(i: {made: File, bin: Dir, sub: Dir}) -> File {\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"cat {i.made.path} > out.txt; ls {i.bin.path} >> out.txt; ls {i.sub.path} >> out.txt\"])\n\
                \x20   file(\"out.txt\")\n\
                }\n\
                def build hello() { out = pick(inside(dir(\"root\"))) }\n";
    let dir = workspace(text, &[("root/bin/old", "old\n")]);
    let bin = dir.path().join("app/root/bin");
    fs::copy(busybox, bin.join("busybox")).unwrap();
    std::os::unix::fs::symlink("busybox", bin.join("sh")).unwrap();
    std::os::unix::fs::symlink("busybox", bin.join("mv")).unwrap();
    std::os::unix::fs::symlink("busybox", bin.join("mkdir")).unwrap();
    let state = tempfile::tempdir().unwrap();
    build_in(dir.path(), state.path(), "hello").unwrap();
    let out = dir.path().join("pekit-out/app/hello/pick/out.txt");
    // Each command's writes kept for the next; `/bin` the lower's and the
    // upper's, but what was moved away; `/sub` the upper's alone.
    assert_eq!(
        fs::read_to_string(out).unwrap(),
        "one\ntwo\nbusybox\nextra\nmkdir\nmv\nsh\nf\n"
    );
    // The workspace's directory is untouched.
    assert!(dir.path().join("app/root/bin/old").exists());
    assert!(!dir.path().join("app/root/made.txt").exists());
}

#[test]
fn a_sandbox_of_what_the_step_changes_is_a_copy() {
    let busybox = Path::new("/usr/bin/busybox");
    if !busybox.exists() {
        eprintln!("skipped: no static busybox on this host");
        return;
    }
    let text = "#[hermetic] fn inside(bb: File) -> File {\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"mkdir -p made/bin && cp {bb.path} made/bin/sh\"])\n\
                \x20   let s = dir(\"made\").sandbox()\n\
                \x20   let w = s.exec(\"/bin/sh\", \"-c\", \"echo copied > /x\")\n\
                \x20   s.file(\"/x\")\n\
                }\n\
                def build hello() { out = inside(file(\"busybox\")) }\n";
    let dir = workspace(text, &[]);
    fs::copy(busybox, dir.path().join("app/busybox")).unwrap();
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "hello").unwrap();
    let [(path, _)] = built.value.files()[..] else {
        panic!("{:?}", built.value);
    };
    let out = dir.path().join(path.trim_start_matches('/'));
    assert_eq!(fs::read_to_string(out).unwrap(), "copied\n");
}
