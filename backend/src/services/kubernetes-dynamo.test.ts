import { ApiException } from '@kubernetes/client-node';
import { afterEach, describe, expect, test } from 'bun:test';
import { kubernetesService } from './kubernetes';
import { type ModelDeployment, defaultDynamoIntent } from '@airunway/shared';

interface ApiCall { namespace?: string; labelSelector?: string; name?: string; body?: ModelDeployment; group?: string }
type StubApi = Record<string, (arg: ApiCall) => Promise<unknown>>;
const service = kubernetesService as unknown as { coreV1Api: StubApi; customObjectsApi: StubApi };
const restores: Array<() => void> = [];
afterEach(() => { restores.reverse().forEach(restore => restore()); restores.length = 0; });

describe('Dynamo serving identity', () => {
  test('uses the actual generated name and namespace for pod discovery', async () => {
    const old = service.coreV1Api;
    restores.push(() => { service.coreV1Api = old; });
    const calls: ApiCall[] = [];
    service.coreV1Api = { listNamespacedPod: async args => {
      calls.push(args);
      return { items: args.labelSelector === 'nvidia.com/dynamo-graph-deployment-name=chosen-name'
        ? [{ metadata: { name: 'actual-worker' }, status: { phase: 'Running', containerStatuses: [{ ready: true, restartCount: 0 }] } }] : [] };
    } };
    const pods = await kubernetesService.getDeploymentPods('model-name', 'model-space', { kind: 'DynamoGraphDeployment', name: 'chosen-name', namespace: 'serving-space', uid: 'uid' });
    expect(pods.map(pod => pod.name)).toEqual(['actual-worker']);
    expect(calls.every(call => call.namespace === 'serving-space')).toBe(true);
    expect(calls.some(call => call.labelSelector?.includes('model-name'))).toBe(false);
  });

  test('leaves non-Dynamo pod resolution unchanged', async () => {
    const old = service.coreV1Api;
    restores.push(() => { service.coreV1Api = old; });
    const calls: ApiCall[] = [];
    service.coreV1Api = { listNamespacedPod: async args => { calls.push(args); return { items: [] }; } };
    await kubernetesService.getDeploymentPods('original', 'models', { kind: 'RayService', name: 'other', namespace: 'elsewhere' });
    expect(calls.every(call => call.namespace === 'models')).toBe(true);
    expect(calls.some(call => call.labelSelector === 'app.kubernetes.io/instance=original')).toBe(true);
  });

  test('treats a generated-client 404 as a missing deployment', async () => {
    const old = service.customObjectsApi;
    restores.push(() => { service.customObjectsApi = old; });
    let reads = 0;
    service.customObjectsApi = { getNamespacedCustomObject: async () => {
      reads++;
      throw new ApiException(404, 'Unknown API Status Code!', JSON.stringify({ code: 404, message: 'Not found' }), {});
    } };
    expect(await kubernetesService.getDeploymentManifest('missing', 'models')).toBeNull();
    expect(reads).toBe(1);
  });

  test.each([401, 403, 503])('strict manifest reads propagate generated-client %s failures while best-effort reads stay nullable', async status => {
    const old = service.customObjectsApi;
    restores.push(() => { service.customObjectsApi = old; });
    const error = new ApiException(status, 'Unknown API Status Code!', JSON.stringify({ code: status, message: 'Read failed' }), {});
    service.customObjectsApi = { getNamespacedCustomObject: async () => { throw error; } };
    await expect(kubernetesService.getDeploymentManifest('auto', 'models', undefined, { strict: true })).rejects.toBe(error);
    expect(await kubernetesService.getDeploymentManifest('auto', 'models')).toBeNull();
  });

  test('strict manifest reads return null only for NotFound and propagate unknown failures', async () => {
    const old = service.customObjectsApi;
    restores.push(() => { service.customObjectsApi = old; });
    service.customObjectsApi = { getNamespacedCustomObject: async () => {
      throw new ApiException(404, 'Unknown API Status Code!', JSON.stringify({ code: 404, reason: 'NotFound' }), {});
    } };
    expect(await kubernetesService.getDeploymentManifest('missing', 'models', undefined, { strict: true })).toBeNull();
    const error = new Error('Unexpected read failure');
    service.customObjectsApi = { getNamespacedCustomObject: async () => { throw error; } };
    await expect(kubernetesService.getDeploymentManifest('auto', 'models', undefined, { strict: true })).rejects.toBe(error);
    expect(await kubernetesService.getDeploymentManifest('auto', 'models')).toBeNull();
  });

  test('strict manifest reads retain transient-read retries and return the original manifest', async () => {
    const old = service.customObjectsApi;
    restores.push(() => { service.customObjectsApi = old; });
    let reads = 0;
    const manifest = { metadata: { name: 'auto', namespace: 'models', resourceVersion: '42' } };
    service.customObjectsApi = { getNamespacedCustomObject: async args => {
      expect(args).toEqual({ group: 'airunway.ai', version: 'v1alpha1', namespace: 'models', plural: 'modeldeployments', name: 'auto' });
      if (++reads === 1) throw { statusCode: 503, message: 'Temporarily unavailable' };
      return manifest;
    } };
    expect(await kubernetesService.getDeploymentManifest('auto', 'models', undefined, { strict: true })).toBe(manifest);
    expect(reads).toBe(2);
  });

  test('sends a single resourceVersion-guarded replacement, not a create or unguarded patch', async () => {
    const old = service.customObjectsApi;
    restores.push(() => { service.customObjectsApi = old; });
    const calls: ApiCall[] = [];
    service.customObjectsApi = { replaceNamespacedCustomObject: async args => { calls.push(args); } };
    const md: ModelDeployment = { apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: 'auto', namespace: 'models', resourceVersion: '42', annotations: { 'airunway.ai/dynamo-attempt': 'uuid' } }, spec: { model: { id: 'Qwen/Qwen3-0.6B' }, engine: { type: 'vllm' }, provider: { name: 'dynamo', overrides: { deploymentMode: 'intent', intent: defaultDynamoIntent() } } } };
    await kubernetesService.replaceDeployment(md);
    expect(calls).toHaveLength(1);
    expect(calls[0]).toMatchObject({ group: 'airunway.ai', name: 'auto', namespace: 'models', body: { metadata: { resourceVersion: '42', annotations: { 'airunway.ai/dynamo-attempt': 'uuid' } }, spec: md.spec } });
  });
});
