use serde::Serialize;
use sha2::{Digest, Sha256};
use std::path::Path;
use thiserror::Error;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VersionPins {
    pub runtime_version: String,
    pub image_digest: String,
    pub dependency_lock: String,
    pub dataset_version: String,
    pub scorer_version: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkspaceMode {
    Ephemeral,
    Persistent,
    Snapshot,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EnvironmentSpec {
    pub template_ref: String,
    pub pins: VersionPins,
    pub vcpus: u32,
    pub memory_mib: u32,
    pub disk_mib: u64,
    pub allow_egress: bool,
    pub allow_hosts: Vec<String>,
    pub network_enforcement: String,
    pub workspace_mode: WorkspaceMode,
    pub workspace_reference: String,
    pub credential_policy_ref: String,
    pub credentials_brokered: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EnvironmentRevision {
    pub digest: String,
    pub runtime_version: String,
    pub image_digest: String,
}

#[derive(Debug, Error, PartialEq, Eq)]
pub enum RevisionError {
    #[error("environment field is required: {0}")]
    MissingField(&'static str),
    #[error("template reference must contain an immutable sha256 digest")]
    MutableTemplate,
    #[error("image digest must be lowercase sha256:<64 hex characters>")]
    InvalidImageDigest,
    #[error("runtime version mismatch: requested {requested}, actual {actual}")]
    RuntimeVersionMismatch { requested: String, actual: String },
    #[error("image digest mismatch: requested {requested}, actual {actual}")]
    ImageDigestMismatch { requested: String, actual: String },
    #[error("resources must be non-zero")]
    InvalidResources,
    #[error("workspace reference does not match workspace mode")]
    InvalidWorkspace,
    #[error("credential policies must use brokered credentials")]
    UnbrokeredCredentials,
    #[error("unsupported network enforcement mode")]
    InvalidNetworkEnforcement,
    #[error("cannot read environment image: {0}")]
    ImageRead(String),
    #[error("cannot encode environment revision: {0}")]
    Encoding(String),
}

impl EnvironmentRevision {
    pub fn verify(
        spec: &EnvironmentSpec,
        actual_runtime_version: &str,
        image_path: &Path,
    ) -> Result<Self, RevisionError> {
        validate(spec, actual_runtime_version)?;
        let image = std::fs::read(image_path)
            .map_err(|error| RevisionError::ImageRead(error.to_string()))?;
        let actual_image_digest = format!("sha256:{:x}", Sha256::digest(image));
        if actual_image_digest != spec.pins.image_digest {
            return Err(RevisionError::ImageDigestMismatch {
                requested: spec.pins.image_digest.clone(),
                actual: actual_image_digest,
            });
        }
        let mut allow_hosts = spec.allow_hosts.clone();
        allow_hosts.sort_unstable();
        allow_hosts.dedup();
        let canonical = CanonicalEnvironment {
            template_ref: &spec.template_ref,
            runtime_version: &spec.pins.runtime_version,
            image_digest: &spec.pins.image_digest,
            dependency_lock: &spec.pins.dependency_lock,
            dataset_version: &spec.pins.dataset_version,
            scorer_version: &spec.pins.scorer_version,
            vcpus: spec.vcpus,
            memory_mib: spec.memory_mib,
            disk_mib: spec.disk_mib,
            allow_egress: spec.allow_egress,
            allow_hosts: &allow_hosts,
            network_enforcement: &spec.network_enforcement,
            workspace_mode: spec.workspace_mode,
            workspace_reference: &spec.workspace_reference,
            credential_policy_ref: &spec.credential_policy_ref,
            credentials_brokered: spec.credentials_brokered,
        };
        let bytes = serde_json::to_vec(&canonical)
            .map_err(|error| RevisionError::Encoding(error.to_string()))?;
        Ok(Self {
            digest: format!("sha256:{:x}", Sha256::digest(bytes)),
            runtime_version: actual_runtime_version.into(),
            image_digest: actual_image_digest,
        })
    }
}

#[derive(Serialize)]
struct CanonicalEnvironment<'a> {
    template_ref: &'a str,
    runtime_version: &'a str,
    image_digest: &'a str,
    dependency_lock: &'a str,
    dataset_version: &'a str,
    scorer_version: &'a str,
    vcpus: u32,
    memory_mib: u32,
    disk_mib: u64,
    allow_egress: bool,
    allow_hosts: &'a [String],
    network_enforcement: &'a str,
    workspace_mode: WorkspaceMode,
    workspace_reference: &'a str,
    credential_policy_ref: &'a str,
    credentials_brokered: bool,
}

fn validate(spec: &EnvironmentSpec, actual_runtime_version: &str) -> Result<(), RevisionError> {
    for (name, value) in [
        ("template_ref", spec.template_ref.as_str()),
        ("runtime_version", spec.pins.runtime_version.as_str()),
        ("image_digest", spec.pins.image_digest.as_str()),
        ("dependency_lock", spec.pins.dependency_lock.as_str()),
        ("dataset_version", spec.pins.dataset_version.as_str()),
        ("scorer_version", spec.pins.scorer_version.as_str()),
        ("network_enforcement", spec.network_enforcement.as_str()),
    ] {
        if value.trim().is_empty() {
            return Err(RevisionError::MissingField(name));
        }
    }
    let template_digest = spec
        .template_ref
        .rsplit_once("@sha256:")
        .map(|(_, digest)| digest)
        .filter(|digest| valid_hex_digest(digest));
    if template_digest.is_none() {
        return Err(RevisionError::MutableTemplate);
    }
    let image_digest = spec
        .pins
        .image_digest
        .strip_prefix("sha256:")
        .filter(|digest| valid_hex_digest(digest));
    if image_digest.is_none() {
        return Err(RevisionError::InvalidImageDigest);
    }
    if spec.pins.runtime_version != actual_runtime_version {
        return Err(RevisionError::RuntimeVersionMismatch {
            requested: spec.pins.runtime_version.clone(),
            actual: actual_runtime_version.into(),
        });
    }
    if spec.vcpus == 0 || spec.memory_mib == 0 || spec.disk_mib == 0 {
        return Err(RevisionError::InvalidResources);
    }
    let workspace_valid = match spec.workspace_mode {
        WorkspaceMode::Ephemeral => spec.workspace_reference.is_empty(),
        WorkspaceMode::Persistent | WorkspaceMode::Snapshot => {
            !spec.workspace_reference.trim().is_empty()
        }
    };
    if !workspace_valid {
        return Err(RevisionError::InvalidWorkspace);
    }
    if !spec.credential_policy_ref.is_empty() && !spec.credentials_brokered {
        return Err(RevisionError::UnbrokeredCredentials);
    }
    if !matches!(
        spec.network_enforcement.as_str(),
        "rejectUnsupported" | "auditOnly" | "enforced"
    ) {
        return Err(RevisionError::InvalidNetworkEnforcement);
    }
    Ok(())
}

fn valid_hex_digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spec(image_digest: String) -> EnvironmentSpec {
        EnvironmentSpec {
            template_ref: "registry.example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa".into(),
            pins: VersionPins {
                runtime_version: "0.1.0".into(),
                image_digest,
                dependency_lock: "sha256:deps".into(),
                dataset_version: "dataset-v1".into(),
                scorer_version: "scorer-v1".into(),
            },
            vcpus: 2,
            memory_mib: 1024,
            disk_mib: 4096,
            allow_egress: false,
            allow_hosts: vec!["packages.example".into(), "models.example".into()],
            network_enforcement: "enforced".into(),
            workspace_mode: WorkspaceMode::Ephemeral,
            workspace_reference: String::new(),
            credential_policy_ref: "training-readonly".into(),
            credentials_brokered: true,
        }
    }

    fn fixture() -> (tempfile::TempDir, EnvironmentSpec) {
        let dir = tempfile::tempdir().unwrap();
        let image = dir.path().join("rootfs");
        std::fs::write(&image, b"immutable rootfs").unwrap();
        let digest = format!("sha256:{:x}", Sha256::digest(b"immutable rootfs"));
        (dir, spec(digest))
    }

    #[test]
    fn complete_environment_has_stable_revision() {
        let (dir, spec) = fixture();
        let image = dir.path().join("rootfs");
        let first = EnvironmentRevision::verify(&spec, "0.1.0", &image).unwrap();
        let second = EnvironmentRevision::verify(&spec, "0.1.0", &image).unwrap();
        assert_eq!(first, second);
        assert!(first.digest.starts_with("sha256:"));
    }

    #[test]
    fn every_policy_dimension_changes_the_revision() {
        let (dir, base) = fixture();
        let image = dir.path().join("rootfs");
        let baseline = EnvironmentRevision::verify(&base, "0.1.0", &image)
            .unwrap()
            .digest;
        let variants = [
            {
                let mut value = base.clone();
                value.pins.dependency_lock = "sha256:other".into();
                value
            },
            {
                let mut value = base.clone();
                value.pins.dataset_version = "dataset-v2".into();
                value
            },
            {
                let mut value = base.clone();
                value.pins.scorer_version = "scorer-v2".into();
                value
            },
            {
                let mut value = base.clone();
                value.vcpus += 1;
                value
            },
            {
                let mut value = base.clone();
                value.allow_egress = true;
                value
            },
            {
                let mut value = base.clone();
                value.workspace_mode = WorkspaceMode::Persistent;
                value.workspace_reference = "workspace-1".into();
                value
            },
            {
                let mut value = base.clone();
                value.credential_policy_ref = "training-write".into();
                value
            },
        ];
        for variant in variants {
            assert_ne!(
                EnvironmentRevision::verify(&variant, "0.1.0", &image)
                    .unwrap()
                    .digest,
                baseline
            );
        }
    }

    #[test]
    fn host_order_is_canonical() {
        let (dir, mut first) = fixture();
        let image = dir.path().join("rootfs");
        let mut second = first.clone();
        second.allow_hosts.reverse();
        assert_eq!(
            EnvironmentRevision::verify(&first, "0.1.0", &image)
                .unwrap()
                .digest,
            EnvironmentRevision::verify(&second, "0.1.0", &image)
                .unwrap()
                .digest
        );
        first.allow_hosts.push("models.example".into());
        assert_eq!(
            EnvironmentRevision::verify(&first, "0.1.0", &image)
                .unwrap()
                .digest,
            EnvironmentRevision::verify(&second, "0.1.0", &image)
                .unwrap()
                .digest
        );
    }

    #[test]
    fn rejects_runtime_and_image_mismatches() {
        let (dir, mut spec) = fixture();
        let image = dir.path().join("rootfs");
        assert!(EnvironmentRevision::verify(&spec, "0.2.0", &image).is_err());
        spec.pins.image_digest =
            "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb".into();
        assert!(EnvironmentRevision::verify(&spec, "0.1.0", &image).is_err());
    }

    #[test]
    fn rejects_mutable_or_incomplete_revisions() {
        let (dir, mut spec) = fixture();
        let image = dir.path().join("rootfs");
        spec.template_ref = "registry.example/agent:latest".into();
        assert!(EnvironmentRevision::verify(&spec, "0.1.0", &image).is_err());
        spec.template_ref = "registry.example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa".into();
        spec.pins.dataset_version.clear();
        assert!(EnvironmentRevision::verify(&spec, "0.1.0", &image).is_err());
    }
}
