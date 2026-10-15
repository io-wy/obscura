use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::path::{Path, PathBuf};
use thiserror::Error;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ImageManifest {
    pub digest: String,
    pub architecture: String,
    pub kernel_path: PathBuf,
    pub rootfs_path: PathBuf,
    pub runtime_protocol: String,
}

#[derive(Debug, Error)]
pub enum ImageError {
    #[error("image file not found: {0}")]
    Missing(PathBuf),
    #[error("image digest mismatch: expected {expected}, actual {actual}")]
    DigestMismatch { expected: String, actual: String },
}

pub fn verify_file_digest(path: &Path, expected: &str) -> Result<(), ImageError> {
    let bytes = std::fs::read(path).map_err(|_| ImageError::Missing(path.into()))?;
    let actual = format!("sha256:{:x}", Sha256::digest(bytes));
    if actual != expected {
        return Err(ImageError::DigestMismatch {
            expected: expected.into(),
            actual,
        });
    }
    Ok(())
}
