import assert from "node:assert/strict";
import test from "node:test";

import { FrameDecoder, encodeFrame } from "../src/protocol.ts";

test("decodes split and coalesced protobuf payload frames", () => {
  const first = encodeFrame(new Uint8Array([1, 2]));
  const second = encodeFrame(new Uint8Array([3, 4, 5]));
  const decoder = new FrameDecoder();

  assert.deepEqual(decoder.push(first.slice(0, 3)), []);
  assert.deepEqual(decoder.push(new Uint8Array([...first.slice(3), ...second])), [
    new Uint8Array([1, 2]),
    new Uint8Array([3, 4, 5]),
  ]);
});

test("rejects oversized frames", () => {
  const decoder = new FrameDecoder(2);
  assert.throws(() => decoder.push(encodeFrame(new Uint8Array([1, 2, 3]), 10)), /too large/);
});
