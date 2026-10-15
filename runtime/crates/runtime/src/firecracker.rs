use crate::model::{LaunchSpec, SandboxHandle};
use crate::provider::{FirecrackerProvider, RuntimeError};
use serde_json::json;
use std::collections::HashMap;
use std::fs;
use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};
use uuid::Uuid;

const API_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_API_RESPONSE: usize = 64 * 1024;

pub struct FirecrackerConfig {
    pub binary_path: PathBuf,
    pub socket_dir: PathBuf,
    pub startup_timeout: Duration,
}

pub struct ProcessFirecracker {
    config: FirecrackerConfig,
    processes: HashMap<Uuid, ProcessHandle>,
}

struct ProcessHandle {
    child: Child,
    api_socket: PathBuf,
}

impl ProcessFirecracker {
    pub fn new(config: FirecrackerConfig) -> Result<Self, RuntimeError> {
        if config.socket_dir.join("sandbox.sock").as_os_str().len() >= 100 {
            return Err(RuntimeError::InvalidSpec(
                "Firecracker API socket directory produces an overlong Unix socket path".into(),
            ));
        }
        fs::create_dir_all(&config.socket_dir).map_err(RuntimeError::Io)?;
        Ok(Self {
            config,
            processes: HashMap::new(),
        })
    }

    fn configure(&self, socket: &Path, spec: &LaunchSpec) -> Result<(), RuntimeError> {
        put_json(
            socket,
            "/machine-config",
            json!({
                "vcpu_count": spec.vcpus,
                "mem_size_mib": spec.memory_mib,
                "smt": false,
            }),
        )?;
        put_json(
            socket,
            "/boot-source",
            json!({
                "kernel_image_path": spec.kernel_image_path,
                "boot_args": spec.boot_args,
            }),
        )?;
        put_json(
            socket,
            "/drives/rootfs",
            json!({
                "drive_id": "rootfs",
                "path_on_host": spec.rootfs_path,
                "is_root_device": true,
                "is_read_only": false,
            }),
        )?;
        put_json(
            socket,
            "/network-interfaces/net1",
            json!({
                "iface_id": "net1",
                "guest_mac": spec.guest_mac,
                "host_dev_name": spec.tap_device,
            }),
        )?;
        if let Some(vsock) = &spec.vsock {
            put_json(
                socket,
                "/vsock",
                json!({
                    "vsock_id": "vsock0",
                    "guest_cid": vsock.guest_cid,
                    "uds": vsock.uds_path,
                }),
            )?;
        }
        post_json(
            socket,
            "/actions",
            json!({ "action_type": "InstanceStart" }),
        )
    }

    fn wait_for_socket(&self, socket: &Path) -> Result<(), RuntimeError> {
        let deadline = Instant::now() + self.config.startup_timeout;
        while Instant::now() < deadline {
            if UnixStream::connect(socket).is_ok() {
                return Ok(());
            }
            thread::sleep(Duration::from_millis(10));
        }
        Err(RuntimeError::StartupTimeout(socket.to_path_buf()))
    }
}

impl FirecrackerProvider for ProcessFirecracker {
    fn launch(&mut self, spec: &LaunchSpec) -> Result<SandboxHandle, RuntimeError> {
        validate_spec(spec)?;
        let sandbox_id = spec.sandbox_id;
        let api_socket = self.config.socket_dir.join(format!("{sandbox_id}.sock"));
        let child = Command::new(&self.config.binary_path)
            .arg("--api-sock")
            .arg(&api_socket)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .map_err(RuntimeError::Io)?;
        self.processes.insert(
            sandbox_id,
            ProcessHandle {
                child,
                api_socket: api_socket.clone(),
            },
        );
        if let Err(error) = self
            .wait_for_socket(&api_socket)
            .and_then(|_| self.configure(&api_socket, spec))
        {
            let sandbox = SandboxHandle {
                sandbox_id,
                runtime_handle: api_socket.display().to_string(),
            };
            let _ = self.stop(&sandbox);
            return Err(error);
        }
        Ok(SandboxHandle {
            sandbox_id,
            runtime_handle: api_socket.display().to_string(),
        })
    }

    fn is_running(&mut self, handle: &SandboxHandle) -> Result<bool, RuntimeError> {
        match self.processes.get_mut(&handle.sandbox_id) {
            Some(process) => Ok(process
                .child
                .try_wait()
                .map_err(RuntimeError::Io)?
                .is_none()),
            None => Ok(false),
        }
    }

    fn stop(&mut self, handle: &SandboxHandle) -> Result<(), RuntimeError> {
        let Some(process) = self.processes.get_mut(&handle.sandbox_id) else {
            return Ok(());
        };
        if process
            .child
            .try_wait()
            .map_err(RuntimeError::Io)?
            .is_none()
        {
            process.child.kill().map_err(RuntimeError::Io)?;
            process.child.wait().map_err(RuntimeError::Io)?;
        }
        if let Err(error) = fs::remove_file(&process.api_socket) {
            if error.kind() != std::io::ErrorKind::NotFound {
                return Err(RuntimeError::Io(error));
            }
        }
        self.processes.remove(&handle.sandbox_id);
        Ok(())
    }
}

fn validate_spec(spec: &LaunchSpec) -> Result<(), RuntimeError> {
    if spec.template_ref.is_empty() {
        return Err(RuntimeError::EmptyTemplate);
    }
    if spec.vcpus == 0 || spec.memory_mib == 0 || spec.disk_mib == 0 {
        return Err(RuntimeError::InvalidSpec(
            "resources must be non-zero".into(),
        ));
    }
    if spec.tap_device.is_empty() || spec.guest_mac.is_empty() {
        return Err(RuntimeError::InvalidSpec(
            "network identity is required".into(),
        ));
    }
    if !spec.kernel_image_path.is_file() || !spec.rootfs_path.is_file() {
        return Err(RuntimeError::InvalidSpec(
            "kernel and rootfs must be files".into(),
        ));
    }
    Ok(())
}

fn put_json(socket: &Path, path: &str, body: serde_json::Value) -> Result<(), RuntimeError> {
    request(socket, "PUT", path, body)
}

fn post_json(socket: &Path, path: &str, body: serde_json::Value) -> Result<(), RuntimeError> {
    request(socket, "POST", path, body)
}

fn request(
    socket: &Path,
    method: &str,
    path: &str,
    body: serde_json::Value,
) -> Result<(), RuntimeError> {
    request_with_timeout(socket, method, path, body, API_TIMEOUT)
}

fn request_with_timeout(
    socket: &Path,
    method: &str,
    path: &str,
    body: serde_json::Value,
    timeout: Duration,
) -> Result<(), RuntimeError> {
    let payload =
        serde_json::to_vec(&body).map_err(|error| RuntimeError::Api(error.to_string()))?;
    let mut stream = UnixStream::connect(socket).map_err(RuntimeError::Io)?;
    stream
        .set_read_timeout(Some(timeout))
        .map_err(RuntimeError::Io)?;
    stream
        .set_write_timeout(Some(timeout))
        .map_err(RuntimeError::Io)?;
    let request = format!(
        "{method} {path} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n",
        payload.len()
    );
    stream
        .write_all(request.as_bytes())
        .map_err(RuntimeError::Io)?;
    stream.write_all(&payload).map_err(RuntimeError::Io)?;
    stream
        .shutdown(std::net::Shutdown::Write)
        .map_err(RuntimeError::Io)?;
    let mut response = Vec::new();
    stream
        .take((MAX_API_RESPONSE + 1) as u64)
        .read_to_end(&mut response)
        .map_err(RuntimeError::Io)?;
    if response.len() > MAX_API_RESPONSE {
        return Err(RuntimeError::Api(
            "Firecracker API response too large".into(),
        ));
    }
    let status = parse_status(&response)?;
    if !(200..300).contains(&status) {
        return Err(RuntimeError::Api(format!(
            "Firecracker returned HTTP {status}"
        )));
    }
    Ok(())
}

fn parse_status(response: &[u8]) -> Result<u16, RuntimeError> {
    let line_end = response
        .windows(2)
        .position(|window| window == b"\r\n")
        .ok_or_else(|| RuntimeError::Api("malformed HTTP response".into()))?;
    let line = std::str::from_utf8(&response[..line_end])
        .map_err(|_| RuntimeError::Api("non-UTF-8 HTTP status".into()))?;
    line.split_whitespace()
        .nth(1)
        .and_then(|value| value.parse().ok())
        .ok_or_else(|| RuntimeError::Api(format!("malformed HTTP status line: {line}")))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::net::UnixListener;

    #[test]
    fn parses_successful_api_response() {
        let socket = PathBuf::from(format!("/tmp/ar-{}.sock", Uuid::new_v4()));
        let listener = UnixListener::bind(&socket).unwrap();
        let server = thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut request = [0_u8; 2048];
            let _ = stream.read(&mut request).unwrap();
            stream
                .write_all(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
                .unwrap();
        });
        request(&socket, "PUT", "/machine-config", json!({"vcpu_count": 1})).unwrap();
        server.join().unwrap();
        let _ = fs::remove_file(socket);
    }

    #[test]
    fn times_out_on_unresponsive_api() {
        let directory = tempfile::tempdir().unwrap();
        let socket = directory.path().join("api.sock");
        let listener = UnixListener::bind(&socket).unwrap();
        let server = thread::spawn(move || {
            let (_stream, _) = listener.accept().unwrap();
            thread::sleep(Duration::from_millis(150));
        });
        let error = request_with_timeout(
            &socket,
            "PUT",
            "/machine-config",
            json!({}),
            Duration::from_millis(25),
        )
        .unwrap_err();
        assert!(
            matches!(error, RuntimeError::Io(ref cause) if cause.kind() == std::io::ErrorKind::WouldBlock || cause.kind() == std::io::ErrorKind::TimedOut)
        );
        server.join().unwrap();
    }

    #[test]
    fn rejects_oversized_api_response() {
        let directory = tempfile::tempdir().unwrap();
        let socket = directory.path().join("api.sock");
        let listener = UnixListener::bind(&socket).unwrap();
        let server = thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut request = [0_u8; 2048];
            let _ = stream.read(&mut request).unwrap();
            stream.write_all(b"HTTP/1.1 500 Error\r\n\r\n").unwrap();
            stream.write_all(&vec![b'x'; MAX_API_RESPONSE]).unwrap();
        });
        let error = request(&socket, "PUT", "/machine-config", json!({})).unwrap_err();
        assert!(matches!(error, RuntimeError::Api(message) if message.contains("too large")));
        server.join().unwrap();
    }

    #[test]
    fn stop_reports_socket_cleanup_failure() {
        let directory = tempfile::tempdir().unwrap();
        let sandbox_id = Uuid::new_v4();
        let api_socket = directory.path().join("api.sock");
        fs::create_dir(&api_socket).unwrap();
        let mut provider = ProcessFirecracker::new(FirecrackerConfig {
            binary_path: PathBuf::from("/bin/true"),
            socket_dir: directory.path().join("sockets"),
            startup_timeout: Duration::from_secs(1),
        })
        .unwrap();
        provider.processes.insert(
            sandbox_id,
            ProcessHandle {
                child: Command::new("sleep").arg("10").spawn().unwrap(),
                api_socket: api_socket.clone(),
            },
        );
        let handle = SandboxHandle {
            sandbox_id,
            runtime_handle: api_socket.display().to_string(),
        };
        assert!(FirecrackerProvider::stop(&mut provider, &handle).is_err());
    }
}
