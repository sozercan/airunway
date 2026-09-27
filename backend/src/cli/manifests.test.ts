import { afterEach, describe, expect, test } from 'bun:test';
import { mkdtemp, rmdir, unlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { parseCLIArgs } from './args';
import { buildAgent, buildModel, updateResource } from './manifests';
import { CLIError, type Flags, type IO, type Resource } from './types';

// Build dummy userinfo through the URL API so secret scanners do not attempt to
// authenticate a deliberately invalid fixture embedded in source.
function credentialBearingURL(value: string): string {
  const url = new URL(value);
  url.username = 'user'; url.password = 'private-value';
  return url.toString();
}

function memoryIO(input = ''): IO & { reads: number; output: string[] } {
  return { interactive: false, reads: 0, output: [], out(text) { this.output.push(text); }, err(text) { this.output.push(text); }, async input() { this.reads++; return input; } };
}
const files: string[] = [], directories: string[] = [];
async function fixture(content: string, filename = 'input.txt'): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), 'airunway-manifests-'));
  const path = join(directory, filename);
  directories.push(directory); files.push(path);
  await writeFile(path, content);
  return path;
}
afterEach(async () => {
  for (const path of files.splice(0)) await unlink(path);
  for (const path of directories.splice(0)) await rmdir(path);
});
const model = (flags: Flags = {}, io = memoryIO()) => buildModel('demo', { id: 'org/model', ...flags }, 'team', io);
const agent = (flags: Flags = {}, io = memoryIO()) => buildAgent('helper', { framework: 'langgraph', 'model-ref': 'demo', ...flags }, 'team', io);
const external = (flags: Flags = {}) => ({ 'model-ref': undefined, 'model-url': 'https://api.example.test/v1', 'model-api': 'openai', 'model-id': 'remote-model', ...flags });
const gateway = (flags: Flags = {}) => ({ 'model-ref': undefined, 'model-gateway': 'edge', 'model-id': 'served-model', ...flags });
async function invalid(operation: Promise<unknown>, pattern?: string): Promise<void> {
  try { await operation; } catch (error) {
    expect(error).toBeInstanceOf(CLIError);
    expect((error as CLIError).exitCode).toBe(2);
    if (pattern) expect((error as Error).message).toContain(pattern);
    return;
  }
  throw new Error('Expected a CLI usage error');
}
function withServerFields(resource: Resource): Resource {
  resource.metadata.resourceVersion = '42';
  resource.metadata.uid = 'existing-uid';
  resource.metadata.labels = { owner: 'team' };
  resource.metadata.annotations = { untouched: 'annotation' };
  resource.metadata.managedFields = [{ manager: 'controller' }];
  resource.status = { phase: 'Running', private: { details: 'server state' } };
  return resource;
}
// Apply RFC 7396 semantics to catch accidental replacements and union members
// that would survive an omitted field in the submitted patch.
function mergePatch<T>(target: T, patch: unknown): T {
  if (!patch || typeof patch !== 'object' || Array.isArray(patch)) return structuredClone(patch) as T;
  const merged: Record<string, unknown> = target && typeof target === 'object' && !Array.isArray(target) ? structuredClone(target) as Record<string, unknown> : {};
  for (const [key, value] of Object.entries(patch)) {
    if (value === null) delete merged[key];
    else merged[key] = mergePatch(merged[key], value);
  }
  return merged as T;
}

describe('model manifest construction', () => {
  for (const id of ['org/model', 'hf://org/model', 'hf://Qwen/Qwen3-8B', 'gpt2']) {
    test(`plain HF ${id} stays unstaged and defaults to one GPU`, async () => {
      const result = await model({ id });
      expect(result).toEqual({
        apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment',
        metadata: { name: 'demo', namespace: 'team', annotations: { 'airunway.ai/managed-by': 'cli' } },
        spec: { resources: { gpu: { count: 1 } }, scaling: { replicas: 1 }, model: { source: 'huggingface', id: id.replace(/^hf:\/\//, '') } },
      });
    });
  }
  test('explicit zero requests CPU without selecting a provider or engine', async () => {
    const result = await model({ gpus: '0', cpu: '500m', memory: '4Gi', replicas: '0' });
    expect(result.spec?.resources).toEqual({ gpu: { count: 0 }, cpu: '500m', memory: '4Gi' });
    expect(result.spec?.scaling).toEqual({ replicas: 0 });
    expect(result.spec?.provider).toBeUndefined(); expect(result.spec?.engine).toBeUndefined();
  });
  test('maps explicit engine options without shell parsing raw arguments', async () => {
    const args = ['--quantization', 'awq', '--json={"name":"value with spaces"}', '--tensor-parallel-size=2'];
    const result = await model({ provider: 'dynamo', engine: 'sglang', image: 'registry.example.test/runtime:v2', 'served-name': 'chat', 'context-length': '8192', gpus: '2', credential: 'hf-token', 'engine-arg': args, 'trust-remote-code': false, gateway: false });
    expect(result.spec?.provider).toEqual({ name: 'dynamo' });
    expect(result.spec?.model).toEqual({ source: 'huggingface', id: 'org/model', servedName: 'chat' });
    expect(result.spec?.engine).toEqual({ type: 'sglang', image: 'registry.example.test/runtime:v2', contextLength: 8192, trustRemoteCode: false, extraArgs: args });
    expect(result.spec?.image).toBeUndefined(); expect(result.spec?.secrets).toEqual({ huggingFaceToken: 'hf-token' });
    expect(result.spec?.gateway).toEqual({ enabled: false });
    args.push('later'); expect(result.spec?.engine.extraArgs).toHaveLength(4);
  });
  test('accepts repeated raw engine flags from the real parser', async () => {
    const { flags } = parseCLIArgs(['--id', 'org/model', '--engine-arg=--dtype=half', '--engine-arg=--enforce-eager', '--trust-remote-code=true']);
    const result = await buildModel('demo', flags, 'team', memoryIO());
    expect(result.spec?.engine.extraArgs).toEqual(['--dtype=half', '--enforce-eager']);
    expect(result.spec?.engine.trustRemoteCode).toBe(true);
  });
  test('bundled images have an absolute custom model path and a distinct runtime image', async () => {
    const result = await buildModel('demo', { image: 'registry.example.test/bundled:v1', 'model-path': '/models/chat' }, 'team', memoryIO());
    expect(result.spec?.model).toEqual({ source: 'custom', id: '/models/chat' });
    expect(result.spec?.engine).toEqual({ type: 'vllm', image: 'registry.example.test/bundled:v1' });
    expect(result.spec?.provider).toEqual({ name: 'vllm' });
  });
  for (const flags of [
    { id: undefined }, { id: '' }, { engine: 'not-an-engine' }, { provider: '../bad' },
    { image: 'https://registry/image' }, { 'engine-arg': 'not-an-array' }, { 'engine-arg': [''] },
    { prompt: 'not a model option' }, { 'trust-remote-code': 'false' }, { 'served-name': ' ' },
    { 'model-path': '/models/demo' }, { id: undefined, image: 'demo:v1', 'model-path': 'relative' },
    { id: undefined, image: 'demo:v1', 'model-path': '/models/../private' },
    { id: undefined, image: 'demo:v1', 'model-path': '/models/demo', provider: 'kaito' },
    { id: undefined, image: 'demo:v1', 'model-path': '/models/demo', 'served-name': 'not-supported' },
    { id: undefined, image: 'demo:v1', 'model-path': '/models/demo', credential: 'credentials' },
  ] as Flags[]) test(`rejects invalid model flags ${JSON.stringify(flags)}`, async () => { await invalid(model(flags)); });
  for (const key of ['gpus', 'replicas', 'context-length']) {
    for (const value of ['-1', '1.5', 'nope', 'Infinity', '2147483648', ' 1']) test(`rejects invalid ${key}=${value}`, async () => { await invalid(model({ [key]: value }), `--${key}`); });
  }
  test('context length cannot be zero', async () => { await invalid(model({ 'context-length': '0' })); });
  for (const key of ['cpu', 'memory']) {
    for (const value of ['0', '-1', 'NaN', '1GB', ' 2', '2 Gi']) test(`rejects invalid ${key}=${value}`, async () => { await invalid(model({ [key]: value }), `--${key}`); });
  }
  test('HF token keys cannot silently change', async () => {
    expect((await model({ credential: 'hf-token/HF_TOKEN' })).spec?.secrets).toEqual({ huggingFaceToken: 'hf-token' });
    await invalid(model({ credential: 'hf-token/other-key' }), 'HF_TOKEN');
  });
  for (const [name, namespace] of [['BadName', 'team'], ['demo', '../team']]) test(`rejects name/namespace ${name}/${namespace}`, async () => { await invalid(buildModel(name, { id: 'org/model' }, namespace, memoryIO())); });
});

describe('model source lowering', () => {
  for (const uri of ['s3://bucket/models/demo', 'gs://bucket/models/demo', 'oci://registry.example.test/models/demo:v1', `oci://registry.example.test/models/demo@sha256:${'a'.repeat(64)}`]) {
    test(`stages ${uri} in the agreed artifacts directory`, async () => {
      const result = await model({ id: uri });
      expect(result.spec?.model).toEqual({ source: 'custom', id: '/model-cache/artifacts', artifact: { uri }, storage: { volumes: [{ name: 'model-cache', purpose: 'modelCache', mountPath: '/model-cache', readOnly: false, size: '100Gi' }] } });
      expect(result.spec?.provider).toEqual({ name: 'vllm' }); expect(result.spec?.engine.type).toBe('vllm');
    });
  }
  test('stages HTTPS to the URL filename or the explicitly named output', async () => {
    const uri = 'https://models.example.test/files/model.gguf';
    expect((await model({ id: uri })).spec?.model.id).toBe('/model-cache/artifacts/model.gguf');
    const result = await model({ id: uri, file: 'quantized/chat.gguf' });
    expect(result.spec?.model.id).toBe('/model-cache/artifacts/quantized/chat.gguf');
    expect(result.spec?.model.artifact).toEqual({ uri, file: 'quantized/chat.gguf' });
  });
  test('source credentials, workload identity and downloader image are separate from the runtime', async () => {
    const result = await model({ id: 's3://bucket/prefix', file: 'weights/chat.gguf', credential: 'cloud/key.json', 'service-account': 'model-loader', image: 'vllm/runtime:v1', 'artifact-image': 'registry.example.test/loader:v2', 'storage-size': '250Gi', 'storage-class': 'fast-rwx', 'served-name': 'chat' });
    expect(result.spec?.model.artifact).toEqual({ uri: 's3://bucket/prefix', file: 'weights/chat.gguf', credentialsRef: { name: 'cloud', key: 'key.json' }, image: 'registry.example.test/loader:v2', serviceAccountName: 'model-loader' });
    expect(result.spec?.model.id).toBe('/model-cache/artifacts/weights/chat.gguf');
    expect(result.spec?.model.servedName).toBe('chat');
    expect(result.spec?.model.storage.volumes[0]).toMatchObject({ size: '250Gi', storageClassName: 'fast-rwx', readOnly: false });
    expect(result.spec?.engine.image).toBe('vllm/runtime:v1'); expect(result.spec?.secrets).toBeUndefined();
  });
  test('omits an unspecified credential key for the loader default and permits an empty storage class', async () => {
    const result = await model({ id: 'gs://bucket/model', credential: 'cloud', 'storage-class': '' });
    expect(result.spec?.model.artifact.credentialsRef).toEqual({ name: 'cloud' });
    expect(result.spec?.model.storage.volumes[0].storageClassName).toBe('');
  });
  for (const flags of [{ revision: 'main' }, { file: 'weights/model.gguf' }, { revision: 'refs/pr/1', file: 'weights/model.gguf' }]) {
    test(`HF selection stages through vLLM ${JSON.stringify(flags)}`, async () => {
      const result = await model({ ...flags, credential: 'hf-token' });
      expect(result.spec?.model.artifact).toEqual({ uri: 'hf://org/model', ...flags });
      expect(result.spec?.model.id).toBe(`/model-cache/artifacts${flags.file ? `/${flags.file}` : ''}`);
      expect(result.spec?.model.source).toBe('custom'); expect(result.spec?.provider.name).toBe('vllm');
      expect(result.spec?.secrets).toEqual({ huggingFaceToken: 'hf-token' });
    });
  }
  for (const [uri, expected] of [['pvc://weights/models/chat', '/model-cache/models/chat'], ['pvc://weights', '/model-cache'], ['pvc://weights/', '/model-cache']]) {
    test(`existing storage ${uri} is read only and never downloads`, async () => {
      const result = await model({ id: uri });
      expect(result.spec?.model).toEqual({ source: 'custom', id: expected, storage: { volumes: [{ name: 'model-cache', purpose: 'modelCache', mountPath: '/model-cache', claimName: 'weights', readOnly: true }] } });
      expect(result.spec?.provider.name).toBe('vllm');
    });
  }
  for (const flags of [
    { 'storage-size': '100Gi' }, { 'storage-class': 'fast' }, { 'artifact-image': 'loader:v1' }, { 'service-account': 'loader' },
    { id: 's3://bucket/model', provider: 'kaito' }, { id: 's3://bucket/model', engine: 'sglang' },
    { revision: 'main', provider: 'dynamo' }, { revision: 'main', engine: 'llamacpp' },
    { id: 'pvc://claim/path', file: 'model.gguf' }, { id: 'pvc://claim/path', credential: 'cloud' },
    { id: 'pvc://claim/path', 'storage-size': '100Gi' }, { id: 'pvc://claim/path', 'service-account': 'loader' },
    { id: 'pvc://claim/path', 'served-name': 'chat' },
    { id: 's3://bucket/model', revision: 'main' }, { revision: 'a'.repeat(257) },
    { id: 'gs://bucket/model', 'storage-size': '0' }, { id: 'gs://bucket/model', 'storage-size': '100GB' },
    { id: 'gs://bucket/model', 'storage-class': '../fast' }, { id: 'gs://bucket/model', 'service-account': '../loader' },
    { id: 'gs://bucket/model', 'artifact-image': 'loader' }, { id: 'gs://bucket/model', credential: 'cloud/key/extra' },
  ] as Flags[]) test(`rejects unsupported source combination ${JSON.stringify(flags)}`, async () => { await invalid(model(flags)); });
  for (const uri of [
    'ftp://example.test/model', 'http://example.test/model', 'file:///models/chat', '/tmp/model', '../model',
    'hf://org/model/file', 'hf://org/../model', 'hf://org/model?token=private-value',
    's3://bucket/../model', 's3://bucket/a/./model', 'gs://bucket/a//model', 'gs://bucket/%2e%2e/model',
    's3://bucket/model?signature=private-value', credentialBearingURL('s3://bucket/model'),
    credentialBearingURL('https://example.test/model.gguf'), 'https://example.test/model?sig=private-value',
    'https://example.test/model#private-value', 'https://example.test/../model', 'https://example.test/%252e%252e/model',
    'https://example.test/', 'https://example.test/models/', 'https://example.test/a\\..\\model',
    'oci://registry.example.test/models/demo', 'oci://registry.example.test/models/demo@sha256:bad',
    credentialBearingURL('oci://registry.example.test/model:v1'), 'oci://registry.example.test/../model:v1',
    'pvc://claim/../../private', 'pvc://claim/%2fprivate', 'pvc://claim/path?token=private-value',
    's3://bad_bucket/model', 's3://bucket:123/model', 's3://bucket//',
  ]) test(`rejects unsafe or unsupported source ${uri.split('?')[0].replace(/private-value/g, 'redacted')}`, async () => {
    const io = memoryIO();
    try { await model({ id: uri }, io); throw new Error('expected source rejection'); } catch (error) {
      expect(error).toBeInstanceOf(CLIError); expect(String(error)).not.toContain('private-value'); expect(io.output).toEqual([]);
    }
  });
  for (const path of ['../model.gguf', '/model.gguf', 'a/../model', 'a//model', 'a/./model', '%2e%2e/model', 'a\\model', '', 'model\0file']) {
    test(`rejects unsafe selected file ${JSON.stringify(path)}`, async () => { await invalid(model({ file: path })); });
  }
});

describe('agent manifest construction', () => {
  test('requires a framework and one binding without inventing settings', async () => {
    const result = await agent();
    expect(result).toEqual({ apiVersion: 'airunway.ai/v1alpha1', kind: 'AgentDeployment', metadata: { name: 'helper', namespace: 'team', annotations: { 'airunway.ai/managed-by': 'cli' } }, spec: { framework: { name: 'langgraph' }, lifecycle: 'deployment', model: { deploymentRef: { name: 'demo' } } } });
    await invalid(agent({ framework: undefined }), '--framework'); await invalid(agent({ 'model-ref': undefined }), 'Provide');
  });
  test('supports explicit cross-namespace deployment and gateway references', async () => {
    expect((await agent({ 'model-ref': 'models/demo' })).spec?.model).toEqual({ deploymentRef: { namespace: 'models', name: 'demo' } });
    expect((await agent(gateway({ 'model-gateway': 'edge/shared', 'gateway-listener': 'https' }))).spec?.model).toEqual({ gatewayEndpoint: { gatewayRef: { name: 'shared', namespace: 'edge', listenerName: 'https' }, modelName: 'served-model' } });
  });
  for (const [input, type] of [['openai', 'openai'], ['anthropic', 'anthropic'], ['azure-openai', 'azureOpenAI'], ['azureOpenAI', 'azureOpenAI'], ['custom', 'custom']]) {
    test(`maps external API ${input} and credential key`, async () => {
      expect((await agent(external({ 'model-api': input, 'model-credential': 'api-key/token' }))).spec?.model).toEqual({ externalAPI: { baseURL: 'https://api.example.test/v1', type, modelName: 'remote-model', credentialsRef: { name: 'api-key', key: 'token' } } });
    });
  }
  test('accepts an HTTP in-cluster endpoint without credentials', async () => {
    const result = await agent(external({ 'model-url': 'http://model.team.svc:8000/v1' }));
    expect(result.spec?.model.externalAPI.baseURL).toBe('http://model.team.svc:8000/v1');
    expect(result.spec?.model.externalAPI.credentialsRef).toBeUndefined();
  });
  test('reads system prompt files and maps container settings', async () => {
    const path = await fixture('Keep this prompt.\nIncluding trailing newline.\n');
    const result = await agent({ 'prompt-file': path, image: 'registry.example.test/agent:v1', cpu: '500m', memory: '2Gi' });
    expect(result.spec?.config).toEqual({ systemPrompt: 'Keep this prompt.\nIncluding trailing newline.\n', image: 'registry.example.test/agent:v1' });
    expect(result.spec?.resources).toEqual({ requests: { cpu: '500m', memory: '2Gi' } });
    expect(result.spec?.image).toBeUndefined();
  });
  test('once maps to job and task input does not replace system instructions', async () => {
    const io = memoryIO('Summarize the incident.\n');
    const result = await agent({ mode: 'once', prompt: 'Be concise.', 'task-file': '-' }, io);
    expect(result.spec?.lifecycle).toBe('job'); expect(result.spec?.config).toEqual({ systemPrompt: 'Be concise.', task: 'Summarize the incident.\n' }); expect(io.reads).toBe(1);
  });
  test('task files and explicit job mode work offline', async () => {
    const path = await fixture('Complete the task.');
    const result = await agent({ mode: 'job', 'task-file': path });
    expect(result.spec?.lifecycle).toBe('job'); expect(result.spec?.config.task).toBe('Complete the task.');
  });
  test('preset config, file config and explicit flags merge only without conflicts', async () => {
    const path = await fixture(JSON.stringify({ nested: { second: 2 }, command: ['python', 'agent.py'] }), 'config.json');
    const result = await agent({ '__preset-config': JSON.stringify({ nested: { first: 1 }, image: 'example/agent:v1' }), 'config-file': path, image: 'example/agent:v1', prompt: 'Instructions.' });
    expect(result.spec?.config).toEqual({ nested: { first: 1, second: 2 }, command: ['python', 'agent.py'], image: 'example/agent:v1', systemPrompt: 'Instructions.' });
  });
  test('semantically identical duplicate nested configuration does not conflict', async () => {
    const path = await fixture('{"nested":{"second":2,"first":1}}', 'config.json');
    const result = await agent({ '__preset-config': '{"nested":{"first":1,"second":2}}', 'config-file': path });
    expect(result.spec?.config).toEqual({ nested: { first: 1, second: 2 } });
  });
  test('job task may come from configuration', async () => {
    const path = await fixture('{"task":"Do the work."}', 'config.json');
    expect((await agent({ mode: 'once', 'config-file': path })).spec?.config.task).toBe('Do the work.');
  });
  test('prototype-looking configuration keys are inert JSON', async () => {
    const result = await agent({ '__preset-config': '{"__proto__":{"polluted":true},"constructor":{"name":"data"}}' });
    expect(({} as Record<string, unknown>).polluted).toBeUndefined();
    expect(Object.prototype.hasOwnProperty.call(result.spec?.config, '__proto__')).toBe(true);
    expect(JSON.parse(JSON.stringify(result.spec?.config)).__proto__).toEqual({ polluted: true });
  });
  for (const flags of [
    { 'model-url': 'https://api.example.test/v1' }, { 'model-gateway': 'gateway' }, { 'model-api': 'openai' },
    external({ 'model-gateway': 'edge' }), external({ 'model-api': undefined }), external({ 'model-id': undefined }),
    external({ 'model-api': 'unsupported' }), external({ 'model-credential': 'key-without-field' }), external({ 'model-credential': 'name/' }), external({ 'model-credential': 'name/key/extra' }),
    external({ 'gateway-listener': 'https' }), external({ 'model-url': credentialBearingURL('https://example.test/v1') }),
    external({ 'model-url': 'https://example.test/v1?api_key=private-value' }), external({ 'model-url': 'ftp://example.test/v1' }),
    gateway({ 'model-id': undefined }), gateway({ 'model-credential': 'key/token' }), gateway({ 'model-api': 'openai' }),
    { 'model-ref': 'too/many/segments' }, { framework: '../invalid' }, { mode: 'unknown' }, { mode: 'once' }, { mode: 'once', task: ' ' },
    { task: 'only jobs' }, { gpus: '1' }, { replicas: '2' }, { provider: 'vllm' }, { cpu: '-1' },
    { prompt: 'inline', 'prompt-file': '-' }, { task: 'inline', 'task-file': '-', mode: 'once' },
    { '__preset-config': '[]' }, { '__preset-config': 'invalid-json' },
  ] as Flags[]) test(`rejects invalid agent flags ${JSON.stringify(flags).replace(/private-value/g, 'redacted')}`, async () => { await invalid(agent(flags)); });
  for (const content of ['plain: yaml', '[]', 'null', '42', '"string"', '{broken-json']) test(`config files require JSON objects ${content}`, async () => {
    const path = await fixture(content, 'config.json'); await invalid(agent({ 'config-file': path }), '--config-file');
  });
  for (const [config, flags] of [
    [{ systemPrompt: 'from file' }, { prompt: 'explicit' }],
    [{ task: 'from file' }, { task: 'explicit', mode: 'once' }],
    [{ image: 'from-file:v1' }, { image: 'explicit:v1' }],
    [{ nested: { setting: 2 } }, { '__preset-config': '{"nested":{"setting":1}}' }],
  ] as [Record<string, unknown>, Flags][]) test(`rejects config conflict ${JSON.stringify(config)}`, async () => {
    const path = await fixture(JSON.stringify(config), 'config.json'); await invalid(agent({ ...flags, 'config-file': path }), 'conflicts');
  });
  test('stdin is single-use across prompt, task and config input', async () => {
    const io = memoryIO('{}'); await invalid(agent({ 'config-file': '-', 'prompt-file': '-' }, io), 'Only one'); expect(io.reads).toBe(0);
  });
  test('missing and oversized files fail with usage errors', async () => {
    await invalid(agent({ 'prompt-file': '/this-file-does-not-exist-airunway' }), 'Cannot read');
    const path = await fixture('x'.repeat(4 * 1024 * 1024 + 1)); await invalid(agent({ 'prompt-file': path }), '4 MiB');
  });
});

describe('sparse merge-patch updates', () => {
  test('model updates preserve concurrency metadata and only explicit mutable fields', async () => {
    const existing = withServerFields(await model({ provider: 'vllm', engine: 'vllm', gpus: '4', cpu: '8', memory: '32Gi', replicas: '3', 'context-length': '4096' }));
    existing.spec!.resources.gpu.type = 'vendor.example/gpu'; existing.spec!.futureSetting = { enabled: true };
    const snapshot = structuredClone(existing);
    const patch = await updateResource('model', existing, { memory: '64Gi' }, memoryIO());
    expect(patch).toEqual({ apiVersion: existing.apiVersion, kind: existing.kind, metadata: { name: 'demo', namespace: 'team', resourceVersion: '42' }, spec: { resources: { memory: '64Gi' } } });
    expect(existing).toEqual(snapshot);
    const merged = mergePatch(existing, patch);
    expect(merged.spec!.resources).toEqual({ gpu: { count: 4, type: 'vendor.example/gpu' }, cpu: '8', memory: '64Gi' });
    expect(merged.spec!.scaling.replicas).toBe(3); expect(merged.spec!.engine.contextLength).toBe(4096);
    expect(merged.spec!.futureSetting).toEqual({ enabled: true }); expect(merged.metadata.annotations).toEqual(snapshot.metadata.annotations); expect(merged.status).toEqual(snapshot.status);
  });
  test('explicit zeros and false booleans are included without new defaults', async () => {
    const existing = withServerFields(await model());
    const patch = await updateResource('model', existing, { gpus: '0', replicas: '0', gateway: false, 'trust-remote-code': false }, memoryIO());
    expect(patch.spec).toEqual({ resources: { gpu: { count: 0 } }, scaling: { replicas: 0 }, engine: { trustRemoteCode: false }, gateway: { enabled: false } });
    expect(patch.status).toBeUndefined(); expect(patch.metadata.uid).toBeUndefined(); expect(patch.metadata.managedFields).toBeUndefined();
  });
  test('updates all supported model runtime fields', async () => {
    const patch = await updateResource('model', withServerFields(await model()), { 'served-name': 'chat', 'context-length': '2048', image: 'runtime:v2', 'engine-arg': ['--dtype=half'], credential: 'next-token', cpu: '2' }, memoryIO());
    expect(patch.spec).toEqual({ model: { servedName: 'chat' }, engine: { contextLength: 2048, image: 'runtime:v2', extraArgs: ['--dtype=half'] }, secrets: { huggingFaceToken: 'next-token' }, resources: { cpu: '2' } });
  });
  test('artifact state is not included or changed by a resource update', async () => {
    const existing = withServerFields(await model({ id: 's3://bucket/model', credential: 'cloud' }));
    const patch = await updateResource('model', existing, { 'context-length': '16384', 'served-name': 'chat' }, memoryIO());
    expect(patch.spec).toEqual({ engine: { contextLength: 16384 }, model: { servedName: 'chat' } });
    expect(mergePatch(existing, patch).spec!.model.artifact).toEqual(existing.spec?.model.artifact);
    await invalid(updateResource('model', existing, { credential: 'replacement' }, memoryIO()), 'immutable');
  });
  test('plain custom models reject ineffective credential and served-name updates', async () => {
    const existing = await model({ id: 'pvc://claim/model' });
    for (const flags of [{ credential: 'new-token' }, { 'served-name': 'chat' }]) await invalid(updateResource('model', existing, flags, memoryIO()));
  });
  test('prompt updates preserve other config, binding and immutable settings', async () => {
    const existing = withServerFields(await agent({ prompt: 'old', image: 'agent:v1', '__preset-config': '{"skills":["search"],"nested":{"setting":true}}' }));
    const before = structuredClone(existing);
    const patch = await updateResource('agent', existing, { 'prompt-file': '-' }, memoryIO('new\n'));
    expect(patch.spec).toEqual({ config: { systemPrompt: 'new\n' } }); expect(patch.metadata.resourceVersion).toBe('42');
    expect(patch.status).toBeUndefined(); expect(existing).toEqual(before);
    expect(mergePatch(existing, patch).spec!.config).toEqual({ skills: ['search'], nested: { setting: true }, systemPrompt: 'new\n', image: 'agent:v1' });
  });
  test('an empty prompt explicitly clears the prompt', async () => {
    const patch = await updateResource('agent', await agent({ prompt: 'old' }), { prompt: '' }, memoryIO());
    expect(patch.spec).toEqual({ config: { systemPrompt: '' } });
  });
  const modes = [
    ['deploymentRef', {}], ['externalAPI', external()], ['gatewayEndpoint', gateway()],
  ] as const;
  for (const [oldKind, oldFlags] of modes) {
    for (const [newKind, newFlags] of modes) {
      if (oldKind === newKind) continue;
      test(`binding transition ${oldKind} to ${newKind} removes the old union member`, async () => {
        const existing = withServerFields(await agent(oldFlags));
        const patch = await updateResource('agent', existing, newKind === 'deploymentRef' ? { 'model-ref': 'replacement' } : newFlags, memoryIO());
        expect(patch.spec?.model[oldKind]).toBeNull();
        expect(Object.keys(mergePatch(existing, patch).spec!.model)).toEqual([newKind]);
        expect(patch.spec?.framework).toBeUndefined(); expect(patch.spec?.lifecycle).toBeUndefined(); expect(patch.spec?.config).toBeUndefined();
      });
    }
  }
  test('partial external binding edits preserve unsupplied URL, API and credentials', async () => {
    const existing = await agent(external({ 'model-credential': 'existing/key' }));
    const patch = await updateResource('agent', existing, { 'model-id': 'new-model' }, memoryIO());
    expect(patch.spec).toEqual({ model: { externalAPI: { modelName: 'new-model' } } });
    expect(mergePatch(existing, patch).spec!.model.externalAPI).toEqual({ ...existing.spec?.model.externalAPI, modelName: 'new-model' });
    const next = await updateResource('agent', existing, { 'model-api': 'azure-openai', 'model-credential': 'new/key' }, memoryIO());
    expect(next.spec).toEqual({ model: { externalAPI: { type: 'azureOpenAI', credentialsRef: { name: 'new', key: 'key' } } } });
  });
  test('partial gateway edits do not invent a new gateway', async () => {
    const existing = await agent(gateway());
    const patch = await updateResource('agent', existing, { 'gateway-listener': 'https', 'model-id': 'new-model' }, memoryIO());
    expect(patch.spec).toEqual({ model: { gatewayEndpoint: { gatewayRef: { listenerName: 'https' }, modelName: 'new-model' } } });
    expect(mergePatch(existing, patch).spec!.model.gatewayEndpoint.gatewayRef).toEqual({ name: 'edge', listenerName: 'https' });
  });
  test('unqualified references reset a previously explicit namespace', async () => {
    const existing = await agent({ 'model-ref': 'other/demo' });
    const patch = await updateResource('agent', existing, { 'model-ref': 'local' }, memoryIO());
    expect(patch.spec?.model.deploymentRef).toEqual({ name: 'local', namespace: null });
    expect(mergePatch(existing, patch).spec!.model.deploymentRef).toEqual({ name: 'local' });
    const oldGateway = await agent(gateway({ 'model-gateway': 'other/edge', 'gateway-listener': 'old' }));
    const gatewayPatch = await updateResource('agent', oldGateway, { 'model-gateway': 'new-edge' }, memoryIO());
    expect(mergePatch(oldGateway, gatewayPatch).spec!.model.gatewayEndpoint.gatewayRef).toEqual({ name: 'new-edge' });
  });
  test('updates reject incomplete new binding modes and conflicts', async () => {
    const existing = await agent();
    await invalid(updateResource('agent', existing, { 'model-url': 'https://api.example.test/v1' }, memoryIO()), '--model-api');
    await invalid(updateResource('agent', existing, { 'model-id': 'no-endpoint' }, memoryIO()));
    await invalid(updateResource('agent', existing, { 'model-ref': 'new', 'model-gateway': 'edge' }, memoryIO()));
  });
  for (const noun of ['model', 'agent'] as const) {
    for (const key of ['id', 'provider', 'engine', 'model-source', 'framework', 'mode', 'model-path', 'revision', 'file', 'storage-size', 'storage-class', 'artifact-image', 'service-account']) {
      test(`${noun} update rejects immutable --${key} even if it matches`, async () => {
        const existing = await (noun === 'model' ? model() : agent());
        await invalid(updateResource(noun, existing, { [key]: key === 'id' ? 'org/model' : 'same' }, memoryIO()), 'immutable');
      });
    }
    test(`${noun} update cannot be empty or global flags only`, async () => {
      const existing = await (noun === 'model' ? model() : agent());
      await invalid(updateResource(noun, existing, {}, memoryIO()), 'mutable');
      await invalid(updateResource(noun, existing, { wait: false, output: 'json', namespace: 'elsewhere', timeout: '1m' }, memoryIO()), 'mutable');
    });
  }
  for (const flags of [{ image: 'agent:v2' }, { cpu: '2' }, { replicas: '2' }, { task: 'new' }, { 'config-file': '-' }, { '__preset-config': '{}' }]) test(`agent update rejects nonmutable flags ${JSON.stringify(flags)}`, async () => {
    await invalid(updateResource('agent', await agent(), flags, memoryIO()));
  });
  test('one-shot jobs cannot be updated even with mutable fields', async () => {
    const existing = await agent({ mode: 'once', task: 'Do the work.' });
    await invalid(updateResource('agent', existing, { prompt: 'new' }, memoryIO()), 'One-shot');
  });
  test('mismatched resource kinds reject before constructing a patch', async () => { await invalid(updateResource('agent', await model(), { prompt: 'new' }, memoryIO()), 'not a agent'); });
});


describe('manifest contract edge cases', () => {
  test('staged HF supports an explicitly selected JSON credential key', async () => {
    const result = await model({ revision: 'main', credential: 'cloud/credentials' });
    expect(result.spec?.model.artifact.credentialsRef).toEqual({ name: 'cloud', key: 'credentials' });
    expect(result.spec?.secrets).toBeUndefined();
  });
  test('HTTPS file paths preserve Unicode rather than storing URL-encoded filenames', async () => {
    const result = await model({ id: 'https://models.example.test/模型.gguf' });
    expect(result.spec?.model.id).toBe('/model-cache/artifacts/模型.gguf');
  });
  test('PVC paths retain their actual file names', async () => {
    const result = await model({ id: 'pvc://weights/模型' });
    expect(result.spec?.model.id).toBe('/model-cache/模型');
  });
  test('framework and reference namespaces must be DNS labels', async () => {
    await invalid(agent({ framework: 'bad.framework' }));
    await invalid(agent({ 'model-ref': 'bad.namespace/model' }));
    await invalid(agent(gateway({ 'model-gateway': 'bad.namespace/gateway' })));
  });
  test('OCI and downloader registries must be valid hostnames', async () => {
    await invalid(model({ id: 'oci://invalid..example.test/model:v1' }));
    await invalid(model({ id: 's3://bucket/model', 'artifact-image': 'invalid..example.test/loader:v1' }));
  });
  for (const config of [{ systemPrompt: 42 }, { task: {} }, { image: null }, { image: 'https://example.test/image' }]) {
    test(`rejects invalid managed configuration fields ${JSON.stringify(config)}`, async () => {
      await invalid(agent({ '__preset-config': JSON.stringify(config) }));
    });
  }
});
