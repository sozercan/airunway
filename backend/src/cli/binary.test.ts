import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, test } from 'bun:test';
import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';

// Black-box tests: no imports from the CLI implementation or its helpers.
// AIRUNWAY_TEST_BINARY selects an already-built executable; the default runs
// the real source entrypoint in a separate Bun process.
type Json = string | number | boolean | null | Json[] | JsonObject;
interface JsonObject { [key: string]: Json }
interface APIRequest { method: string; path: string; query: Record<string, string>; body?: JsonObject }
interface Result { code: number | null; signal: NodeJS.Signals | null; stdout: string; stderr: string; forced: boolean }
interface Child { process: ChildProcessWithoutNullStreams; result: Promise<Result>; done: boolean }

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const executable = process.env.AIRUNWAY_TEST_BINARY;
const command = executable ? [resolve(executable)] : [process.execPath, join(root, 'backend/src/index.ts')];
const processLimit = 10000;
const outputLimit = 1024 * 1024;
const formats = ['json', 'yaml', 'text'];
const fixtureToken = 'integration-fixture-not-valid-credentials';
const types: Record<string, { kind: string; apiVersion: string; namespaced: boolean }> = {
  modeldeployments: { kind: 'ModelDeployment', apiVersion: 'airunway.ai/v1alpha1', namespaced: true },
  agentdeployments: { kind: 'AgentDeployment', apiVersion: 'airunway.ai/v1alpha1', namespaced: true },
  inferenceproviderconfigs: { kind: 'InferenceProviderConfig', apiVersion: 'airunway.ai/v1alpha1', namespaced: false },
  agentproviderconfigs: { kind: 'AgentProviderConfig', apiVersion: 'airunway.ai/v1alpha1', namespaced: false },
  secrets: { kind: 'Secret', apiVersion: 'v1', namespaced: true },
};
function object(value: Json | unknown): JsonObject {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error('Expected a JSON object');
  return value as JsonObject;
}
function field(value: Json | undefined, ...keys: string[]): Json | undefined {
  for (const key of keys) {
    if (value === null || typeof value !== 'object' || Array.isArray(value)) return undefined;
    value = value[key];
  }
  return value;
}
function merge(target: JsonObject, patch: JsonObject): JsonObject {
  const result = structuredClone(target);
  for (const [key, value] of Object.entries(patch)) {
    if (value === null) delete result[key];
    else if (typeof value === 'object' && !Array.isArray(value)) {
      const previous = result[key];
      result[key] = merge(previous && typeof previous === 'object' && !Array.isArray(previous) ? previous : {}, value);
    } else result[key] = value;
  }
  return result;
}
function status(ready: boolean, generation: number): JsonObject {
  return { phase: ready ? 'Running' : 'Pending', observedGeneration: generation,
    conditions: [{ type: 'Ready', status: ready ? 'True' : 'False', observedGeneration: generation, reason: ready ? 'MockReady' : 'MockPending' }] };
}
function startAPI() {
  const requests: APIRequest[] = [];
  const documents = new Map<string, JsonObject>();
  let revision = 0;
  let createdReady = false;
  const key = (plural: string, namespace: string, name: string) => `${plural}/${namespace}/${name}`;
  function seed(plural: string, namespace: string, name: string, spec: JsonObject = {}): JsonObject {
    const type = types[plural];
    const value: JsonObject = { apiVersion: type.apiVersion, kind: type.kind,
      metadata: { name, uid: `fixture-${++revision}`, resourceVersion: String(revision), generation: 1 }, spec };
    if (type.namespaced) object(value.metadata).namespace = namespace;
    value.status = status(false, 1);
    documents.set(key(plural, namespace, name), value);
    return value;
  }
  const provider = seed('inferenceproviderconfigs', '', 'vllm', { capabilities: { engines: [{ name: 'vllm', servingModes: ['aggregated', 'disaggregated'], gpuSupport: true, cpuSupport: true }] } });
  provider.status = { ...status(true, 1), ready: true };
  for (const name of ['langgraph', 'openclaw']) {
    const provider = seed('agentproviderconfigs', '', name, { capabilities: { backend: 'container', requiresOperator: false, modelBindingModes: ['deploymentRef', 'externalAPI', 'gatewayEndpoint'], protocols: ['openai'], cpuSupport: true } });
    provider.status = { ...status(true, 1), ready: true };
  }
  function error(code: number, reason: string): Response {
    return Response.json({ apiVersion: 'v1', kind: 'Status', status: 'Failure', code, reason, message: reason }, { status: code });
  }
  const server = Bun.serve({ hostname: '127.0.0.1', port: 0, idleTimeout: 5, async fetch(request) {
    const url = new URL(request.url);
    let body: JsonObject | undefined;
    const raw = await request.text();
    if (raw) {
      try { body = object(JSON.parse(raw)); } catch { return error(400, 'Invalid JSON'); }
    }
    const call: APIRequest = { method: request.method, path: url.pathname, query: Object.fromEntries(url.searchParams), body };
    requests.push(call);
    if (url.pathname === '/version') return Response.json({ major: '1', minor: '32', gitVersion: 'v1.32.0' });
    if (url.pathname === '/api') return Response.json({ kind: 'APIVersions', versions: ['v1'] });
    const group = { name: 'airunway.ai', versions: [{ groupVersion: 'airunway.ai/v1alpha1', version: 'v1alpha1' }], preferredVersion: { groupVersion: 'airunway.ai/v1alpha1', version: 'v1alpha1' } };
    if (url.pathname === '/apis') return Response.json({ kind: 'APIGroupList', groups: [group] });
    if (url.pathname === '/apis/airunway.ai') return Response.json({ kind: 'APIGroup', ...group });
    if (url.pathname === '/apis/airunway.ai/v1alpha1' || url.pathname === '/api/v1') {
      const apiVersion = url.pathname === '/api/v1' ? 'v1' : 'airunway.ai/v1alpha1';
      return Response.json({ kind: 'APIResourceList', groupVersion: apiVersion, resources: Object.entries(types).filter(([, type]) => type.apiVersion === apiVersion).map(([name, type]) => ({ name, kind: type.kind, namespaced: type.namespaced, verbs: ['get', 'list', 'create', 'patch', 'delete'] })) });
    }
    const match = /^\/(?:apis\/airunway\.ai\/v1alpha1|api\/v1)\/(?:(?:namespaces\/([^/]+)\/))?([^/]+)(?:\/([^/]+))?$/.exec(url.pathname);
    if (!match) return error(404, 'NotFound');
    const namespace = decodeURIComponent(match[1] || ''), plural = match[2], name = match[3] ? decodeURIComponent(match[3]) : '';
    const type = types[plural];
    if (!type) return error(404, 'NotFound');
    const resourceKey = key(plural, namespace, name);
    const current = documents.get(resourceKey);
    if (request.method === 'GET') {
      if (name) return current ? Response.json(current) : error(404, 'NotFound');
      return Response.json({ apiVersion: type.apiVersion, kind: `${type.kind}List`, metadata: { resourceVersion: String(revision) }, items: [...documents.values()].filter(item => item.kind === type.kind && (!namespace || field(item, 'metadata', 'namespace') === namespace)) });
    }
    // Discovery/configuration is read-only. Namespace creation is not served.
    if (!type.namespaced || !namespace) return error(405, 'MethodNotAllowed');
    if (request.method === 'DELETE') {
      if (!current) return error(404, 'NotFound');
      const uid = field(body, 'preconditions', 'uid');
      if (uid !== undefined && uid !== field(current, 'metadata', 'uid')) return error(409, 'Conflict');
      if (url.searchParams.get('dryRun') !== 'All') documents.delete(resourceKey);
      return Response.json({ apiVersion: 'v1', kind: 'Status', status: 'Success' });
    }
    if ((request.method !== 'POST' && request.method !== 'PATCH') || !body) return error(405, 'MethodNotAllowed');
    const desiredName = name || field(body, 'metadata', 'name');
    if (typeof desiredName !== 'string') return error(422, 'Invalid');
    const existing = documents.get(key(plural, namespace, desiredName));
    if (request.method === 'POST' && existing) return error(409, 'AlreadyExists');
    if (request.method === 'PATCH' && !existing) return error(404, 'NotFound');
    const expectedVersion = field(body, 'metadata', 'resourceVersion');
    if (expectedVersion !== undefined && expectedVersion !== field(existing, 'metadata', 'resourceVersion')) return error(409, 'Conflict');
    const value = existing ? merge(existing, body) : structuredClone(body);
    if (value.kind !== type.kind || value.apiVersion !== type.apiVersion) return error(422, 'Invalid');
    const metadata = object(value.metadata);
    if (metadata.namespace !== undefined && metadata.namespace !== namespace) return error(422, 'Invalid namespace');
    const generation = existing ? Number(field(existing, 'metadata', 'generation')) + 1 : 1;
    value.metadata = { ...metadata, name: desiredName, namespace, uid: field(existing, 'metadata', 'uid') || `fixture-${++revision}`, resourceVersion: String(++revision), generation };
    if (plural !== 'secrets' && !existing) value.status = status(createdReady, generation);
    if (plural === 'secrets') {
      const data = value.data === undefined ? {} : object(value.data);
      if (value.stringData !== undefined) {
        for (const [key, text] of Object.entries(object(value.stringData))) data[key] = Buffer.from(String(text)).toString('base64');
        delete value.stringData;
      }
      value.data = data;
      // Simulate metadata that would leak a token if raw API output were printed.
      object(value.metadata).annotations = { 'fixture.example/unsafe-note': fixtureToken };
    }
    if (url.searchParams.get('dryRun') !== 'All') documents.set(key(plural, namespace, desiredName), structuredClone(value));
    return Response.json(value, { status: request.method === 'POST' ? 201 : 200 });
  } });
  return { server, requests, seed, documents, key, setCreatedReady: (ready: boolean) => { createdReady = ready; },
    get: (plural: string, namespace: string, name: string) => documents.get(key(plural, namespace, name)) };
}

let api: ReturnType<typeof startAPI>;
let directory: string;
let suiteDirectory: string;
let environment: Record<string, string>;
const children = new Set<Child>();
function launch(args: string[], input = ''): Child {
  const process = spawn(command[0], [...command.slice(1), ...args], { cwd: directory, env: environment, stdio: ['pipe', 'pipe', 'pipe'] });
  const child: Child = { process, result: Promise.resolve({ code: null, signal: null, stdout: '', stderr: '', forced: false }), done: false };
  children.add(child);
  let stdout = '', stderr = '', bytes = 0, forced = false;
  const timer = setTimeout(() => { forced = true; process.kill('SIGKILL'); }, processLimit);
  child.result = new Promise<Result>((resolve, reject) => {
    process.stdout.on('data', (chunk: Buffer) => {
      bytes += chunk.length;
      if (bytes > outputLimit) { forced = true; process.kill('SIGKILL'); return; }
      stdout += chunk.toString();
    });
    process.stderr.on('data', (chunk: Buffer) => {
      bytes += chunk.length;
      if (bytes > outputLimit) { forced = true; process.kill('SIGKILL'); return; }
      stderr += chunk.toString();
    });
    process.once('error', error => { clearTimeout(timer); child.done = true; reject(error); });
    process.once('close', (code, signal) => { clearTimeout(timer); child.done = true; resolve({ code, signal, stdout, stderr, forced }); });
    process.stdin.on('error', () => {}); // A rejected command can close stdin early.
    process.stdin.end(input);
  });
  return child;
}
async function run(args: string[], input = ''): Promise<Result> {
  const result = await launch(args, input).result;
  if (result.forced) throw new Error(`CLI exceeded its process/output limit: ${args.slice(0, 3).join(' ')}; stderr=${result.stderr.slice(-1000)}`);
  return result;
}
function redacted(result: Result, values: string[] = [fixtureToken]): void {
  expect(result.code).toBe(0);
  for (const value of values) {
    expect(result.stdout + result.stderr).not.toContain(value);
    expect(result.stdout + result.stderr).not.toContain(Buffer.from(value).toString('base64'));
  }
  expect(result.stdout + result.stderr).not.toContain('unsafe-note');
  expect(result.stdout).not.toMatch(/(?:"(?:data|stringData)"\s*:|(?:^|\n)\s*(?:data|stringData):)/);
}
function errorOutput(result: Result, code: number): void {
  expect(result.code).toBe(code);
  expect(result.signal).toBeNull();
  expect(result.stderr.trim().length).toBeGreaterThan(0);
}
function structuredStderr(result: Result): void {
  // Accept one JSON document or newline-delimited progress/error records.
  try { JSON.parse(result.stderr); return; } catch { /* Try JSON lines below. */ }
  for (const line of result.stderr.trim().split('\n')) {
    try { JSON.parse(line); } catch { throw new Error(`Non-JSON stderr record: ${line}`); }
  }
}
function successful(result: Result): Json {
  if (result.code !== 0) throw new Error(`CLI failed with exit ${result.code}, signal ${result.signal}: ${result.stderr}`);
  return JSON.parse(result.stdout) as Json;
}
const scoped = (args: string[], context = 'alpha', namespace = 'team-a') => [...args, '--context', context, '--namespace', namespace, '--output', 'json'];
const modelCreate = (name: string) => ['model', 'create', name, '--id', 'hf://Qwen/Qwen3-0.6B', '--provider', 'vllm'];
const agentCreate = (name: string) => ['agent', 'create', name, '--framework', 'langgraph', '--model-ref', 'binding-model', '--prompt', 'Give short answers.'];
function seedBindingModel(): void { api.seed('modeldeployments', 'team-a', 'binding-model', { model: { id: 'Qwen/Qwen3-0.6B', source: 'huggingface' } }); }
function writes(): APIRequest[] { return api.requests.filter(request => ['POST', 'PUT', 'PATCH', 'DELETE'].includes(request.method)); }
async function waitForRead(child: Child, plural: string, name: string): Promise<void> {
  const end = Date.now() + processLimit - 1000;
  while (Date.now() < end) {
    const submitted = api.requests.findIndex(request => request.method === 'POST' && request.path.endsWith(`/${plural}`) && field(request.body, 'metadata', 'name') === name);
    if (submitted >= 0 && api.requests.slice(submitted + 1).some(request => request.method === 'GET' && request.path.endsWith(`/${plural}/${name}`))) return;
    if (child.done) throw new Error(`CLI exited before waiting: ${JSON.stringify(await child.result)}`);
    await delay(10);
  }
  throw new Error('CLI did not enter its wait loop before the subprocess deadline');
}
beforeAll(async () => {
  suiteDirectory = await mkdtemp(join(tmpdir(), 'airunway-binary-test-'));
  await mkdir(join(suiteDirectory, 'transpiler-cache'));
});
afterAll(async () => { await rm(suiteDirectory, { recursive: true, force: true }); });
beforeEach(async () => {
  directory = await mkdtemp(join(suiteDirectory, 'case-'));
  api = startAPI();
  const kubeconfig = join(directory, 'kubeconfig.json');
  await writeFile(kubeconfig, JSON.stringify({ apiVersion: 'v1', kind: 'Config',
    clusters: [{ name: 'loopback', cluster: { server: `http://127.0.0.1:${api.server.port}`, 'insecure-skip-tls-verify': true } }],
    users: [{ name: 'fixture', user: {} }],
    contexts: ['alpha', 'beta'].map(name => ({ name, context: { cluster: 'loopback', user: 'fixture', namespace: `${name}-default` } })),
    'current-context': 'alpha' }));
  environment = { PATH: process.env.PATH || '', HOME: directory, TMPDIR: directory, XDG_CONFIG_HOME: join(directory, '.config'),
    KUBECONFIG: kubeconfig, AIRUNWAY_CONFIG: join(directory, 'cli.json'), BUN_RUNTIME_TRANSPILER_CACHE_PATH: join(suiteDirectory, 'transpiler-cache'), NO_COLOR: '1', TERM: 'dumb' };
});
afterEach(async () => {
  for (const child of children) if (!child.done) child.process.kill('SIGKILL');
  await Promise.allSettled([...children].map(child => child.result)); children.clear();
  await api.server.stop(true);
  await rm(directory, { recursive: true, force: true });
});

describe(`CLI subprocess (${executable ? 'compiled executable' : 'Bun source'})`, () => {
  test('public root and command help work without contacting the API', async () => {
    for (const args of [[], ['model', 'create'], ['agent', 'create'], ['credential', 'get'], ['config']]) {
      const result = await run([...args, '--help']);
      expect(result.code).toBe(0);
      expect(result.stdout).toContain('airunway');
      expect(result.stdout).toContain('--help');
    }
    expect(api.requests).toHaveLength(0);
  }, 30000);

  test('unknown top-level commands and unsupported completion shells exit 2 offline', async () => {
    for (const args of [['not-a-command'], ['completion', 'unsupported-shell']]) {
      const result = await run(scoped(args));
      errorOutput(result, 2);
      expect(result.stdout).toBe('');
    }
    expect(api.requests).toHaveLength(0);
  }, 15000);

  test('version and global version flags print build info without executing another action', async () => {
    // A corrupt local config proves version does not need configuration loading.
    await writeFile(environment.AIRUNWAY_CONFIG, '{invalid-json');
    for (const args of [
      ['version', '--output', 'json'],
      ['--version', '--output', 'json'],
      ['--output', 'json', '--version'],
      ['--context', 'missing-context', '-v', '--output', 'json'],
      [...modelCreate('must-not-exist'), '--version', '--output', 'json'],
    ]) {
      const result = await run(args);
      const info = successful(result);
      expect(typeof field(info, 'version')).toBe('string');
      expect(typeof field(info, 'gitCommit')).toBe('string');
      expect(typeof field(info, 'buildTime')).toBe('string');
      expect(result.stderr).toBe('');
    }
    expect(api.requests).toHaveLength(0);
    expect(await readFile(environment.AIRUNWAY_CONFIG, 'utf8')).toBe('{invalid-json');
  }, 30000);

  test('model create/list/get/update/delete use the selected namespace and preserve identity', async () => {
    const created = successful(await run(scoped([...modelCreate('demo'), '--wait=false'])));
    expect(field(created, 'metadata', 'name')).toBe('demo');
    expect(field(created, 'metadata', 'namespace')).toBe('team-a');
    expect(field(created, 'status', 'phase')).toBe('Pending');
    expect(api.requests.filter(request => request.method === 'GET' && request.path.endsWith('/modeldeployments/demo'))).toHaveLength(0);
    const listed = successful(await run(scoped(['model', 'list'])));
    expect(Array.isArray(listed)).toBe(true);
    expect((listed as Json[]).map(item => field(item, 'metadata', 'name'))).toContain('demo');
    const fetched = successful(await run(scoped(['model', 'get', 'demo'])));
    expect(field(fetched, 'spec', 'model', 'id')).toBe('Qwen/Qwen3-0.6B');
    const version = field(fetched, 'metadata', 'resourceVersion');
    const updated = successful(await run(scoped(['model', 'update', 'demo', '--replicas', '2', '--wait=false'])));
    expect(field(updated, 'spec', 'scaling', 'replicas')).toBe(2);
    expect(field(updated, 'spec', 'model', 'id')).toBe('Qwen/Qwen3-0.6B');
    expect(field(updated, 'metadata', 'uid')).toBe(field(created, 'metadata', 'uid'));
    expect(field(updated, 'status', 'phase')).toBe('Pending');
    expect(api.requests.at(-1)?.method).toBe('PATCH'); // --wait=false must not poll after submission.
    expect(field(writes().find(request => request.method === 'PATCH')?.body, 'metadata', 'resourceVersion')).toBe(version);
    const deleted = await run(scoped(['model', 'delete', 'demo']));
    expect(deleted.code).toBe(0);
    expect(api.get('modeldeployments', 'team-a', 'demo')).toBeUndefined();
    expect(field(writes().find(request => request.method === 'DELETE')?.body, 'preconditions', 'uid')).toBe(field(created, 'metadata', 'uid'));
    expect(writes().every(request => request.path.includes('/namespaces/team-a/modeldeployments'))).toBe(true);
  }, 30000);

  test('agent create/list/get/update/delete retain its shared model and framework', async () => {
    seedBindingModel();
    const created = successful(await run(scoped([...agentCreate('assistant'), '--wait=false'])));
    expect(field(created, 'metadata', 'namespace')).toBe('team-a');
    expect(field(created, 'status', 'phase')).toBe('Pending');
    expect(api.requests.at(-1)?.method).toBe('POST');
    expect(api.requests.some(request => request.path.endsWith('/agentdeployments/assistant') && request.method === 'GET')).toBe(false);
    expect(field(created, 'spec', 'framework', 'name')).toBe('langgraph');
    expect(field(created, 'spec', 'model', 'deploymentRef', 'name')).toBe('binding-model');
    const listed = successful(await run(scoped(['agent', 'list'])));
    expect(Array.isArray(listed)).toBe(true);
    expect((listed as Json[]).map(item => field(item, 'metadata', 'name'))).toContain('assistant');
    const fetched = successful(await run(scoped(['agent', 'get', 'assistant'])));
    expect(field(fetched, 'metadata', 'uid')).toBe(field(created, 'metadata', 'uid'));
    const promptFile = join(directory, 'prompt.txt'); await writeFile(promptFile, 'Use the revised instructions.');
    const updated = successful(await run(scoped(['agent', 'update', 'assistant', '--prompt-file', promptFile, '--wait=false'])));
    expect(field(updated, 'spec', 'config', 'systemPrompt')).toBe('Use the revised instructions.');
    expect(field(updated, 'spec', 'framework', 'name')).toBe('langgraph');
    expect(field(updated, 'spec', 'model', 'deploymentRef', 'name')).toBe('binding-model');
    expect(field(updated, 'metadata', 'uid')).toBe(field(created, 'metadata', 'uid'));
    expect(api.requests.at(-1)?.method).toBe('PATCH');
    expect(field(writes().find(request => request.method === 'PATCH')?.body, 'metadata', 'resourceVersion')).toBe(field(fetched, 'metadata', 'resourceVersion'));
    expect((await run(scoped(['agent', 'delete', 'assistant']))).code).toBe(0);
    expect(api.get('agentdeployments', 'team-a', 'assistant')).toBeUndefined();
    expect(api.get('modeldeployments', 'team-a', 'binding-model')).toBeDefined();
    expect(writes().every(request => request.path.includes('/namespaces/team-a/agentdeployments'))).toBe(true);
    expect(field(writes().find(request => request.method === 'DELETE')?.body, 'preconditions', 'uid')).toBe(field(created, 'metadata', 'uid'));
  }, 30000);

  test('credential create/get/list/update expose only metadata in every format', async () => {
    const created = await run(scoped(['credential', 'create', 'hf-access', '--type', 'huggingface', '--from-file', '-']), fixtureToken + '\n');
    expect(field(successful(created), 'metadata', 'name')).toBe('hf-access');
    expect(field(api.get('secrets', 'team-a', 'hf-access'), 'data', 'HF_TOKEN')).toBe(Buffer.from(fixtureToken).toString('base64'));
    redacted(created);
    for (const action of ['get', 'list']) {
      for (const format of formats) {
        const result = await run(['credential', action, ...(action === 'get' ? ['hf-access'] : []), '--context', 'alpha', '--namespace', 'team-a', '--output', format]);
        redacted(result);
        expect(result.stdout).toContain('hf-access');
      }
    }
    const replacement = fixtureToken + '-replacement';
    const tokenFile = join(directory, 'replacement.txt');
    await writeFile(tokenFile, replacement + '\n');
    redacted(await run(scoped(['credential', 'update', 'hf-access', '--from-file', tokenFile])), [fixtureToken, replacement]);
    expect(field(api.get('secrets', 'team-a', 'hf-access'), 'data', 'HF_TOKEN')).toBe(Buffer.from(replacement).toString('base64'));
    redacted(await run(scoped(['credential', 'get', 'hf-access'])), [fixtureToken, replacement]);
    redacted(await run(scoped(['credential', 'delete', 'hf-access'])), [fixtureToken, replacement]);
    expect(api.get('secrets', 'team-a', 'hf-access')).toBeUndefined();
    expect(writes().map(request => request.method)).toEqual(['POST', 'PATCH', 'DELETE']);
    expect(writes().every(request => request.path.startsWith('/api/v1/namespaces/team-a/secrets'))).toBe(true);
  }, 55000);

  test('API and artifact credential file inputs remain secret in list output', async () => {
    const artifact = JSON.stringify({ AWS_ACCESS_KEY_ID: 'fixture-access-id', AWS_SECRET_ACCESS_KEY: fixtureToken });
    const artifactFile = join(directory, 'artifact.json');
    await writeFile(artifactFile, artifact);
    redacted(await run(scoped(['credential', 'create', 'api-access', '--type', 'api-key', '--from-file', '-']), fixtureToken));
    redacted(await run(scoped(['credential', 'create', 'artifact-access', '--type', 'artifact', '--from-file', artifactFile])), [fixtureToken, artifact]);
    expect(field(api.get('secrets', 'team-a', 'api-access'), 'data', 'API_KEY')).toBe(Buffer.from(fixtureToken).toString('base64'));
    expect(field(api.get('secrets', 'team-a', 'artifact-access'), 'data', 'credentials')).toBe(Buffer.from(artifact).toString('base64'));
    for (const format of formats) {
      const result = await run(['credential', 'list', '--context', 'alpha', '--namespace', 'team-a', '--output', format]);
      redacted(result, [fixtureToken, artifact, 'fixture-access-id']);
      expect(result.stdout).toContain('api-access');
      expect(result.stdout).toContain('artifact-access');
    }
    const other = successful(await run(scoped(['credential', 'list'], 'alpha', 'team-b')));
    expect(other).toEqual([]);
  }, 35000);

  test('saved agent defaults are isolated by context and namespace', async () => {
    const targets = [
      { context: 'alpha', namespace: 'team-a', framework: 'langgraph', model: 'one' },
      { context: 'alpha', namespace: 'team-b', framework: 'openclaw', model: 'two' },
      { context: 'beta', namespace: 'team-a', framework: 'openclaw', model: 'three' },
    ];
    for (const target of targets) {
      for (const [key, value] of [['agent.framework', target.framework], ['agent.model-ref', target.model]]) {
        expect((await run(scoped(['config', 'set', key, value], target.context, target.namespace))).code).toBe(0);
      }
    }
    for (const target of targets) {
      const preview = successful(await run(scoped(['agent', 'create', 'assistant', '--prompt', 'Be helpful.', '--dry-run', 'client'], target.context, target.namespace)));
      expect(field(preview, 'spec', 'framework', 'name')).toBe(target.framework);
      expect(field(preview, 'spec', 'model', 'deploymentRef', 'name')).toBe(target.model);
      expect(field(preview, 'metadata', 'namespace')).toBe(target.namespace);
    }
    expect(api.requests).toHaveLength(0);
    const persisted = await readFile(environment.AIRUNWAY_CONFIG, 'utf8');
    expect(persisted).not.toContain(fixtureToken);
    expect((await stat(environment.AIRUNWAY_CONFIG)).mode & 0o777).toBe(0o600);
  }, 55000);

  test('saved namespace is context-specific and explicit namespace wins', async () => {
    expect((await run(['config', 'set', 'namespace', 'team-b', '--context', 'alpha'])).code).toBe(0);
    const saved = successful(await run([...modelCreate('saved'), '--context', 'alpha', '--dry-run', 'client', '--output', 'json']));
    const other = successful(await run([...modelCreate('other'), '--context', 'beta', '--dry-run', 'client', '--output', 'json']));
    const explicit = successful(await run(scoped([...modelCreate('explicit'), '--dry-run', 'client'])));
    expect(field(saved, 'metadata', 'namespace')).toBe('team-b');
    expect(field(other, 'metadata', 'namespace')).toBe('beta-default');
    expect(field(explicit, 'metadata', 'namespace')).toBe('team-a');
    expect(api.requests).toHaveLength(0);
  }, 25000);

  test('context use preserves kubeconfig and actual calls reach the selected loopback target', async () => {
    const beta = startAPI();
    try {
      const config = object(JSON.parse(await readFile(environment.KUBECONFIG, 'utf8')));
      (config.clusters as Json[]).push({ name: 'beta-loopback', cluster: { server: `http://127.0.0.1:${beta.server.port}`, 'insecure-skip-tls-verify': true } });
      const betaContext = (config.contexts as Json[]).find(item => field(item, 'name') === 'beta');
      object(object(betaContext).context).cluster = 'beta-loopback';
      const original = JSON.stringify(config);
      await writeFile(environment.KUBECONFIG, original);
      expect((await run(['context', 'use', 'beta'])).code).toBe(0);
      expect(await readFile(environment.KUBECONFIG, 'utf8')).toBe(original);
      beta.seed('modeldeployments', 'beta-default', 'beta-only');
      api.seed('modeldeployments', 'alpha-default', 'alpha-only');
      const response = await run(['model', 'list', '--output', 'json']);
      if (response.code !== 0) throw new Error(`Selected-context request failed: ${response.stderr}; alpha=${JSON.stringify(api.requests)}; beta=${JSON.stringify(beta.requests)}`);
      const selected = successful(response);
      expect((selected as Json[]).map(item => field(item, 'metadata', 'name'))).toEqual(['beta-only']);
      expect(api.requests).toHaveLength(0);
      expect(beta.requests.some(request => request.path.endsWith('/namespaces/beta-default/modeldeployments'))).toBe(true);
      const explicit = successful(await run(['model', 'list', '--context', 'alpha', '--output', 'json']));
      expect((explicit as Json[]).map(item => field(item, 'metadata', 'name'))).toEqual(['alpha-only']);
      expect(api.requests.some(request => request.path.endsWith('/namespaces/alpha-default/modeldeployments'))).toBe(true);
      expect(writes()).toHaveLength(0);
      expect(await readFile(environment.KUBECONFIG, 'utf8')).toBe(original);
    } finally { await beta.server.stop(true); }
  }, 20000);

  test('explicit binding replaces saved binding and config unset only affects its target', async () => {
    for (const namespace of ['team-a', 'team-b']) {
      expect((await run(scoped(['config', 'set', 'agent.framework', 'langgraph'], 'alpha', namespace))).code).toBe(0);
      expect((await run(scoped(['config', 'set', 'agent.model-ref', 'saved-model'], 'alpha', namespace))).code).toBe(0);
    }
    const modelURL = `http://127.0.0.1:${api.server.port}/v1`;
    const explicit = successful(await run(scoped(['agent', 'create', 'explicit', '--model-url', modelURL, '--model-api', 'openai', '--model-id', 'served-model', '--prompt', 'Be helpful.', '--dry-run', 'client'])));
    expect(field(explicit, 'spec', 'model', 'deploymentRef')).toBeUndefined();
    expect(JSON.stringify(field(explicit, 'spec', 'model'))).toContain(modelURL);
    expect((await run(scoped(['config', 'unset', 'agent.model-ref']))).code).toBe(0);
    const absent = await run(scoped(['agent', 'create', 'missing-binding', '--prompt', 'Be helpful.', '--dry-run', 'client']));
    errorOutput(absent, 2);
    const other = successful(await run(scoped(['agent', 'create', 'other-target', '--prompt', 'Be helpful.', '--dry-run', 'client'], 'alpha', 'team-b')));
    expect(field(other, 'spec', 'model', 'deploymentRef', 'name')).toBe('saved-model');
    expect(api.requests).toHaveLength(0);
  }, 45000);

  test('saved defaults for new agents do not rewrite an existing agent during update', async () => {
    seedBindingModel();
    successful(await run(scoped([...agentCreate('existing'), '--wait=false'])));
    expect((await run(scoped(['config', 'set', 'agent.framework', 'openclaw']))).code).toBe(0);
    expect((await run(scoped(['config', 'set', 'agent.model-ref', 'different-model']))).code).toBe(0);
    const updated = successful(await run(scoped(['agent', 'update', 'existing', '--prompt', 'Revised prompt.', '--wait=false'])));
    expect(field(updated, 'spec', 'framework', 'name')).toBe('langgraph');
    expect(field(updated, 'spec', 'model', 'deploymentRef', 'name')).toBe('binding-model');
    expect(field(updated, 'spec', 'config', 'systemPrompt')).toBe('Revised prompt.');
  }, 25000);

  test('client previews read kubeconfig namespace without invoking its exec authentication', async () => {
    const marker = join(directory, 'auth-was-executed');
    const config = object(JSON.parse(await readFile(environment.KUBECONFIG, 'utf8')));
    config.users = [{ name: 'fixture', user: { exec: {
      apiVersion: 'client.authentication.k8s.io/v1beta1', command: process.execPath,
      args: ['-e', `require('node:fs').writeFileSync(${JSON.stringify(marker)}, 'invoked'); process.exit(9)`],
    } } }];
    await writeFile(environment.KUBECONFIG, JSON.stringify(config));
    for (const args of [modelCreate('offline-model'), agentCreate('offline-agent')]) {
      const preview = successful(await run([...args, '--context', 'beta', '--dry-run', 'client', '--output', 'json']));
      expect(field(preview, 'metadata', 'namespace')).toBe('beta-default');
    }
    expect(await Bun.file(marker).exists()).toBe(false);
    expect(api.requests).toHaveLength(0);
  }, 15000);

  test('client previews still work without any kubeconfig', async () => {
    environment.KUBECONFIG = join(directory, 'missing-kubeconfig');
    for (const args of [modelCreate('offline-model'), agentCreate('offline-agent')]) {
      const preview = successful(await run([...args, '--namespace', 'offline', '--dry-run', 'client', '--output', 'json']));
      expect(field(preview, 'metadata', 'namespace')).toBe('offline');
    }
    expect(api.requests).toHaveLength(0);
  }, 15000);

  test('client dry-run model and agent creation makes no API requests', async () => {
    successful(await run(scoped([...modelCreate('preview-model'), '--dry-run', 'client'])));
    successful(await run(scoped([...agentCreate('preview-agent'), '--dry-run', 'client'])));
    expect(api.requests).toHaveLength(0);
  }, 15000);

  test('server dry-run sends dryRun=All and does not persist models or agents', async () => {
    seedBindingModel();
    for (const args of [modelCreate('preview-model'), agentCreate('preview-agent')]) successful(await run(scoped([...args, '--dry-run', 'server'])));
    expect(writes()).toHaveLength(2);
    expect(writes().every(request => request.method === 'POST' && request.query.dryRun === 'All')).toBe(true);
    expect(api.requests.some(request => request.method === 'GET' && /\/(preview-model|preview-agent)$/.test(request.path))).toBe(false);
    expect(api.get('modeldeployments', 'team-a', 'preview-model')).toBeUndefined();
    expect(api.get('agentdeployments', 'team-a', 'preview-agent')).toBeUndefined();
    expect(api.get('modeldeployments', 'team-a', 'binding-model')).toBeDefined();
  }, 15000);

  test('unknown options exit 2 before any API call', async () => {
    for (const args of [
      [...modelCreate('invalid'), '--imaginary-option'],
      ['agent', 'list', '--imaginary-option'],
      ['credential', 'get', 'missing', '--imaginary-option'],
      ['provider', 'list', '--replicas', '2'],
      ['--imaginary-option', 'model', 'list'],
      [...modelCreate('invalid-value'), '--imaginary-option=value'],
    ]) {
      const result = await run(scoped(args));
      errorOutput(result, 2);
      expect(result.stdout).toBe('');
    }
    expect(api.requests).toHaveLength(0);
  }, 25000);

  test('JSON output keeps parser errors machine-readable on stderr', async () => {
    for (const args of [
      ['model', 'list', '--imaginary-option', '--output', 'json'],
      ['--output', 'json', 'model', 'list', '--imaginary-option'],
    ]) {
      const result = await run(args);
      errorOutput(result, 2);
      expect(result.stdout).toBe('');
      structuredStderr(result);
    }
    expect(api.requests).toHaveLength(0);
  }, 15000);

  test('JSON output keeps creation progress and timeout errors machine-readable on stderr', async () => {
    const result = await run(scoped([...modelCreate('progress'), '--timeout', '150ms']));
    errorOutput(result, 4);
    expect(api.get('modeldeployments', 'team-a', 'progress')).toBeDefined();
    structuredStderr(result);
  }, 15000);

  for (const noun of ['model', 'agent']) {
    test(`${noun} duplicate create returns conflict without changing the submitted resource`, async () => {
      seedBindingModel();
      const args = noun === 'model' ? modelCreate('duplicate') : agentCreate('duplicate');
      successful(await run(scoped([...args, '--wait=false'])));
      const before = structuredClone(api.get(`${noun}deployments`, 'team-a', 'duplicate'));
      errorOutput(await run(scoped([...args, '--wait=false'])), 5);
      expect(api.get(`${noun}deployments`, 'team-a', 'duplicate')).toEqual(before);
      expect(writes().every(request => request.method === 'POST')).toBe(true);
    }, 15000);

    test(`${noun} update waits for the new generation rather than stale Ready status`, async () => {
      seedBindingModel();
      api.setCreatedReady(true);
      const args = noun === 'model' ? modelCreate('stale') : agentCreate('stale');
      const created = successful(await run(scoped([...args, '--wait=false'])));
      const update = noun === 'model' ? ['--replicas', '2'] : ['--prompt', 'Updated prompt.'];
      const result = await run(scoped([noun, 'update', 'stale', ...update, '--timeout', '150ms']));
      const stored = api.get(`${noun}deployments`, 'team-a', 'stale');
      expect(field(stored, 'metadata', 'generation')).toBe(Number(field(created, 'metadata', 'generation')) + 1);
      expect(field(stored, 'status', 'observedGeneration')).toBe(field(created, 'metadata', 'generation'));
      expect(writes().map(request => request.method)).toEqual(['POST', 'PATCH']);
      errorOutput(result, 4);
    }, 15000);

    test(`${noun} timeout leaves the submitted pending resource intact`, async () => {
      seedBindingModel();
      const args = noun === 'model' ? modelCreate('pending') : agentCreate('pending');
      const result = await run(scoped([...args, '--timeout', '150ms']));
      errorOutput(result, 4);
      const resource = api.get(`${noun}deployments`, 'team-a', 'pending');
      expect(resource).toBeDefined();
      const conditions = field(resource, 'status', 'conditions');
      expect(Array.isArray(conditions)).toBe(true);
      expect((conditions as Json[]).every(condition => field(condition, 'status') === 'False' && field(condition, 'observedGeneration') === field(resource, 'metadata', 'generation'))).toBe(true);
      expect(field(resource, 'status', 'observedGeneration')).toBe(field(resource, 'metadata', 'generation'));
      expect(writes().map(request => request.method)).toEqual(['POST']);
    }, 15000);

    test(`${noun} SIGINT during pending wait exits 130 without deletion`, async () => {
      seedBindingModel();
      const args = noun === 'model' ? modelCreate('interrupted') : agentCreate('interrupted');
      const child = launch(scoped([...args, '--timeout', '30s']));
      await waitForRead(child, `${noun}deployments`, 'interrupted');
      child.process.kill('SIGINT');
      const result = await child.result;
      expect(result.forced).toBe(false);
      errorOutput(result, 130);
      expect(api.get(`${noun}deployments`, 'team-a', 'interrupted')).toBeDefined();
      expect(writes().map(request => request.method)).toEqual(['POST']);
    }, 15000);
  }

  for (const noun of ['model', 'agent']) {
    test(`${noun} wait succeeds with a current-generation Ready fixture`, async () => {
      seedBindingModel();
      api.setCreatedReady(true);
      const args = noun === 'model' ? modelCreate('ready') : agentCreate('ready');
      const result = successful(await run(scoped([...args, '--timeout', '2s'])));
      const generation = field(result, 'metadata', 'generation');
      expect(field(result, 'status', 'observedGeneration')).toBe(generation);
      const conditions = field(result, 'status', 'conditions');
      expect(Array.isArray(conditions)).toBe(true);
      expect((conditions as Json[]).some(condition => field(condition, 'type') === 'Ready' && field(condition, 'status') === 'True' && field(condition, 'observedGeneration') === generation)).toBe(true);
    }, 15000);
  }

  const sources: { name: string; id: string; extra?: string[]; artifact?: boolean; revision?: string; file?: string }[] = [
    { name: 'bare', id: 'Qwen/Qwen3-0.6B' },
    { name: 'huggingface', id: 'hf://Qwen/Qwen3-0.6B' },
    { name: 'pinned-hf', id: 'hf://Qwen/Qwen3-0.6B', extra: ['--revision', '0123456789abcdef0123456789abcdef01234567', '--file', 'model.gguf'], artifact: true, revision: '0123456789abcdef0123456789abcdef01234567', file: 'model.gguf' },
    { name: 's3', id: 's3://model-bucket/qwen/', extra: ['--credential', 'storage-access', '--service-account', 'model-reader'], artifact: true },
    { name: 'gcs', id: 'gs://model-bucket/qwen/', artifact: true },
    { name: 'https', id: 'https://account.blob.core.windows.net/models/model.gguf', extra: ['--file', 'model.gguf'], artifact: true, file: 'model.gguf' },
    { name: 'oci-tag', id: 'oci://registry.example.com/models/qwen:v1', artifact: true },
    { name: 'oci-digest', id: 'oci://registry.example.com/models/qwen@sha256:' + 'a'.repeat(64), artifact: true },
  ];
  for (const source of sources) {
    test(`${source.name} source preview preserves its source and pins without network`, async () => {
      const preview = successful(await run(scoped(['model', 'create', source.name, '--id', source.id, '--provider', 'vllm', '--dry-run', 'client', ...(source.extra || [])])));
      const artifact = field(preview, 'spec', 'model', 'artifact');
      if (source.artifact) {
        expect(field(artifact, 'uri')).toBe(source.id);
        if (source.revision) expect(field(artifact, 'revision')).toBe(source.revision);
        if (source.file) expect(field(artifact, 'file')).toBe(source.file);
        if (source.name === 's3') {
          expect(field(artifact, 'credentialsRef', 'name')).toBe('storage-access');
          expect(field(artifact, 'serviceAccountName')).toBe('model-reader');
        }
      } else {
        expect(artifact).toBeUndefined();
        expect(field(preview, 'spec', 'model', 'id')).toBe('Qwen/Qwen3-0.6B');
        expect(field(preview, 'spec', 'model', 'source')).toBe('huggingface');
      }
      expect(api.requests).toHaveLength(0);
    }, 15000);
  }
  test('existing volume and bundled-image previews do not create download artifacts', async () => {
    const volume = successful(await run(scoped(['model', 'create', 'volume', '--id', 'pvc://model-store/qwen', '--provider', 'vllm', '--dry-run', 'client'])));
    const volumes = field(volume, 'spec', 'model', 'storage', 'volumes');
    expect(Array.isArray(volumes)).toBe(true);
    expect((volumes as Json[]).some(item => field(item, 'claimName') === 'model-store' && field(item, 'size') === undefined)).toBe(true);
    expect(field(volume, 'spec', 'model', 'artifact')).toBeUndefined();
    const bundled = successful(await run(scoped(['model', 'create', 'bundled', '--provider', 'vllm', '--image', 'registry.example.com/model:v1', '--model-path', '/models/qwen', '--dry-run', 'client'])));
    expect(field(bundled, 'spec', 'engine', 'image')).toBe('registry.example.com/model:v1');
    expect(field(bundled, 'spec', 'model', 'artifact')).toBeUndefined();
    expect(api.requests).toHaveLength(0);
  }, 15000);
  test('unsafe artifact file paths and incompatible revision flags fail before network', async () => {
    for (const args of [
      ['--id', 'hf://Qwen/Qwen3-0.6B', '--file', '../outside.gguf'],
      ['--id', 'hf://Qwen/Qwen3-0.6B', '--file', '/absolute.gguf'],
      ['--id', 'oci://registry.example.com/model:v1', '--revision', 'main'],
      ['--id', 's3://model-bucket/model', '--revision', 'main'],
    ]) errorOutput(await run(scoped(['model', 'create', 'invalid-artifact', ...args, '--dry-run', 'client'])), 2);
    expect(api.requests).toHaveLength(0);
  }, 25000);

  test('unsupported or credential-bearing source URLs fail before network', async () => {
    for (const id of [
      'file:///tmp/model.gguf', 'ftp://example.com/model',
      'https://example.com/model?sig=fixture', 'https://user:password@example.com/model',
    ]) {
      expect((await run(scoped(['model', 'create', 'invalid-source', '--id', id, '--dry-run', 'client']))).code).toBe(2);
    }
    expect(api.requests).toHaveLength(0);
  }, 20000);
});
