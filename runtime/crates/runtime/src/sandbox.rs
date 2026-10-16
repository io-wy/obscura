use serde::{Deserialize, Serialize};
use uuid::Uuid;

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
pub enum SandboxState {
    Creating,
    Ready,
    Running,
    Paused,
    Snapshotting,
    Resetting,
    Deleted,
    Failed,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct Sandbox {
    pub id: Uuid,
    pub template_ref: String,
    pub state: SandboxState,
    pub runtime_handle: Option<String>,
    pub network_slot: u32,
    pub updated_at_unix_nanos: i64,
}

#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum LifecycleError {
    #[error("invalid sandbox transition: {from:?} -> {to:?}")]
    InvalidTransition {
        from: SandboxState,
        to: SandboxState,
    },
}

impl Sandbox {
    pub fn transition(&mut self, next: SandboxState) -> Result<(), LifecycleError> {
        let valid = matches!(
            (self.state, next),
            (
                SandboxState::Creating,
                SandboxState::Ready | SandboxState::Failed
            ) | (
                SandboxState::Ready,
                SandboxState::Running
                    | SandboxState::Resetting
                    | SandboxState::Paused
                    | SandboxState::Deleted
                    | SandboxState::Failed
            ) | (
                SandboxState::Running,
                SandboxState::Ready
                    | SandboxState::Resetting
                    | SandboxState::Paused
                    | SandboxState::Snapshotting
                    | SandboxState::Deleted
                    | SandboxState::Failed
            ) | (
                SandboxState::Paused,
                SandboxState::Running | SandboxState::Resetting | SandboxState::Deleted | SandboxState::Failed
            ) | (
                SandboxState::Snapshotting,
                SandboxState::Paused | SandboxState::Ready | SandboxState::Failed
            ) | (SandboxState::Resetting, SandboxState::Ready | SandboxState::Failed)
                | (SandboxState::Failed, SandboxState::Deleted)
        );
        if !valid {
            return Err(LifecycleError::InvalidTransition {
                from: self.state,
                to: next,
            });
        }
        self.state = next;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lifecycle_rejects_deleted_to_running() {
        let mut sandbox = Sandbox {
            id: Uuid::nil(),
            template_ref: "base".into(),
            state: SandboxState::Deleted,
            runtime_handle: None,
            network_slot: 1,
            updated_at_unix_nanos: 0,
        };
        assert!(sandbox.transition(SandboxState::Running).is_err());
    }
}
