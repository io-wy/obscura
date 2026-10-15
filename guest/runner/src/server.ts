import { createServer, type Socket } from "node:net";
import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { once } from "node:events";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { GuestCommandSchema, GuestEventSchema, GuestEventType, type RunCommand } from "./gen/agentruntime/v1/guest_pb.js";
import { FrameDecoder, encodeFrame, PROTOCOL_VERSION } from "./protocol.js";
import { PiAdapter } from "./pi-adapter.js";

const sandboxId = process.env.OBSCURA_SANDBOX_ID ?? "";
if (!sandboxId) throw new Error("OBSCURA_SANDBOX_ID must be set by guest init");

export function listen(address = "127.0.0.1", port = 7000) {
  const server = createServer((socket) => handleConnection(socket, sandboxId));
  server.listen(port, address);
  return server;
}

function handleConnection(socket: Socket, id: string): void {
  const decoder = new FrameDecoder();
  const pi = new PiAdapter();
  let child: ChildProcessWithoutNullStreams | undefined;
  let executionId = "";
  let sequence = 0n;
  let writeQueue = Promise.resolve();
  async function emit(type: GuestEventType, payload: Uint8Array<ArrayBufferLike> = new Uint8Array(0), error = "") {
    const frame = encodeFrame(toBinary(GuestEventSchema, create(GuestEventSchema, {
      protocolVersion: PROTOCOL_VERSION, sandboxId: id, executionId,
      sequence: ++sequence, type, payload: new Uint8Array(payload), error,
    })));
    writeQueue = writeQueue.then(async () => {
      if (!socket.write(frame)) await once(socket, "drain");
    });
    await writeQueue;
  }
  void emit(GuestEventType.READY).catch(() => socket.destroy());
  socket.on("close", () => { child?.kill("SIGTERM"); pi.cancel(); });
  socket.on("data", (chunk: Buffer) => {
    let frames: Uint8Array[];
    try { frames = decoder.push(chunk); } catch { socket.destroy(); return; }
    for (const frame of frames) {
      try {
        const message = fromBinary(GuestCommandSchema, frame);
        if (message.command.case === "cancel") {
          if (message.command.value.executionId === executionId) { child?.kill("SIGTERM"); pi.cancel(); }
        } else if (message.command.case === "run") {
          if (executionId || message.command.value.sandboxId !== id || !message.command.value.executionId) { socket.destroy(); return; }
          executionId = message.command.value.executionId;
          void run(message.command.value).catch(async (error: unknown) => {
            await emit(GuestEventType.ERROR, new Uint8Array(0), String(error)).catch(() => socket.destroy());
          });
        } else {
          socket.destroy();
          return;
        }
      } catch { socket.destroy(); return; }
    }
  });

  async function run(command: RunCommand): Promise<void> {
    await emit(GuestEventType.STARTED);
    if (command.command === "pi") {
      await pi.run(command, async (payload) => emit(GuestEventType.STDOUT, payload));
      await emit(GuestEventType.RESULT);
      return;
    }
    child = spawn("/bin/sh", ["-c", command.command], { stdio: "pipe", env: { ...process.env, ...command.envVars } });
    child.stdin.end(command.input);
    child.stdout.on("data", (chunk: Buffer) => {
      for (let offset = 0; offset < chunk.byteLength; offset += 65536) {
        void emit(GuestEventType.STDOUT, chunk.subarray(offset, offset + 65536)).catch(() => child?.kill());
      }
    });
    child.stderr.on("data", (chunk: Buffer) => {
      for (let offset = 0; offset < chunk.byteLength; offset += 65536) {
        void emit(GuestEventType.STDERR, chunk.subarray(offset, offset + 65536)).catch(() => child?.kill());
      }
    });
    const [code] = await once(child, "close") as [number | null];
    await writeQueue;
    child = undefined;
    if (code !== 0) throw new Error(`command exited with status ${code}`);
    await emit(GuestEventType.RESULT);
  }
}

if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href) {
  listen();
}
