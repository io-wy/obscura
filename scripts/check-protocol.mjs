import { readFile } from "node:fs/promises";

const runtimeProto = await readFile("api/proto/agentruntime/v1/runtime.proto", "utf8");
for (const required of ["service DataPlane", "rpc Execute", "message AgentEvent"]) {
  if (!runtimeProto.includes(required)) throw new Error(`runtime.proto: missing ${required}`);
}
const guestProto = await readFile("api/proto/agentruntime/v1/guest.proto", "utf8");
for (const required of ["message GuestCommand", "message GuestEvent", "oneof command"]) {
  if (!guestProto.includes(required)) throw new Error(`guest.proto: missing ${required}`);
}

const files = [
  "shared/protocol/v1/run-command.json",
  "shared/protocol/v1/result-event.json",
];
for (const file of files) {
  const value = JSON.parse(await readFile(file, "utf8"));
  if (file.endsWith("run-command.json") && value.type !== "run") throw new Error(`${file}: expected run`);
  if (file.endsWith("result-event.json") && value.version !== "agentruntime.v1") throw new Error(`${file}: bad version`);
}
console.log(`validated runtime.proto, guest.proto, and ${files.length} protocol fixtures`);
