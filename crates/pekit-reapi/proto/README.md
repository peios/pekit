# Vendored protocol definitions

pekit's remote cache speaks Bazel's Remote Execution API (REAPI) over gRPC.
`build.rs` compiles these files with `protox`, so building pekit needs no
`protoc`. They are copied unchanged from these sources:

| Files | Source | Commit | License |
|---|---|---|---|
| `build/bazel/remote/execution/v2/remote_execution.proto`, `build/bazel/semver/semver.proto` | [bazelbuild/remote-apis](https://github.com/bazelbuild/remote-apis), tag `v2.12.0` | `9e084d0e43e717128ee72b5be584a7ba33e8006b` | Apache 2.0, `LICENSE.remote-apis` |
| `google/api/*.proto`, `google/bytestream/bytestream.proto`, `google/longrunning/operations.proto`, `google/rpc/status.proto` | [googleapis/googleapis](https://github.com/googleapis/googleapis) | `9cb56f0d09e034ef1cf66818a976deee67216190` | Apache 2.0, `LICENSE.googleapis` |

The `google/protobuf/*.proto` well-known types they import are `protox`'s
own.

To update them, copy the same files from a newer commit of each source,
update this table, and run `tools/check`.
