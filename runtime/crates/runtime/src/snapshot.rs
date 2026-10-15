use serde::{Deserialize, Serialize};
use std::path::PathBuf;
use uuid::Uuid;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct Snapshot {
    pub id: Uuid,
    pub sandbox_id: Uuid,
    pub snapshot_path: PathBuf,
    pub memory_path: PathBuf,
    pub disk_digest: String,
    pub runtime_version: String,
}

pub trait SnapshotStore {
    fn publish(&self, snapshot: Snapshot) -> Result<(), Box<dyn std::error::Error + Send + Sync>>;
    fn resolve(&self, id: Uuid) -> Result<Snapshot, Box<dyn std::error::Error + Send + Sync>>;
}
