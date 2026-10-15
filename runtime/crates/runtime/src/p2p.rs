use async_trait::async_trait;
use std::path::PathBuf;

#[async_trait]
pub trait P2pTransport: Send + Sync {
    async fn publish(&self, digest: &str, path: PathBuf) -> anyhow::Result<()>;
    async fn fetch(&self, digest: &str, destination: PathBuf) -> anyhow::Result<()>;
}

pub struct DisabledP2p;

#[async_trait]
impl P2pTransport for DisabledP2p {
    async fn publish(&self, _digest: &str, _path: PathBuf) -> anyhow::Result<()> {
        anyhow::bail!("p2p transport is disabled")
    }
    async fn fetch(&self, _digest: &str, _destination: PathBuf) -> anyhow::Result<()> {
        anyhow::bail!("p2p transport is disabled")
    }
}
