//! The remote cache: records and their contents found in, and written to, a
//! cache that speaks Bazel's Remote Execution API (see
//! `design/additive.md`, F5).
//!
//! # The remote cache
//!
//! Any REAPI server serves: bazel-remote, Buildbarn, `BuildBuddy` or
//! `NativeLink`. pekit uses its content-addressable store and its action
//! cache, never its execution service:
//!
//! - **Contents are blobs as they are.** A pekit blob's name is the SHA-256
//!   digest of its contents, as a REAPI blob's is, so a file is the same
//!   blob in both.
//! - **A record is an action result,** kept by an *action blob*,
//!   `pekit-action-1\0` and the record's key in hex, whose digest and size
//!   are the action cache's key, as Bazel keys an action by its `Action`
//!   message's. The action blob is uploaded too, so a server that checks an
//!   action exists is satisfied.
//! - **The result names what the record needs.** Its output files are the
//!   record's own encoding, at `record`, and each file the record names, at
//!   `blobs/DIGEST`. Its output directories are each tree the record names,
//!   at `trees/DIGEST`, as a REAPI `Tree` message. A server that checks a
//!   result's outputs exist, or keeps what a hit touches, so does for all
//!   of them.
//! - **Every name is a key's or a key's digest:** a record's key is by
//!   what made it, a step's receipt say, and each content by its digest.
//!
//! Which digests a record names is found as eviction finds them: every
//! string of 64 hex digits in its encoding that names a blob the store
//! holds, a tree if its blob is a tree's ([`Store::is_tree_blob`]).
//!
//! # Finding a record
//!
//! [`Remote::find`] asks the action cache for the record's key. On a hit it
//! downloads every content the result names that the store lacks, checking
//! each by its digest, makes each tree again from its `Tree` message and
//! checks it is the tree its path names, and writes the record last: so a
//! record is in the store only once all it names is. Nothing the cache
//! gives is taken on trust but that the result is the record of its key,
//! as a local record is.
//!
//! # Writing a record
//!
//! [`Remote::upload`] reads a record and what it names from the store, asks
//! the cache which blobs it lacks, uploads those, small ones in batches and
//! large ones by `ByteStream`, and then writes the result.
//!
//! # Configuration
//!
//! A cache is named by its URL, `grpc://HOST:PORT` or `grpcs://HOST:PORT`
//! (or `http://` and `https://`), and an instance name, which Buildbarn
//! needs. Every call carries the headers given, credentials say. One
//! runtime per process runs the calls, which the pool's threads wait on.
//! Once a call fails, the cache unreachable say, the process asks it
//! nothing more and builds as if it had none, so that a cache that is down
//! costs a build one failed call, not one for each step.

use std::collections::{HashMap, HashSet};
use std::fmt;
use std::fs::File;
use std::io::{self, Read as _, Write};
use std::sync::OnceLock;
use std::sync::atomic::{AtomicBool, Ordering};

use pekit_reapi::bytestream::byte_stream_client::ByteStreamClient;
use pekit_reapi::bytestream::{ReadRequest, WriteRequest};
use pekit_reapi::execution as reapi;
use pekit_reapi::execution::action_cache_client::ActionCacheClient;
use pekit_reapi::execution::capabilities_client::CapabilitiesClient;
use pekit_reapi::execution::content_addressable_storage_client::ContentAddressableStorageClient;
use prost::Message as _;
use tonic::metadata::{AsciiMetadataKey, AsciiMetadataValue};
use tonic::transport::{Channel, ClientTlsConfig, Endpoint};
use tonic::{Code, Request, Status};

use crate::evict::digests_in;
use crate::store::{Digest, FileEntry, Store, Tree, TreeEntry};

/// The most a gRPC message may be by default, which a batch of blobs is
/// kept within whatever a server says it takes.
const MESSAGE_LIMIT: u64 = 4 << 20;

/// What a batch counts each blob as besides its contents: its digest and
/// the message's framing.
const BLOB_OVERHEAD: u64 = 256;

/// How much of a blob each message of a `ByteStream` write carries.
const WRITE_CHUNK: usize = 1 << 20;

/// How long connecting to the cache may take before it is taken to be down.
const CONNECT_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(10);

/// Where the remote cache is and how it is spoken to (see
/// [Configuration](#configuration)).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct RemoteSettings {
    /// Its URL.
    pub url: String,
    /// Its instance name, empty for none.
    pub instance: String,
    /// The headers every call carries, each a name and a value.
    pub headers: Vec<(String, String)>,
    /// Whether this process writes to it.
    pub upload: bool,
}

/// A header as `NAME: VALUE` names it, its name and value.
///
/// # Errors
///
/// That it has no `:`, or an empty name.
pub fn header_of(text: &str) -> Result<(String, String), String> {
    match text.split_once(':') {
        Some((name, value)) if !name.trim().is_empty() => {
            Ok((name.trim().to_owned(), value.trim().to_owned()))
        }
        _ => Err(format!("`{text}` is no header: write it `NAME: VALUE`")),
    }
}

/// Why the remote cache could not be used.
#[derive(Debug)]
pub enum RemoteError {
    /// Its settings are not usable: a URL or a header.
    Settings(String),
    /// A call failed: the cache could not be reached, or refused it.
    Call(String),
    /// What the cache gave is not what it names.
    Corrupt(String),
    /// The local store could not be read or written.
    Store(io::Error),
}

impl fmt::Display for RemoteError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Settings(message) | Self::Call(message) => f.write_str(message),
            Self::Corrupt(message) => write!(f, "the remote cache gave {message}"),
            Self::Store(error) => write!(f, "the store: {error}"),
        }
    }
}

impl From<io::Error> for RemoteError {
    fn from(error: io::Error) -> Self {
        Self::Store(error)
    }
}

impl From<Status> for RemoteError {
    fn from(status: Status) -> Self {
        Self::Call(format!(
            "the remote cache failed a call: {:?}: {}",
            status.code(),
            status.message()
        ))
    }
}

/// A remote cache, connected to on its first call (see [The remote
/// cache](#the-remote-cache)).
#[derive(Debug)]
pub struct Remote {
    /// The runtime its calls run on.
    runtime: tokio::runtime::Runtime,
    /// The connection.
    channel: Channel,
    /// Its instance name.
    instance: String,
    /// The headers every call carries.
    headers: Vec<(AsciiMetadataKey, AsciiMetadataValue)>,
    /// Whether this process writes to it.
    upload: bool,
    /// The most a batch of blobs may be, once its capabilities are asked.
    batch: OnceLock<Result<u64, String>>,
    /// Whether a call failed: the cache is asked nothing more by this
    /// process, which builds as if it had none.
    down: AtomicBool,
}

impl Remote {
    /// The remote cache `settings` describe, connected to on its first call.
    ///
    /// # Errors
    ///
    /// That its URL or a header is no good, or no runtime can be made.
    pub fn connect(settings: &RemoteSettings) -> Result<Self, RemoteError> {
        let url = endpoint_url(&settings.url)?;
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .thread_name("pekit-remote")
            .enable_all()
            .build()
            .map_err(|error| RemoteError::Settings(error.to_string()))?;
        let mut endpoint = Endpoint::from_shared(url.clone())
            .map_err(|error| {
                RemoteError::Settings(format!("`{}` is no remote cache: {error}", settings.url))
            })?
            .connect_timeout(CONNECT_TIMEOUT);
        if url.starts_with("https://") {
            endpoint = endpoint
                .tls_config(ClientTlsConfig::new().with_native_roots())
                .map_err(|error| RemoteError::Settings(error.to_string()))?;
        }
        let channel = {
            let _entered = runtime.enter();
            endpoint.connect_lazy()
        };
        let headers = settings
            .headers
            .iter()
            .map(|(name, value)| {
                let key = AsciiMetadataKey::from_bytes(name.to_ascii_lowercase().as_bytes())
                    .map_err(|_| RemoteError::Settings(format!("`{name}` is no header name")))?;
                let value = AsciiMetadataValue::try_from(value.as_str()).map_err(|_| {
                    RemoteError::Settings(format!("the header `{name}` has a value it cannot"))
                })?;
                Ok((key, value))
            })
            .collect::<Result<_, RemoteError>>()?;
        Ok(Self {
            runtime,
            channel,
            instance: settings.instance.clone(),
            headers,
            upload: settings.upload,
            batch: OnceLock::new(),
            down: AtomicBool::new(false),
        })
    }

    /// `outcome`, a call's, noting the cache down if the call failed.
    fn noted<T>(&self, outcome: Result<T, RemoteError>) -> Result<T, RemoteError> {
        if matches!(outcome, Err(RemoteError::Call(_))) {
            self.down.store(true, Ordering::Relaxed);
        }
        outcome
    }

    /// Whether this process writes to the cache.
    #[must_use]
    pub fn uploads(&self) -> bool {
        self.upload
    }

    /// Finds the record kept by `key` in the cache, and puts it into
    /// `store` with everything it names: whether it was there (see [Finding
    /// a record](#finding-a-record)). Once a call has failed, nothing is.
    ///
    /// # Errors
    ///
    /// That the cache could not be asked, that what it gave is not what it
    /// names, or that the store could not be written.
    pub fn find(&self, store: &Store, key: &Digest) -> Result<bool, RemoteError> {
        match self.fetch(store, key)? {
            Some(bytes) => {
                store.put_record_bytes(key, &bytes)?;
                Ok(true)
            }
            None => Ok(false),
        }
    }

    /// The record kept by `key` in the cache, as it is written, if it is
    /// there, everything it names put into `store` but not the record
    /// itself: for a caller to merge with its own, as a manifest is. Once a
    /// call has failed, nothing is.
    ///
    /// # Errors
    ///
    /// As [`Remote::find`].
    pub fn fetch(&self, store: &Store, key: &Digest) -> Result<Option<Vec<u8>>, RemoteError> {
        if self.down.load(Ordering::Relaxed) {
            return Ok(None);
        }
        self.noted(self.fetch_record(store, key))
    }

    /// [`Remote::fetch`], asked.
    fn fetch_record(&self, store: &Store, key: &Digest) -> Result<Option<Vec<u8>>, RemoteError> {
        let request = reapi::GetActionResultRequest {
            instance_name: self.instance.clone(),
            action_digest: Some(action_of(key).1),
            digest_function: reapi::digest_function::Value::Sha256.into(),
            ..Default::default()
        };
        let found = self.block_on(
            ActionCacheClient::new(self.channel.clone()).get_action_result(self.request(request)),
        );
        let result = match found {
            Ok(response) => response.into_inner(),
            Err(status) if status.code() == Code::NotFound => return Ok(None),
            Err(status) => return Err(status.into()),
        };
        let mut record = None;
        let mut files = Vec::new();
        for file in &result.output_files {
            let named = local_digest(file.digest.as_ref())?;
            if file.path == "record" {
                record = Some(named);
            } else if file.path.starts_with("blobs/") {
                files.push(named);
            }
        }
        let record =
            record.ok_or_else(|| RemoteError::Corrupt("a result holding no record".to_owned()))?;
        let mut trees = Vec::new();
        for directory in &result.output_directories {
            let expected = directory
                .path
                .strip_prefix("trees/")
                .and_then(Digest::from_hex)
                .ok_or_else(|| {
                    RemoteError::Corrupt(format!("an output directory `{}`", directory.path))
                })?;
            let bytes = self.read_bytes(local_digest(directory.tree_digest.as_ref())?)?;
            let tree = reapi::Tree::decode(bytes.as_slice())
                .map_err(|error| RemoteError::Corrupt(format!("a tree it cannot read: {error}")))?;
            trees.push((expected, tree));
        }
        for (_, tree) in &trees {
            for directory in tree.root.iter().chain(&tree.children) {
                for file in &directory.files {
                    files.push(local_digest(file.digest.as_ref())?);
                }
            }
        }
        files.sort_unstable_by_key(|(digest, _)| digest.hex());
        files.dedup();
        files.retain(|(digest, _)| !store.has(digest));
        self.fetch_blobs(store, &files)?;
        for (expected, tree) in &trees {
            let made = put_tree(store, tree)?;
            if made != *expected {
                return Err(RemoteError::Corrupt(format!(
                    "the tree `{}` for `{}`",
                    made.hex(),
                    expected.hex()
                )));
            }
        }
        self.read_bytes(record).map(Some)
    }

    /// Writes the record kept by `key` in `store`, and everything it names,
    /// to the cache (see [Writing a record](#writing-a-record)). A record
    /// the store does not have writes nothing; nor does any once a call has
    /// failed.
    ///
    /// # Errors
    ///
    /// That the store could not be read, or the cache written.
    pub fn upload(&self, store: &Store, key: &Digest) -> Result<(), RemoteError> {
        if self.down.load(Ordering::Relaxed) {
            return Ok(());
        }
        self.noted(self.upload_record(store, key))
    }

    /// [`Remote::upload`], asked.
    fn upload_record(&self, store: &Store, key: &Digest) -> Result<(), RemoteError> {
        let Some(bytes) = store.record_bytes(key)? else {
            return Ok(());
        };
        let encoding: serde_json::Value = serde_json::from_slice(&bytes)
            .map_err(|error| RemoteError::Store(io::Error::other(error)))?;
        let mut named = Vec::new();
        digests_in(&encoding, &mut named);
        named.sort_unstable();
        named.dedup();
        let mut blobs = Blobs::default();
        let mut result = reapi::ActionResult::default();
        let record = blobs.bytes(bytes);
        result.output_files.push(reapi::OutputFile {
            path: "record".to_owned(),
            digest: Some(record),
            ..Default::default()
        });
        for digest in named.iter().filter_map(|hex| Digest::from_hex(hex)) {
            let Some((_, size)) = store.held_blob(&digest) else {
                continue;
            };
            let entry = FileEntry {
                digest,
                size,
                executable: false,
            };
            if store.is_tree_blob(&digest) {
                let tree = reapi_tree(store, &digest, &mut blobs)?;
                let tree = blobs.bytes(tree.encode_to_vec());
                result.output_directories.push(reapi::OutputDirectory {
                    path: format!("trees/{}", digest.hex()),
                    tree_digest: Some(tree),
                    ..Default::default()
                });
            } else {
                result.output_files.push(reapi::OutputFile {
                    path: format!("blobs/{}", digest.hex()),
                    digest: Some(blobs.stored(&entry)),
                    ..Default::default()
                });
            }
        }
        let (action_blob, action) = action_of(key);
        blobs.bytes(action_blob);
        self.write_missing(store, &blobs)?;
        let request = reapi::UpdateActionResultRequest {
            instance_name: self.instance.clone(),
            action_digest: Some(action),
            action_result: Some(result),
            digest_function: reapi::digest_function::Value::Sha256.into(),
            ..Default::default()
        };
        self.block_on(
            ActionCacheClient::new(self.channel.clone())
                .update_action_result(self.request(request)),
        )?;
        Ok(())
    }

    /// `message` as a request carrying the cache's headers.
    fn request<T>(&self, message: T) -> Request<T> {
        let mut request = Request::new(message);
        for (key, value) in &self.headers {
            request.metadata_mut().insert(key.clone(), value.clone());
        }
        request
    }

    /// Runs `future` on the cache's runtime, this thread waiting for it.
    fn block_on<F: Future>(&self, future: F) -> F::Output {
        self.runtime.handle().block_on(future)
    }

    /// The most a batch of blobs may be: what the cache's capabilities say,
    /// within a gRPC message's default limit, asked once.
    fn batch_limit(&self) -> Result<u64, RemoteError> {
        self.batch
            .get_or_init(|| {
                let request = reapi::GetCapabilitiesRequest {
                    instance_name: self.instance.clone(),
                };
                let capabilities = self
                    .block_on(
                        CapabilitiesClient::new(self.channel.clone())
                            .get_capabilities(self.request(request)),
                    )
                    .map_err(|status| RemoteError::from(status).to_string())?
                    .into_inner();
                let cache = capabilities.cache_capabilities.unwrap_or_default();
                let sha256: i32 = reapi::digest_function::Value::Sha256.into();
                if !cache.digest_functions.is_empty() && !cache.digest_functions.contains(&sha256) {
                    return Err("the remote cache does not name blobs by SHA-256".to_owned());
                }
                Ok(u64::try_from(cache.max_batch_total_size_bytes)
                    .ok()
                    .filter(|&limit| limit > 0)
                    .map_or(MESSAGE_LIMIT, |limit| limit.min(MESSAGE_LIMIT)))
            })
            .clone()
            .map_err(RemoteError::Call)
    }

    /// The `ByteStream` resource of the blob `digest` of `size` bytes, to
    /// read it, or, with `upload`, to write it.
    fn resource(&self, (digest, size): (Digest, u64), upload: Option<&str>) -> String {
        let blob = match upload {
            Some(id) => format!("uploads/{id}/blobs/{}/{size}", digest.hex()),
            None => format!("blobs/{}/{size}", digest.hex()),
        };
        if self.instance.is_empty() {
            blob
        } else {
            format!("{}/{blob}", self.instance)
        }
    }

    /// The blob `named`, its digest and size, read whole and checked.
    fn read_bytes(&self, named: (Digest, u64)) -> Result<Vec<u8>, RemoteError> {
        let (digest, size) = named;
        // The empty blob is every cache's without asking.
        let mut bytes = Vec::new();
        if size > 0 && size + BLOB_OVERHEAD <= self.batch_limit()? {
            for (_, data) in self.read_batch(&[named])? {
                bytes = data;
            }
        } else if size > 0 {
            self.read_stream(named, &mut bytes)?;
        }
        if Digest::of(&bytes) != digest {
            return Err(RemoteError::Corrupt(format!(
                "other contents for the blob `{}`",
                digest.hex()
            )));
        }
        Ok(bytes)
    }

    /// Puts each blob of `wanted`, by its digest and size, into `store`,
    /// read from the cache and checked: the small ones in batches, the
    /// large ones as streams.
    fn fetch_blobs(&self, store: &Store, wanted: &[(Digest, u64)]) -> Result<(), RemoteError> {
        let limit = self.batch_limit()?;
        let mut batch = Vec::new();
        let mut batched = 0;
        for &(digest, size) in wanted {
            if size == 0 {
                check_put(&store.put_written(|_| Ok(()))?, digest)?;
                continue;
            }
            if size + BLOB_OVERHEAD > limit {
                let entry = store.put_written(|out| {
                    self.read_stream((digest, size), out)
                        .map_err(|error| io::Error::other(error.to_string()))
                })?;
                check_put(&entry, digest)?;
                continue;
            }
            if batched + size + BLOB_OVERHEAD > limit {
                self.put_batch(store, &batch)?;
                batch.clear();
                batched = 0;
            }
            batch.push((digest, size));
            batched += size + BLOB_OVERHEAD;
        }
        self.put_batch(store, &batch)
    }

    /// Puts the blobs of `batch` into `store`, read in one call and checked.
    fn put_batch(&self, store: &Store, batch: &[(Digest, u64)]) -> Result<(), RemoteError> {
        for (digest, data) in self.read_batch(batch)? {
            let entry = store.put_written(|out| out.write_all(&data))?;
            check_put(&entry, digest)?;
        }
        Ok(())
    }

    /// The blobs of `batch`, by digest and size, read in one call, each with
    /// its digest; every one there, or an error.
    fn read_batch(&self, batch: &[(Digest, u64)]) -> Result<Vec<(Digest, Vec<u8>)>, RemoteError> {
        if batch.is_empty() {
            return Ok(Vec::new());
        }
        let request = reapi::BatchReadBlobsRequest {
            instance_name: self.instance.clone(),
            digests: batch
                .iter()
                .map(|&(digest, size)| reapi_digest(&digest, size))
                .collect(),
            digest_function: reapi::digest_function::Value::Sha256.into(),
            ..Default::default()
        };
        let response = self
            .block_on(
                ContentAddressableStorageClient::new(self.channel.clone())
                    .batch_read_blobs(self.request(request)),
            )?
            .into_inner();
        let mut read = Vec::new();
        for blob in response.responses {
            let (digest, _) = local_digest(blob.digest.as_ref())?;
            if let Some(status) = blob.status.filter(|status| status.code != 0) {
                return Err(RemoteError::Corrupt(format!(
                    "no blob `{}`: {}",
                    digest.hex(),
                    status.message
                )));
            }
            read.push((digest, blob.data));
        }
        if read.len() != batch.len() {
            return Err(RemoteError::Corrupt(
                "fewer blobs than it was asked for".to_owned(),
            ));
        }
        Ok(read)
    }

    /// Writes the blob `named`, by its digest and size, read by `ByteStream`,
    /// to `out`.
    fn read_stream(&self, named: (Digest, u64), out: &mut dyn Write) -> Result<(), RemoteError> {
        let request = ReadRequest {
            resource_name: self.resource(named, None),
            read_offset: 0,
            read_limit: 0,
        };
        self.block_on(async {
            let mut stream = ByteStreamClient::new(self.channel.clone())
                .read(self.request(request))
                .await?
                .into_inner();
            while let Some(chunk) = stream.message().await? {
                out.write_all(&chunk.data)?;
            }
            Ok(())
        })
    }

    /// Writes each blob of `blobs` the cache lacks: the small ones in
    /// batches, the large ones as streams.
    fn write_missing(&self, store: &Store, blobs: &Blobs) -> Result<(), RemoteError> {
        let request = reapi::FindMissingBlobsRequest {
            instance_name: self.instance.clone(),
            blob_digests: blobs.digests(),
            digest_function: reapi::digest_function::Value::Sha256.into(),
        };
        let missing = self
            .block_on(
                ContentAddressableStorageClient::new(self.channel.clone())
                    .find_missing_blobs(self.request(request)),
            )?
            .into_inner()
            .missing_blob_digests;
        let limit = self.batch_limit()?;
        let mut batch = Vec::new();
        let mut batched = 0;
        for named in missing {
            let (digest, size) = local_digest(Some(&named))?;
            if size == 0 {
                continue;
            }
            if size + BLOB_OVERHEAD > limit {
                self.write_stream(store, blobs, (digest, size))?;
                continue;
            }
            if batched + size + BLOB_OVERHEAD > limit {
                self.write_batch(std::mem::take(&mut batch))?;
                batched = 0;
            }
            batch.push(reapi::batch_update_blobs_request::Request {
                digest: Some(named),
                data: blobs.read(store, &digest)?,
                ..Default::default()
            });
            batched += size + BLOB_OVERHEAD;
        }
        self.write_batch(batch)
    }

    /// Writes the blobs of `batch` in one call, every one, or an error.
    fn write_batch(
        &self,
        batch: Vec<reapi::batch_update_blobs_request::Request>,
    ) -> Result<(), RemoteError> {
        if batch.is_empty() {
            return Ok(());
        }
        let request = reapi::BatchUpdateBlobsRequest {
            instance_name: self.instance.clone(),
            requests: batch,
            digest_function: reapi::digest_function::Value::Sha256.into(),
        };
        let response = self
            .block_on(
                ContentAddressableStorageClient::new(self.channel.clone())
                    .batch_update_blobs(self.request(request)),
            )?
            .into_inner();
        for written in response.responses {
            if let Some(status) = written.status.filter(|status| status.code != 0) {
                return Err(RemoteError::Call(format!(
                    "the remote cache did not take a blob: {}",
                    status.message
                )));
            }
        }
        Ok(())
    }

    /// Writes the blob `named`, by its digest and size, of `blobs`, by
    /// `ByteStream`, read from the store a part at a time.
    fn write_stream(
        &self,
        store: &Store,
        blobs: &Blobs,
        named: (Digest, u64),
    ) -> Result<(), RemoteError> {
        let resource = self.resource(named, Some(&upload_id()?));
        let chunks = Chunks::of(blobs.open(store, &named.0)?, resource);
        let written = self.block_on(
            ByteStreamClient::new(self.channel.clone())
                .write(self.request(tokio_stream::iter(chunks))),
        )?;
        let committed = u64::try_from(written.into_inner().committed_size).unwrap_or(0);
        if committed != named.1 {
            return Err(RemoteError::Call(format!(
                "the remote cache took {committed} bytes of the blob `{}`, of {}",
                named.0.hex(),
                named.1
            )));
        }
        Ok(())
    }
}

/// The parts of a blob, as the messages of a `ByteStream` write.
struct Chunks {
    /// The blob, read a part at a time.
    file: Box<dyn io::Read + Send>,
    /// The resource it is written to, which the first message names.
    resource: Option<String>,
    /// How much was sent.
    offset: i64,
    /// Whether the last was sent.
    done: bool,
}

impl Chunks {
    /// The parts of what `file` reads, written to `resource`.
    fn of(file: Box<dyn io::Read + Send>, resource: String) -> Self {
        Self {
            file,
            resource: Some(resource),
            offset: 0,
            done: false,
        }
    }
}

impl Iterator for Chunks {
    type Item = WriteRequest;

    fn next(&mut self) -> Option<WriteRequest> {
        if self.done {
            return None;
        }
        let mut data = vec![0; WRITE_CHUNK];
        let mut filled = 0;
        // A short or failed read ends the blob early, which the write's
        // committed size shows.
        while filled < data.len() {
            match self.file.read(&mut data[filled..]) {
                Ok(0) | Err(_) => break,
                Ok(read) => filled += read,
            }
        }
        data.truncate(filled);
        self.done = filled < WRITE_CHUNK;
        let message = WriteRequest {
            resource_name: self.resource.take().unwrap_or_default(),
            write_offset: self.offset,
            finish_write: self.done,
            data,
        };
        self.offset += i64::try_from(filled).unwrap_or(i64::MAX);
        Some(message)
    }
}

/// The blobs an upload writes: each by its digest and size, from bytes in
/// hand or from the store.
#[derive(Default)]
struct Blobs {
    /// Those in hand, by digest.
    held: HashMap<Digest, Vec<u8>>,
    /// Each, by digest, with its size.
    all: Vec<(Digest, u64)>,
    /// Those added, to add each once.
    seen: HashSet<Digest>,
}

impl Blobs {
    /// Adds `bytes`: their REAPI digest.
    fn bytes(&mut self, bytes: Vec<u8>) -> reapi::Digest {
        let digest = Digest::of(&bytes);
        let size = bytes.len() as u64;
        if self.seen.insert(digest) {
            self.all.push((digest, size));
            self.held.insert(digest, bytes);
        }
        reapi_digest(&digest, size)
    }

    /// Adds the blob of `entry` the store holds: its REAPI digest.
    fn stored(&mut self, entry: &FileEntry) -> reapi::Digest {
        if self.seen.insert(entry.digest) {
            self.all.push((entry.digest, entry.size));
        }
        reapi_digest(&entry.digest, entry.size)
    }

    /// Each blob's REAPI digest.
    fn digests(&self) -> Vec<reapi::Digest> {
        self.all
            .iter()
            .map(|(digest, size)| reapi_digest(digest, *size))
            .collect()
    }

    /// The contents of the blob `digest`, whole.
    fn read(&self, store: &Store, digest: &Digest) -> io::Result<Vec<u8>> {
        match self.held.get(digest) {
            Some(bytes) => Ok(bytes.clone()),
            None => store.read_blob(digest),
        }
    }

    /// The blob `digest`, to read a part at a time.
    fn open(&self, store: &Store, digest: &Digest) -> io::Result<Box<dyn io::Read + Send>> {
        if let Some(bytes) = self.held.get(digest) {
            return Ok(Box::new(io::Cursor::new(bytes.clone())));
        }
        let (path, _) = store.held_blob(digest).ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::NotFound,
                format!("the store has no blob `{}`", digest.hex()),
            )
        })?;
        Ok(Box::new(File::open(path)?))
    }
}

/// The REAPI `Tree` of the store's tree `digest`: its root and every
/// directory under it, once each; each file it holds is added to `blobs`.
fn reapi_tree(store: &Store, digest: &Digest, blobs: &mut Blobs) -> io::Result<reapi::Tree> {
    let mut children = Vec::new();
    let mut made = HashMap::new();
    let root = reapi_directory(store, digest, blobs, (&mut children, &mut made))?;
    Ok(reapi::Tree {
        root: Some(root.0),
        children,
    })
}

/// The REAPI `Directory` of the store's tree `digest`, and its digest; each
/// directory under it is added to `children` once, by `made`, and each file
/// to `blobs`.
fn reapi_directory(
    store: &Store,
    digest: &Digest,
    blobs: &mut Blobs,
    (children, made): (
        &mut Vec<reapi::Directory>,
        &mut HashMap<Digest, reapi::Digest>,
    ),
) -> io::Result<(reapi::Directory, reapi::Digest)> {
    let tree = store.read_tree(digest)?;
    let mut directory = reapi::Directory::default();
    for entry in &tree.entries {
        match entry {
            TreeEntry::File { name, file } => directory.files.push(reapi::FileNode {
                name: name.clone(),
                digest: Some(blobs.stored(file)),
                is_executable: file.executable,
                ..Default::default()
            }),
            TreeEntry::Dir { name, tree } => {
                let child = if let Some(known) = made.get(tree) {
                    known.clone()
                } else {
                    let (child, encoded) =
                        reapi_directory(store, tree, blobs, (&mut *children, &mut *made))?;
                    children.push(child);
                    made.insert(*tree, encoded.clone());
                    encoded
                };
                directory.directories.push(reapi::DirectoryNode {
                    name: name.clone(),
                    digest: Some(child),
                });
            }
            TreeEntry::Link { name, target } => directory.symlinks.push(reapi::SymlinkNode {
                name: name.clone(),
                target: target.clone(),
                ..Default::default()
            }),
        }
    }
    let encoded = directory.encode_to_vec();
    let digest = reapi_digest(&Digest::of(&encoded), encoded.len() as u64);
    Ok((directory, digest))
}

/// Puts the REAPI `Tree` `tree` into the store as the store's trees, from
/// its leaves up: its root's digest.
fn put_tree(store: &Store, tree: &reapi::Tree) -> Result<Digest, RemoteError> {
    let children: HashMap<String, &reapi::Directory> = tree
        .children
        .iter()
        .map(|child| (Digest::of(&child.encode_to_vec()).hex(), child))
        .collect();
    let root = tree
        .root
        .as_ref()
        .ok_or_else(|| RemoteError::Corrupt("a tree with no root".to_owned()))?;
    put_directory(store, root, &children, &mut HashMap::new())
}

/// Puts the REAPI `Directory` `directory`, whose directories are among
/// `children` by digest, into the store as a tree: its digest. Each put is
/// remembered in `made`.
fn put_directory(
    store: &Store,
    directory: &reapi::Directory,
    children: &HashMap<String, &reapi::Directory>,
    made: &mut HashMap<String, Digest>,
) -> Result<Digest, RemoteError> {
    let mut entries = Vec::new();
    for file in &directory.files {
        let (digest, size) = local_digest(file.digest.as_ref())?;
        entries.push(TreeEntry::File {
            name: file.name.clone(),
            file: FileEntry {
                digest,
                size,
                executable: file.is_executable,
            },
        });
    }
    for node in &directory.directories {
        let hash = node
            .digest
            .as_ref()
            .map(|digest| digest.hash.clone())
            .unwrap_or_default();
        let tree = if let Some(known) = made.get(&hash) {
            *known
        } else {
            let child = children.get(&hash).ok_or_else(|| {
                RemoteError::Corrupt(format!("a tree missing its directory `{hash}`"))
            })?;
            let tree = put_directory(store, child, children, made)?;
            made.insert(hash, tree);
            tree
        };
        entries.push(TreeEntry::Dir {
            name: node.name.clone(),
            tree,
        });
    }
    for link in &directory.symlinks {
        entries.push(TreeEntry::Link {
            name: link.name.clone(),
            target: link.target.clone(),
        });
    }
    entries.sort_by(|a, b| a.name().cmp(b.name()));
    Ok(store.put_tree_of(&Tree { entries })?)
}

/// The action blob of the record kept by `key`, and its REAPI digest: the
/// action cache's key for it.
fn action_of(key: &Digest) -> (Vec<u8>, reapi::Digest) {
    let blob = format!("pekit-action-1\0{}", key.hex()).into_bytes();
    let digest = reapi_digest(&Digest::of(&blob), blob.len() as u64);
    (blob, digest)
}

/// The REAPI digest of a blob of `digest` and `size` bytes.
fn reapi_digest(digest: &Digest, size: u64) -> reapi::Digest {
    reapi::Digest {
        hash: digest.hex(),
        size_bytes: i64::try_from(size).unwrap_or(i64::MAX),
    }
}

/// The digest and size a REAPI digest names.
fn local_digest(digest: Option<&reapi::Digest>) -> Result<(Digest, u64), RemoteError> {
    let digest = digest.ok_or_else(|| RemoteError::Corrupt("a blob of no digest".to_owned()))?;
    let hash = Digest::from_hex(&digest.hash)
        .ok_or_else(|| RemoteError::Corrupt(format!("a digest `{}`", digest.hash)))?;
    let size = u64::try_from(digest.size_bytes)
        .map_err(|_| RemoteError::Corrupt(format!("a size {}", digest.size_bytes)))?;
    Ok((hash, size))
}

/// That `entry`, a blob just put, is the blob `digest` it was read as.
fn check_put(entry: &FileEntry, digest: Digest) -> Result<(), RemoteError> {
    if entry.digest == digest {
        Ok(())
    } else {
        Err(RemoteError::Corrupt(format!(
            "other contents for the blob `{}`",
            digest.hex()
        )))
    }
}

/// The URL a channel connects to for the cache at `url`: `grpc://` is
/// `http://`, and `grpcs://` is `https://`.
fn endpoint_url(url: &str) -> Result<String, RemoteError> {
    if let Some(rest) = url.strip_prefix("grpc://") {
        Ok(format!("http://{rest}"))
    } else if let Some(rest) = url.strip_prefix("grpcs://") {
        Ok(format!("https://{rest}"))
    } else if url.starts_with("http://") || url.starts_with("https://") {
        Ok(url.to_owned())
    } else {
        Err(RemoteError::Settings(format!(
            "`{url}` is no remote cache: its URL begins `grpc://`, `grpcs://`, `http://` or \
             `https://`"
        )))
    }
}

/// A new upload's name, a random UUID, as `ByteStream`'s resource names one.
fn upload_id() -> io::Result<String> {
    let mut bytes = [0u8; 16];
    File::open("/dev/urandom")?.read_exact(&mut bytes)?;
    // Version 4, variant 1.
    bytes[6] = (bytes[6] & 0x0f) | 0x40;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    let hex = bytes.iter().fold(String::new(), |mut hex, byte| {
        let _ = fmt::Write::write_fmt(&mut hex, format_args!("{byte:02x}"));
        hex
    });
    Ok(format!(
        "{}-{}-{}-{}-{}",
        &hex[..8],
        &hex[8..12],
        &hex[12..16],
        &hex[16..20],
        &hex[20..]
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_cache_is_named_by_a_grpc_or_http_url() {
        assert_eq!(endpoint_url("grpc://h:9092").unwrap(), "http://h:9092");
        assert_eq!(endpoint_url("grpcs://h").unwrap(), "https://h");
        assert_eq!(endpoint_url("https://h").unwrap(), "https://h");
        assert!(endpoint_url("h:9092").is_err());
    }

    #[test]
    fn a_header_is_a_name_and_a_value() {
        assert_eq!(
            header_of("Authorization: Bearer x:y").unwrap(),
            ("Authorization".to_owned(), "Bearer x:y".to_owned())
        );
        assert!(header_of("no colon").is_err());
        assert!(header_of(": empty").is_err());
    }

    #[test]
    fn an_action_blob_names_its_record_s_key() {
        let key = Digest::of(b"receipt");
        let (blob, digest) = action_of(&key);
        assert_eq!(blob, format!("pekit-action-1\0{}", key.hex()).into_bytes());
        assert_eq!(digest.hash, Digest::of(&blob).hex());
        assert_eq!(digest.size_bytes, 79);
    }

    #[test]
    fn an_upload_s_name_is_a_version_4_uuid() {
        let id = upload_id().unwrap();
        let parts: Vec<&str> = id.split('-').collect();
        assert_eq!(
            parts.iter().map(|part| part.len()).collect::<Vec<_>>(),
            [8, 4, 4, 4, 12]
        );
        assert!(parts[2].starts_with('4'));
        assert_ne!(id, upload_id().unwrap());
    }
}
