use thiserror::Error;

pub const DEFAULT_MAX_FRAME_SIZE: usize = 1024 * 1024;

pub struct Decoder {
    pending: Vec<u8>,
    max_size: usize,
}

#[derive(Debug, Error, PartialEq, Eq)]
pub enum FrameError {
    #[error("frame payload is empty")]
    Empty,
    #[error("frame payload is too large: {size} > {max}")]
    TooLarge { size: usize, max: usize },
    #[error("frame is truncated: expected {expected} bytes, got {actual}")]
    Truncated { expected: usize, actual: usize },
}

impl Decoder {
    pub fn new(max_size: usize) -> Self {
        Self {
            pending: Vec::new(),
            max_size,
        }
    }

    pub fn push(&mut self, chunk: &[u8]) -> Result<Vec<Vec<u8>>, FrameError> {
        self.pending.extend_from_slice(chunk);
        let mut frames = Vec::new();
        loop {
            if self.pending.len() < 4 {
                break;
            }
            let size = u32::from_be_bytes([
                self.pending[0],
                self.pending[1],
                self.pending[2],
                self.pending[3],
            ]) as usize;
            if size == 0 {
                return Err(FrameError::Empty);
            }
            if size > self.max_size {
                return Err(FrameError::TooLarge {
                    size,
                    max: self.max_size,
                });
            }
            let expected = 4 + size;
            if self.pending.len() < expected {
                break;
            }
            frames.push(self.pending[4..expected].to_vec());
            self.pending.drain(..expected);
        }
        Ok(frames)
    }
}

pub fn encode(payload: &[u8], max_size: usize) -> Result<Vec<u8>, FrameError> {
    if payload.is_empty() {
        return Err(FrameError::Empty);
    }
    if payload.len() > max_size || payload.len() > u32::MAX as usize {
        return Err(FrameError::TooLarge {
            size: payload.len(),
            max: max_size,
        });
    }
    let mut frame = Vec::with_capacity(4 + payload.len());
    frame.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    frame.extend_from_slice(payload);
    Ok(frame)
}

pub fn decode(frame: &[u8], max_size: usize) -> Result<&[u8], FrameError> {
    if frame.len() < 4 {
        return Err(FrameError::Truncated {
            expected: 4,
            actual: frame.len(),
        });
    }
    let size = u32::from_be_bytes([frame[0], frame[1], frame[2], frame[3]]) as usize;
    if size == 0 {
        return Err(FrameError::Empty);
    }
    if size > max_size {
        return Err(FrameError::TooLarge {
            size,
            max: max_size,
        });
    }
    let expected = 4 + size;
    if frame.len() < expected {
        return Err(FrameError::Truncated {
            expected,
            actual: frame.len(),
        });
    }
    Ok(&frame[4..expected])
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn round_trips_a_payload() {
        let encoded = encode(b"protobuf-bytes", DEFAULT_MAX_FRAME_SIZE).unwrap();
        assert_eq!(
            decode(&encoded, DEFAULT_MAX_FRAME_SIZE).unwrap(),
            b"protobuf-bytes"
        );
    }

    #[test]
    fn rejects_oversized_payloads() {
        let error = encode(b"12345", 4).unwrap_err();
        assert_eq!(error, FrameError::TooLarge { size: 5, max: 4 });
    }

    #[test]
    fn rejects_truncated_frames() {
        let error = decode(&[0, 0, 0, 5, 1], DEFAULT_MAX_FRAME_SIZE).unwrap_err();
        assert_eq!(
            error,
            FrameError::Truncated {
                expected: 9,
                actual: 5
            }
        );
    }

    #[test]
    fn decoder_handles_split_and_coalesced_frames() {
        let first = encode(b"one", DEFAULT_MAX_FRAME_SIZE).unwrap();
        let second = encode(b"two", DEFAULT_MAX_FRAME_SIZE).unwrap();
        let mut decoder = Decoder::new(DEFAULT_MAX_FRAME_SIZE);
        assert!(decoder.push(&first[..2]).unwrap().is_empty());
        let mut combined = first[2..].to_vec();
        combined.extend_from_slice(&second);
        assert_eq!(
            decoder.push(&combined).unwrap(),
            vec![b"one".to_vec(), b"two".to_vec()]
        );
    }
}
