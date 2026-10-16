use std::collections::{HashMap, HashSet};
use thiserror::Error;
use uuid::Uuid;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EpisodeLease {
    pub sandbox_id: Uuid,
    pub episode_id: Uuid,
    pub generation: u64,
    pub environment_revision: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ExecutionLease {
    pub sandbox_id: Uuid,
    pub execution_id: String,
    pub generation: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResetTicket {
    sandbox_id: Uuid,
    generation: u64,
}

#[derive(Debug, Error, PartialEq, Eq)]
pub enum EpisodeError {
    #[error("sandbox episode is not registered")]
    NotFound,
    #[error("sandbox episode is already registered")]
    AlreadyRegistered,
    #[error("episode generation must be greater than zero")]
    InvalidGeneration,
    #[error("environment revision is required")]
    MissingEnvironmentRevision,
    #[error("execution ID is required")]
    MissingExecutionId,
    #[error("execution ID was already used in this episode")]
    DuplicateExecution,
    #[error("execution is not active")]
    ExecutionNotActive,
    #[error("episode generation is stale")]
    StaleGeneration,
    #[error("sandbox reset is already in progress")]
    ResetInProgress,
    #[error("sandbox is not resetting")]
    ResetNotInProgress,
    #[error("episode executions have not been reaped")]
    ExecutionsNotReaped,
    #[error("episode generation overflow")]
    GenerationOverflow,
}

#[derive(Default)]
pub struct EpisodeTracker {
    states: HashMap<Uuid, EpisodeState>,
}

struct EpisodeState {
    lease: EpisodeLease,
    phase: EpisodePhase,
    seen_executions: HashSet<String>,
    active_executions: HashSet<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum EpisodePhase {
    Active,
    Resetting { reaped: bool },
}

impl EpisodeTracker {
    pub fn register(
        &mut self,
        sandbox_id: Uuid,
        environment_revision: String,
        generation: u64,
    ) -> Result<EpisodeLease, EpisodeError> {
        if self.states.contains_key(&sandbox_id) {
            return Err(EpisodeError::AlreadyRegistered);
        }
        if generation == 0 {
            return Err(EpisodeError::InvalidGeneration);
        }
        if environment_revision.trim().is_empty() {
            return Err(EpisodeError::MissingEnvironmentRevision);
        }
        let lease = EpisodeLease {
            sandbox_id,
            episode_id: Uuid::new_v4(),
            generation,
            environment_revision,
        };
        self.states.insert(
            sandbox_id,
            EpisodeState {
                lease: lease.clone(),
                phase: EpisodePhase::Active,
                seen_executions: HashSet::new(),
                active_executions: HashSet::new(),
            },
        );
        Ok(lease)
    }

    pub fn current(&self, sandbox_id: Uuid) -> Result<EpisodeLease, EpisodeError> {
        self.states
            .get(&sandbox_id)
            .map(|state| state.lease.clone())
            .ok_or(EpisodeError::NotFound)
    }

    pub fn begin_execution(
        &mut self,
        sandbox_id: Uuid,
        execution_id: String,
    ) -> Result<ExecutionLease, EpisodeError> {
        if execution_id.trim().is_empty() {
            return Err(EpisodeError::MissingExecutionId);
        }
        let state = self
            .states
            .get_mut(&sandbox_id)
            .ok_or(EpisodeError::NotFound)?;
        if matches!(state.phase, EpisodePhase::Resetting { .. }) {
            return Err(EpisodeError::ResetInProgress);
        }
        if !state.seen_executions.insert(execution_id.clone()) {
            return Err(EpisodeError::DuplicateExecution);
        }
        state.active_executions.insert(execution_id.clone());
        Ok(ExecutionLease {
            sandbox_id,
            execution_id,
            generation: state.lease.generation,
        })
    }

    pub fn finish_execution(&mut self, lease: &ExecutionLease) -> Result<(), EpisodeError> {
        let state = self
            .states
            .get_mut(&lease.sandbox_id)
            .ok_or(EpisodeError::NotFound)?;
        if lease.generation != state.lease.generation {
            return Err(EpisodeError::StaleGeneration);
        }
        if !state.active_executions.remove(&lease.execution_id) {
            return Err(EpisodeError::ExecutionNotActive);
        }
        if state.active_executions.is_empty() {
            if let EpisodePhase::Resetting { reaped } = &mut state.phase {
                *reaped = true;
            }
        }
        Ok(())
    }

    pub fn begin_reset(&mut self, sandbox_id: Uuid) -> Result<ResetTicket, EpisodeError> {
        let state = self
            .states
            .get_mut(&sandbox_id)
            .ok_or(EpisodeError::NotFound)?;
        if matches!(state.phase, EpisodePhase::Resetting { .. }) {
            return Err(EpisodeError::ResetInProgress);
        }
        state.phase = EpisodePhase::Resetting {
            reaped: state.active_executions.is_empty(),
        };
        Ok(ResetTicket {
            sandbox_id,
            generation: state.lease.generation,
        })
    }

    pub fn confirm_reaped(&mut self, ticket: &ResetTicket) -> Result<(), EpisodeError> {
        let state = self.ticket_state_mut(ticket)?;
        let EpisodePhase::Resetting { reaped } = &mut state.phase else {
            return Err(EpisodeError::ResetNotInProgress);
        };
        state.active_executions.clear();
        *reaped = true;
        Ok(())
    }

    pub fn complete_reset(&mut self, ticket: &ResetTicket) -> Result<EpisodeLease, EpisodeError> {
        let state = self.ticket_state_mut(ticket)?;
        match state.phase {
            EpisodePhase::Active => return Err(EpisodeError::ResetNotInProgress),
            EpisodePhase::Resetting { reaped: false } => {
                return Err(EpisodeError::ExecutionsNotReaped)
            }
            EpisodePhase::Resetting { reaped: true } => {}
        }
        let generation = state
            .lease
            .generation
            .checked_add(1)
            .ok_or(EpisodeError::GenerationOverflow)?;
        state.lease = EpisodeLease {
            sandbox_id: ticket.sandbox_id,
            episode_id: Uuid::new_v4(),
            generation,
            environment_revision: state.lease.environment_revision.clone(),
        };
        state.phase = EpisodePhase::Active;
        state.seen_executions.clear();
        state.active_executions.clear();
        Ok(state.lease.clone())
    }

    fn ticket_state_mut(
        &mut self,
        ticket: &ResetTicket,
    ) -> Result<&mut EpisodeState, EpisodeError> {
        let state = self
            .states
            .get_mut(&ticket.sandbox_id)
            .ok_or(EpisodeError::NotFound)?;
        if state.lease.generation != ticket.generation {
            return Err(EpisodeError::StaleGeneration);
        }
        Ok(state)
    }
}

// The public ExecuteRequest does not contain an episode generation. This tracker
// fences node-internal work and active streams; it cannot identify a brand-new
// request that an external caller delayed until after reset.

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn successful_reset_rotates_episode_and_generation() {
        let sandbox_id = Uuid::new_v4();
        let mut tracker = EpisodeTracker::default();
        let first = tracker
            .register(sandbox_id, "sha256:revision".into(), 7)
            .unwrap();
        let ticket = tracker.begin_reset(sandbox_id).unwrap();
        tracker.confirm_reaped(&ticket).unwrap();
        let second = tracker.complete_reset(&ticket).unwrap();
        assert_eq!(second.sandbox_id, sandbox_id);
        assert_eq!(second.generation, 8);
        assert_ne!(second.episode_id, first.episode_id);
        assert_eq!(second.environment_revision, first.environment_revision);
    }

    #[test]
    fn stale_execution_cannot_finish_in_the_new_episode() {
        let sandbox_id = Uuid::new_v4();
        let mut tracker = EpisodeTracker::default();
        tracker
            .register(sandbox_id, "sha256:revision".into(), 1)
            .unwrap();
        let execution = tracker
            .begin_execution(sandbox_id, "execution-1".into())
            .unwrap();
        let ticket = tracker.begin_reset(sandbox_id).unwrap();
        tracker.confirm_reaped(&ticket).unwrap();
        tracker.complete_reset(&ticket).unwrap();
        assert_eq!(
            tracker.finish_execution(&execution),
            Err(EpisodeError::StaleGeneration)
        );
    }

    #[test]
    fn reset_requires_explicit_process_reap_with_active_execution() {
        let sandbox_id = Uuid::new_v4();
        let mut tracker = EpisodeTracker::default();
        tracker
            .register(sandbox_id, "sha256:revision".into(), 1)
            .unwrap();
        tracker
            .begin_execution(sandbox_id, "execution-1".into())
            .unwrap();
        let ticket = tracker.begin_reset(sandbox_id).unwrap();
        assert_eq!(
            tracker.complete_reset(&ticket),
            Err(EpisodeError::ExecutionsNotReaped)
        );
        assert_eq!(
            tracker.begin_execution(sandbox_id, "execution-2".into()),
            Err(EpisodeError::ResetInProgress)
        );
        tracker.confirm_reaped(&ticket).unwrap();
        tracker.complete_reset(&ticket).unwrap();
    }

    #[test]
    fn reset_tickets_are_single_use_and_generation_bound() {
        let sandbox_id = Uuid::new_v4();
        let mut tracker = EpisodeTracker::default();
        tracker
            .register(sandbox_id, "sha256:revision".into(), 1)
            .unwrap();
        let ticket = tracker.begin_reset(sandbox_id).unwrap();
        tracker.confirm_reaped(&ticket).unwrap();
        tracker.complete_reset(&ticket).unwrap();
        assert_eq!(
            tracker.complete_reset(&ticket),
            Err(EpisodeError::StaleGeneration)
        );
    }

    #[test]
    fn duplicate_sandbox_and_execution_identities_are_rejected() {
        let sandbox_id = Uuid::new_v4();
        let mut tracker = EpisodeTracker::default();
        tracker
            .register(sandbox_id, "sha256:revision".into(), 1)
            .unwrap();
        assert_eq!(
            tracker.register(sandbox_id, "sha256:revision".into(), 1),
            Err(EpisodeError::AlreadyRegistered)
        );
        tracker
            .begin_execution(sandbox_id, "execution-1".into())
            .unwrap();
        assert_eq!(
            tracker.begin_execution(sandbox_id, "execution-1".into()),
            Err(EpisodeError::DuplicateExecution)
        );
    }
}
