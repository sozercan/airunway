import { describe, expect, test } from 'bun:test';
import { getDynamoParserDefaults, getDynamoToolCallingError, defaultDynamoIntent, type ModelDeployment } from '@airunway/shared';
import { dynamoReconfigureSchema, reconfigureDynamoDeployment } from './dynamo-intent';

const enabled = { modelId: 'Qwen/Qwen3-0.6B', provider: 'dynamo', engine: 'vllm', toolCalling: true };
const current = (): ModelDeployment => ({ apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment',
  metadata: { name: 'auto', namespace: 'models', resourceVersion: '42' },
  spec: { model: { id: enabled.modelId }, engine: { type: 'vllm', toolCalling: true, toolCallParser: 'hermes', reasoningParser: 'basic' },
    provider: { name: 'dynamo', overrides: { deploymentMode: 'intent', intent: defaultDynamoIntent() } } } });

describe('Dynamo tool parser contract', () => {
  test.each([
    ['Qwen/Qwen3-Coder-30B-A3B-Instruct', { toolCallParser: 'qwen3_coder' }],
    ['qWeN/QwEn3.5-9B', { toolCallParser: 'qwen3_coder', reasoningParser: 'qwen3' }],
    ['Qwen/Qwen3-0.6B', { toolCallParser: 'hermes', reasoningParser: 'qwen3' }],
  ] as const)('selects only the ordered family default for %s', (modelId, expected) => {
    expect(getDynamoParserDefaults(modelId)).toEqual(expected);
    expect(getDynamoToolCallingError({ ...enabled, modelId })).toBeUndefined();
  });
  test.each(['Qwen/Qwen2.5-7B', 'Qwen/Qwen3', 'Qwen/Qwen3.5', 'Qwen/Qwen30-8B', 'alias/Qwen3-8B', 'my-qwen/qwen3-coder', 'acme/model'])(
    'requires an explicit parser for unknown family %s', modelId => {
      expect(getDynamoParserDefaults(modelId)).toEqual({});
      expect(getDynamoToolCallingError({ ...enabled, modelId })).toContain('No automatic tool parser');
      expect(getDynamoToolCallingError({ ...enabled, modelId, toolCallParser: 'custom_parser', reasoningParser: 'basic' })).toBeUndefined();
    });
  test.each(['', 'auto', 'none', 'Hermes', ' qwen3', 'qwen3 ', 'tool-call', '1parser', 'a'.repeat(65)])('rejects parser identifier %j', parser => {
    expect(getDynamoToolCallingError({ ...enabled, toolCallParser: parser })).toBeString();
    expect(getDynamoToolCallingError({ ...enabled, reasoningParser: parser })).toBeString();
    expect(dynamoReconfigureSchema.safeParse({ resourceVersion: '42', toolCallParser: parser }).success).toBe(false);
    expect(dynamoReconfigureSchema.safeParse({ resourceVersion: '42', reasoningParser: parser }).success).toBe(false);
  });
  test('accepts a 64-character explicit parser and rejects parser fields when disabled', () => {
    expect(getDynamoToolCallingError({ ...enabled, toolCallParser: 'a'.repeat(64) })).toBeUndefined();
    for (const toolCalling of [undefined, false]) for (const parser of ['toolCallParser', 'reasoningParser']) {
      expect(getDynamoToolCallingError({ ...enabled, toolCalling, [parser]: 'hermes' })).toContain('require tool calling');
    }
  });
  test('checks explicit and resolved providers and supported engines', () => {
    for (const provider of ['kaito', 'vllm', 'kuberay', 'llmd', undefined]) {
      expect(getDynamoToolCallingError({ ...enabled, provider })).toContain('requires the Dynamo');
    }
    expect(getDynamoToolCallingError({ ...enabled, provider: undefined }, 'dynamo')).toBeUndefined();
    expect(getDynamoToolCallingError({ ...enabled, provider: undefined, providerOverrides: { deploymentMode: 'intent' } })).toBeUndefined();
    expect(getDynamoToolCallingError(enabled, 'kaito')).toContain('requires the Dynamo');
    expect(getDynamoToolCallingError({ ...enabled, engine: 'llamacpp' })).toContain('model server');
  });
  test.each(['dyn-tool-call-parser', 'dyn-reasoning-parser', 'dyn-chat-processor', 'tool-call-parser', 'reasoning-parser', 'enable-auto-tool-choice'])(
    'rejects conflicting flag %s in maps, argv, commands, and nested native overrides', flag => {
      for (const settings of [
        { engineArgs: { [flag]: 'x' } }, { engineExtraArgs: [`--${flag}=x`] },
        { providerOverrides: { spec: { components: [{ args: [`--${flag}`, 'x'] }] } } },
        { providerOverrides: { intent: { overrides: { profilingJob: { command: [`python -m dynamo --${flag} x`] } } } } },
      ]) expect(getDynamoToolCallingError({ ...enabled, ...settings })).toContain(`setting ${flag}`);
    });
  test.each(['DYN_TOOL_CALL_PARSER', 'DYN_REASONING_PARSER', 'DYN_CHAT_PROCESSOR'])(
    'rejects conflicting env %s in all native env encodings', name => {
      for (const settings of [{ env: { [name]: 'x' } }, { env: [{ name, value: 'x' }] },
        { providerOverrides: { intent: { overrides: { dgd: { spec: { envs: [{ name, value: 'x' }] } } } } } },
        { providerOverrides: { spec: { env: [{ name, value: 'x' }] } } },
      ]) expect(getDynamoToolCallingError({ ...enabled, ...settings })).toContain(`setting ${name}`);
    });
});

describe('sparse tool calling reconfiguration', () => {
  test('preserves omitted settings, clears nullable settings and does not mutate the current revision', () => {
    const original = current();
    const before = structuredClone(original);
    expect(reconfigureDynamoDeployment(original, { resourceVersion: '42' }).spec.engine).toEqual(original.spec.engine);
    const next = reconfigureDynamoDeployment(original, { resourceVersion: '42', toolCallParser: null, reasoningParser: null });
    expect(next.spec.engine).toEqual({ type: 'vllm', toolCalling: true });
    expect(original).toEqual(before);
  });
  test('disabling clears both parsers even when omitted in the request', () => {
    const next = reconfigureDynamoDeployment(current(), { resourceVersion: '42', toolCalling: false });
    expect(next.spec.engine).toEqual({ type: 'vllm', toolCalling: false });
  });
  test('validates effective settings against a changed model and the resolved provider', () => {
    const original = current();
    delete original.spec.provider!.name;
    original.status = { provider: { name: 'dynamo' } };
    expect(reconfigureDynamoDeployment(original, { resourceVersion: '42' }).spec.engine.toolCallParser).toBe('hermes');
    expect(() => reconfigureDynamoDeployment(original, { resourceVersion: '42', toolCallParser: null, modelId: 'acme/model' })).toThrow('No automatic tool parser');
    original.status.provider!.name = 'kaito';
    expect(() => reconfigureDynamoDeployment(original, { resourceVersion: '42' })).toThrow('Only automatic Dynamo');
  });
  test('revalidates native conflicts when keeping or enabling tool calling', () => {
    const original = current();
    original.spec.provider!.overrides!.intent = { ...defaultDynamoIntent(), overrides: { profilingJob: { command: ['--dyn-chat-processor=custom'] } } };
    expect(() => reconfigureDynamoDeployment(original, { resourceVersion: '42' })).toThrow('conflicts');
    expect(reconfigureDynamoDeployment(original, { resourceVersion: '42', toolCalling: false }).spec.engine.toolCalling).toBe(false);
  });
});
