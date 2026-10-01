import { afterEach, beforeEach, describe, expect, test } from 'bun:test';
import app from '../hono-app';
import { kubernetesService } from '../services/kubernetes';
import { mockServiceMethod } from '../test/helpers';
import { defaultDynamoIntent, toDeploymentStatus, toModelDeploymentManifest, type DeploymentConfig, type ModelDeployment } from '@airunway/shared';

const restores: Array<() => void> = [];
const base = { name: 'qwen-tools', namespace: 'default', modelId: 'Qwen/Qwen3-0.6B', engine: 'vllm', provider: 'dynamo', toolCalling: true };
const automatic = { deploymentMode: 'intent', intent: defaultDynamoIntent() };
const request = (path: string, body: unknown) => app.request(`/api/deployments${path}`, {
  method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
});
const reconfigure = (payload: Record<string, unknown> = {}) => request('/default/qwen-tools/reconfigure', { resourceVersion: '1', ...payload });
let stored: ModelDeployment;
let writes: ModelDeployment[];
let creates: DeploymentConfig[];

beforeEach(() => {
  creates = [];
  writes = [];
  stored = { apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: 'qwen-tools', namespace: 'default', resourceVersion: '1' },
    spec: { model: { id: base.modelId }, engine: { type: 'vllm', toolCalling: true, toolCallParser: 'hermes', reasoningParser: 'basic' }, provider: { name: 'dynamo', overrides: automatic } } };
  restores.push(mockServiceMethod(kubernetesService, 'checkCRDExists', async () => true));
  restores.push(mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => { throw new Error('offline'); }));
  restores.push(mockServiceMethod(kubernetesService, 'getClusterGpuCapacity', async () => { throw new Error('offline'); }));
  restores.push(mockServiceMethod(kubernetesService, 'createDeployment', async (config: DeploymentConfig) => {
    creates.push(config);
    stored = toModelDeploymentManifest(config);
    stored.metadata.resourceVersion = '1';
  }));
  restores.push(mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => stored as unknown as Record<string, unknown>));
  restores.push(mockServiceMethod(kubernetesService, 'replaceDeployment', async (next: ModelDeployment) => { writes.push(next); stored = next; }));
});
afterEach(() => { restores.reverse().forEach(restore => restore()); restores.length = 0; });

describe('tool calling create and preview', () => {
  test.each([
    { mode: 'aggregated' }, { mode: 'disaggregated' }, { providerOverrides: automatic },
  ])('round trips typed fields through create, preview, stored manifest and status: %j', async layout => {
    const payload = { ...base, ...layout, toolCallParser: 'custom_parser', reasoningParser: 'basic' };
    const fields = { toolCalling: true, toolCallParser: 'custom_parser', reasoningParser: 'basic' };
    expect((await request('', payload)).status).toBe(201);
    expect(creates[0]).toMatchObject(fields);
    expect(stored.spec.engine).toMatchObject(fields);
    expect(toDeploymentStatus(stored)).toMatchObject(fields);
    const preview = await request('/preview', payload);
    expect(preview.status).toBe(200);
    const result = await preview.json() as { resources: Array<{ manifest: ModelDeployment }> };
    expect(result.resources[0].manifest.spec).toEqual(stored.spec);
    const manifest = await app.request('/api/deployments/qwen-tools/manifest?namespace=default');
    expect(manifest.status).toBe(200);
    expect((await manifest.json() as { resources: Array<{ manifest: ModelDeployment }> }).resources[0].manifest.spec.engine).toMatchObject(fields);
    if ('providerOverrides' in layout) {
      expect((await reconfigure()).status).toBe(200);
      expect(writes[0].spec.engine).toEqual(stored.spec.engine);
    }
  });
  test.each(['vllm', 'sglang', 'trtllm'])('keeps parser defaults omitted for %s in either configuration mode', async engine => {
    for (const providerOverrides of [undefined, automatic]) {
      const payload = { ...base, engine, providerOverrides };
      expect((await request('', payload)).status).toBe(201);
      expect(stored.spec.engine.toolCalling).toBe(true);
      expect(JSON.parse(JSON.stringify(stored.spec.engine))).not.toHaveProperty('toolCallParser');
      expect(JSON.parse(JSON.stringify(stored.spec.engine))).not.toHaveProperty('reasoningParser');
      expect((await request('/preview', payload)).status).toBe(200);
    }
  });
  test.each([
    { modelId: 'acme/model' }, { modelId: 'Qwen/Qwen2.5-7B' }, { modelId: 'Qwen/Qwen3.5' },
    { toolCallParser: 'auto' }, { reasoningParser: 'auto' }, { toolCallParser: '' }, { toolCallParser: null },
    { reasoningParser: 'UPPER' }, { toolCallParser: 'a'.repeat(65) }, { toolCalling: 'true' },
    { reasoningParser: 'none' }, { toolCallParser: 'none' }, { toolCalling: false, toolCallParser: 'hermes' }, { toolCalling: undefined, reasoningParser: 'basic' },
    { provider: 'kaito' }, { provider: 'vllm' }, { provider: 'llmd' }, { provider: 'kuberay' }, { provider: undefined },
    { engine: 'llamacpp' }, { engineArgs: { 'tool-call-parser': 'hermes' } },
    { engineExtraArgs: ['--dyn-reasoning-parser=qwen3'] }, { env: { DYN_CHAT_PROCESSOR: 'custom' } },
  ])('rejects invalid create and preview without writes: %j', async invalid => {
    for (const path of ['', '/preview']) expect((await request(path, { ...base, ...invalid })).status).toBe(400);
    expect(creates).toHaveLength(0);
  });
  test('still rejects ordinary runtime flags in typed intent when tool calling is enabled', async () => {
    for (const path of ['', '/preview']) {
      expect((await request(path, { ...base, providerOverrides: automatic, engineExtraArgs: ['--max-model-len=4096'] })).status).toBe(400);
      expect((await request(path, { ...base, providerOverrides: automatic, env: { OTHER: 'value' } })).status).toBe(400);
    }
  });
  test.each(['nvidia.com/v1alpha1', 'nvidia.com/v1beta1'])('checks graph-level and component parser conflicts for %s', async apiVersion => {
    const envKey = apiVersion.endsWith('v1alpha1') ? 'envs' : 'env';
    for (const spec of [
      { [envKey]: [{ name: 'DYN_TOOL_CALL_PARSER', value: 'hermes' }] },
      { components: [{ podTemplate: { spec: { containers: [{ args: ['--enable-auto-tool-choice'] }] } } }] },
    ]) {
      const providerOverrides = { deploymentMode: 'intent', intent: { ...defaultDynamoIntent(), overrides: {
        dgd: { apiVersion, kind: 'DynamoGraphDeployment', spec },
      } } };
      for (const path of ['', '/preview']) expect((await request(path, { ...base, providerOverrides })).status).toBe(400);
    }
  });
  test('allows explicit unknown-model parsers and leaves disabled manual escape hatches untouched', async () => {
    expect((await request('/preview', { ...base, modelId: 'acme/model', toolCallParser: 'hermes', reasoningParser: 'basic' })).status).toBe(200);
    expect((await request('/preview', { ...base, toolCalling: false, engineExtraArgs: ['--tool-call-parser=hermes'] })).status).toBe(200);
  });
});

describe('tool calling reconfigure API', () => {
  test('preserves omissions, replaces values, clears explicit parsers with null and clears all on disable', async () => {
    const original = structuredClone(stored);
    expect((await reconfigure()).status).toBe(200);
    expect(stored.spec.engine).toEqual(original.spec.engine);
    expect((await reconfigure({ toolCallParser: 'qwen3_coder', reasoningParser: 'qwen3' })).status).toBe(200);
    expect(stored.spec.engine).toMatchObject({ toolCallParser: 'qwen3_coder', reasoningParser: 'qwen3' });
    expect((await reconfigure({ toolCallParser: null })).status).toBe(200);
    expect(stored.spec.engine).not.toHaveProperty('toolCallParser');
    expect(stored.spec.engine.reasoningParser).toBe('qwen3');
    expect((await reconfigure({ reasoningParser: null })).status).toBe(200);
    expect(stored.spec.engine).not.toHaveProperty('reasoningParser');
    expect((await reconfigure({ toolCallParser: 'hermes', reasoningParser: 'basic' })).status).toBe(200);
    expect((await reconfigure({ toolCalling: false })).status).toBe(200);
    expect(stored.spec.engine).toEqual({ type: 'vllm', toolCalling: false });
  });
  test('accepts resolved Dynamo without rewriting the explicit provider', async () => {
    delete stored.spec.provider!.name;
    stored.status = { provider: { name: 'dynamo' } };
    expect((await reconfigure({ toolCalling: true, toolCallParser: 'custom' })).status).toBe(200);
    expect(stored.spec.provider).not.toHaveProperty('name');
    expect(stored.spec.engine.toolCallParser).toBe('custom');
  });
  test('rejects invalid effective settings, not just invalid sparse fields, without writes', async () => {
    expect((await reconfigure({ toolCallParser: null, modelId: 'acme/model' })).status).toBe(422);
    expect((await reconfigure({ toolCalling: false, reasoningParser: 'basic' })).status).toBe(400);
    expect((await reconfigure({ toolCallParser: 'auto' })).status).toBe(400);
    expect((await reconfigure({ reasoningParser: 'a'.repeat(65) })).status).toBe(400);
    stored.spec.engine = { type: 'vllm' };
    expect((await reconfigure({ toolCallParser: 'hermes' })).status).toBe(422);
    expect(writes).toHaveLength(0);
  });
});
