pub mod firecracker;
pub mod frame;
pub mod guest;
pub mod image;
pub mod model;
pub mod network;
pub mod orchestrator;
pub mod overlaybd;
pub mod p2p;
pub mod pool;
pub mod provider;
pub mod sandbox;
pub mod snapshot;
pub mod storage;
pub mod vmm;

pub mod rpc {
    tonic::include_proto!("agentruntime.v1");
}
pub mod service;

pub const PROTOCOL_VERSION: &str = "agentruntime.v1";
