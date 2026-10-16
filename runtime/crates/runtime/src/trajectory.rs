//! Bounded, synchronous, metadata-only episode audit spool.
//!
//! `append` durably syncs before returning. Call it on a blocking worker and append
//! before forwarding an event. Full spools return explicit backpressure; this
//! module never rotates or evicts records. An I/O failure poisons the writer until
//! reopened because durability may be uncertain. SHA-256 detects damage; it is
//! not an authenticity proof against a writer with access to this file.
//! IDs are caller-supplied metadata and must themselves exclude secret material.

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::fs::{File, OpenOptions, Permissions};
use std::io::{Read, Seek, SeekFrom, Write};
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::Path;
use thiserror::Error;
use uuid::Uuid;

const MAGIC: &[u8; 8] = b"OBTRJ002";
const FRAME_HEADER_BYTES: usize = 80;

#[derive(Debug, Clone, Copy)]
pub struct SpoolLimits {
    pub max_record_bytes: usize,
    pub max_spool_bytes: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TrajectoryIdentity {
    pub sandbox_id: Uuid,
    pub episode_id: Uuid,
    pub generation: u64,
    pub environment_revision: String,
    pub execution_id: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SequenceScope {
    Lifecycle,
    HostExecution,
    GuestExecution,
    Scoring,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TrajectoryRecord {
    pub identity: TrajectoryIdentity,
    pub scope: SequenceScope,
    pub sequence: u64,
    pub event: TrajectoryEvent,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PayloadDigest {
    pub bytes: u64,
    pub sha256: String,
}

impl PayloadDigest {
    /// Hash at the call site; raw payload bytes never enter the stored type.
    pub fn of(payload: &[u8]) -> Self {
        Self {
            bytes: payload.len() as u64,
            sha256: format!("{:x}", Sha256::digest(payload)),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum OutputStream {
    Stdout,
    Stderr,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LifecycleTransition {
    Creating,
    Ready,
    Resetting,
    Reaped,
    ResetFailed,
    Deleting,
    Deleted,
    Failed,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TerminalOutcome {
    Exited,
    ExecutionFailed,
    Cancelled,
    TimedOut,
    TransportFailed,
    PolicyDenied,
}

/// Only typed metadata is allowed. No arbitrary free-text field, environment,
/// command, raw output, raw error, or score value is stored in event payloads.
/// Identity fields are caller metadata, not a general-purpose secret detector.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case", deny_unknown_fields)]
pub enum TrajectoryEvent {
    Lifecycle {
        transition: LifecycleTransition,
    },
    ExecutionStarted {
        command: PayloadDigest,
        stdin: PayloadDigest,
    },
    Input {
        payload: PayloadDigest,
    },
    Output {
        stream: OutputStream,
        payload: PayloadDigest,
    },
    GuestEvent {
        kind: GuestEventKind,
        guest_sequence: u64,
        payload: PayloadDigest,
    },
    Terminal {
        outcome: TerminalOutcome,
        exit_code: Option<i32>,
    },
    ResourceUsage {
        cpu_micros: u64,
        memory_bytes: u64,
    },
    PolicyDecision {
        allowed: bool,
        policy: PayloadDigest,
    },
    ArtifactManifest {
        manifest: PayloadDigest,
    },
    ScorerHandoff {
        handoff: PayloadDigest,
    },
    ScorerResult {
        result: PayloadDigest,
    },
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GuestEventKind {
    Started,
    Stdout,
    Stderr,
    Exited,
    Error,
    Usage,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AppendReceipt {
    pub offset: u64,
    pub frame_bytes: u64,
    pub checksum: String,
}

#[derive(Debug, Error)]
pub enum TrajectoryError {
    #[error("trajectory spool I/O failed")]
    Io(#[from] std::io::Error),
    #[error("trajectory spool already has a writer")]
    Locked,
    #[error("invalid trajectory spool limits")]
    InvalidLimits,
    #[error("invalid trajectory metadata: {0}")]
    InvalidMetadata(&'static str),
    #[error("trajectory record exceeds maximum size: {bytes} > {limit}")]
    RecordTooLarge { bytes: usize, limit: usize },
    #[error("trajectory spool full: requires {required} bytes, available {available}")]
    Backpressure { required: u64, available: u64 },
    #[error("trajectory sequence mismatch: expected {expected}, received {actual}")]
    Sequence { expected: u64, actual: u64 },
    #[error("trajectory spool corrupt at byte {offset}")]
    Corrupt { offset: u64 },
    #[error("trajectory writer durability uncertain; reopen required")]
    Poisoned,
}

#[derive(Clone, PartialEq, Eq, Hash)]
struct ScopeKey {
    episode_id: Uuid,
    execution_id: Option<String>,
    scope: SequenceScope,
}

#[derive(Default)]
struct Index {
    sequences: HashMap<ScopeKey, u64>,
    episodes: HashMap<Uuid, (Uuid, u64, String)>,
}

impl Index {
    fn check(&self, record: &TrajectoryRecord) -> Result<(), TrajectoryError> {
        let identity = &record.identity;
        if identity.generation == 0 || !valid_digest(&identity.environment_revision) {
            return Err(TrajectoryError::InvalidMetadata(
                "episode revision or generation",
            ));
        }
        if identity.execution_id.as_ref().is_some_and(|v| {
            v.is_empty() || v.len() > 256 || !v.bytes().all(|b| b.is_ascii_graphic())
        }) {
            return Err(TrajectoryError::InvalidMetadata("execution identity"));
        }
        if matches!(
            record.scope,
            SequenceScope::HostExecution | SequenceScope::GuestExecution
        ) && identity.execution_id.is_none()
        {
            return Err(TrajectoryError::InvalidMetadata(
                "execution scope requires execution identity",
            ));
        }
        if let Some(previous) = self.episodes.get(&identity.episode_id) {
            if previous
                != &(
                    identity.sandbox_id,
                    identity.generation,
                    identity.environment_revision.clone(),
                )
            {
                return Err(TrajectoryError::InvalidMetadata("episode identity changed"));
            }
        }
        let expected = self
            .sequences
            .get(&key(record))
            .copied()
            .unwrap_or(0)
            .checked_add(1)
            .ok_or(TrajectoryError::InvalidMetadata("sequence overflow"))?;
        if record.sequence != expected {
            return Err(TrajectoryError::Sequence {
                expected,
                actual: record.sequence,
            });
        }
        let payload = match &record.event {
            TrajectoryEvent::ExecutionStarted { command, stdin } => {
                validate_payload(command)?;
                Some(stdin)
            }
            TrajectoryEvent::Input { payload }
            | TrajectoryEvent::Output { payload, .. }
            | TrajectoryEvent::GuestEvent { payload, .. } => Some(payload),
            TrajectoryEvent::PolicyDecision { policy, .. } => Some(policy),
            TrajectoryEvent::ArtifactManifest { manifest } => Some(manifest),
            TrajectoryEvent::ScorerHandoff { handoff } => Some(handoff),
            TrajectoryEvent::ScorerResult { result } => Some(result),
            _ => None,
        };
        if let Some(payload) = payload {
            validate_payload(payload)?;
        }
        Ok(())
    }

    fn insert(&mut self, record: &TrajectoryRecord) {
        let identity = &record.identity;
        self.sequences.insert(key(record), record.sequence);
        self.episodes.entry(identity.episode_id).or_insert_with(|| {
            (
                identity.sandbox_id,
                identity.generation,
                identity.environment_revision.clone(),
            )
        });
    }
}

fn key(record: &TrajectoryRecord) -> ScopeKey {
    ScopeKey {
        episode_id: record.identity.episode_id,
        execution_id: record.identity.execution_id.clone(),
        scope: record.scope,
    }
}

fn valid_digest(value: &str) -> bool {
    let value = value.strip_prefix("sha256:").unwrap_or(value);
    value.len() == 64
        && value
            .bytes()
            .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
}

fn validate_payload(payload: &PayloadDigest) -> Result<(), TrajectoryError> {
    if !valid_digest(&payload.sha256) {
        return Err(TrajectoryError::InvalidMetadata("payload digest"));
    }
    Ok(())
}

pub struct TrajectorySpool {
    file: File,
    limits: SpoolLimits,
    index: Index,
    bytes: u64,
    poisoned: bool,
}

/// Serialize incrementally so even a small configured record limit is honored
/// while encoding, rather than allocating an oversized JSON buffer first.
struct BoundedRecordBuffer {
    bytes: Vec<u8>,
    limit: usize,
    overflow_bytes: usize,
}

impl BoundedRecordBuffer {
    fn new(limit: usize) -> Self {
        Self {
            bytes: Vec::with_capacity(limit.min(1024)),
            limit,
            overflow_bytes: 0,
        }
    }
}

impl Write for BoundedRecordBuffer {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        let attempted = self.bytes.len().saturating_add(bytes.len());
        if attempted > self.limit {
            self.overflow_bytes = attempted;
            return Err(std::io::Error::other("trajectory record limit reached"));
        }
        self.bytes.extend_from_slice(bytes);
        Ok(bytes.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

impl TrajectorySpool {
    pub fn open(path: &Path, limits: SpoolLimits) -> Result<Self, TrajectoryError> {
        if limits.max_record_bytes == 0
            || limits.max_record_bytes > u32::MAX as usize
            || limits.max_spool_bytes == 0
        {
            return Err(TrajectoryError::InvalidLimits);
        }
        let mut options = OpenOptions::new();
        options.read(true).write(true).create(true).mode(0o600);
        // Do not follow a final-path symlink into another user's data.
        #[cfg(target_os = "linux")]
        options.custom_flags(0x20000);
        #[cfg(target_os = "macos")]
        options.custom_flags(0x100);
        let file = options.open(path)?;
        file.try_lock().map_err(|_| TrajectoryError::Locked)?;
        file.set_permissions(Permissions::from_mode(0o600))?;
        // Sync both the inode and its directory entry before claiming durable
        // appends, including when open created the file for the first time.
        file.sync_all()?;
        let parent = path
            .parent()
            .filter(|p| !p.as_os_str().is_empty())
            .unwrap_or_else(|| Path::new("."));
        File::open(parent)?.sync_all()?;
        let bytes = file.metadata()?.len();
        if bytes > limits.max_spool_bytes {
            return Err(TrajectoryError::Backpressure {
                required: bytes,
                available: limits.max_spool_bytes,
            });
        }
        let mut spool = Self {
            file,
            limits,
            index: Index::default(),
            bytes,
            poisoned: false,
        };
        let (_, index, valid_bytes) = spool.scan(false)?;
        spool.index = index;
        if valid_bytes < bytes {
            spool.file.set_len(valid_bytes)?;
            spool.file.sync_data()?;
            spool.bytes = valid_bytes;
        }
        spool.file.seek(SeekFrom::End(0))?;
        Ok(spool)
    }

    pub fn bytes(&self) -> u64 {
        self.bytes
    }

    pub fn next_sequence(
        &self,
        identity: &TrajectoryIdentity,
        scope: SequenceScope,
    ) -> Result<u64, TrajectoryError> {
        self.index
            .sequences
            .get(&ScopeKey {
                episode_id: identity.episode_id,
                execution_id: identity.execution_id.clone(),
                scope,
            })
            .copied()
            .unwrap_or(0)
            .checked_add(1)
            .ok_or(TrajectoryError::InvalidMetadata("sequence overflow"))
    }

    pub fn append(&mut self, record: &TrajectoryRecord) -> Result<AppendReceipt, TrajectoryError> {
        if self.poisoned {
            return Err(TrajectoryError::Poisoned);
        }
        self.index.check(record)?;
        let mut buffer = BoundedRecordBuffer::new(self.limits.max_record_bytes);
        if serde_json::to_writer(&mut buffer, record).is_err() {
            return if buffer.overflow_bytes > 0 {
                Err(TrajectoryError::RecordTooLarge {
                    bytes: buffer.overflow_bytes,
                    limit: self.limits.max_record_bytes,
                })
            } else {
                Err(TrajectoryError::InvalidMetadata("serialization"))
            };
        }
        let payload = buffer.bytes;
        let frame_bytes = FRAME_HEADER_BYTES as u64 + payload.len() as u64;
        let available = self.limits.max_spool_bytes.saturating_sub(self.bytes);
        if frame_bytes > available {
            return Err(TrajectoryError::Backpressure {
                required: frame_bytes,
                available,
            });
        }
        let length = (payload.len() as u32).to_be_bytes();
        let digest = frame_digest(length, &payload);
        self.poisoned = true;
        self.file.seek(SeekFrom::Start(self.bytes))?;
        self.file.write_all(MAGIC)?;
        self.file.write_all(&length)?;
        self.file
            .write_all(&(!(payload.len() as u32)).to_be_bytes())?;
        self.file.write_all(&digest)?;
        self.file.write_all(&header_digest(length, digest))?;
        self.file.write_all(&payload)?;
        self.file.sync_data()?;
        let receipt = AppendReceipt {
            offset: self.bytes,
            frame_bytes,
            checksum: digest.iter().map(|b| format!("{b:02x}")).collect(),
        };
        self.bytes += frame_bytes;
        self.index.insert(record);
        self.poisoned = false;
        Ok(receipt)
    }

    /// Explicit bounded inspection API; normal appends keep only sequence indexes.
    pub fn read_records(&mut self) -> Result<Vec<TrajectoryRecord>, TrajectoryError> {
        if self.poisoned {
            return Err(TrajectoryError::Poisoned);
        }
        let (records, _, valid_bytes) = self.scan(true)?;
        if valid_bytes != self.bytes {
            return Err(TrajectoryError::Corrupt {
                offset: valid_bytes,
            });
        }
        Ok(records)
    }

    fn scan(
        &mut self,
        collect: bool,
    ) -> Result<(Vec<TrajectoryRecord>, Index, u64), TrajectoryError> {
        self.file.seek(SeekFrom::Start(0))?;
        let mut index = Index::default();
        let mut records = Vec::new();
        let mut offset = 0;
        while offset < self.bytes {
            let remaining = self.bytes - offset;
            let mut header = [0u8; FRAME_HEADER_BYTES];
            let header_bytes = remaining.min(FRAME_HEADER_BYTES as u64) as usize;
            self.file.read_exact(&mut header[..header_bytes])?;
            if header[..header_bytes.min(MAGIC.len())] != MAGIC[..header_bytes.min(MAGIC.len())] {
                return Err(TrajectoryError::Corrupt { offset });
            }
            if header_bytes < FRAME_HEADER_BYTES {
                return Ok((records, index, offset));
            }
            let length = u32::from_be_bytes(header[8..12].try_into().unwrap());
            let inverse = u32::from_be_bytes(header[12..16].try_into().unwrap());
            if length == 0
                || length != !inverse
                || length as usize > self.limits.max_record_bytes
                || Sha256::digest(&header[..48]).as_slice() != &header[48..80]
            {
                return Err(TrajectoryError::Corrupt { offset });
            }
            if remaining - (FRAME_HEADER_BYTES as u64) < length as u64 {
                return Ok((records, index, offset));
            }
            let mut payload = vec![0; length as usize];
            self.file.read_exact(&mut payload)?;
            if frame_digest(length.to_be_bytes(), &payload).as_slice() != &header[16..48] {
                return Err(TrajectoryError::Corrupt { offset });
            }
            let record: TrajectoryRecord = serde_json::from_slice(&payload)
                .map_err(|_| TrajectoryError::Corrupt { offset })?;
            index
                .check(&record)
                .map_err(|_| TrajectoryError::Corrupt { offset })?;
            index.insert(&record);
            if collect {
                records.push(record);
            }
            offset += FRAME_HEADER_BYTES as u64 + length as u64;
        }
        Ok((records, index, offset))
    }
}

fn frame_digest(length: [u8; 4], payload: &[u8]) -> [u8; 32] {
    let mut digest = Sha256::new();
    digest.update(MAGIC);
    digest.update(length);
    digest.update(payload);
    digest.finalize().into()
}

fn header_digest(length: [u8; 4], payload_digest: [u8; 32]) -> [u8; 32] {
    let mut digest = Sha256::new();
    digest.update(MAGIC);
    digest.update(length);
    digest.update((!u32::from_be_bytes(length)).to_be_bytes());
    digest.update(payload_digest);
    digest.finalize().into()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use tempfile::tempdir;

    fn record(sequence: u64) -> TrajectoryRecord {
        TrajectoryRecord {
            identity: TrajectoryIdentity {
                sandbox_id: uuid::Uuid::nil(),
                episode_id: uuid::Uuid::from_u128(1),
                generation: 1,
                environment_revision: format!("sha256:{}", "a".repeat(64)),
                execution_id: Some("execution-1".into()),
            },
            scope: SequenceScope::GuestExecution,
            sequence,
            event: TrajectoryEvent::Output {
                stream: OutputStream::Stdout,
                payload: PayloadDigest::of(b"secret-output"),
            },
        }
    }

    fn limits() -> SpoolLimits {
        SpoolLimits {
            max_record_bytes: 4096,
            max_spool_bytes: 65536,
        }
    }

    #[test]
    fn ordered_append_reopen_and_payload_is_metadata_only() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("trajectory");
        let mut spool = TrajectorySpool::open(&path, limits()).unwrap();
        spool.append(&record(1)).unwrap();
        assert!(matches!(
            spool.append(&record(1)),
            Err(TrajectoryError::Sequence { .. })
        ));
        assert!(matches!(
            spool.append(&record(3)),
            Err(TrajectoryError::Sequence { .. })
        ));
        spool.append(&record(2)).unwrap();
        drop(spool);
        let mut spool = TrajectorySpool::open(&path, limits()).unwrap();
        assert_eq!(spool.read_records().unwrap(), vec![record(1), record(2)]);
        spool.append(&record(3)).unwrap();
        let bytes = fs::read(&path).unwrap();
        assert!(!bytes
            .windows(b"secret-output".len())
            .any(|v| v == b"secret-output"));
    }

    #[test]
    fn limits_backpressure_without_mutation() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("trajectory");
        let mut spool = TrajectorySpool::open(
            &path,
            SpoolLimits {
                max_record_bytes: 32,
                ..limits()
            },
        )
        .unwrap();
        assert!(matches!(
            spool.append(&record(1)),
            Err(TrajectoryError::RecordTooLarge { .. })
        ));
        assert_eq!(fs::metadata(&path).unwrap().len(), 0);
        drop(spool);
        let mut spool = TrajectorySpool::open(&path, limits()).unwrap();
        spool.append(&record(1)).unwrap();
        let one_record = fs::metadata(&path).unwrap().len();
        drop(spool);
        let mut spool = TrajectorySpool::open(
            &path,
            SpoolLimits {
                max_spool_bytes: one_record,
                ..limits()
            },
        )
        .unwrap();
        assert!(matches!(
            spool.append(&record(2)),
            Err(TrajectoryError::Backpressure { .. })
        ));
        assert_eq!(fs::metadata(&path).unwrap().len(), one_record);
    }

    #[test]
    fn unfinished_tail_recovers_but_completed_corruption_is_rejected() {
        use std::io::Write;
        let directory = tempdir().unwrap();
        let path = directory.path().join("trajectory");
        let mut spool = TrajectorySpool::open(&path, limits()).unwrap();
        spool.append(&record(1)).unwrap();
        let valid_len = fs::metadata(&path).unwrap().len();
        spool.append(&record(2)).unwrap();
        drop(spool);
        let bytes = fs::read(&path).unwrap();
        for cut in [1, 10, FRAME_HEADER_BYTES + 5] {
            fs::write(&path, &bytes[..valid_len as usize + cut]).unwrap();
            let mut spool = TrajectorySpool::open(&path, limits()).unwrap();
            assert_eq!(spool.read_records().unwrap(), vec![record(1)]);
            assert_eq!(fs::metadata(&path).unwrap().len(), valid_len);
        }
        fs::write(&path, &bytes).unwrap();
        let mut file = fs::OpenOptions::new().write(true).open(&path).unwrap();
        use std::io::{Seek, SeekFrom};
        file.seek(SeekFrom::Start(FRAME_HEADER_BYTES as u64 + 3))
            .unwrap();
        file.write_all(&[0xff]).unwrap();
        assert!(matches!(
            TrajectorySpool::open(&path, limits()),
            Err(TrajectoryError::Corrupt { .. })
        ));
        assert_eq!(fs::metadata(&path).unwrap().len(), bytes.len() as u64);
    }

    #[test]
    fn exclusive_writer_and_private_permissions() {
        use std::os::unix::fs::PermissionsExt;
        let directory = tempdir().unwrap();
        let path = directory.path().join("trajectory");
        let spool = TrajectorySpool::open(&path, limits()).unwrap();
        assert!(matches!(
            TrajectorySpool::open(&path, limits()),
            Err(TrajectoryError::Locked)
        ));
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        drop(spool);
        TrajectorySpool::open(&path, limits()).unwrap();
    }

    #[test]
    fn scopes_are_independent_and_episode_provenance_is_fixed() {
        let directory = tempdir().unwrap();
        let mut spool =
            TrajectorySpool::open(&directory.path().join("trajectory"), limits()).unwrap();
        spool.append(&record(1)).unwrap();
        let mut host = record(1);
        host.scope = SequenceScope::HostExecution;
        host.event = TrajectoryEvent::ExecutionStarted {
            command: PayloadDigest::of(b"cmd"),
            stdin: PayloadDigest::of(b""),
        };
        spool.append(&host).unwrap();
        let mut changed = record(2);
        changed.identity.generation += 1;
        assert!(matches!(
            spool.append(&changed),
            Err(TrajectoryError::InvalidMetadata(_))
        ));
        changed = record(2);
        changed.identity.environment_revision = "b".repeat(64);
        assert!(matches!(
            spool.append(&changed),
            Err(TrajectoryError::InvalidMetadata(_))
        ));
        changed = record(2);
        changed.event = TrajectoryEvent::Input {
            payload: PayloadDigest {
                bytes: 1,
                sha256: "secret".into(),
            },
        };
        assert!(matches!(
            spool.append(&changed),
            Err(TrajectoryError::InvalidMetadata(_))
        ));
        spool.append(&record(2)).unwrap();
        assert_eq!(
            spool
                .next_sequence(&host.identity, SequenceScope::HostExecution)
                .unwrap(),
            2
        );
        assert_eq!(
            spool
                .next_sequence(&host.identity, SequenceScope::GuestExecution)
                .unwrap(),
            3
        );
    }

    #[test]
    fn corrupt_length_does_not_truncate_completed_records() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("trajectory");
        let mut spool = TrajectorySpool::open(&path, limits()).unwrap();
        spool.append(&record(1)).unwrap();
        spool.append(&record(2)).unwrap();
        drop(spool);
        let mut bytes = fs::read(&path).unwrap();
        bytes[8] ^= 1;
        fs::write(&path, &bytes).unwrap();
        assert!(matches!(
            TrajectorySpool::open(&path, limits()),
            Err(TrajectoryError::Corrupt { offset: 0 })
        ));
        assert_eq!(fs::read(&path).unwrap(), bytes);
    }

    #[test]
    fn corrupted_length_and_inverse_cannot_hide_completed_frames() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("trajectory");
        let mut spool = TrajectorySpool::open(&path, limits()).unwrap();
        spool.append(&record(1)).unwrap();
        spool.append(&record(2)).unwrap();
        drop(spool);
        let mut bytes = fs::read(&path).unwrap();
        assert!(bytes.len() < 4096);
        bytes[8..12].copy_from_slice(&4096_u32.to_be_bytes());
        bytes[12..16].copy_from_slice(&(!4096_u32).to_be_bytes());
        fs::write(&path, &bytes).unwrap();
        assert!(matches!(
            TrajectorySpool::open(&path, limits()),
            Err(TrajectoryError::Corrupt { offset: 0 })
        ));
        assert_eq!(fs::read(&path).unwrap(), bytes);
    }

    #[test]
    fn valid_checksum_cannot_hide_reordered_records() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("trajectory");
        let mut spool = TrajectorySpool::open(&path, limits()).unwrap();
        let first = spool.append(&record(1)).unwrap();
        spool.append(&record(2)).unwrap();
        drop(spool);
        let bytes = fs::read(&path).unwrap();
        let (first_frame, second_frame) = bytes.split_at(first.frame_bytes as usize);
        fs::write(&path, [second_frame, first_frame].concat()).unwrap();
        assert!(matches!(
            TrajectorySpool::open(&path, limits()),
            Err(TrajectoryError::Corrupt { offset: 0 })
        ));
        assert_eq!(fs::metadata(&path).unwrap().len(), bytes.len() as u64);
    }

    #[test]
    fn receipt_checksum_matches_durable_frame() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("trajectory");
        let mut spool = TrajectorySpool::open(&path, limits()).unwrap();
        let receipt = spool.append(&record(1)).unwrap();
        let bytes = fs::read(path).unwrap();
        assert_eq!(
            receipt.checksum,
            bytes[16..48]
                .iter()
                .map(|b| format!("{b:02x}"))
                .collect::<String>()
        );
    }

    #[test]
    fn final_path_symlink_is_rejected_without_touching_target() {
        use std::os::unix::fs::{symlink, PermissionsExt};
        let directory = tempdir().unwrap();
        let target = directory.path().join("target");
        let link = directory.path().join("link");
        fs::write(&target, b"unrelated").unwrap();
        fs::set_permissions(&target, fs::Permissions::from_mode(0o644)).unwrap();
        symlink(&target, &link).unwrap();
        assert!(TrajectorySpool::open(&link, limits()).is_err());
        assert_eq!(fs::read(&target).unwrap(), b"unrelated");
        assert_eq!(
            fs::metadata(&target).unwrap().permissions().mode() & 0o777,
            0o644
        );
    }

    #[test]
    fn serialization_buffer_never_exceeds_record_limit() {
        let mut buffer = BoundedRecordBuffer::new(32);
        assert!(serde_json::to_writer(&mut buffer, &record(1)).is_err());
        assert!(buffer.bytes.len() <= 32);
        assert!(buffer.overflow_bytes > 32);
    }
}
