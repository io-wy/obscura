use std::path::PathBuf;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OverlayLayer {
    pub digest: String,
    pub path: PathBuf,
    pub read_only: bool,
}

#[derive(Debug, Clone, Default)]
pub struct OverlaybdStack {
    pub layers: Vec<OverlayLayer>,
}

impl OverlaybdStack {
    pub fn push(&mut self, layer: OverlayLayer) {
        self.layers.push(layer);
    }

    pub fn top_writable(&self) -> Option<&OverlayLayer> {
        self.layers.iter().rev().find(|layer| !layer.read_only)
    }
}
