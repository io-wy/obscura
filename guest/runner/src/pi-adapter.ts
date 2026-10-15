import { Agent } from "@mariozechner/pi-agent-core";
import { getModels, getProviders } from "@mariozechner/pi-ai";
import type { RunCommand } from "./gen/agentruntime/v1/guest_pb.js";

export class PiAdapter {
  private agent: Agent | undefined;

  async run(command: RunCommand, emit: (payload: Uint8Array) => Promise<void>): Promise<void> {
    const provider = command.envVars.OBSCURA_PI_PROVIDER;
    const modelId = command.envVars.OBSCURA_PI_MODEL;
    if (!provider || !modelId) throw new Error("OBSCURA_PI_PROVIDER and OBSCURA_PI_MODEL are required");
    const knownProvider = getProviders().find((name) => name === provider);
    if (!knownProvider) throw new Error(`unknown pi provider: ${provider}`);
    const model = getModels(knownProvider).find((candidate) => candidate.id === modelId);
    if (!model) throw new Error(`unknown pi model: ${modelId}`);
    const agent = new Agent({
      initialState: { systemPrompt: "You are a sandboxed agent.", model },
      getApiKey: (name) => command.envVars[`${name.toUpperCase().replaceAll("-", "_")}_API_KEY`],
    });
    this.agent = agent;
    const unsubscribe = agent.subscribe(async (event) => {
      if (event.type === "message_update" && event.assistantMessageEvent.type === "text_delta") {
        await emit(new TextEncoder().encode(event.assistantMessageEvent.delta));
      }
    });
    try {
      await agent.prompt(new TextDecoder().decode(command.input));
    } finally {
      unsubscribe();
      this.agent = undefined;
    }
  }

  cancel(): void { this.agent?.abort(); }
}
