//! What the tests that build targets share: building one, as `pekit build`
//! does, its store and scratch in a directory of the test's.

use std::path::Path;

use pekit::engine::{BuildError, Built, Settings, build_observed};
use pekit::events::Event;
use pekit::load::load;
use pekit::platform::ExecutionPlatform;
use pekit::sandbox::HostTools;
use pekit::target::resolve;

/// The host tools steps borrow until a pinned build environment is given:
/// the host snapshot's, as `pekit build` gives them.
pub fn host_tools() -> HostTools {
    pekit::host::HostSnapshot::of_host().tools()
}

/// Builds `build <name>` of the workspace at `root`, its store and scratch
/// beside it in `state`: what it came to, or its error as shown.
pub fn build_in(root: &Path, state: &Path, name: &str) -> Result<Built, String> {
    observed_in(root, state, name).0
}

/// [`build_in`], with the events the build told.
pub fn observed_in(root: &Path, state: &Path, name: &str) -> (Result<Built, String>, Vec<Event>) {
    observed_on(root, state, name, "x86_64")
}

/// [`observed_in`], the execution platform's architecture `arch`.
pub fn observed_on(
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
