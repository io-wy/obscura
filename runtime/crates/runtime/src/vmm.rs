use crate::model::{LaunchSpec, SandboxHandle};
use crate::provider::{FirecrackerProvider, RuntimeError};

pub trait Vmm: Send {
    fn start(&mut self, spec: &LaunchSpec) -> Result<SandboxHandle, RuntimeError>;
    fn stop(&mut self, handle: &SandboxHandle) -> Result<(), RuntimeError>;
    fn is_running(&mut self, handle: &SandboxHandle) -> Result<bool, RuntimeError>;
}

impl<T: FirecrackerProvider + Send> Vmm for T {
    fn start(&mut self, spec: &LaunchSpec) -> Result<SandboxHandle, RuntimeError> {
        self.launch(spec)
    }
    fn is_running(&mut self, handle: &SandboxHandle) -> Result<bool, RuntimeError> {
        FirecrackerProvider::is_running(self, handle)
    }
    fn stop(&mut self, handle: &SandboxHandle) -> Result<(), RuntimeError> {
        FirecrackerProvider::stop(self, handle)
    }
}
