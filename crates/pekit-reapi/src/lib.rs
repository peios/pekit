//! Bazel's Remote Execution API, version 2, and Google's ByteStream API, as
//! `tonic` gRPC clients, compiled from the definitions vendored in `proto/`
//! (see its `README.md`). The modules nest as the definitions' packages do,
//! since the generated code names one package from another by its path.

/// `build.bazel`.
pub mod build {
    /// `build.bazel`.
    pub mod bazel {
        /// `build.bazel.remote`.
        pub mod remote {
            /// `build.bazel.remote.execution`.
            pub mod execution {
                /// `build.bazel.remote.execution.v2`: the content-addressable
                /// store, the action cache and the capabilities.
                pub mod v2 {
                    tonic::include_proto!("build.bazel.remote.execution.v2");
                }
            }
        }

        /// `build.bazel.semver`, which the capabilities name versions by.
        pub mod semver {
            tonic::include_proto!("build.bazel.semver");
        }
    }
}

/// `google`.
pub mod google {
    /// `google.api`, whose options the definitions annotate their services
    /// with.
    pub mod api {
        tonic::include_proto!("google.api");
    }

    /// `google.bytestream`: reading and writing a blob too large for a
    /// batch.
    pub mod bytestream {
        tonic::include_proto!("google.bytestream");
    }

    /// `google.longrunning`, which REAPI's execution service answers with.
    pub mod longrunning {
        tonic::include_proto!("google.longrunning");
    }

    /// `google.rpc`, whose `Status` REAPI's batches report each blob's
    /// outcome with.
    pub mod rpc {
        tonic::include_proto!("google.rpc");
    }
}

pub use build::bazel::remote::execution::v2 as execution;
pub use google::bytestream;
