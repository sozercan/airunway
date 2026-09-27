import { ApiException } from '@kubernetes/client-node';
import { afterEach, describe, expect, test } from 'bun:test';
import app from '../hono-app';
import { kubernetesService } from '../services/kubernetes';
import { mockServiceMethod } from '../test/helpers';
import {
  defaultDynamoIntent, toDeploymentStatus, toModelDeploymentManifest,
  type DeploymentConfig, type DynamoIntentOverrides, type ModelDeployment,
} from '@airunway/shared';

const restores: Array<() => void> = [];
afterEach(() => { restores.reverse().forEach(restore => restore()); restores.length = 0; });
const request = (path: string, body: unknown) => app.request(`/api/deployments${path}`, {
  method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
});
const body = () => ({ name: 'qwen-auto', namespace: 'default', modelId: 'Qwen/Qwen3-0.6B', engine: 'vllm', provider: 'dynamo',
  providerOverrides: { deploymentMode: 'intent', intent: defaultDynamoIntent() } });
const current = (): ModelDeployment => ({ apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment',
  metadata: { name: 'qwen-auto', namespace: 'default', resourceVersion: '1', annotations: { keep: 'yes' } },
  spec: { model: { id: 'Qwen/Qwen3-0.6B', source: 'huggingface' }, engine: { type: 'vllm' }, provider: { name: 'dynamo', overrides: body().providerOverrides } } });

function nativeOverrides(apiVersion: 'nvidia.com/v1alpha1' | 'nvidia.com/v1beta1' = 'nvidia.com/v1beta1'): DynamoIntentOverrides {
  const args = ['--dyn-tool-call-parser', 'hermes'];
  return {
    profilingJob: {
      activeDeadlineSeconds: 1800,
      ...JSON.parse('{"__proto__":{"futureField":[1,null,"unchanged"]}}'),
      template: { spec: { containers: [{ name: 'profiler', args: ['--verbose'] }], restartPolicy: 'Never' } },
    },
    dgd: {
      apiVersion, kind: 'DynamoGraphDeployment', metadata: { annotations: { 'example.com/note': 'preserve me' } },
      spec: apiVersion === 'nvidia.com/v1beta1'
        ? { components: [{ name: 'VllmDecodeWorker', podTemplate: { spec: { containers: [{
          name: 'main', $patch: { args: 'append' }, args,
        }] } } }] }
        : { services: { VllmDecodeWorker: { extraPodSpec: { mainContainer: { args } } } } },
    },
  };
}

describe('Dynamo automatic API', () => {
  test('preview omits manual sizing and layout', async () => {
    restores.push(mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => { throw new Error('offline'); }));
    const response = await request('/preview', body());
    expect(response.status).toBe(200);
    const result = await response.json() as { resources: Array<{ manifest: ModelDeployment }> };
    const spec = result.resources[0].manifest.spec;
    expect(spec.provider.overrides).toEqual(body().providerOverrides);
    expect(spec.resources).toBeUndefined();
    expect(spec.scaling).toBeUndefined();
    expect(spec.serving).toBeUndefined();
  });

  test.each(['nvidia.com/v1alpha1', 'nvidia.com/v1beta1'] as const)(
    'preserves %s native overrides through create, preview, manifest and reconfigure', async apiVersion => {
      const intent = { ...defaultDynamoIntent(), overrides: nativeOverrides(apiVersion) };
      const payload = { ...body(), providerOverrides: { deploymentMode: 'intent', intent } };
      const creates: DeploymentConfig[] = [];
      const writes: ModelDeployment[] = [];
      let stored = current();
      restores.push(mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => { throw new Error('offline'); }));
      restores.push(mockServiceMethod(kubernetesService, 'getClusterGpuCapacity', async () => { throw new Error('offline'); }));
      restores.push(mockServiceMethod(kubernetesService, 'createDeployment', async (config: DeploymentConfig) => {
        creates.push(config);
        stored = toModelDeploymentManifest(config);
        stored.metadata.resourceVersion = '1';
      }));
      restores.push(mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => stored as unknown as Record<string, unknown>));
      restores.push(mockServiceMethod(kubernetesService, 'replaceDeployment', async (md: ModelDeployment) => { writes.push(md); }));

      expect((await request('', payload)).status).toBe(201);
      expect(creates).toHaveLength(1);
      expect(creates[0].providerOverrides).toEqual(payload.providerOverrides);
      const preview = await request('/preview', payload);
      expect(preview.status).toBe(200);
      const previewBody = await preview.json() as { resources: Array<{ manifest: ModelDeployment }> };
      expect(previewBody.resources[0].manifest.spec.provider?.overrides).toEqual(payload.providerOverrides);
      const manifest = await app.request('/api/deployments/qwen-auto/manifest?namespace=default');
      expect(manifest.status).toBe(200);
      const manifestBody = await manifest.json() as { resources: Array<{ manifest: ModelDeployment }> };
      expect(manifestBody.resources[0].manifest.spec.provider?.overrides).toEqual(payload.providerOverrides);

      expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: '1' })).status).toBe(200);
      expect(writes[0].spec.provider?.overrides?.intent).toEqual(intent);
      const replacement = { ...intent, hardware: { totalGpus: 2 } };
      expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: '1', intent: replacement })).status).toBe(200);
      expect(writes).toHaveLength(2);
      expect(writes[1].spec.provider?.overrides?.intent).toEqual(replacement);
    },
  );

  test('rejects invalid override shapes and recursive policy violations before writes', async () => {
    let writes = 0;
    restores.push(mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => { throw new Error('offline'); }));
    restores.push(mockServiceMethod(kubernetesService, 'getClusterGpuCapacity', async () => { throw new Error('offline'); }));
    restores.push(mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => current() as unknown as Record<string, unknown>));
    restores.push(mockServiceMethod(kubernetesService, 'createDeployment', async () => { writes++; }));
    restores.push(mockServiceMethod(kubernetesService, 'replaceDeployment', async () => { writes++; }));
    const dgd = nativeOverrides().dgd!;
    const invalidOverrides: unknown[] = [
      { unknown: {} }, { dgd: {} }, { dgd: { apiVersion: dgd.apiVersion, kind: dgd.kind } },
      { dgd: { ...dgd, apiVersion: 'nvidia.com/v1' } }, { dgd: { ...dgd, kind: 'Job' } },
      { dgd: { ...dgd, status: {} } },
      { profilingJob: JSON.parse('{"__proto__":{"nested":[{"HOSTNETWORK":true}]}}') },
    ];
    for (const value of [null, [], 'raw', 1, false]) {
      invalidOverrides.push(value, { profilingJob: value }, { dgd: value },
        { dgd: { ...dgd, spec: value } }, { dgd: { ...dgd, metadata: value } });
    }
    for (const key of ['securityContext', 'serviceAccountName', 'serviceAccount', 'hostNetwork', 'hostPID', 'hostIPC',
      'automountServiceAccountToken', 'nodeName', 'priorityClassName', 'runtimeClassName', 'resources', 'replicas']) {
      const nested = { nested: [[{ [key.toUpperCase()]: null }]] };
      invalidOverrides.push({ profilingJob: nested }, { dgd: { ...dgd, spec: nested } });
    }
    for (const overrides of invalidOverrides) {
      const intent = { ...defaultDynamoIntent(), overrides };
      for (const path of ['', '/preview']) {
        expect((await request(path, { ...body(), providerOverrides: { deploymentMode: 'intent', intent } })).status).toBe(400);
      }
      expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: '1', intent })).status).toBe(400);
    }
    expect(writes).toBe(0);
  });

  test('rejects conflicting sizing, unknown fields, unsupported modes and search methods', async () => {
    for (const change of [
      { resources: { gpu: 1 } }, { replicas: 1 }, { scaling: { replicas: 1 } }, { podTemplate: { labels: { foo: 'bar' } } }, { nodeSelector: { gpu: 'h100' } }, { mode: 'disaggregated' }, { prefillGpus: 1 },
      { provider: 'vllm' }, { env: { FOO: 'bar' } }, { hfTokenSecret: 'other-secret' }, { storage: { volumes: [{ name: 'cache', claimName: 'cache' }] } }, { enforceEager: true }, { imageRef: 'custom/image' },
      { engineArgs: { 'max-model-len': 4096 } }, { engineExtraArgs: ['--verbose'] },
      { contextLength: 4096 }, { maxModelLen: 4096 }, { trustRemoteCode: true },
      { providerOverrides: { ...body().providerOverrides, spec: {} } },
      { providerOverrides: { ...body().providerOverrides, intent: { ...defaultDynamoIntent(), model: 'other-model' } } },
      { providerOverrides: { ...body().providerOverrides, intent: { ...defaultDynamoIntent(), backend: 'vllm' } } },
      { providerOverrides: { ...body().providerOverrides, intent: { ...defaultDynamoIntent(), autoApply: false } } },
      { providerOverrides: { ...body().providerOverrides, autoApply: false } },
      { providerOverrides: { deploymentMode: 'manual', intent: defaultDynamoIntent() } },
      { providerOverrides: { ...body().providerOverrides, intent: { ...defaultDynamoIntent(), searchStrategy: 'thorough' } } },
    ]) {
      for (const path of ['', '/preview']) {
        expect((await request(path, { ...body(), providerOverrides: {
          deploymentMode: 'intent', intent: { ...defaultDynamoIntent(), overrides: nativeOverrides() },
        }, ...change })).status).toBe(400);
      }
    }
  });

  test('reconfigure sends exactly one conditional update with fresh attempt and preserves metadata', async () => {
    const writes: ModelDeployment[] = [];
    restores.push(mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => current() as unknown as Record<string, unknown>));
    restores.push(mockServiceMethod(kubernetesService, 'replaceDeployment', async (md: ModelDeployment) => { writes.push(md); }));
    const response = await request('/default/qwen-auto/reconfigure', { resourceVersion: '1', modelId: 'Qwen/Qwen3-8B', engine: 'sglang', intent: { hardware: { totalGpus: 2 } } });
    expect(response.status).toBe(200);
    expect(writes).toHaveLength(1);
    expect(writes[0].metadata.resourceVersion).toBe('1');
    expect(writes[0].metadata.annotations?.keep).toBe('yes');
    expect(writes[0].metadata.annotations?.['airunway.ai/dynamo-attempt']).toMatch(/^[a-f0-9-]{36}$/);
    expect(writes[0].spec.engine.type).toBe('sglang');
    expect(writes[0].spec.model.id).toBe('Qwen/Qwen3-8B');
    expect(writes[0].spec.model.source).toBe('huggingface');
  });

  test('rejects stale clients before write, and surfaces API-server conflicts without retry', async () => {
    let writes = 0;
    restores.push(mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => current() as unknown as Record<string, unknown>));
    restores.push(mockServiceMethod(kubernetesService, 'replaceDeployment', async () => { writes++; throw { statusCode: 409, message: 'Conflict' }; }));
    expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: 'old' })).status).toBe(409);
    expect(writes).toBe(0);
    expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: '1' })).status).toBe(409);
    expect(writes).toBe(1);
  });

  test('preserves real-client permission denials and resourceVersion conflicts', async () => {
    restores.push(mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => { throw new Error('offline'); }));
    restores.push(mockServiceMethod(kubernetesService, 'getClusterGpuCapacity', async () => { throw new Error('offline'); }));
    restores.push(mockServiceMethod(kubernetesService, 'createDeployment', async () => {
      throw new ApiException(403, 'Unknown API Status Code!', JSON.stringify({ code: 403, message: 'Namespace write denied' }), {});
    }));
    expect((await request('', body())).status).toBe(403);
    let writes = 0;
    restores.push(mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => current() as unknown as Record<string, unknown>));
    restores.push(mockServiceMethod(kubernetesService, 'replaceDeployment', async () => {
      writes++;
      throw new ApiException(409, 'Unknown API Status Code!', JSON.stringify({ code: 409, message: 'Resource version conflict' }), {});
    }));
    expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: '1' })).status).toBe(409);
    expect(writes).toBe(1);
  });

  test('rejects manual reconfiguration and arbitrary spec replacement', async () => {
    const md = current(); md.spec.provider!.overrides = { deploymentMode: 'manual' };
    restores.push(mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => md as unknown as Record<string, unknown>));
    expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: '1' })).status).toBe(422);
    expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: '1', spec: {} })).status).toBe(400);
    expect((await request('/default/qwen-auto/reconfigure', {})).status).toBe(400);
  });
  test('chat discovers and calls the published Service in its actual namespace', async () => {
    const md = current();
    md.status = { phase: 'Running', endpoint: { service: 'chosen-frontend', port: 9000 }, replicas: { desired: 1, ready: 1, available: 1 }, provider: { workloadRef: { kind: 'DynamoGraphDeployment', name: 'chosen', namespace: 'serving' } } };
    const calls: unknown[][] = [];
    restores.push(mockServiceMethod(kubernetesService, 'getDeployment', async () => toDeploymentStatus(md)));
    restores.push(mockServiceMethod(kubernetesService, 'proxyServiceGet', async (...args: unknown[]) => { calls.push(args); return JSON.stringify({ data: [{ id: 'actual-model' }] }); }));
    restores.push(mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
      calls.push(args); return new Response('data: [DONE]\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
    }));
    const response = await request('/default/qwen-auto/chat', { messages: [{ role: 'user', content: 'Hello' }] });
    expect(response.status).toBe(200);
    await response.text();
    expect(calls).toHaveLength(2);
    expect(calls.every(call => call[0] === 'chosen-frontend' && call[1] === 'serving' && call[2] === 9000)).toBe(true);
    expect(calls[1][4]).toMatchObject({ model: 'actual-model' });
  });

});
