//! Tests of building targets: steps run in their sandboxes, recorded, and
//! found in the store again.

mod building;
mod support;

use std::fs;
use std::path::Path;

use pekit::events::{Event, EventKind};

use building::{Options, build_in, observed_with};

/// Writes `files`, each a path relative to `root` and its text.
fn write(root: &Path, files: &[(&str, &str)]) {
    for (path, text) in files {
        let path = root.join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
    }
}

/// A workspace whose `app` has `build.gcsl` of `text` and the files `files`.
fn workspace(text: &str, files: &[(&str, &str)]) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[("workspace.gcsl", ""), ("app/build.gcsl", text)],
    );
    write(&dir.path().join("app"), files);
    dir
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
    let (built, events) = observed_with(dir.path(), state.path(), "hello", Options::default());
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
    let (again, events) = observed_with(dir.path(), state.path(), "hello", Options::default());
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
    // The prelude's contexts.
    let text = "#[hermetic] fn arch(a: String) -> File {\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"echo {a} > out.txt\"])\n\
                \x20   file(\"out.txt\")\n\
                }\n\
                def build hello() { out = arch(\"{^host_platform.arch} for {^platform.arch}\") }\n";
    let dir = workspace(text, &[]);
    let state = tempfile::tempdir().unwrap();
    observed_with(
        dir.path(),
        state.path(),
        "hello",
        Options {
            arch: "aarch64",
            ..Options::default()
        },
    )
    .0
    .unwrap();
    let out = dir.path().join("pekit-out/app/hello/arch/out.txt");
    assert_eq!(fs::read_to_string(&out).unwrap(), "aarch64 for aarch64\n");
    // On another, another step.
    let other = observed_with(
        dir.path(),
        state.path(),
        "hello",
        Options {
            arch: "riscv64",
            ..Options::default()
        },
    )
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
    let (built, events) = observed_with(dir.path(), state.path(), "hello", Options::default());
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

#[test]
fn a_fetched_file_is_given_to_steps_and_fetched_once() {
    let (url, served) = support::serve(b"fetched\n", "name.txt");
    let pin = pekit::store::Digest::of(b"fetched\n").hex();
    let text = format!(
        "{SHOUT}def build hello() {{ out = shout(fetch(\"{url}\", sha256 = \"{pin}\")) }}\n"
    );
    let dir = workspace(&text, &[]);
    let state = tempfile::tempdir().unwrap();
    let built = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!(built.ran, 1);
    let out = dir.path().join("pekit-out/app/hello/shout/out.txt");
    assert_eq!(fs::read_to_string(&out).unwrap(), "FETCHED\n");
    // Again: the fetch from the store, the step from its record.
    let again = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((again.ran, again.cached), (0, 1));
    assert_eq!(served.load(std::sync::atomic::Ordering::SeqCst), 1);
}

#[test]
fn a_hermetic_step_cannot_fetch() {
    let (url, served) = support::serve(b"x", "x");
    let text = format!(
        "#[hermetic] fn get() -> File {{ fetch(\"{url}\", sha256 = \"{}\") }}\n\
         def build hello() {{ out = get() }}\n",
        "0".repeat(64)
    );
    let dir = workspace(&text, &[]);
    let state = tempfile::tempdir().unwrap();
    let error = build_in(dir.path(), state.path(), "hello").unwrap_err();
    assert!(
        error.starts_with("error: `fetch` runs outside hermetic steps"),
        "{error}"
    );
    assert!(error.contains("app/build.gcsl:1:"), "{error}");
    assert_eq!(served.load(std::sync::atomic::Ordering::SeqCst), 0);
}

/// A gzipped tar archive of `top/x.txt`, holding `text`.
fn archive_holding(text: &str) -> Vec<u8> {
    let mut builder = tar::Builder::new(Vec::new());
    let mut header = tar::Header::new_gnu();
    header.set_size(text.len() as u64);
    header.set_mode(0o644);
    builder
        .append_data(&mut header, "top/x.txt", text.as_bytes())
        .unwrap();
    let tar = builder.into_inner().unwrap();
    let mut encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
    std::io::Write::write_all(&mut encoder, &tar).unwrap();
    encoder.finish().unwrap()
}

#[test]
fn an_archive_is_unpacked_for_a_step_or_by_one() {
    let text = "import archive\n\
                #[hermetic] fn read(d: Dir) -> File {\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"cat {d.path}/x.txt > out.txt\"])\n\
                \x20   file(\"out.txt\")\n\
                }\n\
                #[hermetic] fn inner(a: File) -> File {\n\
                \x20   let d = archive.unpack(a, strip = 1)\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"cat {d.path}/x.txt {d.path}/x.txt > out.txt\"])\n\
                \x20   file(\"out.txt\")\n\
                }\n\
                def build hello() {\n\
                \x20   out = [read(archive.unpack(file(\"src.tar.gz\"), strip = 1)), inner(file(\"src.tar.gz\"))]\n\
                }\n";
    let dir = workspace(text, &[]);
    fs::write(
        dir.path().join("app/src.tar.gz"),
        archive_holding("inside\n"),
    )
    .unwrap();
    let state = tempfile::tempdir().unwrap();
    let built =
        build_in(dir.path(), state.path(), "hello").unwrap_or_else(|error| panic!("{error}"));
    assert_eq!(built.ran, 2);
    let read = dir.path().join("pekit-out/app/hello/read/out.txt");
    assert_eq!(fs::read_to_string(read).unwrap(), "inside\n");
    let inner = dir.path().join("pekit-out/app/hello/inner/out.txt");
    assert_eq!(fs::read_to_string(inner).unwrap(), "inside\ninside\n");
}

#[test]
fn the_target_reads_a_step_s_outputs_as_the_build_goes() {
    // Example 9's dynamic step: what the target does next depends on what
    // a step made, which it lists mid-build.
    let text = "#[hermetic] fn gen() -> Dir {\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"mkdir out && echo 1 > out/a.c && echo 2 > out/b.c\"])\n\
                \x20   dir(\"out\")\n\
                }\n\
                #[hermetic] fn twice(f: File) -> File {\n\
                \x20   let r = exec([\"/bin/sh\", \"-c\", \"cat {f.path} {f.path} > out.txt\"])\n\
                \x20   file(\"out.txt\")\n\
                }\n\
                def build hello() {\n\
                \x20   out = for f in gen().files() { twice(f) }\n\
                }\n";
    let dir = workspace(text, &[]);
    let state = tempfile::tempdir().unwrap();
    let built =
        build_in(dir.path(), state.path(), "hello").unwrap_or_else(|error| panic!("{error}"));
    assert_eq!(built.ran, 3);
    let outputs: Vec<String> = built
        .value
        .files()
        .iter()
        .map(|(path, _)| fs::read_to_string(dir.path().join(path.trim_start_matches('/'))).unwrap())
        .collect();
    assert_eq!(outputs, ["1\n1\n", "2\n2\n"]);
}

#[test]
fn a_step_is_given_the_toolchain_the_workspace_registers() {
    let dir = tempfile::tempdir().unwrap();
    write(
        dir.path(),
        &[
            (
                "workspace.gcsl",
                "import fileroot\n\
                 import Toolchain from toolchain\n\
                 def Toolchain::C fake() {\n\
                 \x20   let tc = \"{fileroot.this}/tc\"\n\
                 \x20   host_platform = {}\n\
                 \x20   platform = {}\n\
                 \x20   provides = {root = dir(tc); runtime = dir(\"/\"); cc = file(\"{tc}/cc\"); \
                 ar = file(\"{tc}/cc\"); ld = file(\"{tc}/cc\"); sysroot = dir(tc)}\n\
                 }\n\
                 pekitconf = {toolchains = [Toolchain::C fake]}\n",
            ),
            (
                "tc/cc",
                "#!/bin/sh\necho \"compiled by fake: $(cat $1)\" > out.o\n",
            ),
            (
                "app/build.gcsl",
                "import toolchain\n\
                 import Toolchain from toolchain\n\
                 #[hermetic] fn compile(src: File, cchain: toolchain.CToolchain = toolchain.get(Toolchain::C)) -> File {\n\
                 \x20   let r = exec([\"/bin/sh\", cchain.cc.path, src.path])\n\
                 \x20   file(\"out.o\")\n\
                 }\n\
                 def build hello() { out = compile(file(\"main.c\")) }\n",
            ),
            ("app/main.c", "int main;\n"),
        ],
    );
    let state = tempfile::tempdir().unwrap();
    let built =
        build_in(dir.path(), state.path(), "hello").unwrap_or_else(|error| panic!("{error}"));
    assert_eq!(built.ran, 1);
    let out = dir.path().join("pekit-out/app/hello/compile/out.o");
    assert_eq!(
        fs::read_to_string(&out).unwrap(),
        "compiled by fake: int main;\n"
    );
    // The toolchain is an input: a new compiler, a new step.
    fs::write(
        dir.path().join("tc/cc"),
        "#!/bin/sh\necho \"compiled again: $(cat $1)\" > out.o\n",
    )
    .unwrap();
    let again = build_in(dir.path(), state.path(), "hello").unwrap();
    assert_eq!((again.ran, again.cached), (1, 0));
    assert_eq!(
        fs::read_to_string(&out).unwrap(),
        "compiled again: int main;\n"
    );
}

#[test]
fn with_no_toolchain_registered_a_step_compiles_with_the_host_s() {
    let has_cc = std::process::Command::new("cc")
        .arg("--version")
        .output()
        .is_ok();
    if !has_cc {
        eprintln!("skipped: no cc on this host");
        return;
    }
    let text = "import toolchain\n\
                import Toolchain from toolchain\n\
                #[hermetic] fn compile(src: File, cchain: toolchain.CToolchain = toolchain.get(Toolchain::C)) -> File {\n\
                \x20   let r = exec([cchain.cc.path, \"-o\", \"hello\", src.path])\n\
                \x20   file(\"hello\")\n\
                }\n\
                def build hello() { out = compile(file(\"hello.c\")) }\n";
    let dir = workspace(
        text,
        &[(
            "hello.c",
            "#include <stdio.h>\nint main(void) { puts(\"hi\"); return 0; }\n",
        )],
    );
    let state = tempfile::tempdir().unwrap();
    // The host's own tools, as a host snapshot gives them.
    let built =
        build_in(dir.path(), state.path(), "hello").unwrap_or_else(|error| panic!("{error}"));
    assert_eq!(built.ran, 1);
    let binary = dir.path().join("pekit-out/app/hello/compile/hello");
    let run = std::process::Command::new(binary).output().unwrap();
    assert_eq!(String::from_utf8_lossy(&run.stdout), "hi\n");
}
