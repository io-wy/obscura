//! Host-side scorer handoff boundary; this module neither runs nor authenticates
//! an external scorer. Never expose registration or opaque handles to candidate
//! code. The host must authenticate a separate scorer worker before giving it a
//! handle, verify artifact bytes against their content digests in a trusted CAS,
//! and derive terminal evidence from its durable trajectory, not guest input.
//! Handles are local capabilities, not wire credentials. Pending work is bounded
//! and deliberately lost on restart (old results fail closed). The `*_recorded`
//! APIs sync handoff/result metadata digests before returning success. Keeping the
//! actual score/metadata and preventing re-handoff after restart belong to the
//! caller; trajectory digests alone cannot reconstruct a score.

use crate::trajectory::{
    PayloadDigest, SequenceScope, TrajectoryError, TrajectoryEvent, TrajectoryIdentity,
    TrajectoryRecord, TrajectorySpool,
};
use serde::Serialize;
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use thiserror::Error;
use uuid::Uuid;

const MAX_ARTIFACTS: usize = 1024;
const MAX_SCORERS: usize = 128;

/// A logical artifact label and a CAS address; `name` is never a filesystem path.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Artifact {
    pub name: String,
    pub digest: String,
    pub size_bytes: u64,
}

/// Canonical metadata only. Construction validates addresses, not external bytes.
/// Private fields and no Deserialize implementation prevent unchecked mutation.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct ArtifactManifest {
    artifacts: Vec<Artifact>,
    digest: String,
}

impl ArtifactManifest {
    pub fn new(mut artifacts: Vec<Artifact>) -> Result<Self, ScoringError> {
        if artifacts.len() > MAX_ARTIFACTS {
            return Err(ScoringError::InvalidManifest);
        }
        let mut total = 0_u64;
        for artifact in &artifacts {
            if !valid_label(&artifact.name) || !valid_digest(&artifact.digest) {
                return Err(ScoringError::InvalidManifest);
            }
            total = total
                .checked_add(artifact.size_bytes)
                .ok_or(ScoringError::InvalidManifest)?;
        }
        artifacts.sort_unstable_by(|a, b| a.name.cmp(&b.name));
        if artifacts
            .windows(2)
            .any(|pair| pair[0].name == pair[1].name)
        {
            return Err(ScoringError::InvalidManifest);
        }
        #[derive(Serialize)]
        struct Canonical<'a> {
            schema: &'static str,
            artifacts: &'a [Artifact],
        }
        let encoded = serde_json::to_vec(&Canonical {
            schema: "obscura.scoring.artifacts.v1",
            artifacts: &artifacts,
        })
        .map_err(|_| ScoringError::Encoding)?;
        let digest = format!("sha256:{:x}", Sha256::digest(encoded));
        Ok(Self { artifacts, digest })
    }

    pub fn digest(&self) -> &str {
        &self.digest
    }
    pub fn artifacts(&self) -> &[Artifact] {
        &self.artifacts
    }
}

/// Trusted host evidence: callers must verify that this exact execution has a
/// durable terminal record. A positive sequence alone does not prove completion.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TerminalTrajectory {
    pub sandbox_id: Uuid,
    pub episode_id: Uuid,
    pub generation: u64,
    pub execution_id: String,
    pub environment_revision: String,
    pub terminal_sequence: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct ScoreProvenance {
    pub sandbox_id: Uuid,
    pub episode_id: Uuid,
    pub generation: u64,
    pub execution_id: String,
    pub environment_revision: String,
    pub terminal_sequence: u64,
    pub scorer_revision: String,
    pub scorer_registration_id: Uuid,
    pub manifest_digest: String,
}

/// Non-serializable, unforgeable outside this module. Its revision string alone
/// never authorizes accepting a result. Keep it in the trusted worker domain.
#[derive(Debug)]
pub struct TrustedScorer {
    authority: Uuid,
    id: Uuid,
}

#[derive(Debug)]
pub struct ScoringTicket {
    authority: Uuid,
    id: Uuid,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct HandoffMetadata {
    pub provenance: ScoreProvenance,
    pub artifacts: ArtifactManifest,
}

#[derive(Debug)]
pub struct ScoringHandoff {
    ticket: ScoringTicket,
    metadata: HandoffMetadata,
}

impl ScoringHandoff {
    pub fn ticket(&self) -> &ScoringTicket {
        &self.ticket
    }
    pub fn metadata(&self) -> &HandoffMetadata {
        &self.metadata
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct ScoreSubmission {
    pub provenance: ScoreProvenance,
    pub value: f64,
}

/// Serializable provenance for the trusted trajectory sink, without capabilities.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct AcceptedScore {
    pub provenance: ScoreProvenance,
    pub value: f64,
}

#[derive(Debug, Error, PartialEq, Eq)]
pub enum ScoringError {
    #[error("invalid content-addressed artifact manifest")]
    InvalidManifest,
    #[error("scorer and environment revisions require lowercase sha256 digests")]
    InvalidRevision,
    #[error("invalid terminal trajectory evidence")]
    InvalidTerminal,
    #[error("scoring capacity exhausted")]
    Capacity,
    #[error("scorer capability is not authorized for this handoff")]
    UnauthorizedScorer,
    #[error("unknown, cancelled, or already accepted scoring ticket")]
    UnknownTicket,
    #[error("result provenance differs from the trusted handoff")]
    ProvenanceMismatch,
    #[error("score must be finite")]
    NonFiniteScore,
    #[error("cannot encode scoring metadata")]
    Encoding,
}

#[derive(Debug, Error)]
pub enum RecordedScoringError {
    #[error(transparent)]
    Scoring(#[from] ScoringError),
    #[error(transparent)]
    Trajectory(#[from] TrajectoryError),
}

/// Node-local coordinator. Its methods are trusted infrastructure APIs, not RPCs
/// for the candidate sandbox. Registration asserts host authentication; this
/// module cannot establish an external process/network/credential boundary.
pub struct ScoringCoordinator {
    authority: Uuid,
    max_pending: usize,
    scorers: HashMap<Uuid, String>,
    pending: HashMap<Uuid, ScoreProvenance>,
}

impl ScoringCoordinator {
    pub fn new(max_pending: usize) -> Result<Self, ScoringError> {
        if max_pending == 0 {
            return Err(ScoringError::Capacity);
        }
        Ok(Self {
            authority: Uuid::new_v4(),
            max_pending,
            scorers: HashMap::new(),
            pending: HashMap::new(),
        })
    }

    /// Call only after the host authenticates a separate, trusted scorer worker
    /// running this immutable scorer revision. Candidate-supplied identity is
    /// insufficient and must never reach this admission API.
    pub fn register_trusted_scorer(
        &mut self,
        scorer_revision: String,
    ) -> Result<TrustedScorer, ScoringError> {
        if !valid_digest(&scorer_revision) {
            return Err(ScoringError::InvalidRevision);
        }
        if self.scorers.len() >= MAX_SCORERS {
            return Err(ScoringError::Capacity);
        }
        let id = Uuid::new_v4();
        self.scorers.insert(id, scorer_revision);
        Ok(TrustedScorer {
            authority: self.authority,
            id,
        })
    }

    /// Caller must verify CAS bytes/lengths and durable terminal provenance before
    /// handoff, and supply artifacts read-only to the separately isolated worker.
    pub fn prepare(
        &mut self,
        scorer: &TrustedScorer,
        terminal: TerminalTrajectory,
        artifacts: ArtifactManifest,
    ) -> Result<ScoringHandoff, ScoringError> {
        let scorer_revision = self.scorer_revision(scorer)?.to_owned();
        if !valid_digest(&terminal.environment_revision) {
            return Err(ScoringError::InvalidRevision);
        }
        if terminal.generation == 0
            || terminal.terminal_sequence == 0
            || terminal.sandbox_id.is_nil()
            || terminal.episode_id.is_nil()
            || terminal.execution_id.is_empty()
            || terminal.execution_id.len() > 256
            || !terminal
                .execution_id
                .bytes()
                .all(|byte| byte.is_ascii_graphic())
        {
            return Err(ScoringError::InvalidTerminal);
        }
        if self.pending.len() >= self.max_pending {
            return Err(ScoringError::Capacity);
        }
        let provenance = ScoreProvenance {
            sandbox_id: terminal.sandbox_id,
            episode_id: terminal.episode_id,
            generation: terminal.generation,
            execution_id: terminal.execution_id,
            environment_revision: terminal.environment_revision,
            terminal_sequence: terminal.terminal_sequence,
            scorer_revision,
            scorer_registration_id: scorer.id,
            manifest_digest: artifacts.digest.clone(),
        };
        let id = Uuid::new_v4();
        self.pending.insert(id, provenance.clone());
        Ok(ScoringHandoff {
            ticket: ScoringTicket {
                authority: self.authority,
                id,
            },
            metadata: HandoffMetadata {
                provenance,
                artifacts,
            },
        })
    }

    /// Accept at most once per handoff, only through its registered scorer
    /// capability. Invalid submissions do not consume legitimate pending work.
    /// The caller must durably record success; acceptance itself is in-memory.
    pub fn accept_result(
        &mut self,
        scorer: &TrustedScorer,
        ticket: &ScoringTicket,
        submission: ScoreSubmission,
    ) -> Result<AcceptedScore, ScoringError> {
        self.validate_result(scorer, ticket, &submission)?;
        self.pending.remove(&ticket.id);
        Ok(AcceptedScore {
            provenance: submission.provenance,
            value: submission.value,
        })
    }

    /// Host adapter: durably append a handoff metadata digest in the independent
    /// scoring sequence before returning the capability for dispatch. Failed
    /// recording cancels the reservation, including uncertain I/O outcomes; no
    /// worker may be dispatched until this method succeeds.
    pub fn prepare_recorded(
        &mut self,
        scorer: &TrustedScorer,
        terminal: TerminalTrajectory,
        artifacts: ArtifactManifest,
        spool: &mut TrajectorySpool,
    ) -> Result<ScoringHandoff, RecordedScoringError> {
        let handoff = self.prepare(scorer, terminal, artifacts)?;
        let recorded = record_metadata(
            spool,
            &handoff.metadata.provenance,
            &handoff.metadata,
            |handoff| TrajectoryEvent::ScorerHandoff { handoff },
        );
        if let Err(error) = recorded {
            self.pending.remove(&handoff.ticket.id);
            return Err(error);
        }
        Ok(handoff)
    }

    /// Validate without consuming the ticket, sync the result metadata digest,
    /// then consume. Backpressure/serialization/I/O failure leaves the ticket
    /// pending and returns no accepted score. On uncertain I/O, reconcile the
    /// poisoned spool before retrying; a digest may already be on disk. This is
    /// not a distributed exactly-once protocol or an external scorer service.
    pub fn accept_result_recorded(
        &mut self,
        scorer: &TrustedScorer,
        ticket: &ScoringTicket,
        submission: ScoreSubmission,
        spool: &mut TrajectorySpool,
    ) -> Result<AcceptedScore, RecordedScoringError> {
        self.validate_result(scorer, ticket, &submission)?;
        let accepted = AcceptedScore {
            provenance: submission.provenance,
            value: submission.value,
        };
        record_metadata(spool, &accepted.provenance, &accepted, |result| {
            TrajectoryEvent::ScorerResult { result }
        })?;
        self.pending.remove(&ticket.id);
        Ok(accepted)
    }

    pub fn cancel(&mut self, ticket: &ScoringTicket) -> Result<(), ScoringError> {
        self.pending_for(ticket)?;
        self.pending.remove(&ticket.id);
        Ok(())
    }

    pub fn revoke_scorer(&mut self, scorer: &TrustedScorer) -> Result<(), ScoringError> {
        self.scorer_revision(scorer)?;
        self.scorers.remove(&scorer.id);
        self.pending
            .retain(|_, provenance| provenance.scorer_registration_id != scorer.id);
        Ok(())
    }

    fn scorer_revision(&self, scorer: &TrustedScorer) -> Result<&str, ScoringError> {
        if scorer.authority != self.authority {
            return Err(ScoringError::UnauthorizedScorer);
        }
        self.scorers
            .get(&scorer.id)
            .map(String::as_str)
            .ok_or(ScoringError::UnauthorizedScorer)
    }

    fn validate_result(
        &self,
        scorer: &TrustedScorer,
        ticket: &ScoringTicket,
        submission: &ScoreSubmission,
    ) -> Result<(), ScoringError> {
        self.scorer_revision(scorer)?;
        let expected = self.pending_for(ticket)?;
        if expected.scorer_registration_id != scorer.id {
            return Err(ScoringError::UnauthorizedScorer);
        }
        if expected != &submission.provenance {
            return Err(ScoringError::ProvenanceMismatch);
        }
        if !submission.value.is_finite() {
            return Err(ScoringError::NonFiniteScore);
        }
        Ok(())
    }

    fn pending_for(&self, ticket: &ScoringTicket) -> Result<&ScoreProvenance, ScoringError> {
        if ticket.authority != self.authority {
            return Err(ScoringError::UnknownTicket);
        }
        self.pending
            .get(&ticket.id)
            .ok_or(ScoringError::UnknownTicket)
    }
}

fn record_metadata<T: Serialize>(
    spool: &mut TrajectorySpool,
    provenance: &ScoreProvenance,
    metadata: &T,
    event: impl FnOnce(PayloadDigest) -> TrajectoryEvent,
) -> Result<(), RecordedScoringError> {
    let encoded = serde_json::to_vec(metadata).map_err(|_| ScoringError::Encoding)?;
    let identity = TrajectoryIdentity {
        sandbox_id: provenance.sandbox_id,
        episode_id: provenance.episode_id,
        generation: provenance.generation,
        environment_revision: provenance.environment_revision.clone(),
        execution_id: Some(provenance.execution_id.clone()),
    };
    let sequence = spool.next_sequence(&identity, SequenceScope::Scoring)?;
    spool.append(&TrajectoryRecord {
        identity,
        scope: SequenceScope::Scoring,
        sequence,
        event: event(PayloadDigest::of(&encoded)),
    })?;
    Ok(())
}

fn valid_digest(value: &str) -> bool {
    value.strip_prefix("sha256:").is_some_and(|hex| {
        hex.len() == 64
            && hex
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    })
}

fn valid_label(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value != "."
        && value != ".."
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-' | b'.'))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn digest(c: char) -> String {
        format!("sha256:{}", c.to_string().repeat(64))
    }

    fn artifacts() -> Vec<Artifact> {
        vec![
            Artifact {
                name: "output".into(),
                digest: digest('a'),
                size_bytes: 42,
            },
            Artifact {
                name: "report".into(),
                digest: digest('b'),
                size_bytes: 7,
            },
        ]
    }

    fn terminal() -> TerminalTrajectory {
        TerminalTrajectory {
            sandbox_id: Uuid::new_v4(),
            episode_id: Uuid::new_v4(),
            generation: 2,
            execution_id: "execution-1".into(),
            environment_revision: digest('c'),
            terminal_sequence: 19,
        }
    }

    #[test]
    fn execution_identity_matches_the_frozen_string_contract() {
        let mut coordinator = ScoringCoordinator::new(4).unwrap();
        let scorer = coordinator.register_trusted_scorer(digest('d')).unwrap();
        for execution_id in ["request/job:17".to_owned(), "x".repeat(256)] {
            let mut evidence = terminal();
            evidence.execution_id = execution_id.clone();
            let handoff = coordinator
                .prepare(
                    &scorer,
                    evidence,
                    ArtifactManifest::new(artifacts()).unwrap(),
                )
                .unwrap();
            assert_eq!(handoff.metadata().provenance.execution_id, execution_id);
        }
        for execution_id in [
            String::new(),
            "x".repeat(257),
            "contains space".into(),
            "\n".into(),
            "中文".into(),
        ] {
            let mut evidence = terminal();
            evidence.execution_id = execution_id;
            assert_eq!(
                coordinator
                    .prepare(
                        &scorer,
                        evidence,
                        ArtifactManifest::new(artifacts()).unwrap()
                    )
                    .unwrap_err(),
                ScoringError::InvalidTerminal
            );
        }
    }

    #[test]
    fn recorded_handoff_and_result_are_durable_ordered_metadata_digests() {
        use crate::trajectory::{
            PayloadDigest, SequenceScope, SpoolLimits, TrajectoryEvent, TrajectorySpool,
        };
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("trajectory");
        let limits = SpoolLimits {
            max_record_bytes: 4096,
            max_spool_bytes: 65536,
        };
        let mut spool = TrajectorySpool::open(&path, limits).unwrap();
        let mut coordinator = ScoringCoordinator::new(1).unwrap();
        let scorer = coordinator.register_trusted_scorer(digest('d')).unwrap();
        let handoff = coordinator
            .prepare_recorded(
                &scorer,
                terminal(),
                ArtifactManifest::new(artifacts()).unwrap(),
                &mut spool,
            )
            .unwrap();
        let handoff_digest = PayloadDigest::of(&serde_json::to_vec(handoff.metadata()).unwrap());
        let accepted = coordinator
            .accept_result_recorded(
                &scorer,
                handoff.ticket(),
                ScoreSubmission {
                    provenance: handoff.metadata().provenance.clone(),
                    value: 0.625,
                },
                &mut spool,
            )
            .unwrap();
        let result_digest = PayloadDigest::of(&serde_json::to_vec(&accepted).unwrap());
        drop(spool);
        let mut reopened = TrajectorySpool::open(&path, limits).unwrap();
        let records = reopened.read_records().unwrap();
        assert_eq!(records.len(), 2);
        for (index, record) in records.iter().enumerate() {
            assert_eq!(record.scope, SequenceScope::Scoring);
            assert_eq!(record.sequence, index as u64 + 1);
            assert_eq!(
                record.identity.execution_id.as_deref(),
                Some(accepted.provenance.execution_id.as_str())
            );
            assert_eq!(record.identity.episode_id, accepted.provenance.episode_id);
        }
        assert_eq!(
            records[0].event,
            TrajectoryEvent::ScorerHandoff {
                handoff: handoff_digest
            }
        );
        assert_eq!(
            records[1].event,
            TrajectoryEvent::ScorerResult {
                result: result_digest
            }
        );
        assert!(matches!(
            coordinator.accept_result_recorded(
                &scorer,
                handoff.ticket(),
                ScoreSubmission {
                    provenance: accepted.provenance,
                    value: 0.625
                },
                &mut reopened
            ),
            Err(RecordedScoringError::Scoring(ScoringError::UnknownTicket))
        ));
    }

    #[test]
    fn recording_backpressure_does_not_consume_capacity_or_result_ticket() {
        use crate::trajectory::{SpoolLimits, TrajectoryError, TrajectorySpool};
        let directory = tempfile::tempdir().unwrap();
        let mut full = TrajectorySpool::open(
            &directory.path().join("full"),
            SpoolLimits {
                max_record_bytes: 4096,
                max_spool_bytes: 1,
            },
        )
        .unwrap();
        let mut coordinator = ScoringCoordinator::new(1).unwrap();
        let scorer = coordinator.register_trusted_scorer(digest('d')).unwrap();
        assert!(matches!(
            coordinator.prepare_recorded(
                &scorer,
                terminal(),
                ArtifactManifest::new(artifacts()).unwrap(),
                &mut full
            ),
            Err(RecordedScoringError::Trajectory(
                TrajectoryError::Backpressure { .. }
            ))
        ));
        let mut healthy = TrajectorySpool::open(
            &directory.path().join("healthy"),
            SpoolLimits {
                max_record_bytes: 4096,
                max_spool_bytes: 65536,
            },
        )
        .unwrap();
        let handoff = coordinator
            .prepare_recorded(
                &scorer,
                terminal(),
                ArtifactManifest::new(artifacts()).unwrap(),
                &mut healthy,
            )
            .unwrap();
        let score = ScoreSubmission {
            provenance: handoff.metadata().provenance.clone(),
            value: 1.0,
        };
        assert!(matches!(
            coordinator.accept_result_recorded(&scorer, handoff.ticket(), score.clone(), &mut full),
            Err(RecordedScoringError::Trajectory(
                TrajectoryError::Backpressure { .. }
            ))
        ));
        assert!(coordinator
            .accept_result_recorded(&scorer, handoff.ticket(), score, &mut healthy)
            .is_ok());
        assert_eq!(healthy.read_records().unwrap().len(), 2);
    }

    #[test]
    fn manifest_digest_is_canonical_and_binds_content() {
        let original = ArtifactManifest::new(artifacts()).unwrap();
        let mut reordered = artifacts();
        reordered.reverse();
        assert_eq!(original, ArtifactManifest::new(reordered).unwrap());
        for mutate in [0, 1, 2] {
            let mut changed = artifacts();
            match mutate {
                0 => changed[0].digest = digest('d'),
                1 => changed[0].size_bytes += 1,
                _ => changed[0].name = "different".into(),
            }
            assert_ne!(
                original.digest(),
                ArtifactManifest::new(changed).unwrap().digest()
            );
        }
    }

    #[test]
    fn manifest_rejects_paths_mutable_digests_duplicates_and_overflow() {
        for bad in ["latest", "sha256:abcd", &digest('A')] {
            let mut entries = artifacts();
            entries[0].digest = bad.into();
            assert!(ArtifactManifest::new(entries).is_err());
        }
        for name in ["../secret", "/etc/passwd", "a/b", ""] {
            let mut entries = artifacts();
            entries[0].name = name.into();
            assert!(ArtifactManifest::new(entries).is_err());
        }
        let mut entries = artifacts();
        entries[1].name = entries[0].name.clone();
        assert_eq!(
            ArtifactManifest::new(entries),
            Err(ScoringError::InvalidManifest)
        );
        let mut entries = artifacts();
        entries[0].size_bytes = u64::MAX;
        assert_eq!(
            ArtifactManifest::new(entries),
            Err(ScoringError::InvalidManifest)
        );
    }

    #[test]
    fn handoff_binds_all_provenance_and_accepts_once() {
        let mut coordinator = ScoringCoordinator::new(4).unwrap();
        let scorer = coordinator.register_trusted_scorer(digest('d')).unwrap();
        let terminal = terminal();
        let manifest = ArtifactManifest::new(artifacts()).unwrap();
        let handoff = coordinator
            .prepare(&scorer, terminal.clone(), manifest.clone())
            .unwrap();
        let expected = handoff.metadata().provenance.clone();
        assert_eq!(expected.environment_revision, terminal.environment_revision);
        assert_eq!(expected.scorer_revision, digest('d'));
        assert_eq!(expected.manifest_digest, manifest.digest());
        assert_eq!(expected.terminal_sequence, 19);
        let score = ScoreSubmission {
            provenance: expected.clone(),
            value: 0.75,
        };
        let accepted = coordinator
            .accept_result(&scorer, handoff.ticket(), score.clone())
            .unwrap();
        assert_eq!(accepted.provenance, expected);
        assert_eq!(accepted.value, 0.75);
        assert_eq!(
            coordinator.accept_result(&scorer, handoff.ticket(), score),
            Err(ScoringError::UnknownTicket)
        );
    }

    #[test]
    fn rejects_each_provenance_mismatch_without_consuming_ticket() {
        let mut coordinator = ScoringCoordinator::new(1).unwrap();
        let scorer = coordinator.register_trusted_scorer(digest('d')).unwrap();
        let handoff = coordinator
            .prepare(
                &scorer,
                terminal(),
                ArtifactManifest::new(artifacts()).unwrap(),
            )
            .unwrap();
        for field in 0..9 {
            let mut provenance = handoff.metadata().provenance.clone();
            match field {
                0 => provenance.sandbox_id = Uuid::new_v4(),
                1 => provenance.episode_id = Uuid::new_v4(),
                2 => provenance.generation += 1,
                3 => provenance.execution_id = "different-execution".into(),
                4 => provenance.environment_revision = digest('e'),
                5 => provenance.scorer_revision = digest('e'),
                6 => provenance.manifest_digest = digest('e'),
                7 => provenance.terminal_sequence += 1,
                _ => provenance.scorer_registration_id = Uuid::new_v4(),
            }
            assert_eq!(
                coordinator.accept_result(
                    &scorer,
                    handoff.ticket(),
                    ScoreSubmission {
                        provenance,
                        value: 1.0
                    }
                ),
                Err(ScoringError::ProvenanceMismatch)
            );
        }
        assert!(coordinator
            .accept_result(
                &scorer,
                handoff.ticket(),
                ScoreSubmission {
                    provenance: handoff.metadata().provenance.clone(),
                    value: 1.0
                }
            )
            .is_ok());
    }

    #[test]
    fn scorer_strings_do_not_grant_authority_and_scores_must_be_finite() {
        let mut host = ScoringCoordinator::new(2).unwrap();
        let trusted = host.register_trusted_scorer(digest('d')).unwrap();
        let other_worker = host.register_trusted_scorer(digest('d')).unwrap();
        let mut candidate_domain = ScoringCoordinator::new(2).unwrap();
        let impostor = candidate_domain
            .register_trusted_scorer(digest('d'))
            .unwrap();
        let handoff = host
            .prepare(
                &trusted,
                terminal(),
                ArtifactManifest::new(artifacts()).unwrap(),
            )
            .unwrap();
        let submit = |value| ScoreSubmission {
            provenance: handoff.metadata().provenance.clone(),
            value,
        };
        for handle in [&impostor, &other_worker] {
            assert_eq!(
                host.accept_result(handle, handoff.ticket(), submit(1.0)),
                Err(ScoringError::UnauthorizedScorer)
            );
        }
        for value in [f64::NAN, f64::INFINITY, f64::NEG_INFINITY] {
            assert_eq!(
                host.accept_result(&trusted, handoff.ticket(), submit(value)),
                Err(ScoringError::NonFiniteScore)
            );
        }
        assert!(host
            .accept_result(&trusted, handoff.ticket(), submit(1.0))
            .is_ok());
    }

    #[test]
    fn invalid_pins_terminal_and_full_queue_fail_closed() {
        assert!(ScoringCoordinator::new(0).is_err());
        let mut coordinator = ScoringCoordinator::new(1).unwrap();
        assert!(coordinator
            .register_trusted_scorer("latest".into())
            .is_err());
        let scorer = coordinator.register_trusted_scorer(digest('d')).unwrap();
        for field in 0..3 {
            let mut invalid = terminal();
            match field {
                0 => invalid.generation = 0,
                1 => invalid.terminal_sequence = 0,
                _ => invalid.environment_revision = "mutable".into(),
            }
            assert!(coordinator
                .prepare(
                    &scorer,
                    invalid,
                    ArtifactManifest::new(artifacts()).unwrap()
                )
                .is_err());
        }
        let first = coordinator
            .prepare(
                &scorer,
                terminal(),
                ArtifactManifest::new(artifacts()).unwrap(),
            )
            .unwrap();
        assert_eq!(
            coordinator
                .prepare(
                    &scorer,
                    terminal(),
                    ArtifactManifest::new(artifacts()).unwrap()
                )
                .unwrap_err(),
            ScoringError::Capacity
        );
        coordinator.cancel(first.ticket()).unwrap();
        assert!(coordinator
            .prepare(
                &scorer,
                terminal(),
                ArtifactManifest::new(artifacts()).unwrap()
            )
            .is_ok());
        coordinator.revoke_scorer(&scorer).unwrap();
        assert!(coordinator
            .prepare(
                &scorer,
                terminal(),
                ArtifactManifest::new(artifacts()).unwrap()
            )
            .is_err());
    }
}
