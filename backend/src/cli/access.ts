import { PortForward } from '@kubernetes/client-node';
import { WebSocketHandler } from '@kubernetes/client-node/dist/web-socket-handler.js';
import { createServer, isIP, type Socket } from 'node:net';
import { checkServerIdentity } from 'node:tls';
import { request as httpRequest } from 'node:http';
import { request as httpsRequest } from 'node:https';
import { createInterface } from 'node:readline';
import { Writable } from 'node:stream';
import { duration, inputValue, integer, text } from './args';
import { output } from './output';
import { CLIError, resourceTypes, type ClusterClient, type CommandContext, type Flags, type IO, type Resource, type ResourceType } from './types';

type Noun = 'model' | 'agent';
type Action = 'endpoint' | 'connect' | 'chat' | 'logs' | 'events';

interface Condition { type: string; status: string; reason?: string; observedGeneration?: number }
interface ServicePort { port: number; targetPort?: number | string; protocol?: string; name?: string; appProtocol?: string }
interface ContainerPort { name?: string; containerPort: number; protocol?: string }
interface Container { name: string; ports?: ContainerPort[] }
interface GatewayListener { name: string; protocol: string; port: number; hostname?: string }
interface RouteHeader { name: string; value: string; type?: string }
interface RouteMatch { method?: string; queryParams?: unknown[]; path?: { type?: string; value?: string }; headers?: RouteHeader[] }
interface EndpointResponse { data?: unknown; choices?: Array<{ message?: { content?: unknown } }> }
const routeType: ResourceType = { group: 'gateway.networking.k8s.io', version: 'v1', plural: 'httproutes', kind: 'HTTPRoute', namespaced: true };
const enc = encodeURIComponent;
const unsupported = (message: string) => new CLIError(message, 2, 'UNSUPPORTED');
const is404 = (error: unknown) => error instanceof CLIError && error.code === 'HTTP_404';
const namespaceOf = (resource: Resource, client: ClusterClient) => resource.metadata.namespace || client.namespace;

function canceled(signal: AbortSignal): CLIError {
  return signal.reason instanceof CLIError ? signal.reason : new CLIError('Operation canceled.', 130, 'CANCELED');
}
function checkSignal(signal: AbortSignal): void { if (signal.aborted) throw canceled(signal); }
// get/list have no per-call signal in the shared interface. Bound the caller's
// wait even when an injected client or a credential plugin does not settle.
function abortable<T>(work: Promise<T>, signal: AbortSignal): Promise<T> {
  return new Promise((resolve, reject) => {
    const abort = () => reject(canceled(signal));
    signal.addEventListener('abort', abort, { once: true });
    work.then(resolve, reject).finally(() => signal.removeEventListener('abort', abort));
    if (signal.aborted) abort();
  });
}
function deadline(flags: Flags, parent: AbortSignal) {
  const timeout = duration(text(flags, 'timeout'));
  const controller = new AbortController();
  const abort = () => controller.abort(canceled(parent));
  parent.addEventListener('abort', abort, { once: true });
  if (parent.aborted) abort();
  const timer = setTimeout(() => controller.abort(new CLIError('Operation timed out. Increase --timeout or inspect logs and events.', 4, 'TIMEOUT')), timeout);
  return { signal: controller.signal, close() { clearTimeout(timer); parent.removeEventListener('abort', abort); controller.abort(); } };
}
function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const done = () => { clearTimeout(timer); signal.removeEventListener('abort', abort); };
    const abort = () => { done(); reject(canceled(signal)); };
    const timer = setTimeout(() => { done(); resolve(); }, ms);
    signal.addEventListener('abort', abort, { once: true });
    if (signal.aborted) abort();
  });
}

export async function waitForResource(client: ClusterClient, noun: Noun, resource: Resource, flags: Flags, _io: IO, signal: AbortSignal): Promise<Resource> {
  const target = (text(flags, 'for') || 'ready').toLowerCase();
  if (!['ready', 'completed'].includes(target) || (target === 'completed' && noun !== 'agent')) throw new CLIError('Use --for ready, or --for completed for an agent job.', 2, 'USAGE');
  const limit = deadline(flags, signal);
  const uid = resource.metadata.uid;
  try {
    let current = resource;
    for (;;) {
      checkSignal(limit.signal);
      if (current.metadata.deletionTimestamp || (uid && current.metadata.uid !== uid)) throw new CLIError('The resource was deleted or replaced while waiting.', 1, 'DELETED');
      const generation = current.metadata.generation;
      const fresh = typeof generation === 'number' && generation > 0 && current.status?.observedGeneration >= generation;
      const conditions: Condition[] = current.status?.conditions || [];
      const currentConditions = conditions.filter(c => typeof generation === 'number' && (c.observedGeneration ?? 0) >= generation);
      // Core advances the aggregate generation independently of provider-owned
      // phase. Require current negative evidence before treating a phase as terminal.
      const phaseOwners = noun === 'agent' ? ['ProviderReady'] : ['Ready', 'Validated', 'ProviderCompatible', 'ResourceCreated'];
      const failed = currentConditions.some(c => (c.type === 'Failed' && c.status === 'True') || (c.status === 'False' && (c.reason === 'JobFailed' || (['Failed', 'Error'].includes(current.status?.phase) && phaseOwners.includes(c.type)))));
      if (failed) {
        throw new CLIError('The resource failed. Inspect its logs and events for details.', 1, 'FAILED');
      }
      const ready = currentConditions.some(c => c.type === 'Ready' && c.status === 'True');
      const completed = noun === 'agent' && current.status?.phase === 'Completed' && currentConditions.some(c => c.status === 'True' && (c.type === 'Ready' || (c.type === 'ProviderReady' && c.reason === 'JobCompleted') || c.type === 'Completed'));
      if (fresh && (target === 'completed' ? completed : ready || completed)) return current;
      await pause(250, limit.signal);
      try { current = await abortable(client.get(resourceTypes[noun], resource.metadata.name, namespaceOf(resource, client)), limit.signal); }
      catch (error) { if (is404(error)) throw new CLIError('The resource was deleted while waiting.', 1, 'DELETED'); throw error; }
    }
  } finally { limit.close(); }
}

interface Endpoint {
  url: string;
  access: 'internal' | 'gateway' | 'external';
  headers: Record<string, string>;
  servedModelName?: string;
  service?: Resource;
  servicePort?: number;
  gateway?: Resource;
  authSecretRef?: { name: string; key: string };
}
function safeURL(value: string): URL {
  let url: URL;
  try { url = new URL(value); } catch { throw unsupported('The published address is not an absolute HTTP or HTTPS URL.'); }
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) throw unsupported('The endpoint must be an HTTP or HTTPS URL without credentials, query parameters, or fragments.');
  return url;
}
function servicePort(service: Resource, requested?: number): ServicePort {
  const ports: ServicePort[] = (service.spec?.ports || []).filter((p: ServicePort) => !p.protocol || p.protocol === 'TCP');
  let matches = requested === undefined ? ports : ports.filter(p => p.port === requested);
  // Older provider status can contain the container port rather than Service port.
  if (!matches.length && requested !== undefined) matches = ports.filter(p => p.targetPort === requested);
  if (matches.length !== 1) throw unsupported(`Cannot choose a unique port on Service ${service.metadata.name}. Check its ports and the published endpoint.`);
  return matches[0];
}
function portScheme(port: ServicePort): string {
  if (port.appProtocol === 'https') return 'https:';
  if (['http', 'kubernetes.io/h2c'].includes(port.appProtocol || '')) return 'http:';
  return /^https(?:-|$)/.test(port.name || '') || port.port === 443 ? 'https:' : 'http:';
}
function addressURL(address: string, protocol: string, port: number): URL {
  const host = address.includes(':') && !address.startsWith('[') ? `[${address}]` : address;
  return safeURL(`${protocol}//${host}:${port}`);
}
async function addressService(client: ClusterClient, url: URL, namespace: string): Promise<Resource | undefined> {
  const match = /^([a-z0-9-]+)\.([a-z0-9-]+)\.svc(?:\.cluster\.local)?\.?$/.exec(url.hostname);
  if (match) return client.get(resourceTypes.service, match[1], match[2]);
  // Short Service names are unambiguous only within the resource's namespace.
  if (/^[a-z][a-z0-9-]*$/.test(url.hostname)) {
    try { return await client.get(resourceTypes.service, url.hostname, namespace); }
    catch (error) { if (!is404(error)) throw error; }
  }
  return undefined;
}
function ownedBy(resource: Resource, owner: Resource): boolean {
  return Boolean(owner.metadata.uid && resource.metadata.ownerReferences?.some(ref => ref.uid === owner.metadata.uid));
}
async function servedModelName(client: ClusterClient, resource: Resource): Promise<string | undefined> {
  // Gateway status is controller-resolved only when no route alias is configured.
  // Raw ModelDeploymentStatus has no servedModelName field: the UI's similarly
  // named field is derived from spec.model.servedName, not spec.model.name.
  const resolved = resource.spec?.gateway?.modelName ? undefined : resource.status?.gateway?.modelName;
  if (resolved) return resolved;
  const declared = resource.spec?.model?.servedName;
  if (!declared) return undefined;
  const providerName = resource.status?.provider?.name || resource.spec?.provider?.name;
  const engine = resource.status?.engine?.type || resource.spec?.engine?.type;
  if (providerName && engine) {
    let provider: Resource;
    try { provider = await client.get(resourceTypes.provider, providerName); }
    catch (error) {
      // Without capability evidence, discover an actual served ID rather than
      // assuming the configured override was honored by this provider.
      if (is404(error) || (error instanceof CLIError && error.code === 'HTTP_403')) return undefined;
      throw error;
    }
    const capability = provider.spec?.capabilities?.engines?.find((entry: { name: string; gateway?: { ignoresServedName?: boolean } }) => entry.name === engine);
    if (!capability || capability.gateway?.ignoresServedName) return undefined;
  }
  return declared;
}
async function gatewayEndpoint(client: ClusterClient, resource: Resource, flags: Flags): Promise<Endpoint> {
  const status = resource.status!.gateway;
  const ns = status.gatewayNamespace || namespaceOf(resource, client);
  const gateway = await client.get(resourceTypes.gateway, status.gatewayName, ns);
  const routes = await client.list(routeType, namespaceOf(resource, client));
  const candidates: Array<{ route: Resource; listener: GatewayListener; match: RouteMatch }> = [];
  for (const route of routes.filter(r => ownedBy(r, resource))) {
    for (const parent of route.spec?.parentRefs || []) {
      if (parent.name !== gateway.metadata.name || (parent.namespace || route.metadata.namespace) !== ns || (parent.kind && parent.kind !== 'Gateway') || (parent.group && parent.group !== routeType.group)) continue;
      for (const listener of gateway.spec?.listeners || []) {
        if (!['HTTP', 'HTTPS'].includes(listener.protocol) || (parent.sectionName && parent.sectionName !== listener.name) || (parent.port && parent.port !== listener.port) || (text(flags, 'gateway-listener') && text(flags, 'gateway-listener') !== listener.name)) continue;
        for (const rule of route.spec?.rules || []) for (const match of rule.matches?.length ? rule.matches : [{}]) {
          if ((match.method && match.method !== 'POST') || match.queryParams?.length || (match.path?.type && match.path.type !== 'PathPrefix')) continue;
          if ((match.headers || []).some((h: RouteHeader) => h.type && h.type !== 'Exact')) continue;
          candidates.push({ route, listener, match });
        }
      }
    }
  }
  if (!candidates.length) throw unsupported('No usable HTTPRoute and HTTP(S) listener were found for this model. Inspect its gateway route.');
  if (candidates.length !== 1) throw unsupported('The gateway has multiple matching routes or listeners. Select one with --gateway-listener or simplify the route.');
  const { route, listener, match } = candidates[0];
  const headers: Record<string, string> = {};
  for (const header of match.headers || []) {
    const key = String(header.name).toLowerCase();
    if (['authorization', 'proxy-authorization', 'cookie', 'connection', 'content-length', 'transfer-encoding'].includes(key) || /[\r\n]/.test(String(header.value))) throw unsupported('This route requires an unsupported credential or transport header.');
    headers[key] = String(header.value);
  }
  const hosts: string[] = route.spec?.hostnames || [];
  const hostname = hosts.find(h => !h.includes('*')) || (!listener.hostname?.includes('*') ? listener.hostname : undefined);
  if (hosts.length > 1 || (hosts.length && !hostname)) throw unsupported('The route needs an explicit hostname. Use a single concrete HTTPRoute hostname.');
  if (hostname) headers.host = hostname;
  const protocol = listener.protocol === 'HTTPS' ? 'https:' : 'http:';
  const published = (gateway.status?.addresses || []).find((a: { type?: string; value: string }) => ['IPAddress', 'Hostname', undefined].includes(a.type) && a.value)?.value;
  const endpoint: Endpoint = { url: '', access: 'gateway', headers, gateway, servicePort: listener.port, servedModelName: await servedModelName(client, resource) };
  if (published) endpoint.url = addressURL(published, protocol, listener.port).href;
  // Discover the gateway's actual Service. Never derive an implementation name.
  const services = await client.list(resourceTypes.service, ns);
  const matching = services.filter(s => ownedBy(s, gateway) || ['gateway.networking.k8s.io/gateway-name', 'istio.io/gateway-name'].some(label => s.metadata.labels?.[label] === gateway.metadata.name) || (s.metadata.labels?.['gateway.envoyproxy.io/owning-gateway-name'] === gateway.metadata.name && s.metadata.labels?.['gateway.envoyproxy.io/owning-gateway-namespace'] === ns));
  const usable = matching.filter(s => (s.spec?.ports || []).some((p: ServicePort) => p.port === listener.port && (!p.protocol || p.protocol === 'TCP')));
  if (usable.length === 1) {
    endpoint.service = usable[0];
    if (!endpoint.url) { endpoint.url = addressURL(`${usable[0].metadata.name}.${ns}.svc`, protocol, listener.port).href; endpoint.access = 'internal'; }
  }
  if (!endpoint.url) throw unsupported('The gateway has no published address or uniquely identified Service. Inspect the Gateway and its Service labels.');
  const url = safeURL(endpoint.url);
  url.pathname = match.path?.value || '/';
  endpoint.url = url.href;
  return endpoint;
}
async function resolveEndpoint(client: ClusterClient, noun: Noun, resource: Resource, flags: Flags): Promise<Endpoint> {
  if (noun === 'agent') {
    if (resource.spec?.lifecycle === 'job' || resource.status?.runtime?.workloadRef?.kind === 'Job') throw unsupported('Agent jobs do not expose endpoints. Use agent wait --for completed, logs, or events.');
    const runtime = resource.status?.runtime;
    if (!runtime?.address) throw unsupported('This agent provider has not published status.runtime.address. For kagent or Orka, use the upstream operator\'s access tools; use a container-backed provider for CLI chat and connect.');
    const url = safeURL(runtime.address);
    if (url.protocol === 'https:' && isIP(url.hostname.replace(/^\[|\]$/g, ''))) throw unsupported('Agent HTTPS endpoints must publish a DNS hostname matching their certificate. TLS hostname fallback for IP addresses is not supported.');
    const service = await addressService(client, url, namespaceOf(resource, client));
    const port = service ? servicePort(service, Number(url.port || (url.protocol === 'https:' ? 443 : 80))).port : undefined;
    return { url: url.href, access: service ? 'internal' : 'external', headers: {}, service, servicePort: port, authSecretRef: runtime.authSecretRef ? { name: runtime.authSecretRef.name, key: runtime.authSecretRef.key } : undefined };
  }
  if (flags.gateway !== false && resource.status?.gateway?.gatewayName) return gatewayEndpoint(client, resource, flags);
  if (flags.gateway === true) throw unsupported('The model has not published a gateway reference. Inspect gateway status or use --gateway=false for its internal Service.');
  const published = resource.status?.endpoint;
  if (!published?.service) throw unsupported('The model has not published an endpoint Service. Wait for readiness or inspect its provider; no Service name can be inferred safely.');
  const service = await client.get(resourceTypes.service, published.service, namespaceOf(resource, client));
  const port = servicePort(service, published.port || undefined);
  return { url: addressURL(`${service.metadata.name}.${namespaceOf(service, client)}.svc`, portScheme(port), port.port).href, access: 'internal', headers: {}, service, servicePort: port.port, servedModelName: await servedModelName(client, resource) };
}
function endpointView(endpoint: Endpoint, resource: Resource) {
  return { name: resource.metadata.name, namespace: resource.metadata.namespace, url: endpoint.url, access: endpoint.access, headers: endpoint.headers, servedModelName: endpoint.servedModelName, service: endpoint.service ? { name: endpoint.service.metadata.name, namespace: endpoint.service.metadata.namespace, port: endpoint.servicePort } : undefined, authRequired: Boolean(endpoint.authSecretRef), authSecretRef: endpoint.authSecretRef };
}

async function selectedPod(client: ClusterClient, endpoint: Endpoint): Promise<{ pod: Resource; port: number }> {
  const service = endpoint.service;
  if (!service) throw unsupported('No gateway or runtime Service was discovered for port forwarding. Inspect its Service labels or use an explicitly trusted --server for chat.');
  const selector = service.spec?.selector;
  if (!selector || !Object.keys(selector).length || service.spec?.type === 'ExternalName') throw unsupported('This Service has no pod selector. CLI port forwarding requires a selector-backed Service.');
  const port = servicePort(service, endpoint.servicePort);
  const pods = await client.list(resourceTypes.pod, namespaceOf(service, client), { labelSelector: Object.entries(selector).map(([k, v]) => `${k}=${v}`).join(',') });
  const pod = pods.filter(p => !p.metadata.deletionTimestamp && p.status?.phase === 'Running' && p.status?.conditions?.some((c: Condition) => c.type === 'Ready' && c.status === 'True') && Object.entries(selector).every(([k, v]) => p.metadata.labels?.[k] === v)).sort((a, b) => a.metadata.name.localeCompare(b.metadata.name))[0];
  if (!pod) throw unsupported('The endpoint Service has no ready matching pods. Wait for readiness or inspect events.');
  let target = port.targetPort ?? port.port;
  if (typeof target === 'string') {
    const matches = (pod.spec?.containers || []).flatMap((c: Container) => c.ports || []).filter((p: ContainerPort) => p.name === target && (!p.protocol || p.protocol === 'TCP'));
    if (matches.length !== 1) throw unsupported('The Service targetPort does not identify a unique TCP container port.');
    target = matches[0].containerPort;
  }
  if (typeof target !== 'number' || !Number.isInteger(target) || target < 1 || target > 65535) throw unsupported('The Service has an invalid targetPort.');
  return { pod, port: target };
}
interface Tunnel { port: number; pod: Resource; close(): void }
async function openTunnel(client: ClusterClient, endpoint: Endpoint, localPort: number, signal: AbortSignal): Promise<Tunnel> {
  if (!client.kubeConfig) throw unsupported('Port forwarding requires the selected cluster\'s kubeconfig.');
  const target = await abortable(selectedPod(client, endpoint), signal);
  checkSignal(signal);
  const sockets = new Set<Socket>();
  type Wire = Awaited<ReturnType<PortForward['portForward']>>;
  const wires = new Set<Wire>();
  let closed = false;
  // Bun provides ws as a built-in compatibility module, including in compiled
  // executables. A static require avoids runtime package-path resolution.
  // The factory tracks upgrades before PortForward's promise has resolved.
  // eslint-disable-next-line @typescript-eslint/no-require-imports -- Bun bundles this static built-in; createRequire would add runtime package resolution.
  const WebSocket = require('ws') as new (...args: Parameters<NonNullable<ConstructorParameters<typeof WebSocketHandler>[1]>>) => Exclude<Awaited<ReturnType<PortForward['portForward']>>, () => unknown>;
  const server = createServer(socket => {
    if (closed) { socket.destroy(); return; }
    sockets.add(socket);
    socket.on('error', () => {});
    let wire: Wire | undefined;
    const closeWire = () => { if (wire && typeof wire !== 'function') { wire.terminate(); wires.delete(wire); } };
    socket.on('close', () => { sockets.delete(socket); closeWire(); });
    const handler = new WebSocketHandler(client.kubeConfig!, (uri, protocols, options) => {
      checkSignal(signal);
      if (closed || socket.destroyed) throw new Error('Tunnel closed');
      const ws = new WebSocket(uri, protocols, { ...options, followRedirects: false, handshakeTimeout: 30000 });
      wire = ws;
      wires.add(ws);
      ws.on('error', () => socket.destroy());
      ws.on('close', () => { wires.delete(ws); socket.destroy(); });
      return ws;
    });
    const forward = new PortForward(client.kubeConfig!, true, handler);
    // Protocol error frames can contain upstream details. Never print them.
    const errors = new Writable({ write(_chunk, _encoding, callback) { socket.destroy(); callback(); } });
    void forward.portForward(namespaceOf(target.pod, client), target.pod.metadata.name, [target.port], socket, errors, socket).then(ws => {
      wire = ws;
      if (closed || socket.destroyed) { closeWire(); return; }
      wires.add(ws);
      if (typeof ws !== 'function') { ws.on('close', () => socket.destroy()); ws.on('error', () => socket.destroy()); }
    }, () => socket.destroy());
  });
  const close = () => {
    if (closed) return;
    closed = true;
    signal.removeEventListener('abort', close);
    for (const socket of sockets) socket.destroy();
    for (const wire of wires) if (typeof wire !== 'function') wire.terminate();
    wires.clear();
    server.close();
  };
  signal.addEventListener('abort', close, { once: true });
  try {
    await abortable(new Promise<void>((resolve, reject) => {
      server.on('error', () => reject(new CLIError('Cannot bind the loopback port. Choose another --port.', 1, 'CONNECTION')));
      server.listen({ host: '127.0.0.1', port: localPort }, () => { if (closed) server.close(); resolve(); });
    }), signal);
    const address = server.address();
    if (!address || typeof address === 'string') throw canceled(signal);
    return { port: address.port, pod: target.pod, close };
  } catch (error) { close(); throw error; }
}

// Resolve owner references through API discovery, never by pluralizing a kind.
async function referenceType(client: ClusterClient, kind: string, apiVersion?: string): Promise<ResourceType> {
  const builtin = Object.values(resourceTypes).find(t => t.kind === kind && (!apiVersion || apiVersion === (t.group ? `${t.group}/${t.version}` : t.version)));
  if (builtin) return builtin;
  const known: Record<string, ResourceType> = {
    Deployment: { group: 'apps', version: 'v1', plural: 'deployments', kind, namespaced: true },
    ReplicaSet: { group: 'apps', version: 'v1', plural: 'replicasets', kind, namespaced: true },
    StatefulSet: { group: 'apps', version: 'v1', plural: 'statefulsets', kind, namespaced: true },
    Job: { group: 'batch', version: 'v1', plural: 'jobs', kind, namespaced: true },
  };
  if (known[kind] && (!apiVersion || apiVersion === `${known[kind].group}/${known[kind].version}`)) return known[kind];
  const versions: string[] = apiVersion ? [apiVersion] : (await client.request<{ groups: Array<{ preferredVersion: { groupVersion: string } }> }>('GET', '/apis')).groups.map(g => g.preferredVersion.groupVersion);
  for (const version of versions) {
    const discovery = await client.request<{ resources: Array<{ name: string; kind: string; namespaced: boolean }> }>('GET', version.includes('/') ? `/apis/${version}` : `/api/${version}`);
    const entry = discovery.resources.find(r => r.kind === kind && !r.name.includes('/'));
    if (entry) { const parts = version.split('/'); return { group: parts.length === 2 ? parts[0] : '', version: parts[parts.length - 1], plural: entry.name, kind, namespaced: entry.namespaced }; }
  }
  throw unsupported(`The API for workload kind ${kind} is not installed or discoverable.`);
}
async function workload(client: ClusterClient, noun: Noun, resource: Resource): Promise<Resource> {
  const ref = noun === 'agent' ? resource.status?.runtime?.workloadRef : resource.status?.workloadRef || (resource.status?.provider?.resourceName ? { name: resource.status.provider.resourceName, kind: resource.status.provider.resourceKind, apiVersion: resource.status.provider.apiVersion } : undefined);
  if (!ref?.name || !ref.kind) throw unsupported('The provider has not published a workload reference. Inspect the resource status and provider.');
  return client.get(await referenceType(client, ref.kind, ref.apiVersion), ref.name, ref.namespace || namespaceOf(resource, client));
}
async function descendsFrom(client: ClusterClient, child: Resource, root: Resource, cache: Map<string, Promise<Resource | undefined>>, seen = new Set<string>()): Promise<boolean> {
  if (!root.metadata.uid) return false;
  if (child.metadata.uid === root.metadata.uid) return true;
  for (const ref of child.metadata.ownerReferences || []) {
    if (ref.uid === root.metadata.uid) return true;
    if (seen.has(ref.uid) || seen.size >= 32) continue;
    seen.add(ref.uid);
    let pending = cache.get(ref.uid);
    if (!pending) {
      pending = (async () => {
        try { const parent = await client.get(await referenceType(client, ref.kind, ref.apiVersion), ref.name, namespaceOf(child, client)); return parent.metadata.uid === ref.uid ? parent : undefined; }
        catch (error) { if (is404(error)) return undefined; throw error; }
      })();
      cache.set(ref.uid, pending);
    }
    const parent = await pending;
    if (parent && await descendsFrom(client, parent, root, cache, seen)) return true;
  }
  return false;
}
async function logs(client: ClusterClient, noun: Noun, resource: Resource, ctx: CommandContext): Promise<void> {
  const root = await workload(client, noun, resource);
  if (!root.metadata.uid) throw unsupported('The backing workload has no UID; pod ownership cannot be verified.');
  const ns = namespaceOf(root, client);
  const candidates = text(ctx.flags, 'pod') ? [await client.get(resourceTypes.pod, text(ctx.flags, 'pod')!, ns)] : await client.list(resourceTypes.pod, ns);
  const cache = new Map<string, Promise<Resource | undefined>>();
  const pods: Resource[] = [];
  for (const pod of candidates) if (await descendsFrom(client, pod, root, cache)) pods.push(pod);
  if (pods.length !== 1) throw unsupported(pods.length ? 'More than one pod belongs to this workload. Select one with --pod.' : 'No owned pod was found. Choose a --pod belonging to the published workload.');
  const pod = pods[0];
  const containers: string[] = [...(pod.spec?.containers || []), ...(pod.spec?.initContainers || []), ...(pod.spec?.ephemeralContainers || [])].map((c: Container) => c.name);
  const container = text(ctx.flags, 'container') || (containers.length === 1 ? containers[0] : undefined);
  if (!container || !containers.includes(container)) throw new CLIError('Select a --container that belongs to this pod.', 2, 'USAGE');
  const response = await client.raw('GET', `/api/v1/namespaces/${enc(ns)}/pods/${enc(pod.metadata.name)}/log`, undefined, { signal: ctx.signal, query: { container, follow: Boolean(ctx.flags.follow), tailLines: integer(ctx.flags, 'tail', 100), timestamps: Boolean(ctx.flags.timestamps) } });
  if (!response.ok) throw new CLIError(`Reading logs failed with HTTP ${response.status}.`, 1, 'LOGS');
  if (!ctx.flags.follow) { output(ctx.io, ctx.flags, await abortable(response.text(), ctx.signal)); return; }
  if (!response.body) return;
  const reader = response.body.getReader();
  const abort = () => { void reader.cancel().catch(() => {}); };
  ctx.signal.addEventListener('abort', abort, { once: true });
  const decoder = new TextDecoder();
  let pending = '';
  const write = (chunk: string, final = false) => {
    if (!text(ctx.flags, 'output') || text(ctx.flags, 'output') === 'text') { if (chunk) ctx.io.out(chunk); return; }
    pending += chunk;
    let index: number;
    while ((index = pending.indexOf('\n')) >= 0) { output(ctx.io, ctx.flags, { pod: pod.metadata.name, container, line: pending.slice(0, index) }); pending = pending.slice(index + 1); }
    if (final && pending) output(ctx.io, ctx.flags, { pod: pod.metadata.name, container, line: pending });
  };
  try {
    for (;;) { const { value, done } = await abortable(reader.read(), ctx.signal); checkSignal(ctx.signal); if (done) break; write(decoder.decode(value, { stream: true })); }
    write(decoder.decode(), true);
  } finally { ctx.signal.removeEventListener('abort', abort); await reader.cancel().catch(() => {}); reader.releaseLock(); }
}

interface Transport { endpoint: Endpoint; tunnel?: Tunnel; close(): void }
async function transport(client: ClusterClient, endpoint: Endpoint, flags: Flags, signal: AbortSignal): Promise<Transport> {
  const explicit = text(flags, 'server');
  if (explicit) {
    const url = safeURL(explicit);
    // An explicit destination is the user's trust decision, but it must not
    // silently redirect an agent's ingress credential to a different origin.
    if (url.href !== safeURL(endpoint.url).href) throw unsupported('--server must exactly match the published endpoint URL.');
    return { endpoint: { ...endpoint, url: url.href }, close() {} };
  }
  if (!endpoint.service) throw unsupported('External status addresses are not contacted automatically. Verify the endpoint, then pass its exact URL with --server.');
  const tunnel = await openTunnel(client, endpoint, 0, signal);
  return { endpoint, tunnel, close: tunnel.close };
}
function apiURL(endpoint: Endpoint, path: string): URL {
  const url = safeURL(endpoint.url);
  const base = url.pathname.replace(/\/$/, '');
  url.pathname = `${base.endsWith('/v1') && path.startsWith('/v1/') ? base.slice(0, -3) : base}${path}`;
  return url;
}
async function httpJSON(connection: Transport, path: string, body: unknown, signal: AbortSignal, token?: string, expectJSON = true): Promise<EndpointResponse> {
  checkSignal(signal);
  const url = apiURL(connection.endpoint, path);
  const data = body === undefined ? undefined : JSON.stringify(body);
  const headers: Record<string, string> = { ...connection.endpoint.headers, accept: 'application/json' };
  if (token) headers.authorization = `Bearer ${token}`;
  if (data) { headers['content-type'] = 'application/json'; headers['content-length'] = String(Buffer.byteLength(data)); }
  // Keep TLS SNI and verification for the original host through a loopback
  // tunnel. Do not inherit kubeconfig credentials, CA bypasses, or redirects.
  const tlsHost = headers.host ? new URL(`${url.protocol}//${headers.host}`).hostname.replace(/^\[|\]$/g, '') : url.hostname.replace(/^\[|\]$/g, '');
  return new Promise((resolve, reject) => {
    const request = (url.protocol === 'https:' ? httpsRequest : httpRequest)({ hostname: connection.tunnel ? '127.0.0.1' : url.hostname.replace(/^\[|\]$/g, ''), port: connection.tunnel?.port || Number(url.port || (url.protocol === 'https:' ? 443 : 80)), path: url.pathname, method: data ? 'POST' : 'GET', headers: { host: url.host, ...headers }, servername: isIP(tlsHost) ? undefined : tlsHost, checkServerIdentity: (_host, certificate) => checkServerIdentity(tlsHost, certificate), rejectUnauthorized: true, signal }, response => {
      if (!response.statusCode || response.statusCode < 200 || response.statusCode >= 300) {
        response.resume(); reject(new CLIError(`Endpoint request failed with HTTP ${response.statusCode ?? 'unknown'}. Redirects are not followed.`, 1, 'HTTP')); return;
      }
      const chunks: Buffer[] = [];
      let size = 0;
      response.on('data', chunk => { size += chunk.length; if (size > 4 * 1024 * 1024) { response.destroy(); reject(new CLIError('Endpoint response exceeds 4 MiB.', 1, 'RESPONSE')); } else chunks.push(Buffer.from(chunk)); });
      response.on('end', () => { const result = Buffer.concat(chunks).toString('utf8'); if (!expectJSON) { resolve({}); return; } try { resolve(result ? JSON.parse(result) : {}); } catch { reject(new CLIError('Endpoint returned an invalid JSON response.', 1, 'RESPONSE')); } });
      response.on('error', () => reject(new CLIError('Endpoint response was interrupted.', 1, 'CONNECTION')));
    });
    request.on('error', () => reject(signal.aborted ? canceled(signal) : new CLIError('Cannot connect to the endpoint. Check connectivity and TLS certificates.', 1, 'CONNECTION')));
    request.end(data);
  });
}
async function ingressToken(client: ClusterClient, noun: Noun, resource: Resource, endpoint: Endpoint, connection: Transport, flags: Flags): Promise<string | undefined> {
  // Never use modelBinding.auth, spec.model credentials, or a model flag for an agent call.
  const ref = noun === 'agent' ? endpoint.authSecretRef : text(flags, 'credential') ? { name: text(flags, 'credential')!, key: 'API_KEY' } : undefined;
  if (!ref) return undefined;
  if (!connection.tunnel && safeURL(endpoint.url).protocol !== 'https:') throw unsupported('Credentials require verified HTTPS for direct external access. Use a cluster tunnel instead.');
  const ns = namespaceOf(resource, client);
  if (noun === 'agent' && connection.tunnel) {
    const root = await workload(client, noun, resource);
    if (!ownedBy(root, resource) || !endpoint.service || namespaceOf(endpoint.service, client) !== ns || !await descendsFrom(client, endpoint.service, resource, new Map())) throw unsupported('The agent endpoint is not owned by this AgentDeployment. Refusing to send its ingress token.');
    if (!await descendsFrom(client, connection.tunnel.pod, root, new Map())) throw unsupported('The selected pod is not owned by the agent workload. Refusing to send its ingress token.');
  }
  if (!ref.name || !ref.key) throw unsupported('The published ingress Secret reference is incomplete.');
  let secret: Resource;
  try { secret = await client.get(resourceTypes.credential, ref.name, ns); }
  catch { throw new CLIError('Cannot read the endpoint ingress Secret. Request permission to read that Secret in the resource namespace.', 3, 'AUTH'); }
  if (noun === 'agent' && !ownedBy(secret, resource)) throw unsupported('The ingress Secret is not owned by this AgentDeployment. Refusing to use a model or unrelated credential.');
  const encoded = secret.data?.[ref.key];
  if (!encoded || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(encoded)) throw new CLIError('The ingress Secret is missing its required key or contains invalid data.', 3, 'AUTH');
  const token = Buffer.from(encoded, 'base64').toString('utf8');
  if (!token || /[^\x21-\x7e]/.test(token)) throw new CLIError('The ingress Secret contains an invalid bearer token.', 3, 'AUTH');
  return token;
}
async function chat(client: ClusterClient, noun: Noun, resource: Resource, endpoint: Endpoint, ctx: CommandContext): Promise<void> {
  if (noun === 'model' && resource.status?.gateway?.apiFormats?.length && !resource.status.gateway.apiFormats.includes('openai-chat')) throw unsupported('This model provider does not advertise the OpenAI chat API. Use a provider with openai-chat support.');
  const message = await abortable(inputValue(ctx.flags, 'message', 'message-file', ctx.io.input.bind(ctx.io)), ctx.signal);
  if (message === undefined && !ctx.io.interactive) throw new CLIError('Provide --message or --message-file when stdin is not a terminal.', 2, 'USAGE');
  if (message !== undefined && !message.trim()) throw new CLIError('The chat message must not be empty.', 2, 'USAGE');
  const temperatureRaw = text(ctx.flags, 'temperature');
  const temperature = temperatureRaw === undefined ? undefined : Number(temperatureRaw);
  if (temperature !== undefined && (!temperatureRaw?.trim() || !Number.isFinite(temperature) || temperature < 0 || temperature > 2)) throw new CLIError('--temperature must be between 0 and 2.', 2, 'USAGE');
  const maxTokens = integer(ctx.flags, 'max-tokens', undefined, 1);
  const connection = await transport(client, endpoint, ctx.flags, ctx.signal);
  try {
    const token = await ingressToken(client, noun, resource, endpoint, connection, ctx.flags);
    let model = noun === 'model' ? endpoint.servedModelName : undefined;
    if (!model) {
      const discovery = await httpJSON(connection, '/v1/models', undefined, ctx.signal, token);
      const ids: string[] = (Array.isArray(discovery?.data) ? discovery.data : []).map((item: { id?: unknown } | null) => item?.id).filter((id: unknown): id is string => typeof id === 'string' && id.length > 0);
      const preferred = noun === 'model' ? [resource.spec?.model?.servedName, resource.spec?.model?.id] : [resource.metadata.name];
      model = preferred.find(id => ids.includes(id)) || (ids.length === 1 ? ids[0] : undefined);
      if (!model) throw unsupported('The endpoint did not identify a unique served model. Publish a resolved model name or configure a single served ID.');
    }
    const messages: Array<{ role: string; content: string }> = [];
    const turn = async (content: string) => {
      messages.push({ role: 'user', content });
      const result = await httpJSON(connection, '/v1/chat/completions', { model, messages, stream: false, ...(temperature === undefined ? {} : { temperature }), ...(maxTokens === undefined ? {} : { max_tokens: maxTokens }) }, ctx.signal, token);
      const redact = (value: unknown): unknown => {
        if (!token) return value;
        if (typeof value === 'string') return value.split(token).join('[redacted]');
        if (Array.isArray(value)) return value.map(redact);
        if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, entry]) => [key.split(token).join('[redacted]'), redact(entry)]));
        return value;
      };
      const safeResult = redact(result) as EndpointResponse;
      const answer = safeResult.choices?.[0]?.message?.content;
      if (typeof answer !== 'string') throw new CLIError('The endpoint did not return a text chat response.', 1, 'RESPONSE');
      messages.push({ role: 'assistant', content: answer });
      output(ctx.io, ctx.flags, text(ctx.flags, 'output') && text(ctx.flags, 'output') !== 'text' ? safeResult : answer);
    };
    if (message !== undefined) { await turn(message); return; }
    // io.interactive is the command host's TTY decision. Never consume process
    // stdin for non-interactive commands, including one-shot file/stdin input.
    const lines = createInterface({ input: process.stdin, terminal: false });
    const abort = () => lines.close();
    ctx.signal.addEventListener('abort', abort, { once: true });
    try {
      ctx.io.err('Message (/exit to quit): ');
      for await (const line of lines) { checkSignal(ctx.signal); if (['/exit', '/quit'].includes(line.trim())) break; if (line.trim()) await turn(line); ctx.io.err('Message (/exit to quit): '); }
      checkSignal(ctx.signal);
    } finally { ctx.signal.removeEventListener('abort', abort); lines.close(); }
  } finally { connection.close(); }
}

export async function runAccess(noun: Noun, action: Action, name: string, ctx: CommandContext): Promise<void> {
  const client = ctx.client();
  const limit = deadline(ctx.flags, ctx.signal);
  const scoped = { ...ctx, signal: limit.signal };
  try {
    checkSignal(scoped.signal);
    const resource = await abortable(client.get(resourceTypes[noun], name, ctx.namespace), scoped.signal);
    if (action === 'logs') { await abortable(logs(client, noun, resource, scoped), scoped.signal); return; }
    if (action === 'events') {
      if (!resource.metadata.uid) throw unsupported('The resource has no UID; its events cannot be selected safely.');
      const events = await abortable(client.list(resourceTypes.event, namespaceOf(resource, client), { fieldSelector: `involvedObject.uid=${resource.metadata.uid}` }), scoped.signal);
      output(ctx.io, ctx.flags, text(ctx.flags, 'output') && text(ctx.flags, 'output') !== 'text' ? events : events.map(e => ({ time: e.lastTimestamp || e.eventTime || e.metadata.creationTimestamp, type: e.type, reason: e.reason, message: e.message, count: e.count })));
      return;
    }
    const endpoint = await abortable(resolveEndpoint(client, noun, resource, ctx.flags), scoped.signal);
    if (action === 'endpoint') {
      if (ctx.flags.check) {
        const connection = await transport(client, endpoint, ctx.flags, scoped.signal);
        try { await httpJSON(connection, noun === 'agent' ? '/readyz' : '/v1/models', undefined, scoped.signal, undefined, noun === 'model'); }
        finally { connection.close(); }
      }
      output(ctx.io, ctx.flags, { ...endpointView(endpoint, resource), ...(ctx.flags.check ? { reachable: true } : {}) });
    } else if (action === 'connect') {
      const tunnel = await openTunnel(client, endpoint, integer(ctx.flags, 'port', 0, 0, 65535)!, scoped.signal);
      try {
        const local = safeURL(endpoint.url); local.hostname = '127.0.0.1'; local.port = String(tunnel.port);
        output(ctx.io, ctx.flags, { ...endpointView(endpoint, resource), url: local.href, upstream: endpoint.url, access: 'loopback' });
        if (endpoint.authSecretRef) ctx.io.err('This raw tunnel preserves upstream authentication. Read the ingress Secret separately if authorized; no token is printed or injected.\n');
        await abortable(new Promise<never>(() => {}), scoped.signal);
      } finally { tunnel.close(); }
    } else if (action === 'chat') await abortable(chat(client, noun, resource, endpoint, scoped), scoped.signal);
    else throw new CLIError('Unknown access action.', 2, 'USAGE');
  } finally { limit.close(); }
}
