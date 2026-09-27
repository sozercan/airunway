import { afterEach, beforeEach, describe, expect, spyOn, test } from 'bun:test';
import { mkdtemp, mkdir, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { dump } from 'js-yaml';
import { resolvePreset, runManagement } from './management';
import { CLIError, resourceTypes, type ClusterClient, type CommandContext, type Flags, type RequestOptions, type Resource, type ResourceType } from './types';

type Call = { op: string; type?: ResourceType; name?: string; namespace?: string; body?: unknown; options?: RequestOptions; path?: string; uid?: string; dry?: boolean };
const managedBy = 'app.kubernetes.io/managed-by';
const credentialLabel = 'airunway.ai/credential-type';
const token = 'test-input-not-a-real-credential';
const secret = (type = 'huggingface'): Resource => ({ apiVersion: 'v1', kind: 'Secret', type: 'Opaque', metadata: {
  name: 'token', namespace: 'team', uid: 'secret-uid', resourceVersion: '7',
  labels: { [managedBy]: 'airunway-cli', [credentialLabel]: type, private: token },
  annotations: { 'kubectl.kubernetes.io/last-applied-configuration': token },
}, data: { HF_TOKEN: Buffer.from(token).toString('base64') }, stringData: { HF_TOKEN: token } });
const model = (name = 'model'): Resource => ({ apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name, namespace: 'team' }, spec: { model: { id: 'Qwen/Qwen3-0.6B', source: 'huggingface' }, engine: { type: 'vllm' } } });
const agent = (name = 'agent', lifecycle = 'deployment'): Resource => ({ apiVersion: 'airunway.ai/v1alpha1', kind: 'AgentDeployment', metadata: { name, namespace: 'team' }, spec: { framework: { name: 'openclaw' }, lifecycle, model: { deploymentRef: { name: 'model' } }, config: { systemPrompt: 'Answer questions.' } } });
const framework = (name = 'openclaw', entries: unknown = [{ name: 'helper', title: 'Helper' }], annotation = 'airunway.ai/agent-catalog'): Resource => ({ apiVersion: 'airunway.ai/v1alpha1', kind: 'AgentProviderConfig', metadata: { name, annotations: { [annotation]: JSON.stringify(entries), 'airunway.ai/install-instructions': 'not executed' } }, spec: { capabilities: { backend: 'container' } }, status: { ready: true } });

class FakeClient implements ClusterClient {
  readonly namespace = 'team';
  readonly context = 'test-context';
  calls: Call[] = [];
  resources: Resource[] = [];
  failure?: (call: Call) => void;
  private record(call: Call): void { this.calls.push(call); this.failure?.(call); }
  async request<T>(method: string, path: string, body?: unknown, options?: RequestOptions): Promise<T> {
    this.record({ op: method, path, body, options });
    return structuredClone(body) as T;
  }
  async raw(): Promise<Response> { throw new Error('No network access in management tests'); }
  async list(type: ResourceType, namespace?: string, query?: RequestOptions['query']): Promise<Resource[]> {
    this.record({ op: 'list', type, namespace, options: { query } });
    return this.resources.filter(item => item.kind === type.kind && (!type.namespaced || item.metadata.namespace === namespace));
  }
  async get(type: ResourceType, name: string, namespace?: string): Promise<Resource> {
    this.record({ op: 'get', type, name, namespace });
    const found = this.resources.find(item => item.kind === type.kind && item.metadata.name === name && (!type.namespaced || item.metadata.namespace === namespace));
    if (!found) throw new CLIError('Not found', 1, 'HTTP_404');
    return structuredClone(found);
  }
  async create(resource: Resource, dry?: boolean): Promise<Resource> {
    this.record({ op: 'create', body: resource, dry });
    return structuredClone(resource);
  }
  async patch(type: ResourceType, name: string, body: unknown, namespace?: string, dry?: boolean): Promise<Resource> {
    this.record({ op: 'patch', type, name, body, namespace, dry });
    return structuredClone(this.resources.find(item => item.kind === type.kind && item.metadata.name === name)!);
  }
  async delete(type: ResourceType, name: string, namespace?: string, uid?: string): Promise<void> { this.record({ op: 'delete', type, name, namespace, uid }); }
}
let client: FakeClient;
let directory: string;
let stdout: string;
let stderr: string;
let fetchSpy: ReturnType<typeof spyOn<typeof globalThis, 'fetch'>>;
function context(flags: Flags = {}): CommandContext {
  return { flags: { output: 'json', ...flags }, io: { out: value => { stdout += value; }, err: value => { stderr += value; }, input: async () => token + '\n', interactive: false },
    signal: new AbortController().signal, client: () => client, namespace: 'team', context: client.context };
}
function writes(): Call[] { return client.calls.filter(call => ['create', 'patch', 'delete', 'PATCH', 'POST', 'DELETE'].includes(call.op)); }
async function file(value: unknown, name = 'resource.yaml'): Promise<string> {
  const path = join(directory, name);
  await writeFile(path, name.endsWith('.json') ? JSON.stringify(value) : dump(value));
  return path;
}
async function rejectedApply(value: unknown, pattern: string): Promise<void> {
  await expect(runManagement(['apply'], context({ file: await file(value) }))).rejects.toThrow(pattern);
  expect(client.calls).toHaveLength(0);
}
beforeEach(async () => {
  directory = await mkdtemp(join(tmpdir(), 'airunway-management-'));
  client = new FakeClient(); stdout = ''; stderr = '';
  fetchSpy = spyOn(globalThis, 'fetch').mockImplementation(Object.assign(async () => { throw new Error('Unexpected network call'); }, { preconnect: () => {} }));
});
afterEach(async () => { fetchSpy.mockRestore(); await rm(directory, { recursive: true, force: true }); });

describe('dispatch', () => {
  test('unsupported nouns return false without connecting', async () => {
    expect(await runManagement(['model', 'get', 'foo'], context())).toBe(false);
    expect(client.calls).toHaveLength(0);
  });
  for (const words of [['credential', 'install'], ['provider', 'install'], ['framework', 'delete'], ['catalog', 'model', 'list'], ['catalog', 'agent', 'search'], ['catalog', 'unknown']]) {
    test(`rejects unsupported action ${words.join(' ')}`, async () => {
      await expect(runManagement(words, context())).rejects.toBeInstanceOf(CLIError);
      expect(client.calls).toHaveLength(0);
    });
  }
  test('rejects invalid output before mutation', async () => {
    await expect(runManagement(['credential', 'create', 'token'], context({ output: 'bad', type: 'huggingface', 'from-file': '-' }))).rejects.toThrow('--output');
    expect(writes()).toHaveLength(0);
  });
});

describe('credentials', () => {
  for (const [type, key] of [['huggingface', 'HF_TOKEN'], ['api-key', 'API_KEY']]) {
    test(`creates ${type} from stdin and emits only metadata`, async () => {
      expect(await runManagement(['credential', 'create', 'token'], context({ type, 'from-file': '-' }))).toBe(true);
      const call = writes()[0];
      expect(call.op).toBe('create');
      expect(call.body).toEqual({ apiVersion: 'v1', kind: 'Secret', type: 'Opaque', metadata: { name: 'token', namespace: 'team', labels: { [managedBy]: 'airunway-cli', [credentialLabel]: type } }, data: { [key]: Buffer.from(token).toString('base64') } });
      expect(stdout + stderr).not.toContain(token);
      expect(stdout).not.toContain(Buffer.from(token).toString('base64'));
      expect(Object.keys(JSON.parse(stdout))).toEqual(['apiVersion', 'kind', 'metadata']);
      expect(fetchSpy).not.toHaveBeenCalled();
    });
  }
  test('artifact creates and updates source-specific JSON under credentials', async () => {
    const value = { aws_access_key_id: 'example-id', aws_secret_access_key: token, region_name: 'us-west-2' };
    const ctx = context({ type: 'artifact', 'from-file': '-' });
    ctx.io.input = async () => JSON.stringify(value, null, 2) + '\n';
    await runManagement(['credential', 'create', 'token'], ctx);
    const created = writes()[0].body as Resource;
    expect(created.metadata.labels?.[credentialLabel]).toBe('artifact');
    expect(JSON.parse(Buffer.from(created.data!.credentials, 'base64').toString())).toEqual(value);
    expect(Object.keys(created.data!)).toEqual(['credentials']);
    expect(stdout + stderr).not.toContain(token);
    client.resources = [created]; created.metadata.resourceVersion = '3';
    stdout = ''; await runManagement(['credential', 'update', 'token'], ctx);
    expect(writes()[1]).toMatchObject({ op: 'patch', body: { metadata: { resourceVersion: '3' }, data: { credentials: created.data!.credentials } } });
    expect(stdout + stderr).not.toContain(token);
  });
  test('artifact enforces the loader size limit for stdin and files', async () => {
    const overhead = JSON.stringify({ token: '' }).length;
    const atLimit = JSON.stringify({ token: 'x'.repeat(64 * 1024 - overhead) });
    const ctx = context({ type: 'artifact', 'from-file': '-' }); ctx.io.input = async () => atLimit;
    await runManagement(['credential', 'create', 'token'], ctx);
    expect(Buffer.from((writes()[0].body as Resource).data!.credentials, 'base64').byteLength).toBe(64 * 1024);
    client.calls = [];
    const oversized = JSON.stringify({ token: 'x'.repeat(64 * 1024 - overhead + 1) });
    ctx.io.input = async () => oversized;
    await expect(runManagement(['credential', 'create', 'token'], ctx)).rejects.toThrow('64 KiB');
    const path = join(directory, 'oversized.json'); await writeFile(path, oversized);
    await expect(runManagement(['credential', 'create', 'token'], context({ type: 'artifact', 'from-file': path }))).rejects.toThrow('64 KiB');
    expect(client.calls).toHaveLength(0);
  });
  test('artifact rejects malformed, empty, non-object and prototype input without excerpts', async () => {
    for (const value of [`{${token}`, '{}', '[]', 'null', JSON.stringify(token), '{"__proto__": {"secret": "example"}}']) {
      const ctx = context({ type: 'artifact', 'from-file': '-' }); ctx.io.input = async () => value;
      await expect(runManagement(['credential', 'create', 'token'], ctx)).rejects.toThrow('nonempty JSON object');
    }
    expect(client.calls).toHaveLength(0); expect(stdout + stderr).not.toContain(token);
  });
  test('reads a regular file without putting its value in argv', async () => {
    const path = join(directory, 'token'); await writeFile(path, token + '\r\n');
    await runManagement(['credential', 'create', 'token'], context({ type: 'api-key', 'from-file': path }));
    expect((writes()[0].body as Resource).data?.API_KEY).toBe(Buffer.from(token).toString('base64'));
  });
  test('rejects token values in positionals or unsupported flags without echoing them', async () => {
    for (const [words, flags] of [
      [['credential', 'create', 'token', token], { type: 'api-key' }],
      [['credential', 'create', 'token'], { type: 'api-key', token }],
    ] as [string[], Flags][]) {
      try { await runManagement(words, context(flags)); throw new Error('expected rejection'); }
      catch (error) { expect(error).toBeInstanceOf(CLIError); expect((error as Error).message).not.toContain(token); }
    }
    expect(client.calls).toHaveLength(0);
  });
  test('requires a recognized type and file source', async () => {
    await expect(runManagement(['credential', 'create', 'token'], context({ type: 'other', 'from-file': '-' }))).rejects.toThrow('--type');
    await expect(runManagement(['credential', 'create', 'token'], context({ type: 'api-key' }))).rejects.toThrow('--from-file');
    expect(client.calls).toHaveLength(0);
  });
  test('rejects empty, multiline and oversized input without echoing it', async () => {
    for (const input of ['', 'first\nsecond', 'x'.repeat(4 * 1024 * 1024 + 1)]) {
      const ctx = context({ type: 'api-key', 'from-file': '-' }); ctx.io.input = async () => input;
      await expect(runManagement(['credential', 'create', 'token'], ctx)).rejects.toBeInstanceOf(CLIError);
    }
    expect(writes()).toHaveLength(0);
  });
  for (const output of ['json', 'yaml', 'text']) {
    test(`get redacts values, arbitrary labels and annotations in ${output}`, async () => {
      client.resources = [secret()];
      await runManagement(['credential', 'get', 'token'], context({ output }));
      expect(stdout + stderr).not.toContain(token);
      expect(stdout).not.toContain(Buffer.from(token).toString('base64'));
      expect(stdout).not.toContain('last-applied');
      expect(stdout).not.toContain('private:');
      expect(stdout).toContain('airunway-cli');
    });
  }
  test('lists only CLI credentials in the context namespace using label selectors', async () => {
    const foreign = secret(); foreign.metadata.name = 'foreign'; foreign.metadata.labels![managedBy] = 'other';
    const otherNs = secret(); otherNs.metadata.namespace = 'other';
    client.resources = [secret(), foreign, otherNs];
    await runManagement(['credential', 'list'], context());
    expect(JSON.parse(stdout)).toHaveLength(1);
    expect(client.calls[0]).toMatchObject({ op: 'list', namespace: 'team', options: { query: { labelSelector: `${managedBy}=airunway-cli,${credentialLabel}` } } });
  });
  test('refuses all namespaces and missing namespaces', async () => {
    await expect(runManagement(['credential', 'list'], context({ 'all-namespaces': true }))).rejects.toBeInstanceOf(CLIError);
    const ctx = context(); ctx.namespace = '';
    await expect(runManagement(['credential', 'list'], ctx)).rejects.toThrow('namespace');
    expect(client.calls).toHaveLength(0);
  });
  for (const action of ['get', 'update', 'delete']) {
    test(`${action} refuses non-CLI secrets`, async () => {
      const item = secret(); item.metadata.labels![managedBy] = 'other'; client.resources = [item];
      await expect(runManagement(['credential', action, 'token'], context(action === 'update' ? { 'from-file': '-' } : {}))).rejects.toThrow('not a CLI-managed');
      expect(writes()).toHaveLength(0);
    });
  }
  test('update sends resourceVersion and only the intended key', async () => {
    client.resources = [secret('api-key')];
    await runManagement(['credential', 'update', 'token'], context({ 'from-file': '-' }));
    expect(writes()[0]).toMatchObject({ op: 'patch', name: 'token', namespace: 'team', body: { metadata: { resourceVersion: '7' }, data: { API_KEY: Buffer.from(token).toString('base64') } } });
    expect(stdout).not.toContain(token);
  });
  test('update rejects type changes and missing versions', async () => {
    client.resources = [secret()];
    await expect(runManagement(['credential', 'update', 'token'], context({ type: 'api-key', 'from-file': '-' }))).rejects.toThrow('type cannot change');
    delete client.resources[0].metadata.resourceVersion;
    await expect(runManagement(['credential', 'update', 'token'], context({ 'from-file': '-' }))).rejects.toThrow('resourceVersion');
    expect(writes()).toHaveLength(0);
  });
  test('update does not retry optimistic concurrency conflicts', async () => {
    client.resources = [secret()]; client.failure = call => { if (call.op === 'patch') throw new CLIError('Changed concurrently', 5, 'HTTP_409'); };
    await expect(runManagement(['credential', 'update', 'token'], context({ 'from-file': '-' }))).rejects.toMatchObject({ code: 'HTTP_409' });
    expect(writes()).toHaveLength(1);
  });
  for (const dry of ['client', 'server']) {
    test(`${dry} dry-run credential creation never discloses the token`, async () => {
      await runManagement(['credential', 'create', 'token'], context({ type: 'huggingface', 'from-file': '-', 'dry-run': dry }));
      expect(writes()).toHaveLength(dry === 'client' ? 0 : 1);
      if (dry === 'server') expect(writes()[0].dry).toBe(true);
      expect(stdout).not.toContain(token);
    });
    test(`${dry} dry-run credential update is metadata-only`, async () => {
      client.resources = [secret()];
      await runManagement(['credential', 'update', 'token'], context({ 'from-file': '-', 'dry-run': dry }));
      expect(writes()).toHaveLength(dry === 'client' ? 0 : 1);
      if (dry === 'server') expect(writes()[0].dry).toBe(true);
      expect(stdout).not.toContain(token);
    });
  }
  const referenced: [string, Resource][] = [
    ['hugging face', { ...model(), spec: { secrets: { huggingFaceToken: 'token' } } }],
    ['artifact', { ...model(), spec: { model: { artifact: { credentialsRef: { name: 'token', key: 'HF_TOKEN' } } } } }],
    ['environment', { ...model(), spec: { env: [{ name: 'HF_TOKEN', valueFrom: { secretKeyRef: { name: 'token', key: 'HF_TOKEN' } } }] } }],
    ['pull secret', { ...model(), spec: { imagePullSecrets: [{ name: 'token' }] } }],
    ['agent external model', { ...agent(), spec: { model: { externalAPI: { credentialsRef: { name: 'token', key: 'API_KEY' } } } } }],
    ['agent resolved model', { ...agent(), status: { modelBinding: { credentialsRef: { name: 'token', key: 'API_KEY' } } } }],
  ];
  for (const [label, resource] of referenced) {
    test(`delete blocks ${label} references`, async () => {
      client.resources = [secret(), resource];
      await expect(runManagement(['credential', 'delete', 'token'], context())).rejects.toMatchObject({ code: 'IN_USE' });
      expect(writes()).toHaveLength(0);
    });
  }
  test('delete scans both collections in the same namespace then uses UID', async () => {
    const other = { ...model(), metadata: { name: 'model', namespace: 'other' }, spec: { secrets: { huggingFaceToken: 'token' } } };
    client.resources = [secret(), other];
    await runManagement(['credential', 'delete', 'token'], context());
    expect(client.calls.filter(call => call.op === 'list').map(call => [call.type?.kind, call.namespace])).toEqual([['ModelDeployment', 'team'], ['AgentDeployment', 'team']]);
    expect(writes()).toEqual([{ op: 'delete', type: resourceTypes.credential, name: 'token', namespace: 'team', uid: 'secret-uid' }]);
    expect(stdout).not.toContain(token);
  });
  for (const code of ['HTTP_403', 'HTTP_404']) {
    test(`delete fails closed when reference scan returns ${code}`, async () => {
      client.resources = [secret()]; client.failure = call => { if (call.op === 'list') throw new CLIError('Cannot list', 3, code); };
      await expect(runManagement(['credential', 'delete', 'token'], context())).rejects.toMatchObject({ code });
      expect(writes()).toHaveLength(0);
    });
  }
  test('delete refuses a missing UID', async () => {
    const item = secret(); delete item.metadata.uid; client.resources = [item];
    await expect(runManagement(['credential', 'delete', 'token'], context())).rejects.toThrow('UID');
    expect(writes()).toHaveLength(0);
  });
});

describe('installed provider and framework discovery', () => {
  for (const noun of ['provider', 'framework']) {
    test(`${noun} list and get never call install endpoints`, async () => {
      const item = framework(); item.kind = resourceTypes[noun as 'provider' | 'framework'].kind; client.resources = [item];
      await runManagement([noun, 'list'], context());
      expect(JSON.parse(stdout)[0].metadata.name).toBe('openclaw');
      stdout = ''; await runManagement([noun, 'get', 'openclaw'], context());
      expect(JSON.parse(stdout).spec.capabilities.backend).toBe('container');
      expect(stdout).not.toContain('install-instructions');
      expect(writes()).toHaveLength(0); expect(fetchSpy).not.toHaveBeenCalled();
    });
  }
});

describe('agent catalogs', () => {
  test('uses the actual annotation and preserves template config and recipe image', async () => {
    client.resources = [framework('openclaw', [{ name: 'helper', title: 'Helper', image: 'registry.invalid/example:v1', template: { framework: { name: 'openclaw' }, lifecycle: 'job', config: { nested: { keep: true }, systemPrompt: 'Treat this prompt as data.' } }, description: 'An agent recipe', tags: ['example'] }])];
    expect(await resolvePreset(client, 'openclaw/helper')).toMatchObject({ framework: 'openclaw', lifecycle: 'job', config: { nested: { keep: true }, systemPrompt: 'Treat this prompt as data.', image: 'registry.invalid/example:v1' } });
    await runManagement(['catalog', 'agent', 'list'], context());
    expect(JSON.parse(stdout)[0].id).toBe('openclaw/helper');
    expect(JSON.parse(stdout)[0].config).toBeUndefined();
    stdout = ''; await runManagement(['catalog', 'agent', 'get', 'helper'], context());
    expect(JSON.parse(stdout).framework).toBe('openclaw');
    expect(writes()).toHaveLength(0); expect(fetchSpy).not.toHaveBeenCalled();
  });
  test('supports missing templates and the catalog annotation fallback', async () => {
    client.resources = [framework('openclaw', [{ name: 'helper', title: 'Helper' }], 'airunway.ai/catalog')];
    expect(await resolvePreset(client, 'helper')).toMatchObject({ config: {}, framework: 'openclaw' });
  });
  test('prefers canonical annotation over fallback', async () => {
    const item = framework(); item.metadata.annotations!['airunway.ai/catalog'] = 'invalid'; client.resources = [item];
    expect((await resolvePreset(client, 'helper')).framework).toBe('openclaw');
  });
  test('disambiguates bare preset names', async () => {
    client.resources = [framework('one'), framework('two')];
    await expect(resolvePreset(client, 'helper')).rejects.toThrow('ambiguous');
    expect((await resolvePreset(client, 'one/helper')).framework).toBe('one');
  });
  test('not found never installs providers', async () => {
    await expect(resolvePreset(client, 'missing')).rejects.toMatchObject({ code: 'NOT_FOUND' });
    expect(writes()).toHaveLength(0);
  });
  const invalid = [
    { entries: 'object, not an array' },
    [{ name: 'helper' }],
    [{ name: 'helper', title: 'Helper', template: { config: [] } }],
    [{ name: 'helper', title: 'Helper', template: { framework: { name: 'other' } } }],
    [{ name: 'helper', title: 'Helper', template: { config: { apiKey: token } } }],
    [{ name: 'helper', title: 'Helper' }, { name: 'helper', title: 'Duplicate' }],
    JSON.parse('[{"name":"helper","title":"Helper","template":{"config":{"__proto__":{"polluted":true}}}}]'),
  ];
  for (const [index, entries] of invalid.entries()) {
    test(`rejects malformed or unsafe catalog ${index} without echoing input`, async () => {
      client.resources = [framework('openclaw', entries)];
      await expect(resolvePreset(client, 'helper')).rejects.toMatchObject({ code: 'CATALOG' });
      expect(stdout + stderr).not.toContain(token); expect(writes()).toHaveLength(0);
    });
  }
  test('JSON syntax errors never disclose annotation source', async () => {
    const item = framework(); item.metadata.annotations!['airunway.ai/agent-catalog'] = `{${token}`; client.resources = [item];
    await expect(resolvePreset(client, 'helper')).rejects.toThrow('Invalid agent catalog for framework "openclaw".');
  });
});

describe('model catalogs', () => {
  test('bundled get works offline without cluster access', async () => {
    await runManagement(['catalog', 'model', 'get', 'hf://Qwen/Qwen3-0.6B'], context());
    expect(JSON.parse(stdout)).toMatchObject({ id: 'Qwen/Qwen3-0.6B', source: 'bundled' });
    expect(fetchSpy).not.toHaveBeenCalled(); expect(client.calls).toHaveLength(0);
  });
  test('search uses fixed HTTPS endpoint, timeout signal and bounded metadata results', async () => {
    fetchSpy.mockResolvedValue(new Response(JSON.stringify([{ id: 'Qwen/Qwen3-0.6B', downloads: 10, cardData: { instructions: 'ignored' } }, { id: 'Qwen/Test', pipeline_tag: 'text-generation', likes: 3, endpoint: 'http://127.0.0.1', instructions: 'ignored' }])));
    await runManagement(['catalog', 'model', 'search', 'hf://Qwen'], context());
    const [url, init] = fetchSpy.mock.calls[0];
    expect(String(url)).toBe('https://huggingface.co/api/models?search=Qwen&limit=20');
    expect(init).toMatchObject({ redirect: 'error', headers: { Accept: 'application/json' } });
    expect(init?.signal).toBeInstanceOf(AbortSignal);
    const results = JSON.parse(stdout);
    expect(results.filter((item: Resource) => item.id === 'Qwen/Qwen3-0.6B')).toHaveLength(1);
    expect(results.find((item: Resource) => item.id === 'Qwen/Qwen3-0.6B').source).toBe('bundled');
    expect(stdout).not.toContain('ignored'); expect(stdout).not.toContain('127.0.0.1');
    expect(client.calls).toHaveLength(0);
  });
  test('fetches unknown model metadata without arbitrary fields', async () => {
    fetchSpy.mockResolvedValue(Response.json({ id: 'org/model', pipeline_tag: 'text-generation', downloads: 12, gated: 'manual', config: { secret: token } }));
    await runManagement(['catalog', 'model', 'get', 'org/model'], context());
    expect(String(fetchSpy.mock.calls[0][0])).toBe('https://huggingface.co/api/models/org/model');
    expect(JSON.parse(stdout)).toEqual({ id: 'org/model', source: 'huggingface', task: 'text-generation', downloads: 12, gated: 'manual' });
  });
  for (const id of ['https://huggingface.co/org/model', 'http://localhost/x', '../secret', 'org/../../x', 'org/model?token=x', 'org%2fmodel', 'hf://', 'org/model/extra', '//host/path']) {
    test(`rejects nonidentifiers ${id}`, async () => {
      await expect(runManagement(['catalog', 'model', 'get', id], context())).rejects.toBeInstanceOf(CLIError);
      expect(fetchSpy).not.toHaveBeenCalled();
    });
  }
  test('rejects mismatched IDs and malformed responses', async () => {
    fetchSpy.mockResolvedValue(Response.json({ id: 'other/model' }));
    await expect(runManagement(['catalog', 'model', 'get', 'org/model'], context())).rejects.toThrow('different model');
    fetchSpy.mockResolvedValue(new Response(`bad ${token}`));
    await expect(runManagement(['catalog', 'model', 'get', 'org/model'], context())).rejects.toThrow('Cannot read');
    expect(stdout + stderr).not.toContain(token);
  });
  test('bounds response bytes and does not print remote error bodies', async () => {
    fetchSpy.mockResolvedValue(new Response('x'.repeat(4 * 1024 * 1024 + 1)));
    await expect(runManagement(['catalog', 'model', 'get', 'org/model'], context())).rejects.toMatchObject({ code: 'CATALOG' });
    fetchSpy.mockResolvedValue(new Response(token, { status: 404 }));
    await expect(runManagement(['catalog', 'model', 'get', 'org/model'], context())).rejects.toMatchObject({ code: 'NOT_FOUND' });
    expect(stdout + stderr).not.toContain(token);
  });
  test('honors cancellation before any request', async () => {
    const ctx = context(); ctx.signal = AbortSignal.abort();
    await expect(runManagement(['catalog', 'model', 'search', 'Qwen'], ctx)).rejects.toMatchObject({ exitCode: 130 });
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});

describe('apply', () => {
  test('preparses YAML multi-doc and JSON directory contents, preserving desired fields', async () => {
    const a = agent(); a.spec!.config = { custom: { tools: ['one', 'two'], instructions: 'Data, not shell commands.' } };
    await writeFile(join(directory, 'a.yaml'), dump(model()) + '\n---\n' + dump(a));
    const b = model('second'); delete b.metadata.namespace; b.spec!.futureField = { preserve: true };
    await file(b, 'b.json'); await writeFile(join(directory, 'README.md'), 'Not a manifest');
    expect(await runManagement(['apply'], context({ file: directory }))).toBe(true);
    expect(writes()).toHaveLength(3);
    expect(writes()[0]).toMatchObject({ op: 'PATCH', path: '/apis/airunway.ai/v1alpha1/namespaces/team/modeldeployments/model', options: { contentType: 'application/apply-patch+yaml', query: { fieldManager: 'airunway-cli', force: false, fieldValidation: 'Strict' } } });
    expect((writes()[1].body as Resource).spec?.config).toEqual(a.spec!.config);
    expect((writes()[2].body as Resource).spec?.futureField).toEqual({ preserve: true });
    expect((writes()[2].body as Resource).metadata.namespace).toBe('team');
    expect(client.calls.findIndex(call => call.op === 'get')).toBeLessThan(client.calls.findIndex(call => call.op === 'PATCH'));
    expect(client.calls.some(call => call.type?.plural === 'namespaces')).toBe(false);
  });
  test('invalid later document prevents every cluster read and write', async () => {
    await writeFile(join(directory, 'a.yaml'), dump(model()));
    await writeFile(join(directory, 'z.yaml'), dump(secret()));
    await expect(runManagement(['apply'], context({ file: directory }))).rejects.toBeInstanceOf(CLIError);
    expect(client.calls).toHaveLength(0);
  });
  test('syntax error after a valid document prevents writes and does not echo the source', async () => {
    await writeFile(join(directory, 'a.yaml'), dump(model()) + `\n---\ninvalid: [${token}`);
    await expect(runManagement(['apply'], context({ file: join(directory, 'a.yaml') }))).rejects.toThrow('Cannot parse apply input');
    expect(client.calls).toHaveLength(0); expect(stdout + stderr).not.toContain(token);
  });
  test('client dry-run validates all documents without constructing the client', async () => {
    const ctx = context({ file: await file(model()), 'dry-run': 'client' }); ctx.client = () => { throw new Error('must stay offline'); };
    await runManagement(['apply'], ctx);
    expect(JSON.parse(stdout)[0]).toEqual(model()); expect(writes()).toHaveLength(0);
  });
  test('server dry-run sends dryRun=All with force=false', async () => {
    await runManagement(['apply'], context({ file: await file(model()), 'dry-run': 'server' }));
    expect(writes()[0].options?.query).toMatchObject({ dryRun: 'All', force: false, fieldManager: 'airunway-cli' });
  });
  test('rejects explicit cross-namespace documents', async () => {
    const item = model(); item.metadata.namespace = 'other';
    await rejectedApply(item, 'namespace differs');
  });
  test('rejects unsupported API, kinds, arrays and invalid names', async () => {
    for (const value of [{ ...model(), apiVersion: 'other/v1' }, { ...model(), kind: 'Job' }, [model()], { ...model(), metadata: { name: '../bad' } }]) {
      await expect(runManagement(['apply'], context({ file: await file(value) }))).rejects.toBeInstanceOf(CLIError);
    }
    expect(client.calls).toHaveLength(0);
  });
  for (const key of ['resourceVersion', 'ownerReferences', 'uid', 'managedFields', 'generateName']) {
    test(`rejects server-owned metadata ${key}`, async () => {
      const item = model(); item.metadata[key] = key === 'ownerReferences' ? [] : 'value';
      await rejectedApply(item, 'server-owned metadata');
    });
  }
  test('rejects status and last-applied snapshots', async () => {
    await rejectedApply({ ...model(), status: {} }, 'desired state only');
    const item = model(); item.metadata.annotations = { 'kubectl.kubernetes.io/last-applied-configuration': '{}' };
    await rejectedApply(item, 'last-applied');
  });
  test('rejects inline secret-like fields and env values, without confusing refs', async () => {
    const bad = agent(); bad.spec!.config = { apiKey: token };
    await rejectedApply(bad, 'Inline credential');
    const environment = model(); environment.spec!.env = [{ name: 'HF_TOKEN', value: token }];
    await rejectedApply(environment, 'Inline credential');
    const good = model(); good.spec!.secrets = { huggingFaceToken: 'token' }; good.spec!.env = [{ name: 'HF_TOKEN', valueFrom: { secretKeyRef: { name: 'token', key: 'HF_TOKEN' } } }];
    await runManagement(['apply'], context({ file: await file(good), 'dry-run': 'client' }));
    expect(JSON.parse(stdout)[0].spec).toEqual(good.spec);
  });
  test('rejects generic token fields and credential-bearing URLs', async () => {
    const credentialURL = new URL('https://example.test'); credentialURL.username = 'example'; credentialURL.password = token;
    for (const config of [{ token }, { secretAccessKey: token }, { url: credentialURL.toString() }, { url: 'https://example.test/model?sig=example' }]) {
      const item = agent(); item.spec!.config = config;
      await rejectedApply(item, 'Inline credential');
    }
  });
  test('existing long-running agents use the preflight resourceVersion', async () => {
    const item = agent(); item.metadata.resourceVersion = '17'; client.resources = [item];
    await runManagement(['apply'], context({ file: await file(agent()) }));
    expect((writes()[0].body as Resource).metadata.resourceVersion).toBe('17');
  });
  test('rejects duplicate resources, cyclic aliases and prototype keys', async () => {
    const path = join(directory, 'bad.yaml');
    for (const data of [dump(model()) + '\n---\n' + dump(model()), 'x: &x [*x]', '{"__proto__": {"polluted": true}}']) {
      await writeFile(path, data);
      await expect(runManagement(['apply'], context({ file: path }))).rejects.toBeInstanceOf(CLIError);
    }
    expect(client.calls).toHaveLength(0);
  });
  test('rejects unsupported files, symlinks and empty directories', async () => {
    const input = await file(model(), 'model.yaml');
    const link = join(directory, 'link.yaml'); await symlink(input, link);
    await expect(runManagement(['apply'], context({ file: link }))).rejects.toThrow('symlink');
    const wrong = join(directory, 'input.txt'); await writeFile(wrong, dump(model()));
    await expect(runManagement(['apply'], context({ file: wrong }))).rejects.toThrow('only .yaml');
    const empty = join(directory, 'empty'); await mkdir(empty);
    await expect(runManagement(['apply'], context({ file: empty }))).rejects.toThrow('between 1 and 100');
    expect(client.calls).toHaveLength(0);
  });
  test('a later existing job blocks the entire batch, including model writes', async () => {
    client.resources = [agent('job', 'job')];
    const path = join(directory, 'batch.yaml'); await writeFile(path, dump(model()) + '\n---\n' + dump(agent('job')));
    await expect(runManagement(['apply'], context({ file: path }))).rejects.toThrow('one-shot');
    expect(writes()).toHaveLength(0);
  });
  test('does not change an existing agent into a job or replay a job in server dry-run', async () => {
    client.resources = [agent()];
    await expect(runManagement(['apply'], context({ file: await file(agent('agent', 'job')) }))).rejects.toThrow('one-shot');
    client.resources = [agent('agent', 'job')];
    await expect(runManagement(['apply'], context({ file: await file(agent('agent', 'job')), 'dry-run': 'server' }))).rejects.toThrow('one-shot');
    expect(writes()).toHaveLength(0);
  });
  test('creates a new one-shot agent only through SSA when not already present', async () => {
    await runManagement(['apply'], context({ file: await file(agent('new-job', 'job')) }));
    expect(writes()).toHaveLength(1); expect(writes()[0].op).toBe('PATCH');
  });
  test('returns receipts for completed writes when a later apply fails', async () => {
    const path = join(directory, 'partial.yaml');
    await writeFile(path, dump(model('first')) + '\n---\n' + dump(model('second')));
    client.failure = call => { if (call.op === 'PATCH' && call.path?.endsWith('/second')) throw new CLIError('Forbidden', 3, 'HTTP_403'); };
    await expect(runManagement(['apply'], context({ file: path }))).rejects.toMatchObject({ code: 'HTTP_403' });
    expect(JSON.parse(stdout)).toEqual([{ apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: 'first', namespace: 'team' } }]);
    expect(stderr).toContain('not rolled back');
    expect(writes()).toHaveLength(2);
  });
  test('does not treat forbidden reads as not-found', async () => {
    client.failure = call => { if (call.op === 'get') throw new CLIError('Forbidden', 3, 'HTTP_403'); };
    await expect(runManagement(['apply'], context({ file: await file(agent()) }))).rejects.toMatchObject({ code: 'HTTP_403' });
    expect(writes()).toHaveLength(0);
  });
  test('never forces conflicts, retries admission errors or deletes workloads', async () => {
    for (const code of ['HTTP_409', 'HTTP_422']) {
      client.calls = []; client.failure = call => { if (call.op === 'PATCH') throw new CLIError('Rejected', 5, code); };
      await expect(runManagement(['apply'], context({ file: await file(model()) }))).rejects.toMatchObject({ code });
      expect(writes()).toHaveLength(1); expect(writes()[0].options?.query?.force).toBe(false);
    }
  });
  test('cancellation prevents any writes', async () => {
    const ctx = context({ file: await file(model()) }); ctx.signal = AbortSignal.abort();
    await expect(runManagement(['apply'], ctx)).rejects.toMatchObject({ exitCode: 130 });
    expect(writes()).toHaveLength(0);
  });
});
