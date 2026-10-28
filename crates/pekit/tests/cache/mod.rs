//! A remote cache of the tests' own, in memory: REAPI's action cache,
//! content-addressable store and capabilities, and `ByteStream`, served on a
//! free port of this machine. It checks what it is given as a careful
//! server does: each blob against its digest, and each result's outputs
//! present, as Buildbarn checks them.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::pin::Pin;
use std::sync::{Arc, Mutex};

use pekit_reapi::bytestream::byte_stream_server::{ByteStream, ByteStreamServer};
use pekit_reapi::bytestream::{
    QueryWriteStatusRequest, QueryWriteStatusResponse, ReadRequest, ReadResponse, WriteRequest,
    WriteResponse,
};
use pekit_reapi::execution as reapi;
use pekit_reapi::execution::action_cache_server::{ActionCache, ActionCacheServer};
use pekit_reapi::execution::capabilities_server::{Capabilities, CapabilitiesServer};
use pekit_reapi::execution::content_addressable_storage_server::{
    ContentAddressableStorage, ContentAddressableStorageServer,
};
use sha2::{Digest as _, Sha256};
use tokio_stream::Stream;
use tonic::{Request, Response, Status, Streaming};

/// What the cache holds, and what it was asked.
#[derive(Debug, Default)]
pub struct Held {
    /// Each blob, by its digest's hex.
    pub blobs: HashMap<String, Vec<u8>>,
    /// Each action result, by its action's digest's hex.
    pub actions: HashMap<String, reapi::ActionResult>,
    /// The value of each `authorization` header a call carried.
    pub authorizations: Vec<String>,
    /// How many blobs were written, by batch or by stream.
    pub written: usize,
    /// How many blobs were written by stream.
    pub streamed: usize,
    /// How many blobs were read by stream.
    pub streamed_out: usize,
}

/// The cache: what it holds, and the most a batch may be, which it says.
#[derive(Clone, Debug)]
pub struct Cache {
    /// What it holds.
    pub held: Arc<Mutex<Held>>,
    /// The most a batch may be.
    batch_limit: i64,
}

/// A served cache, until it is dropped.
pub struct Served {
    /// Where it is, as pekit names a cache.
    pub url: String,
    /// The cache.
    pub cache: Cache,
    /// The runtime serving it.
    _runtime: tokio::runtime::Runtime,
}

/// A cache taking batches of at most `batch_limit` bytes, served.
pub fn serve(batch_limit: i64) -> Served {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let cache = Cache {
        held: Arc::default(),
        batch_limit,
    };
    let listener = runtime
        .block_on(tokio::net::TcpListener::bind("127.0.0.1:0"))
        .unwrap();
    let address: SocketAddr = listener.local_addr().unwrap();
    let router = tonic::transport::Server::builder()
        .add_service(ActionCacheServer::new(cache.clone()))
        .add_service(ContentAddressableStorageServer::new(cache.clone()))
        .add_service(CapabilitiesServer::new(cache.clone()))
        .add_service(ByteStreamServer::new(cache.clone()));
    runtime.spawn(
        router.serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener)),
    );
    Served {
        url: format!("grpc://{address}"),
        cache,
        _runtime: runtime,
    }
}

impl Cache {
    /// Notes the `authorization` header `request` carries, if any.
    fn note<T>(&self, request: &Request<T>) {
        if let Some(value) = request.metadata().get("authorization") {
            self.held
                .lock()
                .unwrap()
                .authorizations
                .push(value.to_str().unwrap_or_default().to_owned());
        }
    }

    /// Holds `data` as the blob `digest`, if it is that blob.
    fn put(&self, digest: &reapi::Digest, data: Vec<u8>) -> Result<(), Status> {
        if hex(&data) != digest.hash || i64::try_from(data.len()).ok() != Some(digest.size_bytes) {
            return Err(Status::invalid_argument("the data is not its digest's"));
        }
        let mut held = self.held.lock().unwrap();
        held.blobs.insert(digest.hash.clone(), data);
        held.written += 1;
        Ok(())
    }
}

/// The hex SHA-256 digest of `data`.
fn hex(data: &[u8]) -> String {
    Sha256::digest(data)
        .iter()
        .fold(String::new(), |mut hex, byte| {
            let _ = std::fmt::Write::write_fmt(&mut hex, format_args!("{byte:02x}"));
            hex
        })
}

/// The digest's hex of a resource name, `[INSTANCE/][uploads/ID/]blobs/HASH/SIZE`.
fn resource_hash(name: &str) -> Result<String, Status> {
    let parts: Vec<&str> = name.split('/').collect();
    let at = parts
        .iter()
        .position(|part| *part == "blobs")
        .ok_or_else(|| Status::invalid_argument(format!("no blob in `{name}`")))?;
    parts
        .get(at + 1)
        .map(|hash| (*hash).to_owned())
        .ok_or_else(|| Status::invalid_argument(format!("no hash in `{name}`")))
}

#[tonic::async_trait]
impl ActionCache for Cache {
    async fn get_action_result(
        &self,
        request: Request<reapi::GetActionResultRequest>,
    ) -> Result<Response<reapi::ActionResult>, Status> {
        self.note(&request);
        let digest = request.into_inner().action_digest.unwrap_or_default();
        self.held
            .lock()
            .unwrap()
            .actions
            .get(&digest.hash)
            .cloned()
            .map(Response::new)
            .ok_or_else(|| Status::not_found("no such action"))
    }

    async fn update_action_result(
        &self,
        request: Request<reapi::UpdateActionResultRequest>,
    ) -> Result<Response<reapi::ActionResult>, Status> {
        self.note(&request);
        let request = request.into_inner();
        let result = request.action_result.unwrap_or_default();
        let action = request.action_digest.unwrap_or_default();
        let mut held = self.held.lock().unwrap();
        let named = result
            .output_files
            .iter()
            .filter_map(|file| file.digest.as_ref())
            .chain(
                result
                    .output_directories
                    .iter()
                    .filter_map(|directory| directory.tree_digest.as_ref()),
            )
            .chain([&action]);
        for digest in named {
            if digest.size_bytes > 0 && !held.blobs.contains_key(&digest.hash) {
                return Err(Status::failed_precondition(format!(
                    "the blob {} is missing",
                    digest.hash
                )));
            }
        }
        held.actions.insert(action.hash, result.clone());
        Ok(Response::new(result))
    }
}

#[tonic::async_trait]
impl ContentAddressableStorage for Cache {
    async fn find_missing_blobs(
        &self,
        request: Request<reapi::FindMissingBlobsRequest>,
    ) -> Result<Response<reapi::FindMissingBlobsResponse>, Status> {
        self.note(&request);
        let held = self.held.lock().unwrap();
        let missing_blob_digests = request
            .into_inner()
            .blob_digests
            .into_iter()
            .filter(|digest| !held.blobs.contains_key(&digest.hash))
            .collect();
        Ok(Response::new(reapi::FindMissingBlobsResponse {
            missing_blob_digests,
        }))
    }

    async fn batch_update_blobs(
        &self,
        request: Request<reapi::BatchUpdateBlobsRequest>,
    ) -> Result<Response<reapi::BatchUpdateBlobsResponse>, Status> {
        self.note(&request);
        let mut responses = Vec::new();
        for blob in request.into_inner().requests {
            let digest = blob.digest.unwrap_or_default();
            let status = match self.put(&digest, blob.data) {
                Ok(()) => None,
                Err(status) => Some(pekit_reapi::google::rpc::Status {
                    code: status.code() as i32,
                    message: status.message().to_owned(),
                    details: Vec::new(),
                }),
            };
            responses.push(reapi::batch_update_blobs_response::Response {
                digest: Some(digest),
                status,
            });
        }
        Ok(Response::new(reapi::BatchUpdateBlobsResponse { responses }))
    }

    async fn batch_read_blobs(
        &self,
        request: Request<reapi::BatchReadBlobsRequest>,
    ) -> Result<Response<reapi::BatchReadBlobsResponse>, Status> {
        self.note(&request);
        let held = self.held.lock().unwrap();
        let responses = request
            .into_inner()
            .digests
            .into_iter()
            .map(|digest| {
                let (data, status) = match held.blobs.get(&digest.hash) {
                    Some(data) => (data.clone(), None),
                    None => (
                        Vec::new(),
                        Some(pekit_reapi::google::rpc::Status {
                            code: tonic::Code::NotFound as i32,
                            message: "no such blob".to_owned(),
                            details: Vec::new(),
                        }),
                    ),
                };
                reapi::batch_read_blobs_response::Response {
                    digest: Some(digest),
                    data,
                    compressor: 0,
                    status,
                }
            })
            .collect();
        Ok(Response::new(reapi::BatchReadBlobsResponse { responses }))
    }

    type GetTreeStream =
        Pin<Box<dyn Stream<Item = Result<reapi::GetTreeResponse, Status>> + Send + 'static>>;

    async fn get_tree(
        &self,
        _: Request<reapi::GetTreeRequest>,
    ) -> Result<Response<Self::GetTreeStream>, Status> {
        Err(Status::unimplemented("pekit never asks for a tree so"))
    }

    async fn split_blob(
        &self,
        _: Request<reapi::SplitBlobRequest>,
    ) -> Result<Response<reapi::SplitBlobResponse>, Status> {
        Err(Status::unimplemented("pekit never splits a blob"))
    }

    async fn splice_blob(
        &self,
        _: Request<reapi::SpliceBlobRequest>,
    ) -> Result<Response<reapi::SpliceBlobResponse>, Status> {
        Err(Status::unimplemented("pekit never splices a blob"))
    }
}

#[tonic::async_trait]
impl Capabilities for Cache {
    async fn get_capabilities(
        &self,
        _: Request<reapi::GetCapabilitiesRequest>,
    ) -> Result<Response<reapi::ServerCapabilities>, Status> {
        Ok(Response::new(reapi::ServerCapabilities {
            cache_capabilities: Some(reapi::CacheCapabilities {
                digest_functions: vec![reapi::digest_function::Value::Sha256.into()],
                max_batch_total_size_bytes: self.batch_limit,
                ..Default::default()
            }),
            ..Default::default()
        }))
    }
}

#[tonic::async_trait]
impl ByteStream for Cache {
    type ReadStream = Pin<Box<dyn Stream<Item = Result<ReadResponse, Status>> + Send + 'static>>;

    async fn read(
        &self,
        request: Request<ReadRequest>,
    ) -> Result<Response<Self::ReadStream>, Status> {
        self.note(&request);
        let hash = resource_hash(&request.into_inner().resource_name)?;
        let mut held = self.held.lock().unwrap();
        let data = held
            .blobs
            .get(&hash)
            .cloned()
            .ok_or_else(|| Status::not_found("no such blob"))?;
        held.streamed_out += 1;
        let chunks: Vec<Result<ReadResponse, Status>> = data
            .chunks(1000)
            .map(|chunk| {
                Ok(ReadResponse {
                    data: chunk.to_vec(),
                })
            })
            .collect();
        Ok(Response::new(Box::pin(tokio_stream::iter(chunks))))
    }

    async fn write(
        &self,
        request: Request<Streaming<WriteRequest>>,
    ) -> Result<Response<WriteResponse>, Status> {
        self.note(&request);
        let mut stream = request.into_inner();
        let mut name = String::new();
        let mut data = Vec::new();
        while let Some(message) = stream.message().await? {
            if name.is_empty() {
                name = message.resource_name;
            }
            data.extend_from_slice(&message.data);
            if message.finish_write {
                break;
            }
        }
        let parts: Vec<&str> = name.split('/').collect();
        let size = parts
            .last()
            .and_then(|size| size.parse::<i64>().ok())
            .ok_or_else(|| Status::invalid_argument("no size"))?;
        let digest = reapi::Digest {
            hash: resource_hash(&name)?,
            size_bytes: size,
        };
        let committed_size = i64::try_from(data.len()).unwrap_or(i64::MAX);
        self.put(&digest, data)?;
        self.held.lock().unwrap().streamed += 1;
        Ok(Response::new(WriteResponse { committed_size }))
    }

    async fn query_write_status(
        &self,
        _: Request<QueryWriteStatusRequest>,
    ) -> Result<Response<QueryWriteStatusResponse>, Status> {
        Err(Status::unimplemented("pekit never resumes a write"))
    }
}
