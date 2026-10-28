//! Compiles the vendored protocol definitions in `proto/` with `protox`, so
//! that building pekit needs no `protoc`, into gRPC clients.

use std::error::Error;

fn main() -> Result<(), Box<dyn Error>> {
    let files = [
        "proto/build/bazel/remote/execution/v2/remote_execution.proto",
        "proto/google/bytestream/bytestream.proto",
    ];
    for file in files {
        println!("cargo:rerun-if-changed={file}");
    }
    println!("cargo:rerun-if-changed=proto");
    let mut descriptors = protox::compile(files, ["proto"])?;
    // The definitions' comments are written for their own documentation,
    // not Rust's, so the generated code has none.
    for file in &mut descriptors.file {
        for location in file
            .source_code_info
            .iter_mut()
            .flat_map(|info| &mut info.location)
        {
            location.leading_comments = None;
            location.trailing_comments = None;
            location.leading_detached_comments.clear();
        }
    }
    // Servers too, for pekit's tests to serve a cache of their own.
    tonic_prost_build::configure().compile_fds(descriptors)?;
    Ok(())
}
