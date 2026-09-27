import { afterEach, describe, expect, test } from 'bun:test';
import { mkdtemp, rm, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import * as k8s from '@kubernetes/client-node';
import { parseCLIArgs, requestedOutput, duration, integer, inputValue, name } from './args';
import { changeConfig, effectiveDefaults, readConfig, writeConfig } from './config';
import { KubernetesCLIClient, resourcePath } from './client';
import { mergeAgentDefaults, runCLI, isCLICommand } from './index';
import { CLIError, resourceTypes, type IO, type JsonObject } from './types';

const dirs: string[] = [];
afterEach(async () => { for (const path of dirs.splice(0)) await rm(path, { recursive: true, force: true }); });
function memoryIO(): IO & { stdout: string; stderr: string } {
  const io = { stdout: '', stderr: '', interactive: false, out(value: string) { this.stdout += value; }, err(value: string) { this.stderr += value; }, async input() { return 'file prompt'; } }; return io;
}
describe('CLI parsing', () => {
  test('positional names, global flags and assigned booleans', () => {
    const parsed = parseCLIArgs(['--context', 'dev', 'model', 'create', 'demo', '--id', 'hf://Qwen/Qwen3-8B', '--wait=false', '-o', 'json']);
    expect(parsed.words).toEqual(['model', 'create', 'demo']); expect(parsed.flags.wait).toBe(false); expect(parsed.flags.output).toBe('json');
  });
  test('assigned booleans preserve ordering and the positional terminator', () => {
    expect(parseCLIArgs(['model', 'create', 'demo', '--wait=false', '--wait']).flags.wait).toBe(true);
    expect(parseCLIArgs(['model', 'create', 'demo', '--wait', '--wait=false']).flags.wait).toBe(false);
    expect(parseCLIArgs(['model', 'get', '--', '--wait=false'])).toEqual({ words: ['model', 'get', '--wait=false'], flags: {} });
    expect(parseCLIArgs(['model', 'chat', 'demo', '--message=--wait=false']).flags.message).toBe('--wait=false');
  });
  test('rejects unknown options, invalid integers, names, durations', () => {
    expect(() => parseCLIArgs(['model', 'list', '--wat'])).toThrow();
    expect(() => integer({ gpus: '1.5' }, 'gpus')).toThrow(); expect(() => integer({ gpus: '-1' }, 'gpus')).toThrow();
    expect(() => name('../bad')).toThrow(); expect(() => duration('forever')).toThrow(); expect(() => duration('25h')).toThrow(); expect(duration('2m')).toBe(120000);
  });
  test('error output selection supports aliases and respects the positional terminator', () => {
    expect(requestedOutput(['model', 'list', '--unknown', '-ojson'])).toBe('json');
    expect(requestedOutput(['--output=json', '--unknown'])).toBe('json');
    expect(requestedOutput(['--output', 'json', '-o', 'text'])).toBe('text');
    expect(requestedOutput(['model', 'get', '--', '--output=json'])).toBeUndefined();
    expect(requestedOutput(['model', 'chat', 'demo', '--message=--output=json'])).toBeUndefined();
  });
  test('prompts use one input source', async () => {
    expect(await inputValue({ 'prompt-file': '-' }, 'prompt', 'prompt-file', async () => 'hello')).toBe('hello');
    await expect(inputValue({ prompt: 'a', 'prompt-file': '-' }, 'prompt', 'prompt-file', async () => '')).rejects.toThrow();
  });
});
describe('CLI configuration', () => {
  test('scopes defaults and stores config privately', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'airunway-cli-test-')); dirs.push(dir); const path = join(dir, 'nested', 'cli.json');
    const config = await readConfig(path); changeConfig(config, 'dev', 'team-a', 'agent.model-ref', 'demo'); await writeConfig(config, path);
    expect(effectiveDefaults(await readConfig(path), 'dev', 'team-a')['model-ref']).toBe('demo');
    expect(effectiveDefaults(config, 'other', 'team-a')['model-ref']).toBeUndefined(); expect(effectiveDefaults(config, 'dev', 'other')['model-ref']).toBeUndefined();
    expect((await readFile(path, 'utf8')).includes('demo')).toBe(true);
  });
  test('explicit endpoint never inherits another model binding', () => {
    expect(mergeAgentDefaults({ 'model-url': 'https://example.test/v1', 'model-id': 'remote' }, { framework: 'langgraph', 'model-ref': 'old' })).toEqual({ framework: 'langgraph', 'model-url': 'https://example.test/v1', 'model-id': 'remote' });
  });
});
describe('CLI cluster client', () => {
  test('constructs paths without mixing namespace and name', () => {
    expect(resourcePath(resourceTypes.model, 'team', 'demo')).toBe('/apis/airunway.ai/v1alpha1/namespaces/team/modeldeployments/demo');
    expect(resourcePath(resourceTypes.framework, 'ignored', 'langgraph')).toBe('/apis/airunway.ai/v1alpha1/agentproviderconfigs/langgraph');
    expect(resourcePath(resourceTypes.model)).toBe('/apis/airunway.ai/v1alpha1/modeldeployments');
  });
  test('forwards caller token, dry-run and optimistic patch without credentials in errors', async () => {
    const calls: { method: string; path: string; authorization: string | null; body?: JsonObject }[] = [];
    const server = Bun.serve({ hostname: '127.0.0.1', port: 0, async fetch(req) {
      calls.push({ method: req.method, path: req.url, authorization: req.headers.get('Authorization'), body: req.method === 'GET' ? undefined : await req.json() });
      if (new URL(req.url).pathname.endsWith('/denied')) return Response.json({ message: 'SECRET_THAT_MUST_NOT_LEAK' }, { status: 403 });
      return Response.json({ apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: 'demo' } });
    } });
    try {
      const config = new k8s.KubeConfig(); config.loadFromOptions({ clusters: [{ name: 'local', server: server.url.toString(), skipTLSVerify: true }], users: [{ name: 'user', token: 'local-test-token' }], contexts: [{ name: 'test', cluster: 'local', user: 'user' }], currentContext: 'test' });
      const client = new KubernetesCLIClient(config, 'team');
      await client.create({ apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: 'demo', namespace: 'team' }, spec: { model: { id: 'test/model' } } }, true);
      expect(calls[0].authorization).toBe('Bearer local-test-token'); expect(calls[0].path).toContain('dryRun=All');
      await client.patch(resourceTypes.model, 'demo', { metadata: { resourceVersion: '2' }, spec: { scaling: { replicas: 2 } } }); expect(calls[1].body?.metadata.resourceVersion).toBe('2');
      try { await client.get(resourceTypes.model, 'denied'); throw new Error('expected error'); } catch (error) { expect(error).toBeInstanceOf(CLIError); expect(String(error)).not.toContain('SECRET_THAT_MUST_NOT_LEAK'); }
    } finally { server.stop(true); }
  });
});
describe('cluster response failures', () => {
  test('malformed responses do not expose their contents', async () => {
    const server = Bun.serve({ hostname: '127.0.0.1', port: 0, fetch: () => new Response('private-response-that-is-not-json') });
    try {
      const config = new k8s.KubeConfig();
      config.loadFromOptions({ clusters: [{ name: 'local', server: server.url.toString(), skipTLSVerify: true }], users: [{ name: 'user' }], contexts: [{ name: 'test', cluster: 'local', user: 'user' }], currentContext: 'test' });
      const client = new KubernetesCLIClient(config, 'team');
      await expect(client.request('GET', '/invalid')).rejects.toMatchObject({ code: 'RESPONSE', message: 'The cluster returned an invalid JSON response.' });
    } finally { server.stop(true); }
  });
  test('body-read cancellation keeps its timeout exit code', async () => {
    const server = Bun.serve({ hostname: '127.0.0.1', port: 0, fetch: () => new Response(new ReadableStream({ start(controller) { controller.enqueue(new TextEncoder().encode('{')); } })) });
    try {
      const config = new k8s.KubeConfig();
      config.loadFromOptions({ clusters: [{ name: 'local', server: server.url.toString(), skipTLSVerify: true }], users: [{ name: 'user' }], contexts: [{ name: 'test', cluster: 'local', user: 'user' }], currentContext: 'test' });
      const client = new KubernetesCLIClient(config, 'team');
      await expect(client.request('GET', '/stream', undefined, { signal: AbortSignal.timeout(100) })).rejects.toMatchObject({ exitCode: 4, code: 'TIMEOUT' });
    } finally { server.stop(true); }
  });
});

describe('CLI entrypoint', () => {
  test('help and completion require no configured cluster', async () => {
    const io = memoryIO(); expect(await runCLI(['model', 'create', '--help'], { io })).toBe(0); expect(io.stdout).toContain('model create NAME'); expect(io.stderr).toBe('');
    const other = memoryIO(); expect(await runCLI(['completion', 'zsh'], { io: other })).toBe(0); expect(other.stdout).toContain('#compdef');
    expect(isCLICommand(['serve'])).toBe(false); expect(isCLICommand(['--context', 'dev', 'model', 'list'])).toBe(true);
  });
  test('invalid input is a structured stderr error', async () => {
    const io = memoryIO(); expect(await runCLI(['model', 'list', '--output', 'json', '--timeout', 'bad'], { io })).toBe(2); expect(io.stdout).toBe(''); expect(JSON.parse(io.stderr).error.code).toBe('USAGE');
  });
});

describe('offline command dispatch', () => {
  test('version bypasses configuration and never executes another action', async () => {
    for (const args of [['--version'], ['-v'], ['version'], ['model', 'delete', 'demo', '--version']]) {
      const io = memoryIO();
      expect(isCLICommand(args)).toBe(true);
      expect(await runCLI([...args, '--kubeconfig', '/missing/config', '-o', 'json'], { io })).toBe(0);
      expect(JSON.parse(io.stdout).version).toBeString();
      expect(io.stderr).toBe('');
    }
  });
  test('unknown commands and unsupported completions are usage errors', async () => {
    for (const args of [['unknown'], ['completion', 'powershell']]) {
      const io = memoryIO();
      expect(isCLICommand(args)).toBe(true);
      expect(await runCLI([...args, '--kubeconfig', '/missing/config'], { io })).toBe(2);
      expect(io.stdout).toBe('');
    }
  });
});
