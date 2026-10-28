//! What the tests that build targets share: building one, as `pekit build`
//! does, its store and scratch in a directory of the test's.

use std::path::Path;

use pekit::engine::{BuildError, Built, Reuse, Settings, build_observed};
use pekit::events::Event;
use pekit::exposure::Exposing;
use pekit::load::load;
use pekit::platform::ExecutionPlatform;
use pekit::sandbox::HostTools;
use pekit::target::resolve;

/// The host tools steps borrow until a pinned build environment is given:
/// the host snapshot's, as `pekit build` gives them.
pub fn host_tools() -> HostTools {
    pekit::host::HostSnapshot::of_host().tools()
}

/// How a test builds: on the execution platform's architecture `arch`,
/// with the keychain's profile `profile`, and strictly or not.
#[derive(Clone, Copy, Debug)]
pub struct Options<'a> {
    /// The execution platform's architecture.
    pub arch: &'a str,
    /// The keychain's profile.
    pub profile: &'a str,
    /// Whether it builds as `pekit build --strict` does: steps borrow no
    /// host tools, and a target's own code runs no program.
    pub strict: bool,
    /// How it reuses what the store holds.
    pub reuse: Reuse,
    /// How large the store may be as it ends, if it has a limit.
    pub cache_limit: Option<u64>,
    /// Which outputs it writes under `pekit-out/`.
    pub exposing: Exposing,
}

impl Default for Options<'_> {
    fn default() -> Self {
        Self {
            arch: "x86_64",
            profile: "dev",
            strict: false,
            reuse: Reuse::Store,
            cache_limit: None,
            exposing: Exposing::AsNeeded,
        }
    }
}

/// The settings a test builds the workspace at `root` with, its store and
/// scratch in `state`, on the architecture `arch`, with the keychain's
/// profile `profile`.
pub fn settings(root: &Path, state: &Path, (arch, profile): (&str, &str)) -> Settings {
    Settings {
        store: state.join("store"),
        outputs: root.join("pekit-out"),
        scratch: state.join("scratch"),
        jobs: 4,
        platform: format!("{arch}-linux"),
        execution: ExecutionPlatform {
            arch: arch.to_owned(),
            os: "linux".to_owned(),
        },
        host_tools: host_tools(),
        reuse: Reuse::Store,
        profile: profile.to_owned(),
        state: state.join("state"),
        building_for: None,
        no_dynamic: false,
        strict: false,
        cache_limit: None,
        profile_gcsl: false,
        exposing: Exposing::AsNeeded,
    }
}

/// Builds `build <name>` of the workspace at `root`, its store and scratch
/// beside it in `state`: what it came to, or its error as shown.
pub fn build_in(root: &Path, state: &Path, name: &str) -> Result<Built, String> {
    observed_with(root, state, name, Options::default()).0
}

/// [`build_in`] as `options` say, with the events the build told.
pub fn observed_with(
    root: &Path,
    state: &Path,
    name: &str,
    Options {
        arch,
        profile,
        strict,
        reuse,
        cache_limit,
        exposing,
    }: Options<'_>,
) -> (Result<Built, String>, Vec<Event>) {
    let arch = arch.to_owned();
    let profile = profile.to_owned();
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
            let mut settings = settings(&root, &state, (&arch, &profile));
            settings.reuse = reuse;
            settings.cache_limit = cache_limit;
            settings.exposing = exposing;
            if strict {
                settings.strict = true;
                settings.host_tools = HostTools::default();
                settings.platform.push_str(" no-host-tools");
            }
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
