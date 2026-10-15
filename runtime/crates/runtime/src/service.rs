use crate::firecracker::ProcessFirecracker;
use crate::network::SystemCommandRunner;
use crate::orchestrator::{Orchestrator, OrchestratorError, Resources};
use crate::rpc::data_plane_server::DataPlane;
use crate::rpc::{
    AgentEvent, CreateSandboxRequest, CreateSandboxResponse, DeleteSandboxRequest,
    DeleteSandboxResponse, ExecuteRequest, GetSandboxRequest, GetSandboxResponse, HealthRequest,
    HealthResponse, Sandbox, SandboxState,
};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Request, Response, Status};

pub struct RuntimeService {
    orchestrator: Arc<Mutex<Orchestrator<ProcessFirecracker, SystemCommandRunner>>>,
    metrics: Arc<Metrics>,
}

#[derive(Default)]
pub struct Metrics {
    creates: AtomicU64,
    failures: AtomicU64,
    deletes: AtomicU64,
}

impl Metrics {
    pub fn render(&self) -> String {
        format!(
            "# TYPE obscura_sandbox_creates_total counter\nobscura_sandbox_creates_total {}\n\
             # TYPE obscura_sandbox_failures_total counter\nobscura_sandbox_failures_total {}\n\
             # TYPE obscura_sandbox_deletes_total counter\nobscura_sandbox_deletes_total {}\n",
            self.creates.load(Ordering::Relaxed),
            self.failures.load(Ordering::Relaxed),
            self.deletes.load(Ordering::Relaxed),
        )
    }
}

impl RuntimeService {
    pub fn new(
        orchestrator: Orchestrator<ProcessFirecracker, SystemCommandRunner>,
        metrics: Arc<Metrics>,
    ) -> Self {
        Self {
            orchestrator: Arc::new(Mutex::new(orchestrator)),
            metrics,
        }
    }
}

#[tonic::async_trait]
impl DataPlane for RuntimeService {
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
        let bound_spec_hash =
            serde_json::to_string(&(request.spec_hash, resources.max_runtime_nanos))
                .map_err(|error| Status::internal(error.to_string()))?;
        let orchestrator = Arc::clone(&self.orchestrator);
        let template_ref = request.template_ref;
        let allow_egress = request
            .network_policy
            .is_some_and(|policy| policy.allow_egress);
        let result = tokio::task::spawn_blocking(move || {
            orchestrator
                .lock()
                .expect("orchestrator lock poisoned")
                .create_with_identity(
                    &template_ref,
                    Resources {
                        vcpus: resources.vcpus,
                        memory_mib: resources.memory_mib,
                        disk_mib: resources.disk_mib,
                    },
                    allow_egress,
                    sandbox_id,
                    &bound_spec_hash,
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
        tokio::task::spawn_blocking(move || {
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
        if request.execution_id.is_empty() || request.command.is_empty() {
            return Err(Status::invalid_argument(
                "execution_id and command are required",
            ));
        }
        let orchestrator = Arc::clone(&self.orchestrator);
        let path = tokio::task::spawn_blocking(move || {
            orchestrator
                .lock()
                .expect("orchestrator lock poisoned")
                .guest_socket(id)
        })
        .await
        .map_err(|error| Status::internal(error.to_string()))?
        .map_err(status)?;
        let sandbox_id = request.sandbox_id;
        let execution_id = request.execution_id;
        let mut stream = tokio::time::timeout(
            std::time::Duration::from_secs(5),
            crate::guest::start_execution(
                &path,
                &sandbox_id,
                &execution_id,
                request.command,
                request.input,
                request.env_vars,
            ),
        )
        .await
        .map_err(|_| Status::deadline_exceeded("guest connect timed out"))?
        .map_err(|error| Status::unavailable(error.to_string()))?;
        let now_nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_err(|error| Status::internal(error.to_string()))?
            .as_nanos() as i64;
        let time_left = if request.deadline_unix_nanos > 0 {
            if request.deadline_unix_nanos <= now_nanos {
                return Err(Status::deadline_exceeded("execution deadline expired"));
            }
            std::time::Duration::from_nanos((request.deadline_unix_nanos - now_nanos) as u64)
        } else {
            std::time::Duration::from_secs(300)
        };
        let (sender, receiver) = tokio::sync::mpsc::channel(16);
        tokio::spawn(async move {
            let deadline = tokio::time::Instant::now() + time_left;
            let mut sequence = 0_u64;
            loop {
                let event =
                    match tokio::time::timeout_at(deadline, crate::guest::read_event(&mut stream))
                        .await
                    {
                        Ok(Ok(event)) => event,
                        Ok(Err(error)) => {
                            let _ = sender
                                .send(Err(Status::unavailable(error.to_string())))
                                .await;
                            break;
                        }
                        Err(_) => {
                            let _ = sender
                                .send(Err(Status::deadline_exceeded("guest execution timed out")))
                                .await;
                            break;
                        }
                    };
                if event.sandbox_id != sandbox_id
                    || event.execution_id != execution_id
                    || event.sequence <= sequence
                {
                    let _ = sender
                        .send(Err(Status::data_loss(
                            "guest event identity or sequence mismatch",
                        )))
                        .await;
                    break;
                }
                sequence = event.sequence;
                let event_type = match crate::rpc::GuestEventType::try_from(event.r#type) {
                    Ok(crate::rpc::GuestEventType::Started) => crate::rpc::EventType::Started,
                    Ok(crate::rpc::GuestEventType::Stdout) => crate::rpc::EventType::Stdout,
                    Ok(crate::rpc::GuestEventType::Stderr) => crate::rpc::EventType::Stderr,
                    Ok(crate::rpc::GuestEventType::Result) => crate::rpc::EventType::Result,
                    Ok(crate::rpc::GuestEventType::Error) => crate::rpc::EventType::Error,
                    _ => {
                        let _ = sender
                            .send(Err(Status::data_loss("unexpected guest event")))
                            .await;
                        break;
                    }
                };
                let terminal = matches!(
                    event_type,
                    crate::rpc::EventType::Result | crate::rpc::EventType::Error
                );
                if sender
                    .send(Ok(AgentEvent {
                        sandbox_id: event.sandbox_id,
                        execution_id: event.execution_id,
                        sequence: event.sequence,
                        r#type: event_type as i32,
                        payload: event.payload,
                        error: event.error,
                    }))
                    .await
                    .is_err()
                {
                    break;
                }
                if terminal {
                    break;
                }
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
            capabilities: vec!["idempotent-create-v1".into()],
        }))
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
        _ => Status::internal(error.to_string()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::storage::StorageError;

    #[test]
    fn network_capacity_is_resource_exhausted() {
        assert_eq!(
            status(OrchestratorError::Storage(StorageError::Capacity)).code(),
            tonic::Code::ResourceExhausted
        );
    }
}

fn rpc_state(state: crate::sandbox::SandboxState) -> SandboxState {
    match state {
        crate::sandbox::SandboxState::Creating => SandboxState::Creating,
        crate::sandbox::SandboxState::Ready => SandboxState::Ready,
        crate::sandbox::SandboxState::Running => SandboxState::Running,
        crate::sandbox::SandboxState::Paused => SandboxState::Paused,
        crate::sandbox::SandboxState::Deleted => SandboxState::Deleted,
        crate::sandbox::SandboxState::Failed => SandboxState::Failed,
        // There is no snapshot operation in this service yet; do not claim it is Ready.
        crate::sandbox::SandboxState::Snapshotting => SandboxState::Paused,
    }
}
