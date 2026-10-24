//! The execution platform: where a build's steps run.
//!
//! # Platforms
//!
//! A step runs on its *execution platform*, which until remote execution
//! (layer F) is the machine pekit runs on (see `design/engine.md`, E7). It
//! is a platform record, `{arch = "x86_64"; os = "linux"}`, the shape the
//! libraries' `Platform` type has.
//!
//! Every target is evaluated with `^host_platform` set at its root to its
//! execution platform, as if wrapped in `with {host_platform = ...}`, where
//! the workspace declares that context: a library does, with the
//! toolchains that read it. What it builds for, `^platform`, defaults to
//! it. Every receipt names the execution platform too, with the tools steps
//! run with ([The host snapshot](crate::host#the-host-snapshot)), so that a
//! step's result is never reused on a machine of another kind.

use gcsl::eval::plain::Plain;

/// The name of the context that holds the execution platform.
pub const HOST_PLATFORM: &str = "host_platform";

/// The context of the platform being built for, which `--platform` sets.
pub const PLATFORM: &str = "platform";

/// An execution platform (see [Platforms](#platforms)).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ExecutionPlatform {
    /// Its architecture, as Rust names it: `x86_64`, `aarch64`.
    pub arch: String,
    /// Its operating system, as Rust names it: `linux`.
    pub os: String,
}

impl ExecutionPlatform {
    /// The machine pekit runs on.
    #[must_use]
    pub fn of_host() -> Self {
        Self {
            arch: std::env::consts::ARCH.to_owned(),
            os: std::env::consts::OS.to_owned(),
        }
    }

    /// The platform as receipts name it: `x86_64-linux`.
    #[must_use]
    pub fn describe(&self) -> String {
        format!("{}-{}", self.arch, self.os)
    }

    /// The platform as a GCSL platform record, `{arch; os}`.
    #[must_use]
    pub fn plain(&self) -> Plain {
        Plain::Record(vec![
            ("arch".to_owned(), Plain::Str(self.arch.clone())),
            ("os".to_owned(), Plain::Str(self.os.clone())),
        ])
    }
}
