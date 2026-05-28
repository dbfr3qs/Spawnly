import express from 'express';
import { v4 as uuidv4 } from 'uuid';
import {
  DefaultRequestHandler,
  InMemoryTaskStore,
  type AgentExecutor,
  type ExecutionEventBus,
  RequestContext,
} from '@a2a-js/sdk/server';
import {
  jsonRpcHandler,
  agentCardHandler,
  UserBuilder,
} from '@a2a-js/sdk/server/express';
import type { AgentCard, Message } from '@a2a-js/sdk';
import { configureProvider } from '@flue/runtime/app';
import {
  createFlueContext,
  InMemorySessionStore,
  resolveModel,
} from '@flue/runtime/internal';
import { local } from '@flue/runtime/node';
import { postEvent, instrumentFlue, promptTimeoutSignal } from '@agent-platform/sdk';

const agentId     = process.env.AGENT_ID      ?? 'unknown';
const registryUrl  = process.env.REGISTRY_URL  ?? 'http://registry:8080';
const aiProvider   = process.env.AI_PROVIDER   ?? 'anthropic';
const aiApiKey     = process.env.AI_API_KEY    ?? '';
const aiModel      = process.env.AI_MODEL      ?? 'anthropic/claude-sonnet-4-6';
const promptTimeoutMs = Number(process.env.PROMPT_TIMEOUT_MS ?? 120000);

configureProvider(aiProvider, { apiKey: aiApiKey });

const agentCard: AgentCard = {
  name: 'child-agent',
  description: 'A child agent that generates a random string using Claude',
  url: `http://${agentId}-svc:8080`,
  version: '1.0.0',
  protocolVersion: '0.2.6',
  capabilities: {
    streaming: false,
    pushNotifications: false,
    stateTransitionHistory: false,
  },
  defaultInputModes: ['text/plain'],
  defaultOutputModes: ['text/plain'],
  skills: [
    {
      id: 'generate-random-string',
      name: 'Generate Random String',
      description: 'Generates a random 8-character alphanumeric string',
      tags: ['random', 'string'],
    },
  ],
};

class ChildAgentExecutor implements AgentExecutor {
  async execute(requestContext: RequestContext, eventBus: ExecutionEventBus): Promise<void> {
    try {
      // Build a minimal FlueContext to use Flue's session.prompt()
      const sessionStore = new InMemorySessionStore();
      const ctx = createFlueContext({
        id: agentId,
        runId: uuidv4(),
        payload: {},
        env: process.env as Record<string, string>,
        agentConfig: {
          systemPrompt: '',
          skills: {},
          roles: {},
          model: undefined,
          resolveModel,
        },
        createDefaultEnv: async () => local().createSessionEnv({ id: agentId, cwd: process.cwd() }),
        defaultStore: sessionStore,
      });
      instrumentFlue(ctx, registryUrl, agentId);

      const harness = await ctx.init({ model: aiModel, sandbox: local() });
      const session = await harness.session();
      const response = await session.prompt(
        'Generate a random 8-character alphanumeric string. Reply with ONLY the string itself, no explanation.',
        { signal: promptTimeoutSignal(promptTimeoutMs) }
      );
      const text = response.text.trim();

      // Post event to registry
      await postEvent(registryUrl, agentId, 'child_result', { result: text });

      // Publish A2A reply
      const replyMessage: Message = {
        kind: 'message',
        messageId: uuidv4(),
        role: 'agent',
        parts: [{ kind: 'text', text }],
        contextId: requestContext.contextId,
        taskId: requestContext.taskId,
      };
      eventBus.publish(replyMessage);
      eventBus.finished();

      // Exit after response is sent
      setTimeout(() => process.exit(0), 500);
    } catch (err) {
      console.error('[child-agent] executor error:', err);
      await postEvent(registryUrl, agentId, 'agent_error', {
        error: err instanceof Error ? err.message : String(err),
        stack: err instanceof Error ? err.stack : undefined,
      });
      const errorMessage: Message = {
        kind: 'message',
        messageId: uuidv4(),
        role: 'agent',
        parts: [{ kind: 'text', text: `Error: ${String(err)}` }],
        contextId: requestContext.contextId,
        taskId: requestContext.taskId,
      };
      eventBus.publish(errorMessage);
      eventBus.finished();
      setTimeout(() => process.exit(1), 500);
    }
  }

  async cancelTask(_taskId: string, eventBus: ExecutionEventBus): Promise<void> {
    eventBus.finished();
  }
}

const taskStore = new InMemoryTaskStore();
const executor = new ChildAgentExecutor();
const requestHandler = new DefaultRequestHandler(agentCard, taskStore, executor);

const app = express();
app.use(express.json());

// Serve agent card at /.well-known/agent.json (also serve at standard path)
app.get('/.well-known/agent.json', agentCardHandler({ agentCardProvider: requestHandler }));
app.get('/.well-known/agent-card.json', agentCardHandler({ agentCardProvider: requestHandler }));

// JSON-RPC handler at root
app.post('/', jsonRpcHandler({ requestHandler, userBuilder: UserBuilder.noAuthentication }));

const PORT = 8080;
app.listen(PORT, () => {
  console.log(`[child-agent] ${agentId} listening on :${PORT}`);
});
