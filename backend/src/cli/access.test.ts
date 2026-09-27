import { afterEach, describe, expect, spyOn, test } from 'bun:test';
import { KubeConfig, PortForward } from '@kubernetes/client-node';
import { createServer as httpServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { connect, type Socket } from 'node:net';
import { EventEmitter } from 'node:events';
import * as https from 'node:https';
import * as readline from 'node:readline';
import { PassThrough } from 'node:stream';
import type { DetailedPeerCertificate } from 'node:tls';
import { runAccess, waitForResource } from './access';
import { CLIError, resourceTypes, type ClusterClient, type CommandContext, type Flags, type IO, type RequestOptions, type Resource, type ResourceType } from './types';

type ForwardConnection = Exclude<Awaited<ReturnType<PortForward['portForward']>>, () => unknown>;
interface ChatRequest { model: string; messages: Array<{ role: string; content: string }>; stream?: boolean; temperature?: number; max_tokens?: number }
type CapturedHTTPSOptions = https.RequestOptions & { headers: Record<string, string>; checkServerIdentity: NonNullable<https.RequestOptions['checkServerIdentity']> };

const cleanups: Array<() => void | Promise<void>> = [];
afterEach(async () => { for (const cleanup of cleanups.splice(0).reverse()) await cleanup(); });
const owner = (resource: Resource) => ({ apiVersion: resource.apiVersion, kind: resource.kind, name: resource.metadata.name, uid: resource.metadata.uid!, controller: true });
function model(extra: Partial<Resource> = {}): Resource {
  return { apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: 'llama', namespace: 'test', uid: 'model-uid', generation: 2 }, spec: { model: { id: 'repository/not-a-served-name', servedName: 'configured-name' } }, status: { phase: 'Running', observedGeneration: 2, conditions: [{ type: 'Ready', status: 'True', observedGeneration: 2 }], endpoint: { service: 'actual-model-api', port: 8000 } }, ...extra };
}
function agent(): Resource {
  return { apiVersion: 'airunway.ai/v1alpha1', kind: 'AgentDeployment', metadata: { name: 'helper', namespace: 'test', uid: 'agent-uid', generation: 2 }, spec: { framework: { name: 'crewai' }, model: { credential: { name: 'never-read-model-key' } } }, status: { phase: 'Running', observedGeneration: 2, conditions: [{ type: 'Ready', status: 'True', observedGeneration: 2 }], runtime: { address: 'http://actual-agent-api.test.svc', workloadRef: { apiVersion: 'apps/v1', kind: 'Deployment', name: 'actual-agent-workload', namespace: 'test' }, authSecretRef: { name: 'ingress-key', key: 'token' } }, modelBinding: { auth: { secretRef: { name: 'never-read-model-key', key: 'token' } } } } };
}
function service(name = 'actual-model-api', port = 8000): Resource {
  return { apiVersion: 'v1', kind: 'Service', metadata: { name, namespace: 'test', uid: `${name}-uid` }, spec: { selector: { workload: name }, ports: [{ name: 'http', port, targetPort: 'api' }] } };
}
function pod(svc: Resource, root?: Resource): Resource {
  return { apiVersion: 'v1', kind: 'Pod', metadata: { name: 'selected-pod', namespace: svc.metadata.namespace, uid: 'pod-uid', labels: svc.spec!.selector, ownerReferences: root ? [owner(root)] : [] }, spec: { containers: [{ name: 'server', ports: [{ name: 'api', containerPort: 8080 }] }] }, status: { phase: 'Running', conditions: [{ type: 'Ready', status: 'True' }] } };
}
class MockClient implements ClusterClient {
  readonly namespace = 'test'; readonly context = 'fake'; kubeConfig?: KubeConfig;
  resources: Resource[];
  calls: Array<{ method: string; type?: ResourceType; name?: string; namespace?: string; path?: string; options?: RequestOptions }> = [];
  onGet?: (type: ResourceType, name: string, namespace?: string) => Promise<Resource>;
  onRaw?: (method: string, path: string, body?: unknown, options?: RequestOptions) => Promise<Response>;
  onRequest?: (method: string, path: string) => Promise<unknown>;
  constructor(...resources: Resource[]) { this.resources = resources; }
  async get(type: ResourceType, name: string, namespace = this.namespace): Promise<Resource> {
    this.calls.push({ method: 'get', type, name, namespace });
    if (this.onGet) return this.onGet(type, name, namespace);
    const found = this.resources.find(r => r.kind === type.kind && r.metadata.name === name && (r.metadata.namespace || this.namespace) === namespace);
    if (!found) throw new CLIError('not found', 1, 'HTTP_404');
    return structuredClone(found);
  }
  async list(type: ResourceType, namespace = this.namespace, query?: RequestOptions['query']): Promise<Resource[]> {
    this.calls.push({ method: 'list', type, namespace, options: { query } });
    // Deliberately do not apply selectors: the module must validate ownership
    // and Service pod membership, not trust a permissive fixture implementation.
    return structuredClone(this.resources.filter(r => r.kind === type.kind && (r.metadata.namespace || this.namespace) === namespace));
  }
  async raw(method: string, path: string, body?: unknown, options?: RequestOptions): Promise<Response> {
    this.calls.push({ method, path, options });
    return this.onRaw ? this.onRaw(method, path, body, options) : new Response('line one\nline two\n');
  }
  async request<T>(method: string, path: string): Promise<T> { this.calls.push({ method, path }); if (this.onRequest) return await this.onRequest(method, path) as T; throw new Error(`Unexpected discovery request ${path}`); }
  async create(): Promise<Resource> { throw new Error('Unexpected write'); }
  async patch(): Promise<Resource> { throw new Error('Unexpected write'); }
  async delete(): Promise<void> { throw new Error('Unexpected write'); }
}
function context(client: MockClient, flags: Flags = {}) {
  const out: string[] = [], err: string[] = [];
  const controller = new AbortController();
  const io: IO = { out: s => out.push(s), err: s => err.push(s), interactive: false, input: async () => { throw new Error('Unexpected stdin read'); } };
  const ctx: CommandContext = { client: () => client, namespace: 'test', context: 'fake', flags: { output: 'json', timeout: '2s', ...flags }, io, signal: controller.signal };
  cleanups.push(() => controller.abort());
  return { ctx, io, out, err, controller, value: () => JSON.parse(out.join('')) };
}
async function localHTTP(handler: (request: IncomingMessage, response: ServerResponse) => void) {
  const server = httpServer(handler);
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
  cleanups.push(() => { server.closeAllConnections(); server.close(); });
  return (server.address() as { port: number }).port;
}
function fakeForward(client: MockClient, upstreamPort: number) {
  client.kubeConfig = new KubeConfig();
  const calls: Array<{ namespace: string; pod: string; ports: number[] }> = [];
  const upstreams: Socket[] = [];
  const spy = spyOn(PortForward.prototype, 'portForward').mockImplementation(async (namespace, name, ports, output, _err, input) => {
    calls.push({ namespace, pod: name, ports });
    const upstream = connect(upstreamPort, '127.0.0.1'); upstreams.push(upstream);
    upstream.on('error', () => {});
    input.pipe(upstream); upstream.pipe(output);
    const ws = new EventEmitter();
    Object.assign(ws, { terminate() { upstream.destroy(); ws.emit('close'); } });
    return ws as unknown as ForwardConnection;
  });
  cleanups.push(() => { spy.mockRestore(); for (const socket of upstreams) socket.destroy(); });
  return calls;
}
async function body(request: IncomingMessage): Promise<ChatRequest> { let value = ''; for await (const chunk of request) value += chunk; return JSON.parse(value); }
function reply(response: ServerResponse, value: unknown) { response.writeHead(200, { 'content-type': 'application/json' }); response.end(JSON.stringify(value)); }
function gatewayResources(md = model()) {
  delete md.spec!.model.servedName;
  md.status!.gateway = { gatewayName: 'shared', gatewayNamespace: 'edge', modelName: 'served-alias', endpoint: 'wrong-scheme:80' };
  const gateway: Resource = { apiVersion: 'gateway.networking.k8s.io/v1', kind: 'Gateway', metadata: { name: 'shared', namespace: 'edge', uid: 'gateway-uid' }, spec: { listeners: [{ name: 'secure', protocol: 'HTTPS', port: 8443 }] }, status: { addresses: [{ type: 'IPAddress', value: '203.0.113.3' }] } };
  const route: Resource = { apiVersion: gateway.apiVersion, kind: 'HTTPRoute', metadata: { name: 'actual-route', namespace: 'test', ownerReferences: [owner(md)] }, spec: { hostnames: ['inference.example.test'], parentRefs: [{ name: 'shared', namespace: 'edge', sectionName: 'secure' }], rules: [{ matches: [{ path: { type: 'PathPrefix', value: '/models' }, headers: [{ name: 'X-Gateway-Model-Name', value: 'served-alias' }] }] }] } };
  const svc = service('implementation-generated-42', 8443); svc.metadata.namespace = 'edge'; svc.metadata.labels = { 'gateway.networking.k8s.io/gateway-name': 'shared' };
  return { md, gateway, route, svc };
}
function agentResources() {
  const ad = agent();
  const root: Resource = { apiVersion: 'apps/v1', kind: 'Deployment', metadata: { name: 'actual-agent-workload', namespace: 'test', uid: 'deployment-uid', ownerReferences: [owner(ad)] } };
  const svc = service('actual-agent-api', 80); svc.metadata.ownerReferences = [owner(ad)];
  const secret: Resource = { apiVersion: 'v1', kind: 'Secret', metadata: { name: 'ingress-key', namespace: 'test', uid: 'secret-uid', ownerReferences: [owner(ad)] }, data: { token: Buffer.from('private-ingress-token').toString('base64') } };
  const workloadPod = pod(svc, root);
  return { ad, root, svc, secret, workloadPod, client: new MockClient(ad, root, svc, secret, workloadPod) };
}

describe('waitForResource', () => {
  test('requires current status and Ready generations, not phase or replicas', async () => {
    const md = model(); md.status!.conditions[0].observedGeneration = 1;
    const client = new MockClient(md); const c = context(client, { timeout: '15ms' });
    await expect(waitForResource(client, 'model', md, c.ctx.flags, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'TIMEOUT', exitCode: 4 });
    md.status!.conditions[0].observedGeneration = 2; md.status!.observedGeneration = 1;
    await expect(waitForResource(client, 'model', md, c.ctx.flags, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'TIMEOUT' });
    md.status!.observedGeneration = 2;
    expect(await waitForResource(client, 'model', md, c.ctx.flags, c.io, c.ctx.signal)).toBe(md);
  });
  test('polls the same namespace and honors new generations', async () => {
    const md = model(); md.status!.phase = 'Pending'; md.status!.conditions = [];
    const current = model(); current.metadata.namespace = md.metadata.namespace = 'other';
    const client = new MockClient(current); const c = context(client);
    expect(await waitForResource(client, 'model', md, c.ctx.flags, c.io, c.ctx.signal)).toEqual(current);
    expect(client.calls[0].namespace).toBe('other');
  });
  test('job Completed succeeds even before core Ready becomes true', async () => {
    const ad = agent(); ad.spec!.lifecycle = 'job'; ad.status!.phase = 'Completed'; ad.status!.conditions = [{ type: 'ProviderReady', status: 'True', reason: 'JobCompleted', observedGeneration: 2 }];
    const client = new MockClient(ad); const c = context(client);
    expect(await waitForResource(client, 'agent', ad, { for: 'completed' }, c.io, c.ctx.signal)).toBe(ad);
    expect(await waitForResource(client, 'agent', ad, {}, c.io, c.ctx.signal)).toBe(ad);
  });
  test('does not accept stale failure, but rejects a current failure', async () => {
    const md = model(); md.status!.phase = 'Failed'; md.status!.observedGeneration = 1;
    md.status!.conditions = [{ type: 'Ready', status: 'False', reason: 'DeploymentFailed', observedGeneration: 1 }];
    const client = new MockClient(md); const c = context(client, { timeout: '10ms' });
    await expect(waitForResource(client, 'model', md, c.ctx.flags, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'TIMEOUT' });
    // Core can publish generation 2 before the provider has reconsidered failure.
    md.status!.observedGeneration = 2;
    md.status!.conditions.push({ type: 'Validated', status: 'True', observedGeneration: 2 });
    await expect(waitForResource(client, 'model', md, c.ctx.flags, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'TIMEOUT' });
    md.status!.conditions[0].observedGeneration = 2;
    await expect(waitForResource(client, 'model', md, c.ctx.flags, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'FAILED' });
  });
  test('agent core readiness does not make an old provider failure current', async () => {
    const ad = agent(); ad.status!.phase = 'Failed';
    ad.status!.conditions = [{ type: 'Ready', status: 'False', reason: 'WaitingForProvider', observedGeneration: 2 }, { type: 'ProviderReady', status: 'False', reason: 'InvalidConfig', observedGeneration: 1 }];
    const client = new MockClient(ad); const c = context(client, { timeout: '10ms' });
    await expect(waitForResource(client, 'agent', ad, c.ctx.flags, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'TIMEOUT' });
    ad.status!.conditions[1].observedGeneration = 2;
    await expect(waitForResource(client, 'agent', ad, c.ctx.flags, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'FAILED' });
  });
  test('current core validation failure is terminal even if provider readiness is stale', async () => {
    const md = model(); md.status!.phase = 'Failed';
    md.status!.conditions = [{ type: 'Ready', status: 'True', observedGeneration: 1 }, { type: 'Validated', status: 'False', reason: 'ValidationFailed', observedGeneration: 2 }];
    const client = new MockClient(md); const c = context(client);
    await expect(waitForResource(client, 'model', md, c.ctx.flags, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'FAILED' });
  });
  test('deletion, replacement, missing resource, and cancel terminate waiting', async () => {
    const md = model(); md.metadata.deletionTimestamp = '2026-09-26T00:00:00Z';
    const client = new MockClient(); const c = context(client);
    await expect(waitForResource(client, 'model', md, {}, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'DELETED' });
    delete md.metadata.deletionTimestamp; md.status!.conditions = [];
    await expect(waitForResource(client, 'model', md, {}, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'DELETED' });
    const replacement = model(); replacement.metadata.uid = 'new-uid'; client.resources = [replacement];
    await expect(waitForResource(client, 'model', md, {}, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'DELETED' });
    c.controller.abort();
    await expect(waitForResource(client, 'model', md, {}, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'CANCELED', exitCode: 130 });
  });
  test('timeout bounds an uncooperative get promise', async () => {
    const md = model(); md.status!.conditions = [];
    const client = new MockClient(); client.onGet = async () => new Promise(() => {});
    const c = context(client, { timeout: '270ms' });
    await expect(waitForResource(client, 'model', md, c.ctx.flags, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'TIMEOUT' });
  });
  test('validates condition and timeout flags', async () => {
    const md = model(); const client = new MockClient(md); const c = context(client);
    await expect(waitForResource(client, 'model', md, { for: 'completed' }, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'USAGE' });
    await expect(waitForResource(client, 'model', md, { timeout: 'forever' }, c.io, c.ctx.signal)).rejects.toMatchObject({ code: 'USAGE' });
  });
});

describe('endpoint discovery', () => {
  test('uses the actual Service and its service port rather than container port', async () => {
    const md = model(); md.status!.endpoint.port = 8080;
    const svc = service('actual-model-api', 80); svc.spec!.ports[0].targetPort = 8080;
    const client = new MockClient(md, svc); const c = context(client);
    await runAccess('model', 'endpoint', 'llama', c.ctx);
    expect(c.value()).toMatchObject({ url: 'http://actual-model-api.test.svc/', access: 'internal', servedModelName: 'configured-name', service: { port: 80 } });
    expect(client.calls.some(call => call.type?.kind === 'Secret')).toBe(false);
  });
  test('does not invent a provider Service when status is absent', async () => {
    const md = model(); delete md.status!.endpoint;
    const client = new MockClient(md, service('llama-predictor')); const c = context(client);
    await expect(runAccess('model', 'endpoint', 'llama', c.ctx)).rejects.toMatchObject({ code: 'UNSUPPORTED' });
    expect(client.calls).toHaveLength(1);
  });
  test('uses gateway address, HTTPS listener, port, route headers and path', async () => {
    const { md, gateway, route, svc } = gatewayResources();
    const client = new MockClient(md, gateway, route, svc); const c = context(client);
    await runAccess('model', 'endpoint', 'llama', c.ctx);
    expect(c.value()).toMatchObject({ url: 'https://203.0.113.3:8443/models', access: 'gateway', headers: { host: 'inference.example.test', 'x-gateway-model-name': 'served-alias' }, servedModelName: 'served-alias', service: { name: 'implementation-generated-42', namespace: 'edge', port: 8443 } });
  });
  test('falls back to a label-discovered Gateway Service, not a guessed name', async () => {
    const { md, gateway, route, svc } = gatewayResources(); gateway.status!.addresses = [];
    const client = new MockClient(md, gateway, route, svc, service('shared-istio')); const c = context(client);
    await runAccess('model', 'endpoint', 'llama', c.ctx);
    expect(c.value().url).toBe('https://implementation-generated-42.edge.svc:8443/models');
    expect(c.value().access).toBe('internal');
    delete svc.metadata.labels;
    client.resources = [md, gateway, route, svc];
    await expect(runAccess('model', 'endpoint', 'llama', c.ctx)).rejects.toMatchObject({ code: 'UNSUPPORTED' });
  });
  test('rejects missing routes, ambiguous ports, and nonexact route matches', async () => {
    const { md, gateway, route } = gatewayResources(); const c = context(new MockClient(md, gateway));
    await expect(runAccess('model', 'endpoint', 'llama', c.ctx)).rejects.toMatchObject({ code: 'UNSUPPORTED' });
    route.spec!.rules[0].matches[0].headers[0].type = 'RegularExpression';
    await expect(runAccess('model', 'endpoint', 'llama', context(new MockClient(md, gateway, route)).ctx)).rejects.toMatchObject({ code: 'UNSUPPORTED' });
    const direct = model(); delete direct.status!.endpoint.port;
    const svc = service(); svc.spec!.ports.push({ port: 9000 });
    await expect(runAccess('model', 'endpoint', 'llama', context(new MockClient(direct, svc)).ctx)).rejects.toMatchObject({ code: 'UNSUPPORTED' });
  });
  test('agent endpoints publish ingress reference only and no model credential', async () => {
    const { client } = agentResources(); const c = context(client);
    await runAccess('agent', 'endpoint', 'helper', c.ctx);
    expect(c.value()).toMatchObject({ url: 'http://actual-agent-api.test.svc/', authRequired: true, authSecretRef: { name: 'ingress-key', key: 'token' } });
    expect(c.out.join('')).not.toContain('private-ingress-token');
    expect(client.calls.some(call => call.type?.kind === 'Secret')).toBe(false);
  });
  test('jobs and providers without a runtime address get actionable errors', async () => {
    for (const framework of ['kagent', 'orka']) {
      const ad = agent(); ad.spec!.framework.name = framework; delete ad.status!.runtime.address;
      await expect(runAccess('agent', 'chat', 'helper', context(new MockClient(ad), { message: 'hello' }).ctx)).rejects.toThrow('upstream operator');
    }
    const ad = agent(); ad.spec!.lifecycle = 'job';
    await expect(runAccess('agent', 'endpoint', 'helper', context(new MockClient(ad)).ctx)).rejects.toThrow('jobs do not expose endpoints');
  });
  test('does not check an arbitrary status URL without explicit trust', async () => {
    let requests = 0;
    const port = await localHTTP((_req, response) => { requests++; reply(response, {}); });
    const ad = agent(); ad.status!.runtime.address = `http://127.0.0.1:${port}/`;
    const c = context(new MockClient(ad), { check: true });
    await expect(runAccess('agent', 'endpoint', 'helper', c.ctx)).rejects.toThrow('--server');
    expect(requests).toBe(0);
  });
  test('endpoint --check only uses discovery GET, never inference', async () => {
    const methods: string[] = [];
    const port = await localHTTP((req, response) => { methods.push(`${req.method} ${req.url}`); reply(response, { data: [{ id: 'served' }] }); });
    const md = model(), svc = service(); const client = new MockClient(md, svc, pod(svc)); fakeForward(client, port);
    const c = context(client, { check: true });
    await runAccess('model', 'endpoint', 'llama', c.ctx);
    expect(methods).toEqual(['GET /v1/models']); expect(c.value().reachable).toBe(true);
  });
  test('agent --check does not read its ingress token and accepts plaintext readiness', async () => {
    const requests: string[] = [];
    const port = await localHTTP((req, response) => { requests.push(`${req.method} ${req.url}`); response.end('ok'); });
    const { client } = agentResources(); fakeForward(client, port); const c = context(client, { check: true });
    await runAccess('agent', 'endpoint', 'helper', c.ctx);
    expect(requests).toEqual(['GET /readyz']);
    expect(client.calls.some(call => call.type?.kind === 'Secret')).toBe(false);
  });
});

describe('chat and connect transport', () => {
  test('model chat uses spec.model.servedName without discovery and emits the JSON response', async () => {
    const seen: Array<{ path: string; body: ChatRequest }> = [];
    const result = { choices: [{ message: { role: 'assistant', content: 'Hello' } }] };
    const port = await localHTTP((req, response) => { void body(req).then(value => { seen.push({ path: req.url!, body: value }); reply(response, result); }); });
    const md = model(), svc = service(); const client = new MockClient(md, svc, pod(svc)); const forwards = fakeForward(client, port);
    const c = context(client, { message: 'hello', temperature: '0.4', 'max-tokens': '32' });
    await runAccess('model', 'chat', 'llama', c.ctx);
    expect(seen).toEqual([{ path: '/v1/chat/completions', body: { model: 'configured-name', messages: [{ role: 'user', content: 'hello' }], stream: false, temperature: 0.4, max_tokens: 32 } }]);
    expect(c.value()).toEqual(result);
    expect(forwards[0]).toEqual({ namespace: 'test', pod: 'selected-pod', ports: [8080] });
  });
  test('discovers only served IDs when neither serving configuration nor resolved status names the model', async () => {
    const models: string[] = [];
    const port = await localHTTP((req, response) => { if (req.method === 'GET') reply(response, { data: [{ id: 'provider-actual-id' }] }); else void body(req).then(value => { models.push(value.model); reply(response, { choices: [{ message: { content: 'done' } }] }); }); });
    const md = model(); delete md.spec!.model.servedName;
    const svc = service(); const client = new MockClient(md, svc, pod(svc)); fakeForward(client, port);
    const c = context(client, { message: 'hello' });
    await runAccess('model', 'chat', 'llama', c.ctx); expect(models).toEqual(['provider-actual-id']);
  });
  test('ignores a configured served name when the provider capability opts out', async () => {
    const md = model(); md.status!.provider = { name: 'kaito' }; md.status!.engine = { type: 'llamacpp' };
    const provider: Resource = { apiVersion: 'airunway.ai/v1alpha1', kind: 'InferenceProviderConfig', metadata: { name: 'kaito' }, spec: { capabilities: { engines: [{ name: 'llamacpp', gateway: { ignoresServedName: true } }] } } };
    const seen: string[] = [];
    const port = await localHTTP((req, response) => { if (req.method === 'GET') reply(response, { data: [{ id: 'actual-gguf-id' }] }); else void body(req).then(value => { seen.push(value.model); reply(response, { choices: [{ message: { content: 'done' } }] }); }); });
    const svc = service(); const client = new MockClient(md, provider, svc, pod(svc)); fakeForward(client, port);
    await runAccess('model', 'chat', 'llama', context(client, { message: 'hello' }).ctx);
    expect(seen).toEqual(['actual-gguf-id']);
  });
  test('raw status.servedModelName and spec.model.name are not invented serving fields', async () => {
    const md = model(); delete md.spec!.model.servedName;
    md.spec!.model.name = 'not-a-serving-field'; md.status!.servedModelName = 'not-a-status-field';
    const svc = service(); const client = new MockClient(md, svc); const c = context(client);
    await runAccess('model', 'endpoint', 'llama', c.ctx);
    expect(c.value().servedModelName).toBeUndefined();
  });
  test('discovery failure does not fall back to a repository ID or perform inference', async () => {
    let inference = false;
    const port = await localHTTP((req, response) => { inference ||= req.method === 'POST'; reply(response, { data: [] }); });
    const md = model(); delete md.spec!.model.servedName;
    const svc = service(); const client = new MockClient(md, svc, pod(svc)); fakeForward(client, port);
    await expect(runAccess('model', 'chat', 'llama', context(client, { message: 'hello' }).ctx)).rejects.toThrow('unique served model');
    expect(inference).toBe(false);
  });
  test('gateway chat carries its route header, path and status served model name', async () => {
    const { md, gateway, route, svc } = gatewayResources(); gateway.spec!.listeners[0].protocol = 'HTTP';
    const seen: unknown[] = [];
    const port = await localHTTP((req, response) => { void body(req).then(value => { seen.push({ path: req.url, host: req.headers.host, route: req.headers['x-gateway-model-name'], model: value.model }); reply(response, { choices: [{ message: { content: 'hello' } }] }); }); });
    const client = new MockClient(md, gateway, route, svc, pod(svc)); fakeForward(client, port);
    await runAccess('model', 'chat', 'llama', context(client, { message: 'hello' }).ctx);
    expect(seen).toEqual([{ path: '/models/v1/chat/completions', host: 'inference.example.test', route: 'served-alias', model: 'served-alias' }]);
  });
  test('gateway routing aliases do not override the server model name', async () => {
    const { md, gateway, route, svc } = gatewayResources();
    md.spec!.gateway = { modelName: 'routing-only-alias' }; md.status!.gateway.modelName = 'routing-only-alias';
    md.spec!.model.servedName = 'actual-serving-name';
    route.spec!.rules[0].matches[0].headers[0].value = 'routing-only-alias';
    gateway.spec!.listeners[0].protocol = 'HTTP';
    const seen: unknown[] = [];
    const port = await localHTTP((req, response) => { if (req.method === 'GET') { seen.push('discovery'); reply(response, { data: [{ id: 'discovered-serving-name' }] }); } else void body(req).then(value => { seen.push({ header: req.headers['x-gateway-model-name'], model: value.model }); reply(response, { choices: [{ message: { content: 'ok' } }] }); }); });
    const client = new MockClient(md, gateway, route, svc, pod(svc)); fakeForward(client, port);
    await runAccess('model', 'chat', 'llama', context(client, { message: 'hi' }).ctx);
    expect(seen).toEqual([{ header: 'routing-only-alias', model: 'actual-serving-name' }]);
    seen.length = 0; delete md.spec!.model.servedName;
    await runAccess('model', 'chat', 'llama', context(client, { message: 'hi' }).ctx);
    expect(seen).toEqual(['discovery', { header: 'routing-only-alias', model: 'discovered-serving-name' }]);
  });
  test('agent chat reads only the ingress Secret, discovers the agent served ID and keeps tokens out of output', async () => {
    const seen: unknown[] = [];
    const port = await localHTTP((req, response) => { seen.push({ method: req.method, token: req.headers.authorization }); if (req.method === 'GET') reply(response, { data: [{ id: 'helper' }] }); else void body(req).then(value => { seen.push(value); reply(response, { choices: [{ message: { content: 'done' } }] }); }); });
    const { client } = agentResources(); fakeForward(client, port);
    const c = context(client, { message: 'work', credential: 'never-read-model-key', 'model-credential': 'never-read-model-key' });
    await runAccess('agent', 'chat', 'helper', c.ctx);
    expect(seen[0]).toEqual({ method: 'GET', token: 'Bearer private-ingress-token' });
    expect(seen[1]).toEqual({ method: 'POST', token: 'Bearer private-ingress-token' });
    expect((seen[2] as ChatRequest).model).toBe('helper');
    expect(client.calls.filter(call => call.type?.kind === 'Secret').map(call => call.name)).toEqual(['ingress-key']);
    expect([...c.out, ...c.err].join('')).not.toContain('private-ingress-token');
  });
  test('permission denial is actionable and never echoes a credential-bearing exception', async () => {
    const { client } = agentResources(); client.resources = client.resources.filter(r => r.kind !== 'Secret');
    const port = await localHTTP((_req, response) => reply(response, {})); fakeForward(client, port);
    const c = context(client, { message: 'hello' });
    await expect(runAccess('agent', 'chat', 'helper', c.ctx)).rejects.toMatchObject({ code: 'AUTH', exitCode: 3 });
  });
  test('rejects a foreign Service or pod before reading or sending an ingress token', async () => {
    let calls = 0;
    const port = await localHTTP((_req, response) => { calls++; reply(response, {}); });
    const { client, svc } = agentResources(); delete svc.metadata.ownerReferences; fakeForward(client, port);
    const c = context(client, { message: 'hello' });
    await expect(runAccess('agent', 'chat', 'helper', c.ctx)).rejects.toThrow('not owned');
    expect(calls).toBe(0); expect(client.calls.some(call => call.type?.kind === 'Secret')).toBe(false);
  });
  test('message-file stdin is one-shot and non-TTY interactive input is rejected', async () => {
    const port = await localHTTP((req, response) => { void body(req).then(value => reply(response, { choices: [{ message: { content: value.messages[0].content } }] })); });
    const md = model(), svc = service(); const client = new MockClient(md, svc, pod(svc)); fakeForward(client, port);
    const c = context(client, { 'message-file': '-', output: 'text' }); let reads = 0;
    c.io.input = async () => { reads++; return 'from stdin'; };
    await runAccess('model', 'chat', 'llama', c.ctx); expect(c.out.join('')).toBe('from stdin\n'); expect(reads).toBe(1);
    await expect(runAccess('model', 'chat', 'llama', context(client).ctx)).rejects.toMatchObject({ code: 'USAGE' });
    await expect(runAccess('model', 'chat', 'llama', context(client, { message: 'x', 'message-file': '-' }).ctx)).rejects.toMatchObject({ code: 'USAGE' });
  });
  test('external access needs an exact explicit URL and refuses cleartext credentials', async () => {
    let requests = 0;
    const port = await localHTTP((_req, response) => { requests++; reply(response, {}); });
    const ad = agent(); ad.status!.runtime.address = `http://127.0.0.1:${port}/`;
    const client = new MockClient(ad);
    await expect(runAccess('agent', 'chat', 'helper', context(client, { message: 'hi' }).ctx)).rejects.toThrow('--server');
    await expect(runAccess('agent', 'chat', 'helper', context(client, { message: 'hi', server: `http://127.0.0.1:${port}/different` }).ctx)).rejects.toThrow('exactly match');
    await expect(runAccess('agent', 'chat', 'helper', context(client, { message: 'hi', server: ad.status!.runtime.address }).ctx)).rejects.toThrow('verified HTTPS');
    expect(requests).toBe(0);
  });
  test('external agent HTTPS uses only the ingress token and verifies the published DNS identity', async () => {
    const { ad, secret, client } = agentResources();
    ad.status!.runtime.address = 'https://agents.example.test/api';
    const requests: CapturedHTTPSOptions[] = [];
    const requestSpy = spyOn(https, 'request').mockImplementation(((options: https.RequestOptions, callback: (response: IncomingMessage) => void) => {
      requests.push(options as CapturedHTTPSOptions);
      const request = new EventEmitter() as EventEmitter & { end(): void };
      request.end = () => queueMicrotask(() => {
        const response = Object.assign(new PassThrough(), { statusCode: 200 });
        callback(response as unknown as IncomingMessage);
        response.end(JSON.stringify(options.method === 'GET' ? { data: [{ id: 'agent-public-id' }] } : { choices: [{ message: { content: 'private-ingress-token must be hidden' } }] }));
      });
      return request;
    }) as unknown as typeof https.request);
    cleanups.push(() => requestSpy.mockRestore());
    const c = context(client, { server: ad.status!.runtime.address, message: 'hello' });
    await runAccess('agent', 'chat', 'helper', c.ctx);
    expect(requests.map(r => r.path)).toEqual(['/api/v1/models', '/api/v1/chat/completions']);
    expect(requests.every(r => r.rejectUnauthorized === true && r.servername === 'agents.example.test')).toBe(true);
    expect(requests.every(r => r.headers.authorization === 'Bearer private-ingress-token')).toBe(true);
    expect(requests[0].checkServerIdentity('127.0.0.1', { subjectaltname: 'DNS:other.example.test' } as DetailedPeerCertificate)).toBeInstanceOf(Error);
    expect(c.out.join('')).not.toContain(Buffer.from(secret.data!.token, 'base64').toString());
    expect(c.value().choices[0].message.content).toBe('[redacted] must be hidden');
  });
  test('model ingress credentials use the CLI API_KEY key, never the model download token', async () => {
    const { md, gateway, route, svc } = gatewayResources();
    gateway.status!.addresses = [{ type: 'Hostname', value: 'public.models.test' }];
    const secret: Resource = { apiVersion: 'v1', kind: 'Secret', metadata: { name: 'model-ingress', namespace: 'test' }, data: { API_KEY: Buffer.from('model-ingress-token').toString('base64'), HF_TOKEN: Buffer.from('never-send-hf').toString('base64') } };
    const requests: CapturedHTTPSOptions[] = [];
    const requestSpy = spyOn(https, 'request').mockImplementation(((options: https.RequestOptions, callback: (response: IncomingMessage) => void) => {
      requests.push(options as CapturedHTTPSOptions); const request = new EventEmitter() as EventEmitter & { end(): void };
      request.end = () => queueMicrotask(() => { const response = Object.assign(new PassThrough(), { statusCode: 200 }); callback(response as unknown as IncomingMessage); response.end(JSON.stringify({ choices: [{ message: { content: 'done' } }] })); });
      return request;
    }) as unknown as typeof https.request);
    cleanups.push(() => requestSpy.mockRestore());
    const client = new MockClient(md, gateway, route, svc, secret);
    await runAccess('model', 'chat', 'llama', context(client, { message: 'hi', server: 'https://public.models.test:8443/models', credential: 'model-ingress' }).ctx);
    expect(requests).toHaveLength(1);
    expect(requests[0].headers.authorization).toBe('Bearer model-ingress-token');
  });
  test('refuses HTTPS agent IP addresses instead of guessing a TLS hostname', async () => {
    for (const host of ['127.0.0.1', '[::1]']) {
      const ad = agent(); ad.status!.runtime.address = `https://${host}:8443/`;
      const client = new MockClient(ad);
      await expect(runAccess('agent', 'chat', 'helper', context(client, { message: 'hi', server: ad.status!.runtime.address }).ctx)).rejects.toThrow('TLS hostname fallback');
      expect(client.calls.some(call => call.type?.kind === 'Secret')).toBe(false);
    }
  });
  test('refuses a model or foreign Secret masquerading as an agent ingress credential', async () => {
    const { client, secret } = agentResources(); secret.metadata.ownerReferences = [];
    let requests = 0;
    const port = await localHTTP((_req, response) => { requests++; reply(response, {}); }); fakeForward(client, port);
    await expect(runAccess('agent', 'chat', 'helper', context(client, { message: 'hi' }).ctx)).rejects.toThrow('Secret is not owned');
    expect(requests).toBe(0);
  });
  test('interactive TTY sessions keep history and stop at /exit', async () => {
    const seen: unknown[] = [];
    const port = await localHTTP((req, response) => { void body(req).then(value => { seen.push(value.messages); reply(response, { choices: [{ message: { content: 'answer' } }] }); }); });
    const md = model(), svc = service(); const client = new MockClient(md, svc, pod(svc)); fakeForward(client, port);
    let closed = false;
    const lines = { async *[Symbol.asyncIterator]() { yield 'first'; yield 'second'; yield '/exit'; yield 'not sent'; }, close() { closed = true; } };
    const repl = spyOn(readline, 'createInterface').mockReturnValue(lines as unknown as readline.Interface); cleanups.push(() => repl.mockRestore());
    const c = context(client); c.io.interactive = true;
    await runAccess('model', 'chat', 'llama', c.ctx);
    expect(seen).toEqual([[{ role: 'user', content: 'first' }], [{ role: 'user', content: 'first' }, { role: 'assistant', content: 'answer' }, { role: 'user', content: 'second' }]]);
    expect(repl).toHaveBeenCalledTimes(1); expect(closed).toBe(true);
  });
  test('does not follow HTTP redirects or relay auth to their targets', async () => {
    let destinationRequests = 0;
    const destination = await localHTTP((_req, response) => { destinationRequests++; reply(response, {}); });
    const source = await localHTTP((_req, response) => { response.writeHead(307, { location: `http://127.0.0.1:${destination}/stolen` }); response.end(); });
    const { client } = agentResources(); fakeForward(client, source);
    await expect(runAccess('agent', 'chat', 'helper', context(client, { message: 'hi' }).ctx)).rejects.toThrow('Redirects are not followed');
    expect(destinationRequests).toBe(0);
  });
  test('connect is a loopback-only raw tunnel that preserves caller authentication and closes on cancel', async () => {
    const received: unknown[] = [];
    const port = await localHTTP((req, response) => { received.push(req.headers.authorization); reply(response, { ok: true }); });
    const { client } = agentResources(); fakeForward(client, port);
    const c = context(client); let published!: (value: string) => void;
    const ready = new Promise<string>(resolve => { published = resolve; });
    c.io.out = value => { c.out.push(value); published(JSON.parse(value).url); };
    const running = runAccess('agent', 'connect', 'helper', c.ctx).catch(error => error);
    const url = await ready;
    expect(new URL(url).hostname).toBe('127.0.0.1');
    expect(await (await fetch(url, { headers: { authorization: 'Bearer caller-provided' } })).json()).toEqual({ ok: true });
    expect(received).toEqual(['Bearer caller-provided']);
    expect(client.calls.some(call => call.type?.kind === 'Secret')).toBe(false);
    c.controller.abort(); expect(await running).toMatchObject({ code: 'CANCELED' });
    await expect(fetch(url)).rejects.toThrow();
  });
  test('cancellation terminates the SDK websocket while its upgrade is still pending', async () => {
    let upgradeStarted!: (socket: Socket) => void;
    const upgraded = new Promise<Socket>(resolve => { upgradeStarted = resolve; });
    const api = httpServer();
    const apiSockets = new Set<Socket>();
    api.on('connection', socket => { apiSockets.add(socket); socket.on('close', () => apiSockets.delete(socket)); });
    let upgradePath = '';
    api.on('upgrade', (request, socket) => { upgradePath = request.url!; upgradeStarted(socket as Socket); });
    await new Promise<void>(resolve => api.listen(0, '127.0.0.1', resolve));
    cleanups.push(() => { for (const socket of apiSockets) socket.destroy(); api.close(); });
    const md = model(), svc = service(); const client = new MockClient(md, svc, pod(svc));
    client.kubeConfig = new KubeConfig();
    client.kubeConfig.loadFromOptions({ clusters: [{ name: 'fake', server: `http://127.0.0.1:${(api.address() as { port: number }).port}`, skipTLSVerify: true }], users: [{ name: 'fake' }], contexts: [{ name: 'fake', cluster: 'fake', user: 'fake' }], currentContext: 'fake' });
    const c = context(client); let publish!: (port: number) => void;
    const ready = new Promise<number>(resolve => { publish = resolve; }); c.io.out = value => publish(Number(new URL(JSON.parse(value).url).port));
    const running = runAccess('model', 'connect', 'llama', c.ctx).catch(error => error);
    const socket = connect(await ready, '127.0.0.1'); socket.on('error', () => {}); cleanups.push(() => { socket.destroy(); });
    const pending = await Promise.race([upgraded, running.then(error => { throw error; })]);
    const closed = new Promise<void>(resolve => pending.once('close', () => resolve()));
    pending.resume();
    c.controller.abort();
    expect(await running).toMatchObject({ code: 'CANCELED' });
    await Promise.race([closed, new Promise((_, reject) => { const timer = setTimeout(() => reject(new Error('Pending SDK socket did not close')), 500); closed.finally(() => clearTimeout(timer)); })]);
    expect(upgradePath).toBe('/api/v1/namespaces/test/pods/selected-pod/portforward?ports=8080');
  });
  test('SDK websocket upgrades do not replay cluster credentials across redirects', async () => {
    let redirected = 0;
    const destination = await localHTTP((_req, response) => { redirected++; response.end('not a websocket'); });
    const source = await localHTTP((_req, response) => { response.writeHead(302, { location: `ws://127.0.0.1:${destination}/redirected` }); response.end(); });
    const md = model(), svc = service(); const client = new MockClient(md, svc, pod(svc)); client.kubeConfig = new KubeConfig();
    client.kubeConfig.loadFromOptions({ clusters: [{ name: 'fake', server: `http://127.0.0.1:${source}`, skipTLSVerify: true }], users: [{ name: 'fake', token: 'fake-cluster-token' }], contexts: [{ name: 'fake', cluster: 'fake', user: 'fake' }], currentContext: 'fake' });
    const c = context(client); let publish!: (port: number) => void;
    const ready = new Promise<number>(resolve => { publish = resolve; }); c.io.out = value => publish(Number(new URL(JSON.parse(value).url).port));
    const running = runAccess('model', 'connect', 'llama', c.ctx).catch(error => error);
    const socket = connect(await ready, '127.0.0.1'); socket.on('error', () => {}); cleanups.push(() => { socket.destroy(); });
    await new Promise<void>(resolve => socket.once('close', () => resolve()));
    c.controller.abort(); await running;
    expect(redirected).toBe(0);
  });
  test('cancellation closes a local connection whose forward handshake is pending', async () => {
    const md = model(), svc = service(); const client = new MockClient(md, svc, pod(svc)); client.kubeConfig = new KubeConfig();
    let complete!: (value: Awaited<ReturnType<PortForward['portForward']>>) => void;
    let handshakeStarted!: () => void;
    const handshake = new Promise<void>(resolve => { handshakeStarted = resolve; });
    const spy = spyOn(PortForward.prototype, 'portForward').mockImplementation(() => new Promise(resolve => { complete = resolve; handshakeStarted(); })); cleanups.push(() => spy.mockRestore());
    const c = context(client); let publish!: (port: number) => void;
    const ready = new Promise<number>(resolve => { publish = resolve; }); c.io.out = value => publish(Number(new URL(JSON.parse(value).url).port));
    const running = runAccess('model', 'connect', 'llama', c.ctx).catch(error => error);
    const socket = connect(await ready, '127.0.0.1'); cleanups.push(() => { socket.destroy(); }); socket.on('error', () => {});
    await new Promise<void>(resolve => socket.once('connect', resolve));
    await handshake;
    const ended = new Promise<void>(resolve => socket.once('close', () => resolve()));
    c.controller.abort(); await ended; expect(await running).toMatchObject({ code: 'CANCELED' });
    let terminated = false; complete({ terminate() { terminated = true; } } as unknown as ForwardConnection);
    await Promise.resolve(); await Promise.resolve(); expect(terminated).toBe(true);
  });
});

describe('logs and events', () => {
  test('follows owner UIDs through ReplicaSets and validates container selection', async () => {
    const { ad, root, svc } = agentResources();
    const rs: Resource = { apiVersion: 'apps/v1', kind: 'ReplicaSet', metadata: { name: 'actual-rs', namespace: 'test', uid: 'rs-uid', ownerReferences: [owner(root)] } };
    const p = pod(svc, rs); const client = new MockClient(ad, root, rs, p); const c = context(client, { tail: '25', timestamps: true });
    await runAccess('agent', 'logs', 'helper', c.ctx);
    expect(c.value()).toBe('line one\nline two\n');
    expect(client.calls.find(call => call.path?.endsWith('/log'))).toMatchObject({ method: 'GET', path: '/api/v1/namespaces/test/pods/selected-pod/log', options: { query: { container: 'server', follow: false, tailLines: 25, timestamps: true } } });
    await expect(runAccess('agent', 'logs', 'helper', context(client, { container: 'foreign' }).ctx)).rejects.toMatchObject({ code: 'USAGE' });
  });
  test('rejects pods whose same-name owner UID has changed', async () => {
    const { ad, root, svc } = agentResources();
    const rs: Resource = { apiVersion: 'apps/v1', kind: 'ReplicaSet', metadata: { name: 'recreated-rs', namespace: 'test', uid: 'new-rs', ownerReferences: [owner(root)] } };
    const p = pod(svc, rs); p.metadata.ownerReferences![0].uid = 'old-rs';
    const client = new MockClient(ad, root, rs, p);
    await expect(runAccess('agent', 'logs', 'helper', context(client, { pod: p.metadata.name }).ctx)).rejects.toThrow('No owned pod');
    expect(client.calls.some(call => call.path?.endsWith('/log'))).toBe(false);
  });
  test('model logs use the provider workload reference, not a guessed label', async () => {
    const md = model(); md.status!.provider = { resourceName: 'upstream', resourceKind: 'CustomWorkload' };
    const root: Resource = { apiVersion: 'example.test/v7', kind: 'CustomWorkload', metadata: { name: 'upstream', namespace: 'test', uid: 'upstream-uid' } };
    const svc = service(); const p = pod(svc, root); const client = new MockClient(md, root, p);
    client.onRequest = async (_method, path) => path === '/apis' ? { groups: [{ preferredVersion: { groupVersion: 'example.test/v7' } }] } : { resources: [{ name: 'customworkloads', kind: 'CustomWorkload', namespaced: true }] };
    await runAccess('model', 'logs', 'llama', context(client).ctx);
    expect(client.calls.find(call => call.name === 'upstream')?.type?.plural).toBe('customworkloads');
  });
  test('follow streams decode UTF-8 across chunks and output JSON lines', async () => {
    const { client } = agentResources();
    const encoded = new TextEncoder().encode('café\nlast');
    client.onRaw = async () => new Response(new ReadableStream({ start(controller) { controller.enqueue(encoded.slice(0, 4)); controller.enqueue(encoded.slice(4, 7)); controller.enqueue(encoded.slice(7)); controller.close(); } }));
    const c = context(client, { follow: true });
    await runAccess('agent', 'logs', 'helper', c.ctx);
    expect(c.out.map(v => JSON.parse(v).line)).toEqual(['café', 'last']);
    expect(client.calls.find(call => call.path?.endsWith('/log'))?.options?.query?.follow).toBe(true);
  });
  test('canceling log follow cancels the underlying body reader', async () => {
    const { client } = agentResources(); let canceled = false; let started!: () => void;
    const ready = new Promise<void>(resolve => { started = resolve; });
    client.onRaw = async () => new Response(new ReadableStream({ start() { started(); }, cancel() { canceled = true; } }));
    const c = context(client, { follow: true }); const running = runAccess('agent', 'logs', 'helper', c.ctx).catch(error => error);
    await ready; await Promise.resolve(); c.controller.abort(); expect(await running).toMatchObject({ code: 'CANCELED' });
    await Promise.resolve(); await Promise.resolve(); expect(canceled).toBe(true);
  });
  test('events request a resource UID selector, not a reusable name', async () => {
    const md = model(); const event: Resource = { apiVersion: 'v1', kind: 'Event', metadata: { name: 'event', namespace: 'test' }, reason: 'Ready', message: 'available', type: 'Normal', involvedObject: { uid: md.metadata.uid } };
    const client = new MockClient(md, event); const c = context(client);
    await runAccess('model', 'events', 'llama', c.ctx);
    expect(client.calls[1]).toMatchObject({ type: resourceTypes.event, options: { query: { fieldSelector: 'involvedObject.uid=model-uid' } } });
    expect(c.value()).toEqual([event]);
  });
});
