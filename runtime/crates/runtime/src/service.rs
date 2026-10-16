use crate::firecracker::ProcessFirecracker;
use crate::network::CommandRunner;
use crate::network::SystemCommandRunner;
use crate::orchestrator::{Orchestrator, OrchestratorError, Resources};
use crate::rpc::data_plane_server::DataPlane;
use crate::rpc::{
    AgentEvent, CreateSandboxRequest, CreateSandboxResponse, DeleteSandboxRequest,
    DeleteSandboxResponse, ExecuteRequest, GetSandboxRequest, GetSandboxResponse, HealthRequest,
    HealthResponse, ResetSandboxRequest, ResetSandboxResponse, Sandbox, SandboxState,
};
use crate::trajectory::{
    GuestEventKind, PayloadDigest, SequenceScope, TerminalOutcome, TrajectoryEvent,
    TrajectoryIdentity, TrajectoryRecord, TrajectorySpool,
};
use crate::vmm::Vmm;
use sha2::Digest;
use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, Weak};
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Request, Response, Status};

#[derive(Default)]
struct Sessions {
    entries: Mutex<HashMap<uuid::Uuid, Weak<SessionFence>>>,
}

struct SessionFence {
    launch: Arc<tokio::sync::RwLock<()>>,
    cancelled: tokio::sync::watch::Sender<u64>,
}

impl SessionFence {
    async fn launch_owned(self: Arc<Self>) -> tokio::sync::OwnedRwLockWriteGuard<()> {
        Arc::clone(&self.launch).write_owned().await
    }
}

impl Sessions {
    fn get(&self, id: uuid::Uuid) -> Arc<SessionFence> {
        let mut entries = self.entries.lock().expect("session lock poisoned");
        entries.retain(|_, fence| fence.strong_count() > 0);
        if let Some(fence) = entries.get(&id).and_then(Weak::upgrade) {
            return fence;
        }
        let fence = Arc::new(SessionFence {
            launch: Arc::new(tokio::sync::RwLock::new(())),
            cancelled: tokio::sync::watch::channel(0).0,
        });
        entries.insert(id, Arc::downgrade(&fence));
        fence
    }
}

pub struct RuntimeService<V = ProcessFirecracker, R = SystemCommandRunner> {
    orchestrator: Arc<Mutex<Orchestrator<V, R>>>,
    metrics: Arc<Metrics>,
    sessions: Sessions,
    trajectory: Option<Arc<Mutex<TrajectorySpool>>>,
}

#[derive(Default)]
pub struct Metrics {
    creates: AtomicU64,
    failures: AtomicU64,
    deletes: AtomicU64,
    resets: AtomicU64,
}

impl Metrics {
    pub fn render(&self) -> String {
        format!(
            "# TYPE obscura_sandbox_creates_total counter\nobscura_sandbox_creates_total {}\n\
             # TYPE obscura_sandbox_failures_total counter\nobscura_sandbox_failures_total {}\n\
             # TYPE obscura_sandbox_deletes_total counter\nobscura_sandbox_deletes_total {}\n\
             # TYPE obscura_sandbox_resets_total counter\nobscura_sandbox_resets_total {}\n",
            self.creates.load(Ordering::Relaxed),
            self.failures.load(Ordering::Relaxed),
            self.deletes.load(Ordering::Relaxed),
            self.resets.load(Ordering::Relaxed),
        )
    }
}

impl<V: Vmm + 'static, R: CommandRunner + Send + 'static> RuntimeService<V, R> {
    pub fn new(orchestrator: Orchestrator<V, R>, metrics: Arc<Metrics>) -> Self {
        let trajectory = orchestrator.trajectory();
        Self {
            orchestrator: Arc::new(Mutex::new(orchestrator)),
            metrics,
            sessions: Sessions::default(),
            trajectory,
        }
    }
}

#[tonic::async_trait]
impl<V: Vmm + 'static, R: CommandRunner + Send + 'static> DataPlane for RuntimeService<V, R> {
    async fn reset_sandbox(
        &self,
        request: Request<ResetSandboxRequest>,
    ) -> Result<Response<ResetSandboxResponse>, Status> {
        let request = request.into_inner();
        let id = request
            .sandbox_id
            .parse()
            .map_err(|_| Status::invalid_argument("invalid sandbox_id"))?;
        if request.mode != crate::rpc::ResetMode::Clean as i32 {
            return Err(Status::unimplemented("only clean reset is implemented"));
        }
        if request.request_id.trim().is_empty() || request.request_id.len() > 256 {
            return Err(Status::invalid_argument(
                "bounded reset request_id is required",
            ));
        }
        let orchestrator = Arc::clone(&self.orchestrator);
        let fence = self.sessions.get(id);
        let guard = Arc::clone(&fence).launch_owned().await;
        let sandbox = tokio::task::spawn_blocking(move || {
            let _guard = guard;
            let mut node = orchestrator.lock().expect("orchestrator lock poisoned");
            node.reset_clean_with_hook(id, &request.request_id, || {
                fence
                    .cancelled
                    .send_modify(|epoch| *epoch = epoch.wrapping_add(1));
            })
        })
        .await
        .map_err(|_| Status::internal("reset worker failed"))?
        .map_err(status)?;
        self.metrics.resets.fetch_add(1, Ordering::Relaxed);
        Ok(Response::new(ResetSandboxResponse {
            sandbox: Some(rpc_sandbox(sandbox)),
        }))
    }

    async fn create_sandbox(
        &self,
        request: Request<CreateSandboxRequest>,
    ) -> Result<Response<CreateSandboxResponse>, Status> {
        let request = request.into_inner();
        let sandbox_id = if request.sandbox_id.is_empty() {
            None
        } else {
            Some(
                request
                    .sandbox_id
                    .parse()
                    .map_err(|_| Status::invalid_argument("invalid sandbox_id"))?,
            )
        };
        if sandbox_id.is_some() && request.spec_hash.is_empty() {
            return Err(Status::invalid_argument(
                "spec_hash is required with sandbox_id",
            ));
        }
        validate_launch_policy(&request)?;
        let mut identity_request = request.clone();
        identity_request.request_id.clear();
        let policy_bytes = prost::Message::encode_to_vec(&identity_request);
        let resources = request
            .resources
            .ok_or_else(|| Status::invalid_argument("resources are required"))?;
        if request
            .network_policy
            .as_ref()
            .is_some_and(|policy| !policy.allow_hosts.is_empty())
        {
            return Err(Status::invalid_argument(
                "domain allowlists are not supported yet; refusing to silently allow egress",
            ));
        }
        let bound_spec_hash = format!(
            "{}:{:x}",
            request.spec_hash,
            sha2::Sha256::digest(policy_bytes)
        );
        let orchestrator = Arc::clone(&self.orchestrator);
        let template_ref = request.template_ref;
        let allow_egress = request
            .network_policy
            .is_some_and(|policy| policy.allow_egress);
        let result = tokio::task::spawn_blocking(move || {
            let mut node = orchestrator.lock().expect("orchestrator lock poisoned");
            node.create_pinned_with_identity(
                &template_ref,
                Resources {
                    vcpus: resources.vcpus,
                    memory_mib: resources.memory_mib,
                    disk_mib: resources.disk_mib,
                },
                allow_egress,
                sandbox_id,
                &bound_spec_hash,
                request
                    .version_pin
                    .as_ref()
                    .map(|pins| pins.image_digest.as_str()),
            )
            .map(|sandbox| (sandbox, resources))
        })
        .await
        .map_err(|error| Status::internal(error.to_string()))?
        .map_err(|error| {
            self.metrics.failures.fetch_add(1, Ordering::Relaxed);
            status(error)
        })?;
        let (sandbox, resources) = result;
        let guest_ipv4 = crate::network::addresses_for_slot(sandbox.network_slot)
            .map_err(|error| Status::internal(error.to_string()))?
            .1;
        self.metrics.creates.fetch_add(1, Ordering::Relaxed);
        tracing::info!(sandbox_id = %sandbox.id, "sandbox created");
        Ok(Response::new(CreateSandboxResponse {
            sandbox: Some(Sandbox {
                sandbox_id: sandbox.id.to_string(),
                template_ref: sandbox.template_ref,
                resources: Some(resources),
                state: rpc_state(sandbox.state) as i32,
                failure: if sandbox.state == crate::sandbox::SandboxState::Failed {
                    "sandbox failed or VM ownership was lost; create cannot replace this identity"
                        .into()
                } else {
                    String::new()
                },
                created_at_unix_nanos: sandbox.updated_at_unix_nanos,
                updated_at_unix_nanos: sandbox.updated_at_unix_nanos,
                guest_ipv4: guest_ipv4.to_string(),
            }),
        }))
    }

    async fn delete_sandbox(
        &self,
        request: Request<DeleteSandboxRequest>,
    ) -> Result<Response<DeleteSandboxResponse>, Status> {
        let sandbox_id = request
            .into_inner()
            .sandbox_id
            .parse()
            .map_err(|_| Status::invalid_argument("invalid sandbox_id"))?;
        let orchestrator = Arc::clone(&self.orchestrator);
        let fence = self.sessions.get(sandbox_id);
        let guard = Arc::clone(&fence).launch_owned().await;
        tokio::task::spawn_blocking(move || {
            let _guard = guard;
            fence
                .cancelled
                .send_modify(|epoch| *epoch = epoch.wrapping_add(1));
            orchestrator
                .lock()
                .expect("orchestrator lock poisoned")
                .delete(sandbox_id)
        })
        .await
        .map_err(|error| Status::internal(error.to_string()))?
        .map_err(status)?;
        self.metrics.deletes.fetch_add(1, Ordering::Relaxed);
        tracing::info!(sandbox_id = %sandbox_id, "sandbox deleted");
        Ok(Response::new(DeleteSandboxResponse {}))
    }

    async fn get_sandbox(
        &self,
        request: Request<GetSandboxRequest>,
    ) -> Result<Response<GetSandboxResponse>, Status> {
        let id = request
            .into_inner()
            .sandbox_id
            .parse()
            .map_err(|_| Status::invalid_argument("invalid sandbox_id"))?;
        let orchestrator = Arc::clone(&self.orchestrator);
        let sandbox = tokio::task::spawn_blocking(move || {
            orchestrator
                .lock()
                .expect("orchestrator lock poisoned")
                .get(id)
        })
        .await
        .map_err(|error| Status::internal(error.to_string()))?
        .map_err(status)?;
        let state = rpc_state(sandbox.state);
        let guest_ipv4 = crate::network::addresses_for_slot(sandbox.network_slot)
            .map_err(|error| Status::internal(error.to_string()))?
            .1;
        Ok(Response::new(GetSandboxResponse {
            sandbox: Some(Sandbox {
                sandbox_id: sandbox.id.to_string(),
                template_ref: sandbox.template_ref,
                resources: None,
                state: state as i32,
                failure: if sandbox.state == crate::sandbox::SandboxState::Failed {
                    "sandbox failed or VM ownership was lost; create cannot replace this identity"
                        .into()
                } else {
                    String::new()
                },
                created_at_unix_nanos: sandbox.updated_at_unix_nanos,
                updated_at_unix_nanos: sandbox.updated_at_unix_nanos,
                guest_ipv4: guest_ipv4.to_string(),
            }),
        }))
    }

    type ExecuteStream = ReceiverStream<Result<AgentEvent, Status>>;

    async fn execute(
        &self,
        request: Request<ExecuteRequest>,
    ) -> Result<Response<Self::ExecuteStream>, Status> {
        let request = request.into_inner();
        let id: uuid::Uuid = request
            .sandbox_id
            .parse()
            .map_err(|_| Status::invalid_argument("invalid sandbox_id"))?;
        if request.execution_id.is_empty()
            || request.execution_id.len() > 256
            || !request
                .execution_id
                .bytes()
                .all(|byte| byte.is_ascii_graphic())
            || request.command.is_empty()
        {
            return Err(Status::invalid_argument(
                "execution_id and command are required",
            ));
        }
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_err(|_| Status::internal("invalid host clock"))?
            .as_nanos() as i64;
        let time_left = if request.deadline_unix_nanos > 0 {
            if request.deadline_unix_nanos <= now {
                return Err(Status::deadline_exceeded("execution deadline expired"));
            }
            std::time::Duration::from_nanos((request.deadline_unix_nanos - now) as u64)
        } else {
            std::time::Duration::from_secs(300)
        };
        let deadline = tokio::time::Instant::now() + time_left;
        let fence = self.sessions.get(id);
        let launch_guard = tokio::time::timeout_at(deadline, fence.launch.read())
            .await
            .map_err(|_| {
                Status::deadline_exceeded("execution deadline expired during admission")
            })?;
        let mut cancelled = fence.cancelled.subscribe();
        let orchestrator = Arc::clone(&self.orchestrator);
        let (path, episode) = tokio::time::timeout_at(
            deadline,
            tokio::task::spawn_blocking(move || {
                let mut node = orchestrator.lock().expect("orchestrator lock poisoned");
                let path = node.guest_socket(id)?;
                Ok::<_, OrchestratorError>((path, node.episode(id)?))
            }),
        )
        .await
        .map_err(|_| Status::deadline_exceeded("execution deadline expired during admission"))?
        .map_err(|error| Status::internal(error.to_string()))?
        .map_err(status)?;
        let sandbox_id = request.sandbox_id;
        let execution_id = request.execution_id;
        let identity = TrajectoryIdentity {
            sandbox_id: id,
            episode_id: episode.episode_id,
            generation: episode.generation,
            environment_revision: episode.environment_revision,
            execution_id: Some(execution_id.clone()),
        };
        append_execution(
            self.trajectory.clone(),
            identity.clone(),
            SequenceScope::HostExecution,
            1,
            TrajectoryEvent::ExecutionStarted {
                command: PayloadDigest::of(request.command.as_bytes()),
                stdin: PayloadDigest::of(&request.input),
            },
        )
        .await?;
        if tokio::time::Instant::now() >= deadline {
            append_execution(
                self.trajectory.clone(),
                identity.clone(),
                SequenceScope::HostExecution,
                2,
                TrajectoryEvent::Terminal {
                    outcome: TerminalOutcome::TimedOut,
                    exit_code: None,
                },
            )
            .await?;
            return Err(Status::deadline_exceeded(
                "execution deadline expired before guest launch",
            ));
        }
        let connected = tokio::time::timeout_at(
            deadline.min(tokio::time::Instant::now() + std::time::Duration::from_secs(5)),
            crate::guest::start_execution(
                &path,
                &sandbox_id,
                &execution_id,
                request.command,
                request.input,
                request.env_vars,
                deadline,
            ),
        )
        .await
        .map_err(|_| Status::deadline_exceeded("guest connect timed out"))
        .and_then(|result| result.map_err(|_| Status::unavailable("guest connection failed")));
        let (mut stream, ready_sequence) = match connected {
            Ok(stream) => stream,
            Err(error) => {
                let _ = append_execution(
                    self.trajectory.clone(),
                    identity.clone(),
                    SequenceScope::HostExecution,
                    2,
                    TrajectoryEvent::Terminal {
                        outcome: TerminalOutcome::TransportFailed,
                        exit_code: None,
                    },
                )
                .await;
                fail_capture(
                    Arc::clone(&self.orchestrator),
                    id,
                    identity.generation,
                    Arc::clone(&fence),
                )
                .await;
                return Err(error);
            }
        };
        drop(launch_guard);
        let (sender, receiver) = tokio::sync::mpsc::channel(16);
        let trajectory = self.trajectory.clone();
        let orchestrator = Arc::clone(&self.orchestrator);
        tokio::spawn(async move {
            // Keep the registry entry alive throughout the stream, including
            // backpressure. A watch receiver alone does not own the fence.
            let mut sequence = ready_sequence;
            let mut trajectory_sequence = 0_u64;
            let mut terminal_recorded = false;
            let (outcome, failure) = loop {
                let next = tokio::select! {
                    biased;
                    _ = cancelled.changed() => break (TerminalOutcome::Cancelled, Some(Status::cancelled("sandbox episode ended"))),
                    _ = sender.closed() => break (TerminalOutcome::Cancelled, None),
                    next = tokio::time::timeout_at(deadline, crate::guest::read_event(&mut stream)) => next,
                };
                let event = match next {
                    Ok(Ok(event)) => event,
                    Ok(Err(_)) => {
                        break (
                            TerminalOutcome::TransportFailed,
                            Some(Status::unavailable("guest event transport failed")),
                        )
                    }
                    Err(_) => {
                        break (
                            TerminalOutcome::TimedOut,
                            Some(Status::deadline_exceeded("guest execution timed out")),
                        )
                    }
                };
                if event.sandbox_id != sandbox_id
                    || event.execution_id != execution_id
                    || sequence.checked_add(1) != Some(event.sequence)
                {
                    break (
                        TerminalOutcome::TransportFailed,
                        Some(Status::data_loss(
                            "guest event identity or sequence mismatch",
                        )),
                    );
                }
                sequence = event.sequence;
                trajectory_sequence += 1;
                let event_type = match crate::rpc::GuestEventType::try_from(event.r#type) {
                    Ok(crate::rpc::GuestEventType::Started) => crate::rpc::EventType::Started,
                    Ok(crate::rpc::GuestEventType::Stdout) => crate::rpc::EventType::Stdout,
                    Ok(crate::rpc::GuestEventType::Stderr) => crate::rpc::EventType::Stderr,
                    Ok(crate::rpc::GuestEventType::Result) => crate::rpc::EventType::Result,
                    Ok(crate::rpc::GuestEventType::Error) => crate::rpc::EventType::Error,
                    _ => {
                        break (
                            TerminalOutcome::TransportFailed,
                            Some(Status::data_loss("unexpected guest event")),
                        );
                    }
                };
                let terminal = matches!(
                    event_type,
                    crate::rpc::EventType::Result | crate::rpc::EventType::Error
                );
                let kind = match event_type {
                    crate::rpc::EventType::Started => GuestEventKind::Started,
                    crate::rpc::EventType::Stdout => GuestEventKind::Stdout,
                    crate::rpc::EventType::Stderr => GuestEventKind::Stderr,
                    crate::rpc::EventType::Result => GuestEventKind::Exited,
                    _ => GuestEventKind::Error,
                };
                if let Err(error) = append_execution(
                    trajectory.clone(),
                    identity.clone(),
                    SequenceScope::GuestExecution,
                    trajectory_sequence,
                    TrajectoryEvent::GuestEvent {
                        kind,
                        guest_sequence: sequence,
                        payload: PayloadDigest::of(&event.payload),
                    },
                )
                .await
                {
                    fail_capture(
                        Arc::clone(&orchestrator),
                        id,
                        identity.generation,
                        Arc::clone(&fence),
                    )
                    .await;
                    break (TerminalOutcome::TransportFailed, Some(error));
                }
                if terminal {
                    if let Err(error) = append_execution(
                        trajectory.clone(),
                        identity.clone(),
                        SequenceScope::HostExecution,
                        2,
                        TrajectoryEvent::Terminal {
                            outcome: if event_type == crate::rpc::EventType::Result {
                                TerminalOutcome::Exited
                            } else {
                                TerminalOutcome::ExecutionFailed
                            },
                            exit_code: None,
                        },
                    )
                    .await
                    {
                        fail_capture(
                            Arc::clone(&orchestrator),
                            id,
                            identity.generation,
                            Arc::clone(&fence),
                        )
                        .await;
                        break (TerminalOutcome::TransportFailed, Some(error));
                    }
                    terminal_recorded = true;
                }
                let delivered = tokio::select! {
                    biased;
                    _ = cancelled.changed() => break (TerminalOutcome::Cancelled, Some(Status::cancelled("sandbox episode ended"))),
                    _ = tokio::time::sleep_until(deadline) => break (TerminalOutcome::TimedOut, Some(Status::deadline_exceeded("execution delivery timed out"))),
                    delivered = sender.send(Ok(AgentEvent {
                        sandbox_id: event.sandbox_id,
                        execution_id: event.execution_id,
                        sequence: event.sequence,
                        r#type: event_type as i32,
                        payload: event.payload,
                        error: event.error,
                    })) => delivered,
                };
                if delivered.is_err() {
                    break (TerminalOutcome::Cancelled, None);
                }
                if terminal {
                    break (TerminalOutcome::Exited, None);
                }
            };
            if !terminal_recorded {
                if let Err(error) = append_execution(
                    trajectory.clone(),
                    identity.clone(),
                    SequenceScope::HostExecution,
                    2,
                    TrajectoryEvent::Terminal {
                        outcome,
                        exit_code: None,
                    },
                )
                .await
                {
                    fail_capture(
                        Arc::clone(&orchestrator),
                        id,
                        identity.generation,
                        Arc::clone(&fence),
                    )
                    .await;
                    let _ = sender.try_send(Err(error));
                    return;
                }
            }
            // Cancellation/deadline status is best effort when the caller has
            // filled its queue. EOF is still bounded; no guest transport leaks.
            if let Some(error) = failure {
                let _ = sender.try_send(Err(error));
            }
        });
        Ok(Response::new(ReceiverStream::new(receiver)))
    }

    async fn health(
        &self,
        _request: Request<HealthRequest>,
    ) -> Result<Response<HealthResponse>, Status> {
        Ok(Response::new(HealthResponse {
            protocol_version: crate::PROTOCOL_VERSION.into(),
            version: env!("CARGO_PKG_VERSION").into(),
            capabilities: vec!["idempotent-create-v1".into(), "reset-clean-v1".into()],
        }))
    }
}

async fn append_execution(
    spool: Option<Arc<Mutex<TrajectorySpool>>>,
    identity: TrajectoryIdentity,
    scope: SequenceScope,
    sequence: u64,
    event: TrajectoryEvent,
) -> Result<(), Status> {
    if let Some(spool) = spool {
        tokio::task::spawn_blocking(move || {
            spool
                .lock()
                .expect("trajectory lock poisoned")
                .append(&TrajectoryRecord {
                    identity,
                    scope,
                    sequence,
                    event,
                })
        })
        .await
        .map_err(|_| Status::internal("trajectory worker failed"))?
        .map_err(trajectory_status)?;
    }
    Ok(())
}

fn trajectory_status(error: crate::trajectory::TrajectoryError) -> Status {
    match error {
        crate::trajectory::TrajectoryError::Backpressure { .. } => {
            Status::resource_exhausted("trajectory spool is full")
        }
        crate::trajectory::TrajectoryError::Sequence { .. } => {
            Status::already_exists("execution identity already recorded")
        }
        crate::trajectory::TrajectoryError::InvalidMetadata(_) => {
            Status::invalid_argument("invalid trajectory metadata")
        }
        _ => Status::internal("trajectory durability failed"),
    }
}

async fn fail_capture<V: Vmm + 'static, R: CommandRunner + Send + 'static>(
    orchestrator: Arc<Mutex<Orchestrator<V, R>>>,
    id: uuid::Uuid,
    generation: u64,
    fence: Arc<SessionFence>,
) {
    let result = tokio::task::spawn_blocking(move || {
        let mut node = orchestrator.lock().expect("orchestrator lock poisoned");
        // Completion from an old episode must never fail or cancel the new one.
        if node.episode(id)?.generation == generation {
            node.fail_capture(id)?;
            fence
                .cancelled
                .send_modify(|epoch| *epoch = epoch.wrapping_add(1));
        }
        Ok::<_, OrchestratorError>(())
    })
    .await;
    if !matches!(result, Ok(Ok(()))) {
        tracing::error!(sandbox_id = %id, "failed to persist capture failure");
    }
}

fn status(error: OrchestratorError) -> Status {
    match error {
        OrchestratorError::Conflict(_) => Status::already_exists(error.to_string()),
        OrchestratorError::Unavailable(_) => Status::failed_precondition(error.to_string()),
        OrchestratorError::Invalid(_) => Status::invalid_argument(error.to_string()),
        OrchestratorError::Storage(crate::storage::StorageError::NotFound) => {
            Status::not_found(error.to_string())
        }
        OrchestratorError::Storage(crate::storage::StorageError::Capacity) => {
            Status::resource_exhausted(error.to_string())
        }
        OrchestratorError::Storage(
            crate::storage::StorageError::ResetPrecondition
            | crate::storage::StorageError::MissingEpisode,
        ) => Status::failed_precondition(error.to_string()),
        OrchestratorError::Image(_) => {
            Status::failed_precondition("environment image does not match the pinned revision")
        }
        OrchestratorError::Trajectory(error) => trajectory_status(error),
        _ => Status::internal(error.to_string()),
    }
}

fn rpc_sandbox(sandbox: crate::sandbox::Sandbox) -> Sandbox {
    Sandbox {
        sandbox_id: sandbox.id.to_string(),
        template_ref: sandbox.template_ref,
        resources: None,
        state: rpc_state(sandbox.state) as i32,
        failure: if sandbox.state == crate::sandbox::SandboxState::Failed {
            "sandbox unavailable".into()
        } else {
            String::new()
        },
        created_at_unix_nanos: sandbox.updated_at_unix_nanos,
        updated_at_unix_nanos: sandbox.updated_at_unix_nanos,
        guest_ipv4: crate::network::addresses_for_slot(sandbox.network_slot)
            .map(|(_, ip)| ip.to_string())
            .unwrap_or_default(),
    }
}

fn validate_launch_policy(request: &CreateSandboxRequest) -> Result<(), Status> {
    if request
        .resources
        .as_ref()
        .is_some_and(|resources| resources.max_runtime_nanos != 0)
    {
        return Err(Status::unimplemented(
            "sandbox maximum lifetime is not enforced yet",
        ));
    }
    if request
        .network_policy
        .as_ref()
        .is_some_and(|policy| crate::rpc::NetworkEnforcement::try_from(policy.enforcement).is_err())
    {
        return Err(Status::invalid_argument("unknown network enforcement mode"));
    }
    if let Some(workspace) = &request.workspace {
        if workspace.mode != crate::rpc::WorkspaceMode::Unspecified as i32
            && workspace.mode != crate::rpc::WorkspaceMode::Ephemeral as i32
            || !workspace.reference.is_empty()
        {
            return Err(Status::unimplemented(
                "only ephemeral workspace is implemented",
            ));
        }
    }
    if request
        .credential_policy
        .as_ref()
        .is_some_and(|policy| policy.brokered || !policy.policy_ref.is_empty())
    {
        return Err(Status::unimplemented("credential broker is not installed"));
    }
    if let Some(pins) = &request.version_pin {
        if !pins.runtime_version.is_empty() && pins.runtime_version != env!("CARGO_PKG_VERSION") {
            return Err(Status::failed_precondition(
                "runtime version does not match pin",
            ));
        }
        if !pins.dependency_lock.is_empty()
            || !pins.dataset_version.is_empty()
            || !pins.scorer_version.is_empty()
        {
            return Err(Status::unimplemented(
                "dataset, dependency and scorer verification require an environment manifest",
            ));
        }
    }
    Ok(())
}

fn rpc_state(state: crate::sandbox::SandboxState) -> SandboxState {
    match state {
        crate::sandbox::SandboxState::Creating => SandboxState::Creating,
        crate::sandbox::SandboxState::Resetting => SandboxState::Creating,
        crate::sandbox::SandboxState::Ready => SandboxState::Ready,
        crate::sandbox::SandboxState::Running => SandboxState::Running,
        crate::sandbox::SandboxState::Paused => SandboxState::Paused,
        crate::sandbox::SandboxState::Deleted => SandboxState::Deleted,
        crate::sandbox::SandboxState::Failed => SandboxState::Failed,
        // There is no snapshot operation in this service yet; do not claim it is Ready.
        crate::sandbox::SandboxState::Snapshotting => SandboxState::Paused,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::network::{NetworkError, NetworkManager};
    use crate::orchestrator::NodeConfig;
    use crate::provider::FakeFirecracker;
    use crate::storage::StorageError;
    use prost::Message;
    use std::time::Duration;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio_stream::StreamExt;

    struct NoopRunner;
    impl CommandRunner for NoopRunner {
        fn run(&mut self, _: &str, _: &[String], _: Option<&[u8]>) -> Result<(), NetworkError> {
            Ok(())
        }
    }

    fn fixture() -> (
        tempfile::TempDir,
        RuntimeService<FakeFirecracker, NoopRunner>,
        uuid::Uuid,
    ) {
        fixture_with_limit(1024 * 1024)
    }

    fn fixture_with_limit(
        max_spool_bytes: u64,
    ) -> (
        tempfile::TempDir,
        RuntimeService<FakeFirecracker, NoopRunner>,
        uuid::Uuid,
    ) {
        let dir = tempfile::tempdir_in("/tmp").unwrap();
        std::fs::write(dir.path().join("kernel"), b"kernel").unwrap();
        std::fs::write(dir.path().join("base"), b"base").unwrap();
        let mut node = Orchestrator::new(
            NodeConfig {
                template_ref: "base".into(),
                kernel: dir.path().join("kernel"),
                rootfs: dir.path().join("base"),
                state_dir: dir.path().join("vm"),
                uplink: "eth0".into(),
                boot_args: String::new(),
                guest_ready_timeout: Duration::ZERO,
            },
            crate::storage::Store::open(dir.path().join("node.db")).unwrap(),
            FakeFirecracker::new(),
            NetworkManager::new(NoopRunner),
        )
        .with_trajectory(
            TrajectorySpool::open(
                &dir.path().join("trajectory"),
                crate::trajectory::SpoolLimits {
                    max_record_bytes: 4096,
                    max_spool_bytes,
                },
            )
            .unwrap(),
        );
        let sandbox = node
            .create(
                "base",
                Resources {
                    vcpus: 1,
                    memory_mib: 128,
                    disk_mib: 8,
                },
                false,
            )
            .unwrap();
        (
            dir,
            RuntimeService::new(node, Arc::new(Metrics::default())),
            sandbox.id,
        )
    }

    fn execute_request(id: uuid::Uuid) -> ExecuteRequest {
        ExecuteRequest {
            sandbox_id: id.to_string(),
            execution_id: "execution-1".into(),
            command: "sleep 60".into(),
            ..Default::default()
        }
    }

    async fn guest(
        service: &RuntimeService<FakeFirecracker, NoopRunner>,
        id: uuid::Uuid,
    ) -> tokio::task::JoinHandle<()> {
        guest_with_output(service, id, 0).await
    }

    async fn guest_with_output(
        service: &RuntimeService<FakeFirecracker, NoopRunner>,
        id: uuid::Uuid,
        outputs: u64,
    ) -> tokio::task::JoinHandle<()> {
        let path = service
            .orchestrator
            .lock()
            .unwrap()
            .guest_socket(id)
            .unwrap();
        let listener = tokio::net::UnixListener::bind(path).unwrap();
        tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.unwrap();
            while socket.read_u8().await.unwrap() != b'\n' {}
            socket.write_all(b"OK 5000\n").await.unwrap();
            for kind in [
                crate::rpc::GuestEventType::Ready,
                crate::rpc::GuestEventType::Started,
            ] {
                if kind == crate::rpc::GuestEventType::Started {
                    let size = socket.read_u32().await.unwrap();
                    let mut bytes = vec![0; size as usize];
                    socket.read_exact(&mut bytes).await.unwrap();
                    let run = crate::rpc::GuestCommand::decode(bytes.as_slice()).unwrap();
                    assert!(matches!(
                        run.command,
                        Some(crate::rpc::guest_command::Command::Run(_))
                    ));
                }
                let bytes = crate::rpc::GuestEvent {
                    protocol_version: crate::PROTOCOL_VERSION.into(),
                    sandbox_id: id.to_string(),
                    execution_id: if kind == crate::rpc::GuestEventType::Ready {
                        String::new()
                    } else {
                        "execution-1".into()
                    },
                    sequence: if kind == crate::rpc::GuestEventType::Ready {
                        1
                    } else {
                        2
                    },
                    r#type: kind as i32,
                    ..Default::default()
                }
                .encode_to_vec();
                socket.write_u32(bytes.len() as u32).await.unwrap();
                socket.write_all(&bytes).await.unwrap();
            }
            for sequence in 3..outputs + 3 {
                let bytes = crate::rpc::GuestEvent {
                    protocol_version: crate::PROTOCOL_VERSION.into(),
                    sandbox_id: id.to_string(),
                    execution_id: "execution-1".into(),
                    sequence,
                    r#type: crate::rpc::GuestEventType::Stdout as i32,
                    payload: b"secret-output".to_vec(),
                    ..Default::default()
                }
                .encode_to_vec();
                socket.write_u32(bytes.len() as u32).await.unwrap();
                socket.write_all(&bytes).await.unwrap();
            }
            // The execution transport must close when reset cancels its stream.
            assert_eq!(socket.read(&mut [0]).await.unwrap(), 0);
        })
    }

    #[tokio::test]
    async fn clean_reset_cancels_active_execution_and_replay_does_not_increment() {
        let (_dir, service, id) = fixture();
        let guest = guest(&service, id).await;
        let mut stream = service
            .execute(Request::new(execute_request(id)))
            .await
            .unwrap()
            .into_inner();
        assert!(stream.next().await.unwrap().is_ok());
        let request = ResetSandboxRequest {
            sandbox_id: id.to_string(),
            request_id: "reset-1".into(),
            mode: crate::rpc::ResetMode::Clean as i32,
        };
        let response = service
            .reset_sandbox(Request::new(request.clone()))
            .await
            .unwrap();
        assert_eq!(
            response.into_inner().sandbox.unwrap().state,
            SandboxState::Ready as i32
        );
        assert_eq!(
            stream.next().await.unwrap().unwrap_err().code(),
            tonic::Code::Cancelled
        );
        service.reset_sandbox(Request::new(request)).await.unwrap();
        assert_eq!(
            service
                .orchestrator
                .lock()
                .unwrap()
                .episode(id)
                .unwrap()
                .generation,
            2
        );
        tokio::time::timeout(Duration::from_secs(1), guest)
            .await
            .unwrap()
            .unwrap();
    }

    #[tokio::test]
    async fn rejected_reset_keeps_the_original_execution_stream() {
        let (dir, service, id) = fixture();
        let guest = guest(&service, id).await;
        let mut stream = service
            .execute(Request::new(execute_request(id)))
            .await
            .unwrap()
            .into_inner();
        stream.next().await.unwrap().unwrap();
        std::fs::write(dir.path().join("base"), b"changed").unwrap();
        let error = service
            .reset_sandbox(Request::new(ResetSandboxRequest {
                sandbox_id: id.to_string(),
                request_id: "reset".into(),
                mode: crate::rpc::ResetMode::Clean as i32,
            }))
            .await
            .unwrap_err();
        assert_eq!(error.code(), tonic::Code::FailedPrecondition);
        assert!(
            tokio::time::timeout(Duration::from_millis(20), stream.next())
                .await
                .is_err()
        );
        service
            .delete_sandbox(Request::new(DeleteSandboxRequest {
                sandbox_id: id.to_string(),
                ..Default::default()
            }))
            .await
            .unwrap();
        assert_eq!(
            stream.next().await.unwrap().unwrap_err().code(),
            tonic::Code::Cancelled
        );
        tokio::time::timeout(Duration::from_secs(1), guest)
            .await
            .unwrap()
            .unwrap();
    }

    #[tokio::test]
    async fn deadline_covers_waiting_for_reset_admission() {
        let (_dir, service, id) = fixture();
        let fence = service.sessions.get(id);
        let _guard = fence.launch.write().await;
        let mut request = execute_request(id);
        request.deadline_unix_nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos() as i64
            + 20_000_000;
        let result = tokio::time::timeout(
            Duration::from_secs(1),
            service.execute(Request::new(request)),
        )
        .await
        .unwrap();
        assert_eq!(result.err().unwrap().code(), tonic::Code::DeadlineExceeded);
    }

    #[tokio::test]
    async fn reset_releases_guest_transport_with_a_full_client_queue() {
        let (_dir, service, id) = fixture();
        let guest = guest_with_output(&service, id, 32).await;
        let _unread_stream = service
            .execute(Request::new(execute_request(id)))
            .await
            .unwrap()
            .into_inner();
        // The seventeenth forwarded event cannot fit in the unread 16-slot
        // queue. Its durable record proves this exercises backpressure.
        let episode = service.orchestrator.lock().unwrap().episode(id).unwrap();
        let identity = TrajectoryIdentity {
            sandbox_id: id,
            episode_id: episode.episode_id,
            generation: episode.generation,
            environment_revision: episode.environment_revision,
            execution_id: Some("execution-1".into()),
        };
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                if service
                    .trajectory
                    .as_ref()
                    .unwrap()
                    .lock()
                    .unwrap()
                    .next_sequence(&identity, SequenceScope::GuestExecution)
                    .unwrap()
                    >= 18
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        service
            .reset_sandbox(Request::new(ResetSandboxRequest {
                sandbox_id: id.to_string(),
                request_id: "reset".into(),
                mode: crate::rpc::ResetMode::Clean as i32,
            }))
            .await
            .unwrap();
        tokio::time::timeout(Duration::from_secs(1), guest)
            .await
            .unwrap()
            .unwrap();
    }

    #[tokio::test]
    async fn duplicate_execution_is_rejected_before_another_guest_command() {
        let (_dir, service, id) = fixture();
        let guest = guest(&service, id).await;
        let mut request = execute_request(id);
        request.command = "secret-command".into();
        request.input = b"secret-input".to_vec();
        request
            .env_vars
            .insert("TOKEN".into(), "secret-credential".into());
        let mut stream = service
            .execute(Request::new(request.clone()))
            .await
            .unwrap()
            .into_inner();
        stream.next().await.unwrap().unwrap();
        assert_eq!(
            service
                .execute(Request::new(request))
                .await
                .err()
                .unwrap()
                .code(),
            tonic::Code::AlreadyExists
        );
        let records = service
            .trajectory
            .as_ref()
            .unwrap()
            .lock()
            .unwrap()
            .read_records()
            .unwrap();
        let bytes = serde_json::to_string(&records).unwrap();
        for secret in ["secret-command", "secret-input", "secret-credential"] {
            assert!(!bytes.contains(secret));
        }
        service
            .delete_sandbox(Request::new(DeleteSandboxRequest {
                sandbox_id: id.to_string(),
                ..Default::default()
            }))
            .await
            .unwrap();
        stream.next().await.unwrap().unwrap_err();
        tokio::time::timeout(Duration::from_secs(1), guest)
            .await
            .unwrap()
            .unwrap();
    }

    #[tokio::test]
    async fn full_spool_rejects_execution_before_guest_admission() {
        let (_dir, service, id) = fixture_with_limit(4096);
        let episode = service.orchestrator.lock().unwrap().episode(id).unwrap();
        let identity = TrajectoryIdentity {
            sandbox_id: id,
            episode_id: episode.episode_id,
            generation: episode.generation,
            environment_revision: episode.environment_revision,
            execution_id: None,
        };
        {
            let mut spool = service.trajectory.as_ref().unwrap().lock().unwrap();
            loop {
                let sequence = spool
                    .next_sequence(&identity, SequenceScope::Lifecycle)
                    .unwrap();
                if spool
                    .append(&TrajectoryRecord {
                        identity: identity.clone(),
                        scope: SequenceScope::Lifecycle,
                        sequence,
                        event: TrajectoryEvent::ResourceUsage {
                            cpu_micros: 1,
                            memory_bytes: 1,
                        },
                    })
                    .is_err()
                {
                    break;
                }
            }
        }
        assert_eq!(
            service
                .execute(Request::new(execute_request(id)))
                .await
                .err()
                .unwrap()
                .code(),
            tonic::Code::ResourceExhausted
        );
        // No guest listener exists: reaching connect would return Unavailable.
        assert_eq!(
            service.orchestrator.lock().unwrap().get(id).unwrap().state,
            crate::sandbox::SandboxState::Ready
        );
        service
            .delete_sandbox(Request::new(DeleteSandboxRequest {
                sandbox_id: id.to_string(),
                ..Default::default()
            }))
            .await
            .unwrap();
    }

    #[tokio::test]
    async fn old_capture_failure_cannot_fail_the_new_episode() {
        let (_dir, service, id) = fixture();
        service
            .reset_sandbox(Request::new(ResetSandboxRequest {
                sandbox_id: id.to_string(),
                request_id: "reset".into(),
                mode: crate::rpc::ResetMode::Clean as i32,
            }))
            .await
            .unwrap();
        fail_capture(
            Arc::clone(&service.orchestrator),
            id,
            1,
            service.sessions.get(id),
        )
        .await;
        assert_eq!(
            service.orchestrator.lock().unwrap().get(id).unwrap().state,
            crate::sandbox::SandboxState::Ready
        );
    }

    #[test]
    fn inactive_session_registry_is_bounded_and_active_fences_are_shared() {
        let sessions = Sessions::default();
        let id = uuid::Uuid::new_v4();
        let held = sessions.get(id);
        for _ in 0..1000 {
            drop(sessions.get(uuid::Uuid::new_v4()));
        }
        assert!(Arc::ptr_eq(&held, &sessions.get(id)));
        assert_eq!(sessions.entries.lock().unwrap().len(), 1);
    }

    #[tokio::test]
    async fn unsupported_modes_and_unenforced_policies_fail_closed() {
        let (_dir, service, id) = fixture();
        for mode in [
            crate::rpc::ResetMode::Unspecified,
            crate::rpc::ResetMode::Checkpoint,
            crate::rpc::ResetMode::Template,
        ] {
            let error = service
                .reset_sandbox(Request::new(ResetSandboxRequest {
                    sandbox_id: id.to_string(),
                    request_id: "reset".into(),
                    mode: mode as i32,
                }))
                .await
                .unwrap_err();
            assert_eq!(error.code(), tonic::Code::Unimplemented);
        }
        let mut request = CreateSandboxRequest {
            workspace: Some(crate::rpc::WorkspaceSpec {
                mode: crate::rpc::WorkspaceMode::Persistent as i32,
                reference: "volume".into(),
            }),
            ..Default::default()
        };
        assert_eq!(
            validate_launch_policy(&request).unwrap_err().code(),
            tonic::Code::Unimplemented
        );
        request.workspace = None;
        request.credential_policy = Some(crate::rpc::CredentialPolicy {
            brokered: true,
            policy_ref: "policy".into(),
        });
        assert_eq!(
            validate_launch_policy(&request).unwrap_err().code(),
            tonic::Code::Unimplemented
        );
        request.credential_policy = None;
        request.version_pin = Some(crate::rpc::VersionPin {
            runtime_version: "wrong".into(),
            ..Default::default()
        });
        assert_eq!(
            validate_launch_policy(&request).unwrap_err().code(),
            tonic::Code::FailedPrecondition
        );
        let capabilities = service
            .health(Request::new(HealthRequest {}))
            .await
            .unwrap()
            .into_inner()
            .capabilities;
        assert_eq!(capabilities, ["idempotent-create-v1", "reset-clean-v1"]);
    }

    #[test]
    fn network_capacity_is_resource_exhausted() {
        assert_eq!(
            status(OrchestratorError::Storage(StorageError::Capacity)).code(),
            tonic::Code::ResourceExhausted
        );
    }
}
