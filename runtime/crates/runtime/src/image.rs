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
    let actual = file_digest(path)?;
    if actual != expected {
        return Err(ImageError::DigestMismatch {
            expected: expected.into(),
            actual,
        });
    }
    Ok(())
}

pub fn file_digest(path: &Path) -> Result<String, ImageError> {
    use std::io::Read;
    let mut file = std::fs::File::open(path).map_err(|_| ImageError::Missing(path.into()))?;
    let mut hash = Sha256::new();
    let mut buffer = [0u8; 64 * 1024];
    loop {
        let count = file.read(&mut buffer).map_err(|_| ImageError::Missing(path.into()))?;
        if count == 0 { break; }
        hash.update(&buffer[..count]);
    }
    Ok(format!("sha256:{:x}", hash.finalize()))
}
