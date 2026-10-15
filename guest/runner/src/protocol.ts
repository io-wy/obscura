export const PROTOCOL_VERSION = "agentruntime.v1" as const;
export const DEFAULT_MAX_FRAME_SIZE = 1024 * 1024;

export type GuestCommand =
  | { kind: "run"; sandboxId: string; executionId: string; command: string; input?: Uint8Array }
  | { kind: "cancel"; sandboxId: string; executionId: string }
  | { kind: "ping"; sandboxId: string };

export type GuestEvent = {
  protocolVersion: typeof PROTOCOL_VERSION;
  sandboxId: string;
  executionId: string;
  sequence: number;
  kind: "ready" | "started" | "stdout" | "stderr" | "result" | "error" | "pong";
  payload?: Uint8Array;
  error?: string;
};

export function encodeFrame(payload: Uint8Array, maxSize = DEFAULT_MAX_FRAME_SIZE): Uint8Array {
  if (payload.byteLength === 0) throw new Error("frame payload is empty");
  if (payload.byteLength > maxSize) throw new Error("frame payload is too large");
  const frame = new Uint8Array(4 + payload.byteLength);
  new DataView(frame.buffer).setUint32(0, payload.byteLength, false);
  frame.set(payload, 4);
  return frame;
}

export function decodeFrame(frame: Uint8Array, maxSize = DEFAULT_MAX_FRAME_SIZE): Uint8Array {
  if (frame.byteLength < 4) throw new Error("frame is truncated");
  const size = new DataView(frame.buffer, frame.byteOffset, frame.byteLength).getUint32(0, false);
  if (size === 0) throw new Error("frame payload is empty");
  if (size > maxSize) throw new Error("frame payload is too large");
  if (frame.byteLength < size + 4) throw new Error("frame is truncated");
  return frame.slice(4, size + 4);
}

export class FrameDecoder {
  private pending = new Uint8Array(0);
  private readonly maxSize: number;

  constructor(maxSize = DEFAULT_MAX_FRAME_SIZE) {
    this.maxSize = maxSize;
  }

  push(chunk: Uint8Array): Uint8Array[] {
    const merged = new Uint8Array(this.pending.byteLength + chunk.byteLength);
    merged.set(this.pending);
    merged.set(chunk, this.pending.byteLength);
    this.pending = merged;

    const frames: Uint8Array[] = [];
    while (this.pending.byteLength >= 4) {
      const size = new DataView(this.pending.buffer, this.pending.byteOffset, this.pending.byteLength).getUint32(0, false);
      if (size === 0) throw new Error("frame payload is empty");
      if (size > this.maxSize) throw new Error("frame payload is too large");
      if (this.pending.byteLength < size + 4) break;
      frames.push(this.pending.slice(4, size + 4));
      this.pending = this.pending.slice(size + 4);
    }
    return frames;
  }
}
