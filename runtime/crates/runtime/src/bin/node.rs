use agentruntime_runtime::firecracker::{FirecrackerConfig, ProcessFirecracker};
use agentruntime_runtime::network::{NetworkManager, SystemCommandRunner};
use agentruntime_runtime::orchestrator::{NodeConfig, Orchestrator};
use agentruntime_runtime::rpc::data_plane_server::DataPlaneServer;
use agentruntime_runtime::service::{Metrics, RuntimeService};
use agentruntime_runtime::storage::Store;
use axum::{routing::get, Router};
use std::env;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;
use tokio::net::UnixListener;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::Server;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "agentruntime_runtime=info".into()),
        )
        .init();
    if !cfg!(target_os = "linux") || !Path::new("/dev/kvm").exists() {
        anyhow::bail!("node runtime requires Linux with /dev/kvm");
    }
    if fs::read_to_string("/proc/sys/net/ipv4/ip_forward")?.trim() != "1" {
        anyhow::bail!("enable net.ipv4.ip_forward=1 on the host before starting");
    }
    let state_dir = PathBuf::from(required("OBSCURA_STATE_DIR")?);
    let socket = PathBuf::from(required("OBSCURA_NODE_SOCKET")?);
    let kernel = PathBuf::from(required("OBSCURA_KERNEL")?);
    let rootfs = PathBuf::from(required("OBSCURA_ROOTFS")?);
    let fc_binary = PathBuf::from(required("OBSCURA_FIRECRACKER")?);
    for image in [&kernel, &rootfs, &fc_binary] {
        if !image.is_file() {
            anyhow::bail!("required file missing: {}", image.display());
        }
    }
    fs::create_dir_all(&state_dir)?;
    let api_socket_dir = state_dir.join("sockets");
    let firecracker = ProcessFirecracker::new(FirecrackerConfig {
        binary_path: fc_binary,
        socket_dir: api_socket_dir,
        startup_timeout: Duration::from_secs(5),
    })?;
    let store = Store::open(state_dir.join("node.sqlite"))?;
    let active_ids = store.active_sandbox_ids()?;
    if !active_ids.is_empty() {
        tracing::warn!(sandboxes = ?active_ids, "previous sandbox resources quarantined: ownership cannot be proven; automatic cleanup is disabled");
    }
    let orchestrator = Orchestrator::new(
        NodeConfig {
            template_ref: required("OBSCURA_TEMPLATE_REF")?,
            kernel,
            rootfs,
            state_dir,
            uplink: required("OBSCURA_UPLINK")?,
            boot_args: env::var("OBSCURA_BOOT_ARGS").unwrap_or_else(|_| {
                "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/sbin/init".into()
            }),
            guest_ready_timeout: Duration::from_secs(30),
        },
        store,
        firecracker,
        NetworkManager::new(SystemCommandRunner),
    );
    if socket.exists() {
        anyhow::bail!("node socket already exists: {}", socket.display());
    }
    let listener = UnixListener::bind(&socket)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&socket, fs::Permissions::from_mode(0o600))?;
    }
    let metrics_address = env::var("OBSCURA_NODE_HTTP").unwrap_or_else(|_| "127.0.0.1:9001".into());
    let http_listener = tokio::net::TcpListener::bind(&metrics_address).await?;
    let metrics = Arc::new(Metrics::default());
    let http_metrics = Arc::clone(&metrics);
    tracing::info!(socket = %socket.display(), http = %metrics_address, "node runtime started");
    let http = axum::serve(
        http_listener,
        Router::new()
            .route("/healthz", get(|| async { "ok" }))
            .route(
                "/metrics",
                get(move || {
                    let metrics = Arc::clone(&http_metrics);
                    async move { metrics.render() }
                }),
            ),
    );
    let grpc = Server::builder()
        .add_service(DataPlaneServer::new(RuntimeService::new(
            orchestrator,
            metrics,
        )))
        .serve_with_incoming(UnixListenerStream::new(listener));
    tokio::select! {
        result = http => result?,
        result = grpc => result?,
        _ = tokio::signal::ctrl_c() => tracing::info!("shutdown signal received"),
    }
    let _ = fs::remove_file(&socket);
    Ok(())
}

fn required(name: &str) -> anyhow::Result<String> {
    let value = env::var(name)?;
    if value.trim().is_empty() {
        anyhow::bail!("{name} cannot be empty");
    }
    Ok(value)
}
