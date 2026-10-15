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
        if version > 1 {
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
        // SQLite cannot prove ownership of a child process from a previous daemon.
        // Preserve every uncertain resource and expose Failed instead of a stale Ready.
        let transaction =
            connection.transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
        transaction.execute_batch("INSERT OR IGNORE INTO quarantined_sandboxes
            SELECT id FROM sandboxes WHERE state NOT IN ('Deleted', 'Failed') OR runtime_handle IS NOT NULL;
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
        connection.pragma_update(None, "user_version", 2).unwrap();
        drop(connection);
        assert!(matches!(
            Store::open(&path),
            Err(StorageError::UnsupportedSchema(2))
        ));
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
