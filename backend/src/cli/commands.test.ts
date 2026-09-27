import { describe, expect, test } from 'bun:test';
import { runCLI } from './index';
import { CLIError, type JsonObject, type ClusterClient, type IO, type Resource, type ResourceType } from './types';

function harness() {
  const resources = new Map<string, Resource>(); const operations: string[] = [];
  const key = (kind: string, name: string) => `${kind}/${name}`;
  const io = { stdout: '', stderr: '', interactive: false, out(s: string) { this.stdout += s; }, err(s: string) { this.stderr += s; }, async input() { return 'Task contents'; } } satisfies IO & { stdout: string; stderr: string };
  const client: ClusterClient = {
    context: 'test', namespace: 'team', async raw() { throw new Error('Unexpected raw request'); },
    async request<T>(method: string, path: string): Promise<T> { operations.push(`${method} ${path}`); return { status: { allowed: true } } as T; },
    async list(type: ResourceType) { operations.push(`list ${type.kind}`); if (type.kind === 'InferenceProviderConfig') return [{ apiVersion: 'airunway.ai/v1alpha1', kind: type.kind, metadata: { name: 'vllm' }, status: { ready: true }, spec: { capabilities: { engines: [{ name: 'vllm', gpuSupport: true, servingModes: ['aggregated'] }] } } }]; return [...resources.values()].filter(r => r.kind === type.kind); },
    async get(type, name) { operations.push(`get ${type.kind}/${name}`); if (type.kind === 'AgentProviderConfig') return { apiVersion: 'airunway.ai/v1alpha1', kind: type.kind, metadata: { name }, status: { ready: true }, spec: { capabilities: { backend: 'container', modelBindingModes: ['deploymentRef', 'externalAPI', 'gatewayEndpoint'] } } }; const resource = resources.get(key(type.kind, name)); if (!resource) throw new CLIError('Not found', 1, 'HTTP_404'); return resource; },
    async create(resource, dry) { operations.push(`create ${resource.kind}/${resource.metadata.name} dry=${dry}`); const created = structuredClone(resource); created.metadata.uid = 'test-uid'; created.metadata.generation = 1; created.metadata.resourceVersion = '1'; if (!dry) resources.set(key(resource.kind, resource.metadata.name), created); return created; },
    async patch(type, name, patch: JsonObject) { operations.push(`patch ${type.kind}/${name}`); const existing = resources.get(key(type.kind, name))!; expect(patch.metadata.resourceVersion).toBe(existing.metadata.resourceVersion); const updated = { ...existing, spec: { ...existing.spec, ...patch.spec } }; resources.set(key(type.kind, name), updated); return updated; },
    async delete(type, name, namespace, uid) { operations.push(`delete ${type.kind}/${name}`); expect(uid).toBe('test-uid'); resources.delete(key(type.kind, name)); },
  };
  const config = { version: 1 as const, contexts: {} };
  return { io, client, resources, operations, run: (args: string[]) => runCLI(args, { io, client, config }) };
}
describe('command integration', () => {
  test('model create, list, get, mutable update and delete use the direct client', async () => {
    const h = harness(); expect(await h.run(['model', 'create', 'demo', '--id', 'hf://Qwen/Qwen3-8B', '--wait=false', '-o', 'json'])).toBe(0);
    expect(h.resources.get('ModelDeployment/demo')?.spec?.model.id).toBe('Qwen/Qwen3-8B'); expect(JSON.parse(h.io.stdout).metadata.name).toBe('demo');
    expect(await h.run(['model', 'list'])).toBe(0); expect(await h.run(['model', 'get', 'demo'])).toBe(0);
    expect(await h.run(['model', 'update', 'demo', '--replicas', '2', '--wait=false'])).toBe(0); expect(h.resources.get('ModelDeployment/demo')?.spec?.scaling.replicas).toBe(2);
    expect(await h.run(['model', 'delete', 'demo'])).toBe(0); expect(h.resources.has('ModelDeployment/demo')).toBe(false);
  });
  test('agent defaults bind an existing deployment and do not create a model', async () => {
    const h = harness(); h.resources.set('ModelDeployment/demo', { apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: 'demo', namespace: 'team', uid: 'test-uid' } });
    const code = await runCLI(['agent', 'create', 'assistant', '--prompt', 'Be helpful.', '--wait=false'], { io: h.io, client: h.client, config: { version: 1, contexts: { test: { namespaces: { team: { 'agent.framework': 'langgraph', 'agent.model-ref': 'demo' } } } } } });
    expect(code).toBe(0); expect(h.resources.get('AgentDeployment/assistant')?.spec?.model).toEqual({ deploymentRef: { name: 'demo' } }); expect(h.operations.filter(o => o.startsWith('create ModelDeployment'))).toEqual([]);
  });
  test('agent preflight reads model and gateway references in their declared namespace', async () => {
    for (const [flag, kind] of [['--model-ref', 'ModelDeployment'], ['--model-gateway', 'Gateway']]) {
      const h = harness();
      h.resources.set(`${kind}/shared-model`, { apiVersion: 'airunway.ai/v1alpha1', kind, metadata: { name: 'shared-model', namespace: 'shared' } });
      const reads: Array<string | undefined> = [];
      const get = h.client.get.bind(h.client);
      h.client.get = async (type, name, namespace) => { if (type.kind === kind) reads.push(namespace); return get(type, name, namespace); };
      const flags = flag === '--model-gateway' ? ['--model-id', 'chat'] : [];
      expect(await h.run(['agent', 'create', 'assistant', '--framework', 'langgraph', flag, 'shared/shared-model', '--prompt', 'Be helpful.', '--wait=false', ...flags])).toBe(0);
      expect(reads).toEqual(['shared']);
    }
  });
  test('all source references generate actual manifests without cluster access', async () => {
    for (const id of ['hf://Qwen/Qwen3-8B', 's3://bucket/models/', 'gs://bucket/models/', 'https://storage.example.com/model.gguf', 'oci://registry.example.com/models/qwen:v1', 'pvc://model-store/qwen/']) {
      const h = harness(); expect(await h.run(['model', 'create', 'demo', '--id', id, '--dry-run', 'client', '-o', 'json'])).toBe(0);
      expect(h.operations).toEqual([]); const resource = JSON.parse(h.io.stdout); expect(resource.metadata.name).toBe('demo'); expect(resource.spec.model.id).toBeTruthy();
      if (!id.startsWith('hf:') && !id.startsWith('pvc:')) expect(resource.spec.model.artifact.uri).toBe(id);
    }
  });
  test('agent once mode and external binding render the intended CR fields', async () => {
    const h = harness(); expect(await h.run(['agent', 'create', 'report', '--framework', 'crewai', '--model-url', 'https://api.example.com/v1', '--model-api', 'openai', '--model-id', 'qwen', '--mode', 'once', '--task-file', '-', '--dry-run', 'client', '-o', 'json'])).toBe(0);
    const resource = JSON.parse(h.io.stdout); expect(resource.spec.lifecycle).toBe('job'); expect(resource.spec.config.task).toBe('Task contents'); expect(resource.spec.model.externalAPI.modelName).toBe('qwen'); expect(h.operations).toEqual([]);
  });
  test('invalid command-specific options and dry-run values fail before a write', async () => {
    const h = harness(); expect(await h.run(['model', 'create', 'demo', '--id', 'Qwen/Qwen3-8B', '--dry-run', 'maybe'])).toBe(2); expect(await h.run(['model', 'get', 'demo', '--gpus', '2'])).toBe(2); expect(h.operations).toEqual([]);
  });
  test('server dry-run sends admission request without starting a readiness wait', async () => {
    const h = harness(); expect(await h.run(['model', 'create', 'demo', '--id', 'Qwen/Qwen3-8B', '--dry-run', 'server'])).toBe(0); expect(h.resources.size).toBe(0); expect(h.operations).toContain('create ModelDeployment/demo dry=true');
  });
});
