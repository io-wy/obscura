use crate::sandbox::{Sandbox, SandboxState};
use rusqlite::{params, Connection};
use std::fs::{File, OpenOptions};
use std::path::Path;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum StorageError {
    #[error("sqlite error: {0}")]
    Sqlite(#[from] rusqlite::Error),
    #[error("store ownership: {0}")]
    Ownership(String),
    #[error("sandbox not found")]
    NotFound,
    #[error("node network capacity exhausted")]
    Capacity,
    #[error("unsupported database schema version: {0}")]
    UnsupportedSchema(i64),
    #[error("episode metadata is missing")]
    MissingEpisode,
    #[error("reset precondition failed")]
    ResetPrecondition,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EpisodeRecord {
    pub sandbox_id: uuid::Uuid,
    pub episode_id: uuid::Uuid,
    pub generation: u64,
    pub environment_revision: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResetRecord {
    pub sandbox_id: uuid::Uuid,
    pub generation: u64,
    pub mode: String,
    pub phase: String,
    pub request_id: String,
}

#[derive(Debug, Clone, serde::Serialize, serde::Deserialize, PartialEq, Eq)]
pub struct LaunchProfile {
    pub vcpus: u32,
    pub memory_mib: u32,
    pub disk_mib: u64,
    pub allow_egress: bool,
    pub rootfs_digest: String,
    pub kernel_digest: String,
}

pub struct Store {
    connection: Connection,
    _ownership_lock: File,
}

impl Store {
    pub fn open(path: impl AsRef<Path>) -> Result<Self, StorageError> {
        let path = path.as_ref();
        let mut lock_path = path.as_os_str().to_os_string();
        lock_path.push(".lock");
        let ownership_lock = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(lock_path)
            .map_err(|error| StorageError::Ownership(error.to_string()))?;
        ownership_lock
            .try_lock()
            .map_err(|error| StorageError::Ownership(error.to_string()))?;
        let mut connection = Connection::open(path)?;
        connection.pragma_update(None, "journal_mode", "WAL")?;
        connection.pragma_update(None, "foreign_keys", "ON")?;
        let version: i64 = connection.pragma_query_value(None, "user_version", |row| row.get(0))?;
        if version > 2 {
            return Err(StorageError::UnsupportedSchema(version));
        }
        if version == 0 {
            // Rebuild the legacy table because SQLite cannot drop its UNIQUE column constraint.
            // Foreign keys are checked again before the node starts serving requests.
            connection.pragma_update(None, "foreign_keys", "OFF")?;
            let migration = (|| -> Result<(), StorageError> {
                let transaction = connection
                    .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
                let legacy: bool = transaction.query_row(
                    "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='sandboxes')",
                    [],
                    |row| row.get(0),
                )?;
                let table_name = if legacy {
                    "sandboxes_migrating"
                } else {
                    "sandboxes"
                };
                transaction.execute_batch(&format!(
                    "CREATE TABLE {table_name} (
                        id TEXT PRIMARY KEY,
                        template_ref TEXT NOT NULL,
                        state TEXT NOT NULL,
                        runtime_handle TEXT,
                        network_slot INTEGER NOT NULL,
                        updated_at_unix_nanos INTEGER NOT NULL
                    );"
                ))?;
                if legacy {
                    transaction.execute_batch(
                        "INSERT INTO sandboxes_migrating SELECT * FROM sandboxes;
                         DROP TABLE sandboxes;
                         ALTER TABLE sandboxes_migrating RENAME TO sandboxes;",
                    )?;
                }
                transaction.execute_batch(
                    "CREATE UNIQUE INDEX active_network_slot ON sandboxes(network_slot)
                         WHERE state != 'Deleted' OR runtime_handle IS NOT NULL;
                     CREATE TABLE IF NOT EXISTS sandbox_bindings (
                         sandbox_id TEXT PRIMARY KEY REFERENCES sandboxes(id), fingerprint TEXT NOT NULL);
                     CREATE TABLE IF NOT EXISTS quarantined_sandboxes (
                         sandbox_id TEXT PRIMARY KEY REFERENCES sandboxes(id));
                     CREATE TABLE IF NOT EXISTS images (
                         digest TEXT PRIMARY KEY, manifest_json TEXT NOT NULL,
                         created_at_unix_nanos INTEGER NOT NULL);
                     CREATE TABLE IF NOT EXISTS snapshots (
                         id TEXT PRIMARY KEY, sandbox_id TEXT NOT NULL,
                         manifest_json TEXT NOT NULL, created_at_unix_nanos INTEGER NOT NULL);
                     PRAGMA user_version = 1;",
                )?;
                let mut check = transaction.prepare("PRAGMA foreign_key_check")?;
                if check.query([])?.next()?.is_some() {
                    return Err(StorageError::Sqlite(rusqlite::Error::InvalidQuery));
                }
                drop(check);
                transaction.commit()?;
                Ok(())
            })();
            connection.pragma_update(None, "foreign_keys", "ON")?;
            migration?;
        }
        if version < 2 {
            let transaction =
                connection.transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
            transaction.execute_batch(
                "CREATE TABLE IF NOT EXISTS sandbox_episodes (
                    sandbox_id TEXT PRIMARY KEY REFERENCES sandboxes(id),
                    episode_id TEXT NOT NULL,
                    generation INTEGER NOT NULL CHECK (generation > 0),
                    environment_revision TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS sandbox_resets (
                    sandbox_id TEXT PRIMARY KEY REFERENCES sandboxes(id),
                    generation INTEGER NOT NULL CHECK (generation > 0),
                    mode TEXT NOT NULL,
                    phase TEXT NOT NULL,
                    request_id TEXT NOT NULL
                );
                CREATE TABLE sandbox_launches (
                    sandbox_id TEXT PRIMARY KEY REFERENCES sandboxes(id),
                    profile_json TEXT NOT NULL
                );
                CREATE TABLE reset_receipts (
                    sandbox_id TEXT NOT NULL REFERENCES sandboxes(id),
                    request_id TEXT NOT NULL,
                    PRIMARY KEY (sandbox_id, request_id)
                );
                PRAGMA user_version = 2;",
            )?;
            transaction.commit()?;
        }
        // SQLite cannot prove ownership of a child process from a previous daemon.
        // Preserve every uncertain resource and expose Failed instead of a stale Ready.
        let transaction =
            connection.transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
        transaction.execute_batch("INSERT OR IGNORE INTO quarantined_sandboxes
            SELECT id FROM sandboxes WHERE state NOT IN ('Deleted', 'Failed') OR runtime_handle IS NOT NULL;
            INSERT OR IGNORE INTO quarantined_sandboxes
            SELECT sandbox_id FROM sandbox_resets;
            UPDATE sandboxes SET state='Failed' WHERE id IN (SELECT sandbox_id FROM quarantined_sandboxes);")?;
        transaction.commit()?;
        Ok(Self {
            connection,
            _ownership_lock: ownership_lock,
        })
    }

    /// Atomically reserve identity and network slot before any VM/network side effects.
    pub fn reserve_sandbox(
        &mut self,
        sandbox: &Sandbox,
        fingerprint: &str,
    ) -> Result<(), StorageError> {
        let transaction = self
            .connection
            .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
        transaction.execute(
            "INSERT INTO sandboxes (id, template_ref, state, runtime_handle, network_slot, updated_at_unix_nanos)
             VALUES (?1, ?2, 'Creating', ?3, ?4, ?5)",
            params![sandbox.id.to_string(), sandbox.template_ref, sandbox.runtime_handle, sandbox.network_slot, sandbox.updated_at_unix_nanos],
        )?;
        transaction.execute(
            "INSERT INTO sandbox_bindings (sandbox_id, fingerprint) VALUES (?1, ?2)",
            params![sandbox.id.to_string(), fingerprint],
        )?;
        transaction.commit()?;
        Ok(())
    }

    pub fn reserve_runtime_sandbox(
        &mut self,
        sandbox: &Sandbox,
        fingerprint: &str,
        profile: &LaunchProfile,
        episode: &EpisodeRecord,
    ) -> Result<(), StorageError> {
        let profile_json =
            serde_json::to_string(profile).map_err(|_| StorageError::ResetPrecondition)?;
        if episode.sandbox_id != sandbox.id || episode.generation != 1 {
            return Err(StorageError::ResetPrecondition);
        }
        let transaction = self
            .connection
            .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
        transaction.execute(
            "INSERT INTO sandboxes VALUES (?1, ?2, 'Creating', ?3, ?4, ?5)",
            params![
                sandbox.id.to_string(),
                sandbox.template_ref,
                sandbox.runtime_handle,
                sandbox.network_slot,
                sandbox.updated_at_unix_nanos
            ],
        )?;
        transaction.execute(
            "INSERT INTO sandbox_bindings VALUES (?1, ?2)",
            params![sandbox.id.to_string(), fingerprint],
        )?;
        transaction.execute(
            "INSERT INTO sandbox_launches VALUES (?1, ?2)",
            params![sandbox.id.to_string(), profile_json],
        )?;
        transaction.execute(
            "INSERT INTO sandbox_episodes VALUES (?1, ?2, ?3, ?4)",
            params![
                sandbox.id.to_string(),
                episode.episode_id.to_string(),
                episode.generation,
                episode.environment_revision
            ],
        )?;
        transaction.commit()?;
        Ok(())
    }

    pub fn launch_profile(&self, id: uuid::Uuid) -> Result<LaunchProfile, StorageError> {
        let json: String = self.connection.query_row(
            "SELECT profile_json FROM sandbox_launches WHERE sandbox_id=?1",
            [id.to_string()],
            |row| row.get(0),
        )?;
        serde_json::from_str(&json).map_err(|_| StorageError::ResetPrecondition)
    }

    pub fn fingerprint(&self, id: uuid::Uuid) -> Result<String, StorageError> {
        self.connection
            .query_row(
                "SELECT fingerprint FROM sandbox_bindings WHERE sandbox_id=?1",
                params![id.to_string()],
                |row| row.get(0),
            )
            .map_err(StorageError::from)
    }

    pub fn is_quarantined(&self, id: uuid::Uuid) -> Result<bool, StorageError> {
        self.connection
            .query_row(
                "SELECT EXISTS(SELECT 1 FROM quarantined_sandboxes WHERE sandbox_id=?1)",
                params![id.to_string()],
                |row| row.get(0),
            )
            .map_err(StorageError::from)
    }

    pub fn upsert_sandbox(&self, sandbox: &Sandbox) -> Result<(), StorageError> {
        self.connection.execute(
            "INSERT INTO sandboxes (id, template_ref, state, runtime_handle, network_slot, updated_at_unix_nanos)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6)
             ON CONFLICT(id) DO UPDATE SET template_ref=excluded.template_ref, state=excluded.state,
             runtime_handle=excluded.runtime_handle, updated_at_unix_nanos=excluded.updated_at_unix_nanos",
            params![sandbox.id.to_string(), sandbox.template_ref, format!("{:?}", sandbox.state), sandbox.runtime_handle, sandbox.network_slot, sandbox.updated_at_unix_nanos],
        )?;
        Ok(())
    }

    pub fn put_episode(&mut self, episode: &EpisodeRecord) -> Result<(), StorageError> {
        self.connection.execute(
            "INSERT INTO sandbox_episodes (sandbox_id, episode_id, generation, environment_revision)
             VALUES (?1, ?2, ?3, ?4)
             ",
            params![
                episode.sandbox_id.to_string(),
                episode.episode_id.to_string(),
                episode.generation,
                episode.environment_revision
            ],
        )?;
        Ok(())
    }

    pub fn episode(&self, sandbox_id: uuid::Uuid) -> Result<EpisodeRecord, StorageError> {
        self.connection
            .query_row(
                "SELECT episode_id, generation, environment_revision FROM sandbox_episodes WHERE sandbox_id=?1",
                params![sandbox_id.to_string()],
                |row| {
                    let episode_id: String = row.get(0)?;
                    Ok(EpisodeRecord {
                        sandbox_id,
                        episode_id: episode_id.parse().map_err(|_| rusqlite::Error::InvalidQuery)?,
                        generation: row.get::<_, u64>(1)?,
                        environment_revision: row.get(2)?,
                    })
                },
            )
            .map_err(|error| {
                if matches!(error, rusqlite::Error::QueryReturnedNoRows) {
                    StorageError::MissingEpisode
                } else {
                    StorageError::Sqlite(error)
                }
            })
    }

    pub fn begin_reset(
        &mut self,
        sandbox_id: uuid::Uuid,
        generation: u64,
        mode: &str,
        request_id: &str,
    ) -> Result<(), StorageError> {
        if request_id.trim().is_empty() || mode != "clean" || generation >= i64::MAX as u64 {
            return Err(StorageError::ResetPrecondition);
        }
        let transaction = self
            .connection
            .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
        let changed = transaction.execute(
            "UPDATE sandboxes SET state='Resetting'
             WHERE id=?1 AND state IN ('Ready', 'Running', 'Paused')
             AND EXISTS (SELECT 1 FROM sandbox_episodes WHERE sandbox_id=?1 AND generation=?2)
             AND NOT EXISTS (SELECT 1 FROM quarantined_sandboxes WHERE sandbox_id=?1)
             AND NOT EXISTS (SELECT 1 FROM reset_receipts WHERE sandbox_id=?1 AND request_id=?3)",
            params![sandbox_id.to_string(), generation, request_id],
        )?;
        if changed != 1 {
            return Err(StorageError::ResetPrecondition);
        }
        transaction.execute(
            "INSERT INTO sandbox_resets (sandbox_id, generation, mode, phase, request_id)
             VALUES (?1, ?2, ?3, 'requested', ?4)",
            params![sandbox_id.to_string(), generation, mode, request_id],
        )?;
        transaction.commit()?;
        Ok(())
    }

    pub fn set_reset_phase(&self, sandbox_id: uuid::Uuid, phase: &str) -> Result<(), StorageError> {
        let changed = self.connection.execute(
            "UPDATE sandbox_resets SET phase=?2 WHERE sandbox_id=?1",
            params![sandbox_id.to_string(), phase],
        )?;
        if changed == 0 {
            return Err(StorageError::NotFound);
        }
        Ok(())
    }

    pub fn reset(&self, sandbox_id: uuid::Uuid) -> Result<ResetRecord, StorageError> {
        self.connection
            .query_row(
                "SELECT generation, mode, phase, request_id FROM sandbox_resets WHERE sandbox_id=?1",
                params![sandbox_id.to_string()],
                |row| Ok(ResetRecord {
                    sandbox_id,
                    generation: row.get(0)?,
                    mode: row.get(1)?,
                    phase: row.get(2)?,
                    request_id: row.get(3)?,
                }),
            )
            .map_err(|error| {
                if matches!(error, rusqlite::Error::QueryReturnedNoRows) {
                    StorageError::NotFound
                } else {
                    StorageError::Sqlite(error)
                }
            })
    }

    pub fn reset_completed(&self, id: uuid::Uuid, request_id: &str) -> Result<bool, StorageError> {
        Ok(self.connection.query_row(
            "SELECT EXISTS(SELECT 1 FROM reset_receipts WHERE sandbox_id=?1 AND request_id=?2)",
            params![id.to_string(), request_id],
            |row| row.get(0),
        )?)
    }

    pub fn complete_reset(
        &mut self,
        sandbox: &Sandbox,
        episode_id: uuid::Uuid,
    ) -> Result<(), StorageError> {
        let transaction = self
            .connection
            .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
        let id = sandbox.id.to_string();
        let changed = transaction.execute(
            "UPDATE sandbox_episodes SET episode_id=?2, generation=generation+1
             WHERE sandbox_id=?1 AND generation=(SELECT generation FROM sandbox_resets WHERE sandbox_id=?1)
             AND EXISTS (SELECT 1 FROM sandboxes WHERE id=?1 AND state='Resetting')",
            params![id, episode_id.to_string()],
        )?;
        if changed != 1 {
            return Err(StorageError::ResetPrecondition);
        }
        transaction.execute(
            "UPDATE sandboxes SET state='Ready', runtime_handle=?2, updated_at_unix_nanos=?3 WHERE id=?1",
            params![id, sandbox.runtime_handle, sandbox.updated_at_unix_nanos],
        )?;
        transaction.execute(
            "INSERT INTO reset_receipts SELECT sandbox_id, request_id FROM sandbox_resets WHERE sandbox_id=?1", [&id],
        )?;
        transaction.execute("DELETE FROM sandbox_resets WHERE sandbox_id=?1", [&id])?;
        transaction.commit()?;
        Ok(())
    }

    pub fn fail_reset(&mut self, id: uuid::Uuid) -> Result<(), StorageError> {
        let transaction = self
            .connection
            .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
        transaction.execute(
            "UPDATE sandboxes SET state='Failed' WHERE id=?1",
            [id.to_string()],
        )?;
        transaction.execute(
            "UPDATE sandbox_resets SET phase='failed' WHERE sandbox_id=?1",
            [id.to_string()],
        )?;
        transaction.commit()?;
        Ok(())
    }

    pub fn complete_delete(&mut self, sandbox: &Sandbox) -> Result<(), StorageError> {
        let transaction = self
            .connection
            .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
        transaction.execute(
            "DELETE FROM sandbox_resets WHERE sandbox_id=?1",
            [sandbox.id.to_string()],
        )?;
        transaction.execute(
            "UPDATE sandboxes SET state='Deleted', runtime_handle=NULL,
            updated_at_unix_nanos=?2 WHERE id=?1",
            params![sandbox.id.to_string(), sandbox.updated_at_unix_nanos],
        )?;
        transaction.commit()?;
        Ok(())
    }

    pub fn get_sandbox(&self, id: uuid::Uuid) -> Result<Sandbox, StorageError> {
        self.connection.query_row(
            "SELECT id, template_ref, state, runtime_handle, network_slot, updated_at_unix_nanos FROM sandboxes WHERE id=?1",
            params![id.to_string()],
            |row| {
                let state: String = row.get(2)?;
                Ok(Sandbox {
                    id: row.get::<_, String>(0)?.parse().map_err(|_| rusqlite::Error::InvalidQuery)?,
                    template_ref: row.get(1)?,
                    state: match state.as_str() {
                        "Creating" => SandboxState::Creating,
                        "Ready" => SandboxState::Ready,
                        "Running" => SandboxState::Running,
                        "Paused" => SandboxState::Paused,
                        "Snapshotting" => SandboxState::Snapshotting,
                        "Resetting" => SandboxState::Resetting,
                        "Deleted" => SandboxState::Deleted,
                        "Failed" => SandboxState::Failed,
                        _ => return Err(rusqlite::Error::InvalidQuery),
                    },
                    runtime_handle: row.get(3)?,
                    network_slot: row.get(4)?,
                    updated_at_unix_nanos: row.get(5)?,
                })
            },
        ).map_err(|error| if matches!(error, rusqlite::Error::QueryReturnedNoRows) { StorageError::NotFound } else { StorageError::Sqlite(error) })
    }

    pub fn active_sandbox_ids(&self) -> Result<Vec<uuid::Uuid>, StorageError> {
        let mut statement = self.connection.prepare(
            "SELECT id FROM sandboxes WHERE state NOT IN ('Deleted', 'Failed') OR runtime_handle IS NOT NULL ORDER BY id",
        )?;
        let ids = statement
            .query_map([], |row| {
                row.get::<_, String>(0)?
                    .parse()
                    .map_err(|_| rusqlite::Error::InvalidQuery)
            })?
            .collect::<Result<Vec<_>, _>>()?;
        Ok(ids)
    }

    pub fn next_network_slot(&self) -> Result<u32, StorageError> {
        let mut occupied = [false; 16_384];
        let mut statement = self.connection.prepare(
            "SELECT network_slot FROM sandboxes WHERE state != 'Deleted' OR runtime_handle IS NOT NULL",
        )?;
        for slot in statement.query_map([], |row| row.get::<_, u32>(0))? {
            let slot = slot?;
            if slot > 0 && slot < occupied.len() as u32 {
                occupied[slot as usize] = true;
            }
        }
        (1..occupied.len())
            .find(|&slot| !occupied[slot])
            .map(|slot| slot as u32)
            .ok_or(StorageError::Capacity)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use uuid::Uuid;

    fn sandbox(id: Uuid, slot: u32, state: SandboxState) -> Sandbox {
        Sandbox {
            id,
            template_ref: "test".into(),
            state,
            runtime_handle: None,
            network_slot: slot,
            updated_at_unix_nanos: 0,
        }
    }

    #[test]
    fn deleted_identity_keeps_its_fingerprint_and_releases_its_slot() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = Store::open(dir.path().join("node.sqlite")).unwrap();
        let first = Uuid::new_v4();
        store
            .reserve_sandbox(&sandbox(first, 1, SandboxState::Creating), "first-hash")
            .unwrap();
        assert_eq!(store.next_network_slot().unwrap(), 2);
        store
            .upsert_sandbox(&sandbox(first, 1, SandboxState::Deleted))
            .unwrap();
        assert_eq!(store.next_network_slot().unwrap(), 1);
        let second = Uuid::new_v4();
        store
            .reserve_sandbox(&sandbox(second, 1, SandboxState::Creating), "second-hash")
            .unwrap();
        assert_eq!(
            store.get_sandbox(first).unwrap().state,
            SandboxState::Deleted
        );
        assert_eq!(store.fingerprint(first).unwrap(), "first-hash");
        assert_eq!(store.next_network_slot().unwrap(), 2);
    }

    #[test]
    fn migrates_legacy_unique_slot_schema_without_losing_tombstones() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("node.sqlite");
        let first = Uuid::new_v4();
        {
            let connection = Connection::open(&path).unwrap();
            connection
                .execute_batch(
                    "CREATE TABLE sandboxes (
                id TEXT PRIMARY KEY, template_ref TEXT NOT NULL, state TEXT NOT NULL,
                runtime_handle TEXT, network_slot INTEGER NOT NULL UNIQUE,
                updated_at_unix_nanos INTEGER NOT NULL);
                CREATE TABLE sandbox_bindings (
                sandbox_id TEXT PRIMARY KEY REFERENCES sandboxes(id), fingerprint TEXT NOT NULL);",
                )
                .unwrap();
            connection
                .execute(
                    "INSERT INTO sandboxes VALUES (?1, 'test', 'Deleted', NULL, 1, 0)",
                    [first.to_string()],
                )
                .unwrap();
            connection
                .execute(
                    "INSERT INTO sandbox_bindings VALUES (?1, 'first-hash')",
                    [first.to_string()],
                )
                .unwrap();
        }
        let mut store = Store::open(&path).unwrap();
        assert_eq!(store.next_network_slot().unwrap(), 1);
        let second = Uuid::new_v4();
        store
            .reserve_sandbox(&sandbox(second, 1, SandboxState::Creating), "second-hash")
            .unwrap();
        assert_eq!(store.fingerprint(first).unwrap(), "first-hash");
        assert_eq!(store.get_sandbox(second).unwrap().network_slot, 1);
        drop(store);
        let store = Store::open(&path).unwrap();
        assert_eq!(store.fingerprint(first).unwrap(), "first-hash");
        assert_eq!(store.get_sandbox(second).unwrap().network_slot, 1);
    }

    #[test]
    fn failed_and_quarantined_sandboxes_keep_their_slots() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("node.sqlite");
        let first = Uuid::new_v4();
        {
            let mut store = Store::open(&path).unwrap();
            let mut failed = sandbox(first, 1, SandboxState::Creating);
            failed.runtime_handle = Some(String::new());
            store.reserve_sandbox(&failed, "hash").unwrap();
            failed.state = SandboxState::Failed;
            store.upsert_sandbox(&failed).unwrap();
            assert_eq!(store.next_network_slot().unwrap(), 2);
        }
        let store = Store::open(&path).unwrap();
        assert!(store.is_quarantined(first).unwrap());
        assert_eq!(store.next_network_slot().unwrap(), 2);
    }

    #[test]
    fn deleted_sandbox_with_uncertain_resources_keeps_its_slot() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = Store::open(dir.path().join("node.sqlite")).unwrap();
        let first = Uuid::new_v4();
        let mut uncertain = sandbox(first, 1, SandboxState::Creating);
        uncertain.runtime_handle = Some(String::new());
        store.reserve_sandbox(&uncertain, "hash").unwrap();
        uncertain.state = SandboxState::Deleted;
        store.upsert_sandbox(&uncertain).unwrap();
        assert_eq!(store.next_network_slot().unwrap(), 2);
        assert!(store
            .reserve_sandbox(&sandbox(Uuid::new_v4(), 1, SandboxState::Creating), "other")
            .is_err());
    }

    #[test]
    fn future_schema_version_is_rejected() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("node.sqlite");
        let connection = Connection::open(&path).unwrap();
        connection.pragma_update(None, "user_version", 3).unwrap();
        drop(connection);
        assert!(matches!(
            Store::open(&path),
            Err(StorageError::UnsupportedSchema(3))
        ));
    }

    #[test]
    fn migrates_schema_v1_with_episode_and_reset_tables() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("node.sqlite");
        let connection = Connection::open(&path).unwrap();
        connection
            .execute_batch(
                "CREATE TABLE sandboxes (
                id TEXT PRIMARY KEY, template_ref TEXT NOT NULL, state TEXT NOT NULL,
                runtime_handle TEXT, network_slot INTEGER NOT NULL,
                updated_at_unix_nanos INTEGER NOT NULL);
                CREATE TABLE sandbox_bindings (
                sandbox_id TEXT PRIMARY KEY REFERENCES sandboxes(id), fingerprint TEXT NOT NULL);
                CREATE UNIQUE INDEX active_network_slot ON sandboxes(network_slot)
                    WHERE state != 'Deleted' OR runtime_handle IS NOT NULL;
                CREATE TABLE quarantined_sandboxes (sandbox_id TEXT PRIMARY KEY REFERENCES sandboxes(id));
                CREATE TABLE images (digest TEXT PRIMARY KEY, manifest_json TEXT NOT NULL, created_at_unix_nanos INTEGER NOT NULL);
                CREATE TABLE snapshots (id TEXT PRIMARY KEY, sandbox_id TEXT NOT NULL, manifest_json TEXT NOT NULL, created_at_unix_nanos INTEGER NOT NULL);
                PRAGMA user_version = 1;",
            )
            .unwrap();
        drop(connection);
        let store = Store::open(&path).unwrap();
        let version = Connection::open(&path)
            .unwrap()
            .pragma_query_value(None, "user_version", |row| row.get::<_, i64>(0))
            .unwrap();
        assert_eq!(version, 2);
        assert!(matches!(
            store.reset(Uuid::new_v4()),
            Err(StorageError::NotFound)
        ));
    }

    #[test]
    fn interrupted_reset_is_quarantined_on_reopen() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("node.sqlite");
        let mut store = Store::open(&path).unwrap();
        let id = Uuid::new_v4();
        let sandbox = sandbox(id, 1, SandboxState::Ready);
        store.reserve_sandbox(&sandbox, "revision").unwrap();
        store.upsert_sandbox(&sandbox).unwrap();
        store
            .put_episode(&EpisodeRecord {
                sandbox_id: id,
                episode_id: Uuid::new_v4(),
                generation: 1,
                environment_revision: "sha256:revision".into(),
            })
            .unwrap();
        store.begin_reset(id, 1, "clean", "reset-1").unwrap();
        drop(store);
        let reopened = Store::open(&path).unwrap();
        assert!(reopened.is_quarantined(id).unwrap());
        assert_eq!(
            reopened.get_sandbox(id).unwrap().state,
            SandboxState::Failed
        );
        assert_eq!(reopened.reset(id).unwrap().phase, "requested");
    }

    #[test]
    fn reset_metadata_is_atomic_and_clearable() {
        let dir = tempfile::tempdir().unwrap();
        let mut store = Store::open(dir.path().join("node.sqlite")).unwrap();
        let id = Uuid::new_v4();
        store
            .reserve_sandbox(&sandbox(id, 1, SandboxState::Ready), "revision")
            .unwrap();
        store
            .upsert_sandbox(&sandbox(id, 1, SandboxState::Ready))
            .unwrap();
        let episode = EpisodeRecord {
            sandbox_id: id,
            episode_id: Uuid::new_v4(),
            generation: 4,
            environment_revision: "sha256:revision".into(),
        };
        store.put_episode(&episode).unwrap();
        assert_eq!(store.episode(id).unwrap(), episode);
        assert!(store.begin_reset(id, 3, "clean", "reset-1").is_err());
        store.begin_reset(id, 4, "clean", "reset-1").unwrap();
        assert_eq!(
            store.get_sandbox(id).unwrap().state,
            SandboxState::Resetting
        );
        store.set_reset_phase(id, "reaped").unwrap();
        assert_eq!(store.reset(id).unwrap().phase, "reaped");
        let next_episode = Uuid::new_v4();
        store
            .complete_reset(&sandbox(id, 1, SandboxState::Resetting), next_episode)
            .unwrap();
        assert_eq!(store.episode(id).unwrap().generation, 5);
        assert_eq!(store.episode(id).unwrap().episode_id, next_episode);
        assert!(store.reset_completed(id, "reset-1").unwrap());
        assert!(store.begin_reset(id, 5, "clean", "reset-1").is_err());
        assert!(matches!(store.reset(id), Err(StorageError::NotFound)));
    }

    #[test]
    fn failed_ready_commit_rolls_back_episode_and_reset_receipt() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("node.sqlite");
        let mut store = Store::open(&path).unwrap();
        let id = Uuid::new_v4();
        let initial = sandbox(id, 1, SandboxState::Ready);
        store.reserve_sandbox(&initial, "fingerprint").unwrap();
        store.upsert_sandbox(&initial).unwrap();
        store
            .put_episode(&EpisodeRecord {
                sandbox_id: id,
                episode_id: Uuid::new_v4(),
                generation: 1,
                environment_revision: "revision".into(),
            })
            .unwrap();
        store.begin_reset(id, 1, "clean", "reset-1").unwrap();
        store
            .connection
            .execute_batch(
                "CREATE TRIGGER fail_ready BEFORE UPDATE ON sandboxes WHEN NEW.state='Ready'
             BEGIN SELECT RAISE(FAIL, 'failure'); END;",
            )
            .unwrap();
        assert!(store.complete_reset(&initial, Uuid::new_v4()).is_err());
        assert_eq!(store.episode(id).unwrap().generation, 1);
        assert_eq!(
            store.get_sandbox(id).unwrap().state,
            SandboxState::Resetting
        );
        assert!(!store.reset_completed(id, "reset-1").unwrap());
        assert_eq!(store.reset(id).unwrap().phase, "requested");
    }

    #[test]
    fn invalid_legacy_foreign_key_rolls_back_migration() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("node.sqlite");
        let connection = Connection::open(&path).unwrap();
        connection
            .execute_batch(
                "PRAGMA foreign_keys = OFF;
            CREATE TABLE sandboxes (
            id TEXT PRIMARY KEY, template_ref TEXT NOT NULL, state TEXT NOT NULL,
            runtime_handle TEXT, network_slot INTEGER NOT NULL UNIQUE,
            updated_at_unix_nanos INTEGER NOT NULL);
            CREATE TABLE sandbox_bindings (
            sandbox_id TEXT PRIMARY KEY REFERENCES sandboxes(id), fingerprint TEXT NOT NULL);
            INSERT INTO sandbox_bindings VALUES ('missing', 'hash');",
            )
            .unwrap();
        drop(connection);
        assert!(Store::open(&path).is_err());
        let connection = Connection::open(&path).unwrap();
        let version: i64 = connection
            .pragma_query_value(None, "user_version", |row| row.get(0))
            .unwrap();
        assert_eq!(version, 0);
        let schema: String = connection
            .query_row(
                "SELECT sql FROM sqlite_master WHERE name='sandboxes'",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert!(schema.contains("network_slot INTEGER NOT NULL UNIQUE"));
    }

    #[test]
    fn a_second_daemon_cannot_open_and_quarantine_a_live_store() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("node.sqlite");
        let store = Store::open(&path).unwrap();
        assert!(Store::open(&path).is_err());
        drop(store);
        Store::open(&path).unwrap();
    }

    #[test]
    fn active_sandboxes_block_restart_but_deleted_ones_do_not() {
        let database = tempfile::NamedTempFile::new().unwrap();
        let store = Store::open(database.path()).unwrap();
        let id = Uuid::new_v4();
        let mut sandbox = Sandbox {
            id,
            template_ref: "test".into(),
            state: SandboxState::Ready,
            runtime_handle: Some("/tmp/example.sock".into()),
            network_slot: 1,
            updated_at_unix_nanos: 0,
        };
        store.upsert_sandbox(&sandbox).unwrap();
        drop(store);
        let store = Store::open(database.path()).unwrap();
        assert_eq!(store.active_sandbox_ids().unwrap(), vec![id]);
        sandbox.state = SandboxState::Deleted;
        sandbox.runtime_handle = None;
        store.upsert_sandbox(&sandbox).unwrap();
        assert!(store.active_sandbox_ids().unwrap().is_empty());
    }
}
