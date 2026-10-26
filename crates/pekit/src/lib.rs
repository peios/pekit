//! Pekit: a build system whose build files are GCSL, run as an action graph.
//!
//! Pekit loads a workspace of GCSL files ([`load`]), finds the target
//! someone names ([`target`]), evaluates it in GCSL's graph mode, and runs
//! the steps it needs, each hermetic one in a sandbox holding only its
//! inputs, from a content-addressed cache where it has them (see the
//! design, `design/engine.md`).

pub mod cli;
pub mod engine;
pub mod events;
pub mod fetch;
pub mod host;
pub mod keychain;
pub mod layers;
pub mod load;
pub mod lock;
pub mod platform;
pub mod pool;
pub mod programs;
pub mod query;
pub mod receipt;
pub mod record;
pub mod release;
pub mod sandbox;
pub mod scratch;
pub mod store;
pub mod target;
pub mod trace;
pub mod unpack;
