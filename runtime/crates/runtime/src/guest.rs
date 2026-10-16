use crate::rpc::{guest_command, GuestCommand, GuestEvent, RunCommand};
use prost::Message;
use std::io::{Read, Write};
use std::os::unix::net::UnixStream as BlockingStream;
use std::path::Path;
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

const GUEST_PORT: u16 = 5000;
const MAX_FRAME: usize = 1024 * 1024;

pub fn wait_ready_blocking(path: &Path, sandbox_id: &str, timeout: Duration) -> anyhow::Result<()> {
    let deadline = std::time::Instant::now() + timeout;
    loop {
        if std::time::Instant::now() >= deadline {
            anyhow::bail!("guest readiness timed out");
        }
        if let Ok(mut stream) = BlockingStream::connect(path) {
            stream.set_read_timeout(Some(Duration::from_millis(500)))?;
            stream.set_write_timeout(Some(Duration::from_millis(500)))?;
            if stream
                .write_all(format!("CONNECT {GUEST_PORT}\n").as_bytes())
                .is_ok()
            {
                let mut line = Vec::new();
                while line.len() < 64 {
                    let mut byte = [0];
                    if stream.read_exact(&mut byte).is_err() {
                        break;
                    }
                    if byte[0] == b'\n' {
                        break;
                    }
                    line.push(byte[0]);
                }
                if line.starts_with(b"OK ") {
                    let mut header = [0_u8; 4];
                    if stream.read_exact(&mut header).is_ok() {
                        let size = u32::from_be_bytes(header) as usize;
                        if size > 0 && size <= MAX_FRAME {
                            let mut payload = vec![0_u8; size];
                            if stream.read_exact(&mut payload).is_ok() {
                                if let Ok(event) = GuestEvent::decode(payload.as_slice()) {
                                    if event.protocol_version == crate::PROTOCOL_VERSION
                                        && event.sandbox_id == sandbox_id
                                        && event.r#type == crate::rpc::GuestEventType::Ready as i32
                                    {
                                        return Ok(());
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
        std::thread::sleep(Duration::from_millis(100));
    }
}

pub async fn connect(path: &Path) -> anyhow::Result<UnixStream> {
    let mut stream = UnixStream::connect(path).await?;
    stream
        .write_all(format!("CONNECT {GUEST_PORT}\n").as_bytes())
        .await?;
    let mut line = Vec::new();
    loop {
        let byte = stream.read_u8().await?;
        if byte == b'\n' {
            break;
        }
        if line.len() >= 64 {
            anyhow::bail!("Firecracker vsock acknowledgment too long");
        }
        line.push(byte);
    }
    let acknowledgment = std::str::from_utf8(&line)?;
    if !acknowledgment.starts_with("OK ") || acknowledgment[3..].parse::<u32>().is_err() {
        anyhow::bail!("Firecracker vsock connection rejected");
    }
    Ok(stream)
}

pub async fn write_command(stream: &mut UnixStream, command: GuestCommand) -> anyhow::Result<()> {
    let payload = command.encode_to_vec();
    if payload.is_empty() || payload.len() > MAX_FRAME {
        anyhow::bail!("guest command exceeds frame limit");
    }
    stream
        .write_all(&(payload.len() as u32).to_be_bytes())
        .await?;
    stream.write_all(&payload).await?;
    Ok(())
}

pub async fn read_event(stream: &mut UnixStream) -> anyhow::Result<GuestEvent> {
    let size = stream.read_u32().await? as usize;
    if size == 0 || size > MAX_FRAME {
        anyhow::bail!("invalid guest frame size: {size}");
    }
    let mut payload = vec![0_u8; size];
    stream.read_exact(&mut payload).await?;
    let event = GuestEvent::decode(payload.as_slice())?;
    if event.protocol_version != crate::PROTOCOL_VERSION {
        anyhow::bail!("guest protocol version mismatch");
    }
    Ok(event)
}

pub async fn wait_ready(path: &Path, sandbox_id: &str, timeout: Duration) -> anyhow::Result<()> {
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        if tokio::time::Instant::now() >= deadline {
            anyhow::bail!("guest readiness timed out");
        }
        if let Ok(Ok(mut connection)) =
            tokio::time::timeout(Duration::from_secs(1), connect(path)).await
        {
            if let Ok(Ok(event)) =
                tokio::time::timeout(Duration::from_secs(1), read_event(&mut connection)).await
            {
                if event.sandbox_id == sandbox_id
                    && event.r#type == crate::rpc::GuestEventType::Ready as i32
                {
                    return Ok(());
                }
            }
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

pub async fn start_execution(
    path: &Path,
    sandbox_id: &str,
    execution_id: &str,
    command: String,
    input: Vec<u8>,
    env_vars: std::collections::HashMap<String, String>,
    deadline: tokio::time::Instant,
) -> anyhow::Result<(UnixStream, u64)> {
    let mut stream = connect(path).await?;
    let ready = read_event(&mut stream).await?;
    if ready.sandbox_id != sandbox_id || ready.r#type != crate::rpc::GuestEventType::Ready as i32 {
        anyhow::bail!("guest identity or readiness mismatch");
    }
    if ready.sequence == 0 || ready.sequence == u64::MAX {
        anyhow::bail!("invalid guest readiness sequence");
    }
    if tokio::time::Instant::now() >= deadline {
        anyhow::bail!("execution expired before Run");
    }
    write_command(
        &mut stream,
        GuestCommand {
            command: Some(guest_command::Command::Run(RunCommand {
                sandbox_id: sandbox_id.into(),
                execution_id: execution_id.into(),
                command,
                input,
                env_vars,
            })),
        },
    )
    .await?;
    // READY participates in the guest's connection-local sequence. Preserve it
    // so the caller can validate STARTED without renumbering public events.
    Ok((stream, ready.sequence))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn readiness_after_deadline_never_sends_run() {
        let dir = tempfile::tempdir_in("/tmp").unwrap();
        let path = dir.path().join("guest");
        let listener = tokio::net::UnixListener::bind(&path).unwrap();
        let server = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            while stream.read_u8().await.unwrap() != b'\n' {}
            stream.write_all(b"OK 5000\n").await.unwrap();
            tokio::time::sleep(Duration::from_millis(25)).await;
            let ready = GuestEvent {
                protocol_version: crate::PROTOCOL_VERSION.into(),
                sandbox_id: "sandbox".into(),
                sequence: 1,
                r#type: crate::rpc::GuestEventType::Ready as i32,
                ..Default::default()
            }
            .encode_to_vec();
            stream.write_u32(ready.len() as u32).await.unwrap();
            stream.write_all(&ready).await.unwrap();
            assert_eq!(stream.read(&mut [0]).await.unwrap(), 0);
        });
        assert!(start_execution(
            &path,
            "sandbox",
            "execution",
            "echo secret".into(),
            vec![],
            Default::default(),
            tokio::time::Instant::now() + Duration::from_millis(5)
        )
        .await
        .is_err());
        tokio::time::timeout(Duration::from_secs(1), server)
            .await
            .unwrap()
            .unwrap();
    }
}
