use serde::{Deserialize, Serialize};
use std::path::PathBuf;
use uuid::Uuid;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SandboxHandle {
    pub sandbox_id: Uuid,
    pub runtime_handle: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct LaunchSpec {
    pub sandbox_id: Uuid,
    pub template_ref: String,
    pub vcpus: u32,
    pub memory_mib: u32,
    pub disk_mib: u64,
    pub kernel_image_path: PathBuf,
    pub rootfs_path: PathBuf,
    pub boot_args: String,
    pub tap_device: String,
    pub guest_mac: String,
    pub vsock: Option<VsockSpec>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct VsockSpec {
    pub guest_cid: u32,
    pub uds_path: PathBuf,
}
