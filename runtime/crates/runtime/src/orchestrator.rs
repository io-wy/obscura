use crate::model::{LaunchSpec, SandboxHandle};
use crate::network::{
    addresses_for_slot, CommandRunner, NetworkHandle, NetworkManager, NetworkSpec,
};
use crate::sandbox::{Sandbox, SandboxState};
use crate::storage::{EpisodeRecord, LaunchProfile, StorageError, Store};
use crate::trajectory::{
    LifecycleTransition, SequenceScope, TrajectoryEvent, TrajectoryIdentity, TrajectoryRecord,
    TrajectorySpool,
};
use crate::vmm::Vmm;
use sha2::{Digest, Sha256};
use std::fs;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use std::time::{SystemTime, UNIX_EPOCH};
use thiserror::Error;
use uuid::Uuid;

pub struct NodeConfig {
    pub template_ref: String,
    pub kernel: PathBuf,
    pub rootfs: PathBuf,
    pub state_dir: PathBuf,
    pub uplink: String,
    pub boot_args: String,
    pub guest_ready_timeout: Duration,
}

pub struct Resources {
    pub vcpus: u32,
    pub memory_mib: u32,
    pub disk_mib: u64,
}

#[derive(Debug, Error)]
pub enum OrchestratorError {
    #[error("invalid request: {0}")]
    Invalid(String),
    #[error("sandbox identity conflict: {0}")]
    Conflict(String),
    #[error("sandbox unavailable: {0}")]
    Unavailable(String),
    #[error("storage: {0}")]
    Storage(#[from] StorageError),
    #[error("network: {0}")]
    Network(#[from] crate::network::NetworkError),
    #[error("vmm: {0}")]
    Vmm(#[from] crate::provider::RuntimeError),
    #[error("filesystem: {0}")]
    Io(#[from] std::io::Error),
    #[error("image: {0}")]
    Image(#[from] crate::image::ImageError),
    #[error("trajectory: {0}")]
    Trajectory(#[from] crate::trajectory::TrajectoryError),
}

pub struct Orchestrator<V, R> {
    config: NodeConfig,
    store: Store,
    vmm: V,
    network: NetworkManager<R>,
    trajectory: Option<Arc<Mutex<TrajectorySpool>>>,
}

impl<V: Vmm, R: CommandRunner> Orchestrator<V, R> {
    pub fn new(config: NodeConfig, store: Store, vmm: V, network: NetworkManager<R>) -> Self {
        Self {
            config,
            store,
            vmm,
            network,
            trajectory: None,
        }
    }

    pub fn with_trajectory(mut self, spool: TrajectorySpool) -> Self {
        self.trajectory = Some(Arc::new(Mutex::new(spool)));
        self
    }

    pub fn trajectory(&self) -> Option<Arc<Mutex<TrajectorySpool>>> {
        self.trajectory.clone()
    }

    fn record_lifecycle(
        &self,
        id: Uuid,
        transition: LifecycleTransition,
    ) -> Result<(), OrchestratorError> {
        if let Some(spool) = &self.trajectory {
            let episode = self.store.episode(id)?;
            let identity = TrajectoryIdentity {
                sandbox_id: id,
                episode_id: episode.episode_id,
                generation: episode.generation,
                environment_revision: episode.environment_revision,
                execution_id: None,
            };
            let mut spool = spool.lock().expect("trajectory lock poisoned");
            let sequence = spool.next_sequence(&identity, SequenceScope::Lifecycle)?;
            spool.append(&TrajectoryRecord {
                identity,
                scope: SequenceScope::Lifecycle,
                sequence,
                event: TrajectoryEvent::Lifecycle { transition },
            })?;
        }
        Ok(())
    }

    /// Capture failure prevents reuse even if the candidate process still exists.
    /// The retained handle allows an explicit Delete to stop and reap it.
    pub fn fail_capture(&mut self, id: Uuid) -> Result<(), OrchestratorError> {
        let mut sandbox = self.store.get_sandbox(id)?;
        if sandbox.state != SandboxState::Deleted {
            sandbox.state = SandboxState::Failed;
            sandbox.updated_at_unix_nanos = now();
            self.store.upsert_sandbox(&sandbox)?;
        }
        Ok(())
    }

    pub fn create(
        &mut self,
        template_ref: &str,
        resources: Resources,
        allow_egress: bool,
    ) -> Result<Sandbox, OrchestratorError> {
        self.create_with_identity(template_ref, resources, allow_egress, None, "")
    }

    pub fn create_with_identity(
        &mut self,
        template_ref: &str,
        resources: Resources,
        allow_egress: bool,
        sandbox_id: Option<Uuid>,
        spec_hash: &str,
    ) -> Result<Sandbox, OrchestratorError> {
        self.create_pinned_with_identity(
            template_ref,
            resources,
            allow_egress,
            sandbox_id,
            spec_hash,
            None,
        )
    }

    pub fn create_pinned_with_identity(
        &mut self,
        template_ref: &str,
        resources: Resources,
        allow_egress: bool,
        sandbox_id: Option<Uuid>,
        spec_hash: &str,
        expected_rootfs_digest: Option<&str>,
    ) -> Result<Sandbox, OrchestratorError> {
        if sandbox_id.is_some() && spec_hash.is_empty() {
            return Err(OrchestratorError::Invalid(
                "spec_hash is required with sandbox_id".into(),
            ));
        }
        let fingerprint = format!(
            "{:x}",
            Sha256::digest(
                serde_json::to_vec(&(
                    template_ref,
                    resources.vcpus,
                    resources.memory_mib,
                    resources.disk_mib,
                    allow_egress,
                    spec_hash,
                ))
                .expect("request tuple is serializable")
            )
        );
        if let Some(id) = sandbox_id {
            match self.get(id) {
                Ok(existing) => {
                    if self.store.fingerprint(id)? != fingerprint {
                        return Err(OrchestratorError::Conflict(
                            "immutable specification differs".into(),
                        ));
                    }
                    return match existing.state {
                        SandboxState::Ready | SandboxState::Running | SandboxState::Paused => {
                            Ok(existing)
                        }
                        _ => Err(OrchestratorError::Unavailable(format!(
                            "identity {id} is {:?}; create will not replace it",
                            existing.state
                        ))),
                    };
                }
                Err(OrchestratorError::Storage(StorageError::NotFound)) => {}
                Err(error) => return Err(error),
            }
        }
        if template_ref != self.config.template_ref
            || resources.vcpus == 0
            || resources.memory_mib == 0
            || resources.disk_mib == 0
            || resources.disk_mib.checked_mul(1024 * 1024).is_none()
        {
            return Err(OrchestratorError::Invalid(
                "unknown template or invalid resources".into(),
            ));
        }
        let id = sandbox_id.unwrap_or_else(Uuid::new_v4);
        let slot = self.store.next_network_slot()?;
        let (host_ipv4, guest_ipv4) = addresses_for_slot(slot)?;
        let state_dir = self.config.state_dir.join(id.to_string());
        let disk = state_dir.join("rootfs.ext4");
        let mut sandbox = Sandbox {
            id,
            template_ref: template_ref.into(),
            state: SandboxState::Creating,
            // The process might start before its concrete handle can be persisted.
            runtime_handle: Some(String::new()),
            network_slot: slot,
            updated_at_unix_nanos: now(),
        };
        let profile = LaunchProfile {
            vcpus: resources.vcpus,
            memory_mib: resources.memory_mib,
            disk_mib: resources.disk_mib,
            allow_egress,
            rootfs_digest: crate::image::file_digest(&self.config.rootfs)?,
            kernel_digest: crate::image::file_digest(&self.config.kernel)?,
        };
        if expected_rootfs_digest
            .is_some_and(|digest| !digest.is_empty() && digest != profile.rootfs_digest)
        {
            return Err(OrchestratorError::Unavailable(
                "rootfs does not match requested image pin".into(),
            ));
        }
        // Request identity and environment identity have different lifetimes.
        // Bind measured launch inputs, never sandbox_id or tracing request_id.
        let environment_revision = format!(
            "sha256:{:x}",
            Sha256::digest(
                serde_json::to_vec(&(
                    template_ref,
                    env!("CARGO_PKG_VERSION"),
                    &profile,
                    &self.config.boot_args,
                    "ephemeral",
                    "nftables-v1",
                ))
                .expect("launch revision is serializable")
            )
        );
        self.store.reserve_runtime_sandbox(
            &sandbox,
            &fingerprint,
            &profile,
            &EpisodeRecord {
                sandbox_id: id,
                episode_id: Uuid::new_v4(),
                generation: 1,
                environment_revision,
            },
        )?;
        let tap_device = format!("ar{}", &id.simple().to_string()[..10]);
        let network_spec = NetworkSpec {
            sandbox_id: id,
            tap_device: tap_device.clone(),
            host_ipv4,
            guest_ipv4,
            prefix_len: 30,
            uplink: self.config.uplink.clone(),
            allow_egress,
        };
        let result = self
            .record_lifecycle(id, LifecycleTransition::Creating)
            .and_then(|()| self.launch(&network_spec, &resources, &disk, &state_dir, 1));
        match result {
            Ok((_network, handle)) => {
                sandbox.runtime_handle = Some(handle.runtime_handle.clone());
                sandbox
                    .transition(SandboxState::Ready)
                    .expect("Creating -> Ready");
                sandbox.updated_at_unix_nanos = now();
                if let Err(error) = self.store.upsert_sandbox(&sandbox) {
                    // Leave the durable Creating reservation and resources for explicit cleanup.
                    return Err(error.into());
                }
                if let Err(error) = self.record_lifecycle(id, LifecycleTransition::Ready) {
                    self.fail_capture(id)?;
                    return Err(error);
                }
                Ok(sandbox)
            }
            Err(error) => {
                sandbox
                    .transition(SandboxState::Failed)
                    .expect("Creating -> Failed");
                sandbox.updated_at_unix_nanos = now();
                let _ = self.store.upsert_sandbox(&sandbox);
                let _ = self.record_lifecycle(id, LifecycleTransition::Failed);
                // Cleanup errors retain the durable resource claim for a later Delete retry.
                let _ = self.cleanup_resources(&mut sandbox);
                Err(error)
            }
        }
    }

    fn launch(
        &mut self,
        network_spec: &NetworkSpec,
        resources: &Resources,
        disk: &PathBuf,
        state_dir: &PathBuf,
        generation: u64,
    ) -> Result<(NetworkHandle, SandboxHandle), OrchestratorError> {
        fs::create_dir_all(state_dir)?;
        let disk_bytes = fs::copy(&self.config.rootfs, disk)?;
        let kernel = state_dir.join("vmlinux");
        fs::copy(&self.config.kernel, &kernel)?;
        let profile = self.store.launch_profile(network_spec.sandbox_id)?;
        crate::image::verify_file_digest(disk, &profile.rootfs_digest)?;
        crate::image::verify_file_digest(&kernel, &profile.kernel_digest)?;
        if disk_bytes > resources.disk_mib * 1024 * 1024 {
            return Err(OrchestratorError::Invalid(
                "rootfs exceeds requested disk size".into(),
            ));
        }
        let network = self.network.provision(network_spec)?;
        let id = network_spec.sandbox_id;
        let spec = LaunchSpec {
            sandbox_id: id,
            template_ref: self.config.template_ref.clone(),
            vcpus: resources.vcpus,
            memory_mib: resources.memory_mib,
            disk_mib: resources.disk_mib,
            kernel_image_path: kernel,
            rootfs_path: disk.clone(),
            boot_args: format!(
                "{} obscura_id={} ip={}:{}:{}:255.255.255.252::eth0:off",
                self.config.boot_args,
                id,
                network_spec.guest_ipv4,
                network_spec.host_ipv4,
                network_spec.host_ipv4
            ),
            tap_device: network_spec.tap_device.clone(),
            guest_mac: format!(
                "02:FC:{:02X}:{:02X}:{:02X}:{:02X}",
                id.as_bytes()[0],
                id.as_bytes()[1],
                id.as_bytes()[2],
                id.as_bytes()[3]
            ),
            vsock: Some(crate::model::VsockSpec {
                guest_cid: network_spec.sandbox_id.as_u128() as u32 | 3,
                uds_path: state_dir.join(format!("guest-{generation}.vsock")),
            }),
        };
        match self.vmm.start(&spec) {
            Ok(handle) => {
                if !self.config.guest_ready_timeout.is_zero() {
                    if let Err(error) = crate::guest::wait_ready_blocking(
                        &state_dir.join(format!("guest-{generation}.vsock")),
                        &id.to_string(),
                        self.config.guest_ready_timeout,
                    ) {
                        return Err(OrchestratorError::Invalid(format!(
                            "guest readiness: {error}"
                        )));
                    }
                }
                Ok((network, handle))
            }
            Err(error) => Err(error.into()),
        }
    }

    pub fn delete(&mut self, id: Uuid) -> Result<(), OrchestratorError> {
        let mut sandbox = match self.store.get_sandbox(id) {
            Ok(sandbox) => sandbox,
            Err(StorageError::NotFound) => return Ok(()),
            Err(error) => return Err(error.into()),
        };
        if sandbox.state == SandboxState::Deleted {
            return Ok(());
        }
        if self.store.is_quarantined(id)? {
            return Err(OrchestratorError::Unavailable(
                "VM ownership lost after restart; operator must fence node resources before cleanup".into()));
        }
        // Persist non-ready state before stopping the VM, including on interrupted deletion.
        sandbox.state = SandboxState::Failed;
        sandbox.updated_at_unix_nanos = now();
        self.store.upsert_sandbox(&sandbox)?;
        self.cleanup_resources(&mut sandbox)?;
        sandbox.state = SandboxState::Deleted;
        sandbox.updated_at_unix_nanos = now();
        self.store.complete_delete(&sandbox)?;
        // Cleanup remains possible when the spool is full. Deleted is durable
        // in SQLite even if its optional audit event cannot be appended.
        let _ = self.record_lifecycle(id, LifecycleTransition::Deleted);
        Ok(())
    }

    pub fn reset_clean(
        &mut self,
        id: Uuid,
        request_id: &str,
    ) -> Result<Sandbox, OrchestratorError> {
        self.reset_clean_with_hook(id, request_id, || {})
    }

    /// Invalidate active transports only after reset intent is durable.
    pub fn reset_clean_with_hook(
        &mut self,
        id: Uuid,
        request_id: &str,
        on_committed: impl FnOnce(),
    ) -> Result<Sandbox, OrchestratorError> {
        if request_id.trim().is_empty() || request_id.len() > 256 {
            return Err(OrchestratorError::Invalid(
                "bounded reset request_id is required".into(),
            ));
        }
        if self.store.reset_completed(id, request_id)? {
            return self.get(id);
        }
        let mut sandbox = self.get(id)?;
        let episode = self.store.episode(id)?;
        let profile = self.store.launch_profile(id)?;
        crate::image::verify_file_digest(&self.config.rootfs, &profile.rootfs_digest)?;
        crate::image::verify_file_digest(&self.config.kernel, &profile.kernel_digest)?;
        self.store
            .begin_reset(id, episode.generation, "clean", request_id)?;
        on_committed();
        sandbox.state = SandboxState::Resetting;
        let result = (|| -> Result<Sandbox, OrchestratorError> {
            self.record_lifecycle(id, LifecycleTransition::Resetting)?;
            self.cleanup_resources(&mut sandbox)?;
            self.store.set_reset_phase(id, "reaped")?;
            self.record_lifecycle(id, LifecycleTransition::Reaped)?;
            let generation = episode.generation + 1;
            let (host_ipv4, guest_ipv4) = addresses_for_slot(sandbox.network_slot)?;
            let network_spec = NetworkSpec {
                sandbox_id: id,
                tap_device: format!("ar{}", &id.simple().to_string()[..10]),
                host_ipv4,
                guest_ipv4,
                prefix_len: 30,
                uplink: self.config.uplink.clone(),
                allow_egress: profile.allow_egress,
            };
            // Persist possible ownership before starting a replacement process.
            sandbox.runtime_handle = Some(String::new());
            self.store.upsert_sandbox(&sandbox)?;
            let state_dir = self.config.state_dir.join(id.to_string());
            let (_, handle) = self.launch(
                &network_spec,
                &Resources {
                    vcpus: profile.vcpus,
                    memory_mib: profile.memory_mib,
                    disk_mib: profile.disk_mib,
                },
                &state_dir.join("rootfs.ext4"),
                &state_dir,
                generation,
            )?;
            sandbox.runtime_handle = Some(handle.runtime_handle);
            sandbox.updated_at_unix_nanos = now();
            self.store.complete_reset(&sandbox, Uuid::new_v4())?;
            sandbox.state = SandboxState::Ready;
            self.record_lifecycle(id, LifecycleTransition::Ready)?;
            Ok(sandbox.clone())
        })();
        if result.is_err() {
            self.store.fail_reset(id)?;
            let _ = self.record_lifecycle(id, LifecycleTransition::ResetFailed);
        }
        result
    }

    pub fn episode(&self, id: Uuid) -> Result<EpisodeRecord, OrchestratorError> {
        Ok(self.store.episode(id)?)
    }

    pub fn reset_completed(&self, id: Uuid, request_id: &str) -> Result<bool, OrchestratorError> {
        Ok(self.store.reset_completed(id, request_id)?)
    }

    pub fn verify_image_pin(&self, digest: &str) -> Result<(), OrchestratorError> {
        if !digest.is_empty() {
            crate::image::verify_file_digest(&self.config.rootfs, digest)?;
        }
        Ok(())
    }

    fn cleanup_resources(&mut self, sandbox: &mut Sandbox) -> Result<(), OrchestratorError> {
        let id = sandbox.id;
        if let Some(runtime_handle) = &sandbox.runtime_handle {
            self.vmm.stop(&SandboxHandle {
                sandbox_id: id,
                runtime_handle: runtime_handle.clone(),
            })?;
            sandbox.runtime_handle = None;
            self.store.upsert_sandbox(sandbox)?;
        }
        self.network.cleanup(&NetworkHandle {
            tap_device: format!("ar{}", &id.simple().to_string()[..10]),
            table_name: format!("ar_{}", id.simple()),
            guest_ipv4: addresses_for_slot(sandbox.network_slot)?.1,
        })?;
        // Also required on Delete: its IP can be assigned to another sandbox.
        self.network
            .clear_connections(addresses_for_slot(sandbox.network_slot)?.1)?;
        let state_dir = self.config.state_dir.join(id.to_string());
        if state_dir.exists() {
            fs::remove_dir_all(state_dir)?;
        }
        Ok(())
    }

    pub fn get(&mut self, id: Uuid) -> Result<Sandbox, OrchestratorError> {
        let mut sandbox = self.store.get_sandbox(id)?;
        if matches!(
            sandbox.state,
            SandboxState::Ready | SandboxState::Running | SandboxState::Paused
        ) {
            let running = match &sandbox.runtime_handle {
                Some(runtime_handle) => self.vmm.is_running(&SandboxHandle {
                    sandbox_id: id,
                    runtime_handle: runtime_handle.clone(),
                })?,
                None => false,
            };
            if !running {
                sandbox.state = SandboxState::Failed;
                sandbox.updated_at_unix_nanos = now();
                self.store.upsert_sandbox(&sandbox)?;
            }
        }
        Ok(sandbox)
    }

    pub fn guest_socket(&mut self, id: Uuid) -> Result<PathBuf, OrchestratorError> {
        let sandbox = self.get(id)?;
        if sandbox.state != SandboxState::Ready && sandbox.state != SandboxState::Running {
            return Err(OrchestratorError::Invalid("sandbox is not ready".into()));
        }
        let generation = self.store.episode(id)?.generation;
        Ok(self
            .config
            .state_dir
            .join(id.to_string())
            .join(format!("guest-{generation}.vsock")))
    }
}

fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("system clock predates epoch")
        .as_nanos() as i64
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::network::NetworkError;
    use crate::provider::FakeFirecracker;

    #[derive(Default)]
    struct RecordingRunner {
        calls: Vec<(String, Vec<String>)>,
    }
    impl CommandRunner for RecordingRunner {
        fn run(
            &mut self,
            program: &str,
            args: &[String],
            _: Option<&[u8]>,
        ) -> Result<(), NetworkError> {
            self.calls.push((program.into(), args.to_vec()));
            Ok(())
        }
    }

    #[test]
    fn persists_distinct_vm_ids_and_network_slots_and_releases_on_delete() {
        let dir = tempfile::tempdir().unwrap();
        let kernel = dir.path().join("vmlinux");
        let rootfs = dir.path().join("base.ext4");
        fs::write(&kernel, b"kernel").unwrap();
        fs::write(&rootfs, b"rootfs").unwrap();
        let store = Store::open(dir.path().join("node.db")).unwrap();
        let mut node = Orchestrator::new(
            NodeConfig {
                template_ref: "base@sha256:test".into(),
                kernel,
                rootfs,
                state_dir: dir.path().join("vm"),
                uplink: "eth0".into(),
                boot_args: "console=ttyS0".into(),
                guest_ready_timeout: Duration::ZERO,
            },
            store,
            FakeFirecracker::new(),
            NetworkManager::new(RecordingRunner::default()),
        );
        let first = node
            .create(
                "base@sha256:test",
                Resources {
                    vcpus: 1,
                    memory_mib: 256,
                    disk_mib: 64,
                },
                false,
            )
            .unwrap();
        let second = node
            .create(
                "base@sha256:test",
                Resources {
                    vcpus: 1,
                    memory_mib: 256,
                    disk_mib: 64,
                },
                false,
            )
            .unwrap();
        assert_ne!(first.id, second.id);
        assert_ne!(first.network_slot, second.network_slot);
        assert_eq!(first.state, SandboxState::Ready);
        assert!(node
            .config
            .state_dir
            .join(first.id.to_string())
            .join("rootfs.ext4")
            .exists());
        assert_eq!(node.get(first.id).unwrap().id, first.id);
        node.delete(first.id).unwrap();
        assert_eq!(node.get(first.id).unwrap().state, SandboxState::Deleted);
        assert!(!node.config.state_dir.join(first.id.to_string()).exists());
    }

    fn fixture(dir: &std::path::Path) -> Orchestrator<FakeFirecracker, RecordingRunner> {
        fs::write(dir.join("vmlinux"), b"kernel").unwrap();
        fs::write(dir.join("base.ext4"), b"rootfs").unwrap();
        Orchestrator::new(
            NodeConfig {
                template_ref: "base".into(),
                kernel: dir.join("vmlinux"),
                rootfs: dir.join("base.ext4"),
                state_dir: dir.join("vm"),
                uplink: "eth0".into(),
                boot_args: "console=ttyS0".into(),
                guest_ready_timeout: Duration::ZERO,
            },
            Store::open(dir.join("node.db")).unwrap(),
            FakeFirecracker::new(),
            NetworkManager::new(RecordingRunner::default()),
        )
    }

    fn resources() -> Resources {
        Resources {
            vcpus: 1,
            memory_mib: 256,
            disk_mib: 64,
        }
    }

    #[test]
    fn deleting_missing_identity_succeeds() {
        let dir = tempfile::tempdir().unwrap();
        let mut node = fixture(dir.path());
        node.delete(Uuid::new_v4()).unwrap();
    }

    #[test]
    fn clean_reset_replaces_dirty_disk_and_deduplicates_lost_response() {
        let dir = tempfile::tempdir().unwrap();
        let mut node = fixture(dir.path());
        let id = Uuid::new_v4();
        node.create_with_identity("base", resources(), false, Some(id), "spec")
            .unwrap();
        let old_socket = node.guest_socket(id).unwrap();
        let disk = dir
            .path()
            .join("vm")
            .join(id.to_string())
            .join("rootfs.ext4");
        fs::write(&disk, b"candidate dirty state").unwrap();
        node.reset_clean(id, "reset-1").unwrap();
        assert_eq!(fs::read(&disk).unwrap(), b"rootfs");
        assert_ne!(old_socket, node.guest_socket(id).unwrap());
        assert_eq!(node.store.episode(id).unwrap().generation, 2);
        node.reset_clean(id, "reset-1").unwrap();
        assert_eq!(node.vmm.launched.len(), 2);
        assert_eq!(node.store.episode(id).unwrap().generation, 2);
    }

    #[test]
    fn launch_revision_binds_measured_images_and_ignores_sandbox_identity() {
        let dir = tempfile::tempdir().unwrap();
        let mut node = fixture(dir.path());
        let first = node.create("base", resources(), false).unwrap();
        let second = node.create("base", resources(), false).unwrap();
        let revision = node.episode(first.id).unwrap().environment_revision;
        assert_eq!(
            revision,
            node.episode(second.id).unwrap().environment_revision
        );
        fs::write(dir.path().join("base.ext4"), b"other rootfs").unwrap();
        let third = node.create("base", resources(), false).unwrap();
        assert_ne!(
            revision,
            node.episode(third.id).unwrap().environment_revision
        );
    }

    #[test]
    fn requested_image_pin_is_bound_to_the_measured_launch_profile() {
        let dir = tempfile::tempdir().unwrap();
        let mut node = fixture(dir.path());
        let id = Uuid::new_v4();
        let wrong = format!("sha256:{}", "a".repeat(64));
        assert!(node
            .create_pinned_with_identity("base", resources(), false, Some(id), "spec", Some(&wrong))
            .is_err());
        assert!(node.vmm.launched.is_empty());
        assert!(matches!(
            node.get(id),
            Err(OrchestratorError::Storage(StorageError::NotFound))
        ));
        let actual = crate::image::file_digest(&dir.path().join("base.ext4")).unwrap();
        node.create_pinned_with_identity(
            "base",
            resources(),
            false,
            Some(id),
            "spec",
            Some(&actual),
        )
        .unwrap();
        assert_eq!(
            fs::read(dir.path().join("vm").join(id.to_string()).join("vmlinux")).unwrap(),
            b"kernel"
        );
    }

    struct FailConntrack;
    impl CommandRunner for FailConntrack {
        fn run(
            &mut self,
            program: &str,
            _: &[String],
            _: Option<&[u8]>,
        ) -> Result<(), NetworkError> {
            if program == "conntrack" {
                Err(NetworkError::CommandFailed {
                    program: program.into(),
                    stderr: "permission denied".into(),
                })
            } else {
                Ok(())
            }
        }
    }

    #[test]
    fn connection_cleanup_failure_blocks_reset_relaunch_and_slot_release() {
        let dir = tempfile::tempdir().unwrap();
        let base = fixture(dir.path());
        let mut node = Orchestrator::new(
            base.config,
            base.store,
            base.vmm,
            NetworkManager::new(FailConntrack),
        );
        let sandbox = node.create("base", resources(), false).unwrap();
        assert!(node.reset_clean(sandbox.id, "reset").is_err());
        assert_eq!(node.get(sandbox.id).unwrap().state, SandboxState::Failed);
        assert_eq!(node.vmm.launched.len(), 1);
        assert!(node.delete(sandbox.id).is_err());
        assert_ne!(
            node.store.next_network_slot().unwrap(),
            sandbox.network_slot
        );
    }

    #[test]
    fn reset_refuses_changed_base_image_before_stopping_vm() {
        let dir = tempfile::tempdir().unwrap();
        let mut node = fixture(dir.path());
        let id = Uuid::new_v4();
        node.create_with_identity("base", resources(), false, Some(id), "spec")
            .unwrap();
        fs::write(dir.path().join("base.ext4"), b"different base").unwrap();
        assert!(node.reset_clean(id, "reset-1").is_err());
        assert!(node.vmm.stopped.is_empty());
        assert_eq!(node.store.episode(id).unwrap().generation, 1);
    }

    #[test]
    fn reset_stop_failure_stays_failed_and_never_relaunches() {
        let dir = tempfile::tempdir().unwrap();
        let base = fixture(dir.path());
        let mut node = Orchestrator::new(
            base.config,
            base.store,
            FlakyStop {
                inner: base.vmm,
                fail_once: true,
            },
            base.network,
        );
        let id = Uuid::new_v4();
        node.create_with_identity("base", resources(), false, Some(id), "spec")
            .unwrap();
        assert!(node.reset_clean(id, "reset-1").is_err());
        assert_eq!(node.get(id).unwrap().state, SandboxState::Failed);
        assert!(node.reset_clean(id, "reset-1").is_err());
        assert_eq!(node.vmm.inner.launched.len(), 1);
        node.delete(id).unwrap();
        assert!(matches!(node.store.reset(id), Err(StorageError::NotFound)));
    }

    #[test]
    fn reset_ready_commit_failure_keeps_old_generation_and_cleanup_handle() {
        let dir = tempfile::tempdir().unwrap();
        let mut node = fixture(dir.path());
        let id = Uuid::new_v4();
        node.create_with_identity("base", resources(), false, Some(id), "spec")
            .unwrap();
        let connection = rusqlite::Connection::open(dir.path().join("node.db")).unwrap();
        connection
            .execute_batch(
                "CREATE TRIGGER fail_reset_ready BEFORE UPDATE ON sandboxes
            WHEN NEW.state='Ready' BEGIN SELECT RAISE(FAIL, 'failure'); END;",
            )
            .unwrap();
        assert!(node.reset_clean(id, "reset-1").is_err());
        assert_eq!(node.store.episode(id).unwrap().generation, 1);
        assert_eq!(node.get(id).unwrap().state, SandboxState::Failed);
        assert!(node.get(id).unwrap().runtime_handle.is_some());
        assert!(!node.store.reset_completed(id, "reset-1").unwrap());
        assert!(node.reset_clean(id, "reset-1").is_err());
        assert_eq!(node.vmm.launched.len(), 2);
        node.delete(id).unwrap();
    }

    struct FailNetworkCleanup;
    impl CommandRunner for FailNetworkCleanup {
        fn run(
            &mut self,
            program: &str,
            args: &[String],
            input: Option<&[u8]>,
        ) -> Result<(), NetworkError> {
            if program == "ip" && args.get(1).map(String::as_str) == Some("delete")
                || input.is_some_and(|bytes| bytes.starts_with(b"delete table"))
            {
                return Err(NetworkError::CommandFailed {
                    program: program.into(),
                    stderr: "denied".into(),
                });
            }
            Ok(())
        }
    }

    #[test]
    fn reset_network_cleanup_failure_does_not_start_replacement() {
        let dir = tempfile::tempdir().unwrap();
        let base = fixture(dir.path());
        let mut node = Orchestrator::new(
            base.config,
            base.store,
            base.vmm,
            NetworkManager::new(FailNetworkCleanup),
        );
        let id = Uuid::new_v4();
        node.create_with_identity("base", resources(), false, Some(id), "spec")
            .unwrap();
        assert!(node.reset_clean(id, "reset-1").is_err());
        assert_eq!(node.get(id).unwrap().state, SandboxState::Failed);
        assert_eq!(node.vmm.launched.len(), 1);
        assert_eq!(node.store.episode(id).unwrap().generation, 1);
    }

    #[test]
    fn reset_replacement_launch_and_readiness_failures_remain_non_ready() {
        // Both failure points occur after the old VM has been stopped. Neither
        // may advance generation or create a successful retry receipt.
        for fail_launch in [true, false] {
            let dir = tempfile::tempdir().unwrap();
            let base = fixture(dir.path());
            let mut node = Orchestrator::new(
                base.config,
                base.store,
                FailReplacement {
                    inner: base.vmm,
                    fail_launch,
                },
                base.network,
            );
            let id = Uuid::new_v4();
            node.create_with_identity("base", resources(), false, Some(id), "spec")
                .unwrap();
            if !fail_launch {
                node.config.guest_ready_timeout = Duration::from_millis(1);
            }
            assert!(node.reset_clean(id, "reset").is_err());
            assert_eq!(node.get(id).unwrap().state, SandboxState::Failed);
            assert_eq!(node.episode(id).unwrap().generation, 1);
            assert!(!node.reset_completed(id, "reset").unwrap());
            assert!(node.reset_clean(id, "reset").is_err());
            node.delete(id).unwrap();
            assert_eq!(node.get(id).unwrap().state, SandboxState::Deleted);
        }
    }

    struct FailReplacement {
        inner: FakeFirecracker,
        fail_launch: bool,
    }
    impl Vmm for FailReplacement {
        fn start(
            &mut self,
            spec: &LaunchSpec,
        ) -> Result<SandboxHandle, crate::provider::RuntimeError> {
            if self.fail_launch && !self.inner.launched.is_empty() {
                return Err(crate::provider::RuntimeError::Provider(
                    "replacement launch failed".into(),
                ));
            }
            self.inner.start(spec)
        }
        fn is_running(
            &mut self,
            handle: &SandboxHandle,
        ) -> Result<bool, crate::provider::RuntimeError> {
            self.inner.is_running(handle)
        }
        fn stop(&mut self, handle: &SandboxHandle) -> Result<(), crate::provider::RuntimeError> {
            Vmm::stop(&mut self.inner, handle)
        }
    }

    #[test]
    fn response_lost_retry_reuses_instance_and_rejects_changed_spec() {
        let dir = tempfile::tempdir().unwrap();
        let mut node = fixture(dir.path());
        let id = Uuid::new_v4();
        let first = node
            .create_with_identity("base", resources(), false, Some(id), "spec-a")
            .unwrap();
        let retry = node
            .create_with_identity("base", resources(), false, Some(id), "spec-a")
            .unwrap();
        assert_eq!(first, retry);
        assert_eq!(node.vmm.launched.len(), 1);
        assert!(node
            .create_with_identity("base", resources(), true, Some(id), "spec-a")
            .is_err());
        assert!(node
            .create_with_identity("base", resources(), false, Some(id), "spec-b")
            .is_err());
        node.delete(id).unwrap();
        node.delete(id).unwrap();
        assert!(node
            .create_with_identity("base", resources(), false, Some(id), "spec-a")
            .is_err());
        assert_eq!(node.vmm.launched.len(), 1);
    }

    #[test]
    fn reopened_identity_is_failed_and_resources_remain_quarantined() {
        let dir = tempfile::tempdir().unwrap();
        let id = Uuid::new_v4();
        {
            let mut node = fixture(dir.path());
            node.create_with_identity("base", resources(), false, Some(id), "spec-a")
                .unwrap();
        }
        let mut restarted = fixture(dir.path());
        assert_eq!(restarted.get(id).unwrap().state, SandboxState::Failed);
        assert!(restarted
            .create_with_identity("base", resources(), false, Some(id), "spec-a")
            .is_err());
        assert!(restarted.delete(id).is_err());
        assert!(dir
            .path()
            .join("vm")
            .join(id.to_string())
            .join("rootfs.ext4")
            .exists());
        assert!(restarted.vmm.launched.is_empty());
    }

    #[test]
    fn creating_identity_can_be_deleted_without_relaunch() {
        let dir = tempfile::tempdir().unwrap();
        let mut node = fixture(dir.path());
        let id = Uuid::new_v4();
        node.create_with_identity("base", resources(), false, Some(id), "spec-a")
            .unwrap();
        let mut sandbox = node.get(id).unwrap();
        sandbox.state = SandboxState::Creating;
        node.store.upsert_sandbox(&sandbox).unwrap();
        assert!(node
            .create_with_identity("base", resources(), false, Some(id), "spec-a")
            .is_err());
        node.delete(id).unwrap();
        assert_eq!(node.get(id).unwrap().state, SandboxState::Deleted);
        assert_eq!(node.vmm.launched.len(), 1);
    }

    struct FlakyStop {
        inner: FakeFirecracker,
        fail_once: bool,
    }
    impl Vmm for FlakyStop {
        fn start(
            &mut self,
            spec: &LaunchSpec,
        ) -> Result<SandboxHandle, crate::provider::RuntimeError> {
            self.inner.start(spec)
        }
        fn is_running(
            &mut self,
            handle: &SandboxHandle,
        ) -> Result<bool, crate::provider::RuntimeError> {
            self.inner.is_running(handle)
        }
        fn stop(&mut self, handle: &SandboxHandle) -> Result<(), crate::provider::RuntimeError> {
            if std::mem::take(&mut self.fail_once) {
                return Err(crate::provider::RuntimeError::Provider(
                    "injected stop failure".into(),
                ));
            }
            Vmm::stop(&mut self.inner, handle)
        }
    }

    #[test]
    fn deletion_stop_failure_preserves_binding_and_retries() {
        let dir = tempfile::tempdir().unwrap();
        let base = fixture(dir.path());
        let mut node = Orchestrator::new(
            base.config,
            base.store,
            FlakyStop {
                inner: base.vmm,
                fail_once: true,
            },
            base.network,
        );
        let id = Uuid::new_v4();
        node.create_with_identity("base", resources(), false, Some(id), "spec-a")
            .unwrap();
        assert!(node.delete(id).is_err());
        let failed = node.get(id).unwrap();
        assert_eq!(failed.state, SandboxState::Failed);
        assert!(failed.runtime_handle.is_some());
        assert!(node
            .config
            .state_dir
            .join(id.to_string())
            .join("rootfs.ext4")
            .exists());
        assert!(node
            .create_with_identity("base", resources(), false, Some(id), "spec-a")
            .is_err());
        node.delete(id).unwrap();
        assert_eq!(node.get(id).unwrap().state, SandboxState::Deleted);
        assert_eq!(node.vmm.inner.launched.len(), 1);
    }
    #[test]
    fn observe_detects_dead_vm_and_duplicate_create_does_not_report_ready() {
        let dir = tempfile::tempdir().unwrap();
        let mut node = fixture(dir.path());
        let id = Uuid::new_v4();
        node.create_with_identity("base", resources(), false, Some(id), "spec-a")
            .unwrap();
        node.vmm.stopped.push(id);
        assert_eq!(node.get(id).unwrap().state, SandboxState::Failed);
        assert!(node
            .create_with_identity("base", resources(), false, Some(id), "spec-a")
            .is_err());
        assert_eq!(node.vmm.launched.len(), 1);
    }
    #[test]
    fn failed_ready_commit_keeps_reservation_and_retry_never_launches_second_vm() {
        let dir = tempfile::tempdir().unwrap();
        let mut node = fixture(dir.path());
        let connection = rusqlite::Connection::open(dir.path().join("node.db")).unwrap();
        connection
            .execute_batch(
                "CREATE TRIGGER inject_ready_failure BEFORE UPDATE ON sandboxes
            WHEN NEW.state='Ready' BEGIN SELECT RAISE(FAIL, 'injected ready commit failure'); END;",
            )
            .unwrap();
        let id = Uuid::new_v4();
        assert!(node
            .create_with_identity("base", resources(), false, Some(id), "spec-a")
            .is_err());
        assert_eq!(node.get(id).unwrap().state, SandboxState::Creating);
        assert!(node
            .create_with_identity("base", resources(), false, Some(id), "spec-a")
            .is_err());
        assert_eq!(node.vmm.launched.len(), 1);
        assert!(node
            .config
            .state_dir
            .join(id.to_string())
            .join("rootfs.ext4")
            .exists());
        node.delete(id).unwrap();
        assert_eq!(node.get(id).unwrap().state, SandboxState::Deleted);
    }
}
