use crate::sandbox::Sandbox;
use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

#[derive(Clone, Default)]
pub struct WarmVmPool {
    ready: Arc<Mutex<VecDeque<Sandbox>>>,
}

impl WarmVmPool {
    pub fn push(&self, sandbox: Sandbox) {
        self.ready
            .lock()
            .expect("warm pool lock poisoned")
            .push_back(sandbox);
    }
    pub fn take(&self) -> Option<Sandbox> {
        self.ready
            .lock()
            .expect("warm pool lock poisoned")
            .pop_front()
    }
    pub fn len(&self) -> usize {
        self.ready.lock().expect("warm pool lock poisoned").len()
    }
}
