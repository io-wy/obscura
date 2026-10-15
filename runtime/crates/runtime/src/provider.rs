use thiserror::Error;
use uuid::Uuid;

use crate::model::{LaunchSpec, SandboxHandle};

pub trait FirecrackerProvider {
    fn launch(&mut self, spec: &LaunchSpec) -> Result<SandboxHandle, RuntimeError>;
    fn stop(&mut self, handle: &SandboxHandle) -> Result<(), RuntimeError>;
    fn is_running(&mut self, handle: &SandboxHandle) -> Result<bool, RuntimeError>;
}

#[derive(Debug, Error)]
pub enum RuntimeError {
    #[error("template reference is empty")]
    EmptyTemplate,
    #[error("provider error: {0}")]
    Provider(String),
    #[error("invalid launch specification: {0}")]
    InvalidSpec(String),
    #[error("firecracker API error: {0}")]
    Api(String),
    #[error("I/O error: {0}")]
    Io(#[source] std::io::Error),
    #[error("firecracker startup timed out waiting for {0}")]
    StartupTimeout(std::path::PathBuf),
}

pub struct FakeFirecracker {
    pub launched: Vec<SandboxHandle>,
    pub stopped: Vec<Uuid>,
}

impl FakeFirecracker {
    pub fn new() -> Self {
        Self {
            launched: Vec::new(),
            stopped: Vec::new(),
        }
    }
}

impl Default for FakeFirecracker {
    fn default() -> Self {
        Self::new()
    }
}

impl FirecrackerProvider for FakeFirecracker {
    fn launch(&mut self, spec: &LaunchSpec) -> Result<SandboxHandle, RuntimeError> {
        if spec.template_ref.is_empty() {
            return Err(RuntimeError::EmptyTemplate);
        }
        let handle = SandboxHandle {
            sandbox_id: spec.sandbox_id,
            runtime_handle: format!("fake-{}", self.launched.len() + 1),
        };
        self.launched.push(handle.clone());
        Ok(handle)
    }

    fn stop(&mut self, handle: &SandboxHandle) -> Result<(), RuntimeError> {
        self.stopped.push(handle.sandbox_id);
        Ok(())
    }
    fn is_running(&mut self, handle: &SandboxHandle) -> Result<bool, RuntimeError> {
        Ok(self
            .launched
            .iter()
            .any(|item| item.sandbox_id == handle.sandbox_id)
            && !self.stopped.contains(&handle.sandbox_id))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fake_provider_tracks_lifecycle() {
        let mut provider = FakeFirecracker::new();
        let spec = LaunchSpec {
            sandbox_id: Uuid::new_v4(),
            template_ref: "dev-image@sha256:abc".into(),
            vcpus: 1,
            memory_mib: 256,
            disk_mib: 1024,
            kernel_image_path: "/tmp/vmlinux".into(),
            rootfs_path: "/tmp/rootfs.ext4".into(),
            boot_args: "console=ttyS0 reboot=k panic=1 pci=off".into(),
            tap_device: "tap-test".into(),
            guest_mac: "AA:FC:00:00:00:01".into(),
            vsock: None,
        };
        let handle = provider.launch(&spec).expect("launch should succeed");
        provider.stop(&handle).expect("stop should succeed");
        assert_eq!(provider.launched.len(), 1);
        assert_eq!(provider.stopped, vec![handle.sandbox_id]);
    }

    #[test]
    fn fake_provider_rejects_empty_template() {
        let mut provider = FakeFirecracker::new();
        let spec = LaunchSpec {
            sandbox_id: Uuid::new_v4(),
            template_ref: String::new(),
            vcpus: 1,
            memory_mib: 256,
            disk_mib: 1024,
            kernel_image_path: "/tmp/vmlinux".into(),
            rootfs_path: "/tmp/rootfs.ext4".into(),
            boot_args: "console=ttyS0 reboot=k panic=1 pci=off".into(),
            tap_device: "tap-test".into(),
            guest_mac: "AA:FC:00:00:00:01".into(),
            vsock: None,
        };
        assert!(matches!(
            provider.launch(&spec),
            Err(RuntimeError::EmptyTemplate)
        ));
    }
}
