import { describe, expect, test } from 'bun:test';
import {
  defaultDynamoIntent, deploymentRequest, getDynamoIntent, toDeploymentStatus, toModelDeploymentManifest,
  DYNAMO_ATTEMPT_ANNOTATION, type DeploymentConfig, type DynamoIntentOverrides, type ModelDeployment,
} from '@airunway/shared';
import { dynamoIntentSchema, dynamoOverridesSchema, dynamoReconfigureSchema, reconfigureDynamoDeployment } from './dynamo-intent';

export function automaticDeployment(): ModelDeployment {
  return {
    apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment',
    metadata: { name: 'test-auto', namespace: 'models', resourceVersion: '42', annotations: { keep: 'me' }, labels: { team: 'inference' } },
    spec: {
      model: { id: 'Qwen/Qwen3-0.6B', source: 'huggingface' },
      engine: { type: 'vllm' }, provider: { name: 'dynamo', overrides: { deploymentMode: 'intent', intent: defaultDynamoIntent() } },
      gateway: { enabled: true }, secrets: { huggingFaceToken: 'hf-token-secret' },
    },
    status: { phase: 'Running', provider: { intent: { phase: 'Deployed', attempt: 'old' } } },
  };
}

function nativeOverrides(): DynamoIntentOverrides {
  return {
    profilingJob: {
      activeDeadlineSeconds: 1800,
      template: { spec: { containers: [{ name: 'profiler', args: ['--verbose'] }], restartPolicy: 'Never' } },
      futureField: { nested: [null, true, 1.5, { text: '  preserve whitespace  ' }] },
    },
    dgd: {
      apiVersion: 'nvidia.com/v1beta1', kind: 'DynamoGraphDeployment',
      metadata: { annotations: { 'example.com/note': 'keep me' } },
      spec: { components: [{ name: 'VllmDecodeWorker', podTemplate: { spec: { containers: [{
        name: 'main', $patch: { args: 'append' }, args: ['--dyn-tool-call-parser', 'hermes'],
      }] } } }] },
    },
  };
}

describe('Dynamo intent contract', () => {
  test('validates bounded rapid intents and exclusive alternatives', () => {
    expect(dynamoIntentSchema.safeParse(defaultDynamoIntent()).success).toBe(true);
    for (const invalid of [
      { ...defaultDynamoIntent(), hardware: { totalGpus: 0 } },
      { ...defaultDynamoIntent(), hardware: { totalGpus: 65 } },
      { ...defaultDynamoIntent(), hardware: { totalGpus: 1.5 } },
      { ...defaultDynamoIntent(), hardware: { totalGpus: 1, unknown: 1 } },
      { ...defaultDynamoIntent(), searchStrategy: 'thorough' },
      { ...defaultDynamoIntent(), autoApply: false },
      { ...defaultDynamoIntent(), model: 'other-model' },
      { ...defaultDynamoIntent(), backend: 'vllm' },
      { ...defaultDynamoIntent(), workload: { requestRate: 2, concurrency: 4 } },
      { ...defaultDynamoIntent(), workload: { isl: -1 } },
      { ...defaultDynamoIntent(), workload: { requestRate: Infinity } },
      { ...defaultDynamoIntent(), sla: { ttft: 1, e2eLatency: 2 } },
    ]) expect(dynamoIntentSchema.safeParse(invalid).success).toBe(false);
    expect(dynamoOverridesSchema.safeParse({ deploymentMode: 'intent', intent: defaultDynamoIntent(), spec: {} }).success).toBe(false);
    expect(dynamoIntentSchema.safeParse({ hardware: { totalGpus: 64, gpuSku: 'H100' }, workload: { concurrency: 2 }, sla: { e2eLatency: 5000 } }).success).toBe(true);
  });

  test('preserves native JSON, unknown inner fields and beta args append directives', () => {
    const overrides = nativeOverrides();
    const intent = { ...defaultDynamoIntent(), overrides };
    expect(dynamoIntentSchema.parse(intent)).toEqual(intent);
    expect(dynamoOverridesSchema.parse({ deploymentMode: 'intent', intent })).toEqual({ deploymentMode: 'intent', intent });
    expect(dynamoReconfigureSchema.parse({ resourceVersion: '42', intent }).intent).toEqual(intent);
    expect(dynamoOverridesSchema.safeParse({ deploymentMode: 'intent', intent, spec: {} }).success).toBe(false);

    const alpha: DynamoIntentOverrides = {
      dgd: { apiVersion: 'nvidia.com/v1alpha1', kind: 'DynamoGraphDeployment', spec: {
        services: { VllmWorker: { extraPodSpec: { mainContainer: { args: ['--verbose'] } } } },
      } },
    };
    for (const allowed of [{}, { profilingJob: {} }, { profilingJob: overrides.profilingJob }, alpha]) {
      expect(dynamoIntentSchema.parse({ ...defaultDynamoIntent(), overrides: allowed }).overrides).toEqual(allowed);
    }
  });

  test('requires object overrides, children, DGD spec and optional metadata', () => {
    const dgd = nativeOverrides().dgd!;
    for (const value of [null, [], 'raw', 1, false]) {
      for (const overrides of [value, { profilingJob: value }, { dgd: value },
        { dgd: { ...dgd, spec: value } }, { dgd: { ...dgd, metadata: value } }]) {
        expect(dynamoIntentSchema.safeParse({ ...defaultDynamoIntent(), overrides }).success).toBe(false);
      }
    }
    for (const overrides of [
      { unknown: {} }, { profilingJob: {}, spec: {} },
      { dgd: {} }, { dgd: { apiVersion: dgd.apiVersion, kind: dgd.kind } },
      { dgd: { ...dgd, apiVersion: undefined } }, { dgd: { ...dgd, kind: undefined } },
      { dgd: { ...dgd, apiVersion: 'nvidia.com/v1' } }, { dgd: { ...dgd, kind: 'Job' } },
      { dgd: { ...dgd, status: {} } }, { dgd: { ...dgd, unknown: {} } },
    ]) expect(dynamoIntentSchema.safeParse({ ...defaultDynamoIntent(), overrides }).success).toBe(false);
  });

  test('rejects non-JSON and non-finite values inside native objects and arrays', () => {
    for (const invalid of [Infinity, -Infinity, NaN, undefined, 1n, Symbol('invalid'), () => {}, new Date(), new Map(), new Set()]) {
      const nested = { nested: [[{ value: invalid }]] };
      for (const overrides of [
        { profilingJob: nested },
        { dgd: { ...nativeOverrides().dgd, spec: nested } },
        { dgd: { ...nativeOverrides().dgd, metadata: nested } },
      ]) expect(dynamoIntentSchema.safeParse({ ...defaultDynamoIntent(), overrides }).success).toBe(false);
    }
  });

  test('validates every native field before parsing and preserves unknown JSON keys', () => {
    const profilingJob = JSON.parse('{"__proto__":{"futureField":[1,null,"unchanged"]}}');
    const intent = { ...defaultDynamoIntent(), overrides: { profilingJob } };
    const parsed = dynamoIntentSchema.parse(intent);
    expect(parsed).toEqual(intent);
    expect(parsed.overrides?.profilingJob).not.toBe(profilingJob);
    const original = automaticDeployment();
    original.spec.provider!.overrides!.intent = intent;
    expect(reconfigureDynamoDeployment(original, { resourceVersion: '42' }).spec.provider?.overrides?.intent).toEqual(intent);
    for (const value of [
      JSON.parse('{"__proto__":{"nested":[{"HOSTNETWORK":true}]}}'),
      JSON.parse('{"__proto__":{"value":1e400}}'),
    ]) {
      expect(dynamoIntentSchema.safeParse({ ...intent, overrides: { profilingJob: value } }).success).toBe(false);
    }
  });

  test('rejects circular and sparse JSON values without rejecting shared object references', () => {
    const object: Record<string, unknown> = {};
    object.self = object;
    const array: unknown[] = [];
    array.push(array);
    for (const value of [object, array, new Array(1)]) {
      expect(dynamoIntentSchema.safeParse({ ...defaultDynamoIntent(), overrides: { profilingJob: { value } } }).success).toBe(false);
    }
    const child = { value: true };
    const intent = { ...defaultDynamoIntent(), overrides: { profilingJob: { first: child, second: child } } };
    expect(dynamoIntentSchema.parse(intent)).toEqual(intent);
  });

  test.each([
    'securityContext', 'serviceAccountName', 'serviceAccount', 'hostNetwork', 'hostPID', 'hostIPC',
    'automountServiceAccountToken', 'nodeName', 'priorityClassName', 'runtimeClassName', 'resources', 'replicas',
  ])('rejects %s recursively and case-insensitively in both native overrides', key => {
    for (const variant of [key, key.toLowerCase(), key.toUpperCase()]) {
      const forbidden = { [variant]: null };
      for (const value of [forbidden, { nested: [[forbidden]] }]) {
        for (const overrides of [
          { profilingJob: value },
          { dgd: { ...nativeOverrides().dgd, spec: value } },
          { dgd: { ...nativeOverrides().dgd, metadata: value } },
        ]) {
          const result = dynamoIntentSchema.safeParse({ ...defaultDynamoIntent(), overrides });
          expect(result.success).toBe(false);
          if (!result.success) expect(result.error.issues[0].path.at(-1)).toBe(variant);
        }
      }
    }
  });

  test('shared request, manifest and status helpers retain nested overrides', () => {
    const intent = { ...defaultDynamoIntent(), overrides: nativeOverrides() };
    const providerOverrides = { deploymentMode: 'intent', intent };
    const config: DeploymentConfig = {
      name: 'test-auto', namespace: 'models', modelId: 'Qwen/Qwen3-0.6B', engine: 'vllm', provider: 'dynamo',
      mode: 'aggregated', routerMode: 'default', replicas: 1, resources: { gpu: 1 },
      enforceEager: false, enablePrefixCaching: true, trustRemoteCode: false, providerOverrides,
    };
    const request = deploymentRequest(config);
    expect(request.providerOverrides).toEqual(providerOverrides);
    expect(request.resources).toBeUndefined();
    const manifest = toModelDeploymentManifest(config);
    expect(manifest.spec.provider?.overrides).toEqual(providerOverrides);
    expect(manifest.spec.resources).toBeUndefined();
    expect(getDynamoIntent('dynamo', manifest.spec.provider?.overrides)).toEqual(intent);
    expect(toDeploymentStatus(manifest).intent).toEqual(intent);
  });

  test('reconfigure retains omitted intent and replaces supplied intent without merging overrides', () => {
    const original = automaticDeployment();
    const intent = { ...defaultDynamoIntent(), overrides: nativeOverrides() };
    original.spec.provider!.overrides!.intent = intent;
    const retry = reconfigureDynamoDeployment(original, { resourceVersion: '42', engine: 'sglang' });
    expect(retry.spec.provider?.overrides?.intent).toEqual(intent);
    const supplied = { ...intent, hardware: { totalGpus: 2 } };
    const next = reconfigureDynamoDeployment(original, { resourceVersion: '42', intent: supplied });
    expect(next.spec.provider?.overrides?.intent).toEqual(supplied);
    expect(original.spec.provider?.overrides?.intent).toEqual(intent);
    const replaced = { ...defaultDynamoIntent(), overrides: { profilingJob: { backoffLimit: 0 } } };
    expect(reconfigureDynamoDeployment(original, { resourceVersion: '42', intent: replaced }).spec.provider?.overrides?.intent).toEqual(replaced);
    expect(reconfigureDynamoDeployment(original, { resourceVersion: '42', intent: defaultDynamoIntent() }).spec.provider?.overrides?.intent).toEqual(defaultDynamoIntent());
  });

  test('rejects invalid stored overrides and preserves top-level engine restrictions', () => {
    for (const overrides of [{ profilingJob: [] }, { dgd: { ...nativeOverrides().dgd, spec: { ReSoUrCeS: {} } } }]) {
      const original = automaticDeployment();
      original.spec.provider!.overrides!.intent = { ...defaultDynamoIntent(), overrides };
      expect(() => reconfigureDynamoDeployment(original, { resourceVersion: '42' })).toThrow('valid typed intent');
    }
    for (const settings of [
      { args: { 'max-model-len': '4096' } }, { extraArgs: ['--verbose'] }, { image: 'custom/image' },
      { contextLength: 4096 }, { trustRemoteCode: true }, { enforceEager: true },
    ]) {
      const original = automaticDeployment();
      original.spec.provider!.overrides!.intent = { ...defaultDynamoIntent(), overrides: nativeOverrides() };
      original.spec.engine = { ...original.spec.engine, ...settings };
      expect(() => reconfigureDynamoDeployment(original, { resourceVersion: '42' })).toThrow('require manual configuration');
    }
  });

  test('atomically changes intent, model, engine, and attempt without losing unrelated values', () => {
    const original = automaticDeployment();
    const next = reconfigureDynamoDeployment(original, {
      resourceVersion: '42', intent: { hardware: { totalGpus: 2 }, searchStrategy: 'rapid' },
      modelId: 'Qwen/Qwen3-8B', engine: 'sglang',
    }, 'new-attempt');
    expect(next.metadata.annotations).toEqual({ keep: 'me', [DYNAMO_ATTEMPT_ANNOTATION]: 'new-attempt' });
    expect(next.metadata.resourceVersion).toBe('42');
    expect(next.metadata.labels).toEqual(original.metadata.labels);
    expect(next.spec.provider?.overrides?.intent).toEqual({ hardware: { totalGpus: 2 }, searchStrategy: 'rapid' });
    expect(next.spec.model).toEqual({ ...original.spec.model, id: 'Qwen/Qwen3-8B' });
    expect(next.spec.engine.type).toBe('sglang');
    expect(next.spec.gateway).toEqual(original.spec.gateway);
    expect(next.spec.secrets).toEqual(original.spec.secrets);
    expect(original.metadata.annotations).toEqual({ keep: 'me' });
  });

  test('reconfigures auto-selected Dynamo intent without adding a spec provider name or losing overrides', () => {
    const original = automaticDeployment();
    delete original.spec.provider!.name;
    original.status!.provider!.name = 'dynamo';
    const intent = { ...defaultDynamoIntent(), overrides: nativeOverrides() };
    original.spec.provider!.overrides!.intent = intent;
    const before = structuredClone(original);

    const retry = reconfigureDynamoDeployment(original, { resourceVersion: '42' }, 'retry-attempt');
    expect(retry.spec).toEqual(original.spec);
    expect(retry.status).toEqual(original.status);
    expect(retry.spec.provider).not.toHaveProperty('name');
    expect(retry.metadata.annotations).toEqual({ keep: 'me', [DYNAMO_ATTEMPT_ANNOTATION]: 'retry-attempt' });

    const replacement = { ...intent, hardware: { totalGpus: 2 } };
    const next = reconfigureDynamoDeployment(original, { resourceVersion: '42', intent: replacement }, 'new-attempt');
    expect(next.spec.provider).toEqual({ overrides: { deploymentMode: 'intent', intent: replacement } });
    expect(next.status).toEqual(original.status);
    expect(original).toEqual(before);
  });

  test('an explicit provider takes precedence over the resolved status provider', () => {
    for (const name of ['kaito', 'vllm']) {
      const original = automaticDeployment();
      original.spec.provider!.name = name;
      original.status!.provider!.name = 'dynamo';
      expect(() => reconfigureDynamoDeployment(original, { resourceVersion: '42' })).toThrow('Only automatic Dynamo');
    }
    const explicitDynamo = automaticDeployment();
    explicitDynamo.status!.provider!.name = 'kaito';
    expect(reconfigureDynamoDeployment(explicitDynamo, { resourceVersion: '42' }).spec).toEqual(explicitDynamo.spec);
  });

  test('requires resolved Dynamo and stored intent when the spec provider name is absent', () => {
    for (const name of [undefined, 'kaito']) {
      const original = automaticDeployment();
      delete original.spec.provider!.name;
      original.status!.provider!.name = name;
      expect(() => reconfigureDynamoDeployment(original, { resourceVersion: '42' })).toThrow('Only automatic Dynamo');
    }
    const noIntent = automaticDeployment();
    delete noIntent.spec.provider;
    noIntent.status!.provider!.name = 'dynamo';
    expect(() => reconfigureDynamoDeployment(noIntent, { resourceVersion: '42' })).toThrow('Only automatic Dynamo');
  });

  test('retry preserves inputs and creates a unique attempt', () => {
    const current = automaticDeployment();
    const a = reconfigureDynamoDeployment(current, { resourceVersion: '42' });
    const b = reconfigureDynamoDeployment(current, { resourceVersion: '42' });
    expect(a.spec).toEqual(current.spec);
    expect(a.metadata.annotations?.[DYNAMO_ATTEMPT_ANNOTATION]).not.toBe(b.metadata.annotations?.[DYNAMO_ATTEMPT_ANNOTATION]);
  });

  test('refuses stale revisions, invalid stored intent, and manual deployments', () => {
    expect(() => reconfigureDynamoDeployment(automaticDeployment(), { resourceVersion: '41' })).toThrow('Deployment changed');
    const manual = automaticDeployment();
    manual.spec.provider!.overrides = {};
    expect(() => reconfigureDynamoDeployment(manual, { resourceVersion: '42' })).toThrow('Only automatic');
    const custom = automaticDeployment();
    custom.spec.model.storage = { volumes: [{ name: 'cache', claimName: 'cache' }] };
    expect(() => reconfigureDynamoDeployment(custom, { resourceVersion: '42' })).toThrow('require manual configuration');
    const invalid = automaticDeployment();
    invalid.spec.provider!.overrides!.intent = { hardware: { totalGpus: 80 } };
    expect(() => reconfigureDynamoDeployment(invalid, { resourceVersion: '42' })).toThrow('valid typed intent');
  });
});
