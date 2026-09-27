import * as k8s from '@kubernetes/client-node';
import { kubeConfigToBunTls } from '../lib/kubeconfig';
import { CLIError, resourceTypes, type ClusterClient, type JsonObject, type RequestOptions, type Resource, type ResourceType } from './types';

export function resourcePath(type: ResourceType, namespace?: string, name?: string): string {
  const prefix = type.group ? `/apis/${type.group}/${type.version}` : `/api/${type.version}`;
  return `${prefix}${type.namespaced && namespace ? `/namespaces/${encodeURIComponent(namespace)}` : ''}/${type.plural}${name ? `/${encodeURIComponent(name)}` : ''}`;
}
export function typeFor(resource: Resource): ResourceType {
  const type = Object.values(resourceTypes).find(t => t.kind === resource.kind && resource.apiVersion === (t.group ? `${t.group}/${t.version}` : t.version));
  if (!type) throw new CLIError(`Unsupported resource ${resource.apiVersion}/${resource.kind}.`, 2, 'USAGE');
  return type;
}
export function loadClientConfig(file?: string, context?: string): k8s.KubeConfig {
  const config = new k8s.KubeConfig();
  try { if (file) config.loadFromFile(file); else config.loadFromDefault(); }
  catch { throw new CLIError('Cannot load cluster credentials. Check --kubeconfig or KUBECONFIG.', 3, 'AUTH'); }
  if (context) {
    if (!config.getContexts().some(c => c.name === context)) throw new CLIError(`Context "${context}" does not exist in your kubeconfig.`, 2, 'USAGE');
    config.setCurrentContext(context);
  }
  return config;
}
export class KubernetesCLIClient implements ClusterClient {
  readonly context: string;
  constructor(readonly kubeConfig: k8s.KubeConfig, readonly namespace: string, private readonly signal?: AbortSignal) {
    this.context = kubeConfig.getCurrentContext();
    if (!kubeConfig.getCurrentCluster()) throw new CLIError('No cluster selected. Configure kubeconfig, then run airunway context list.', 3, 'AUTH');
  }
  async raw(method: string, path: string, body?: unknown, options: RequestOptions = {}): Promise<Response> {
    const cluster = this.kubeConfig.getCurrentCluster()!;
    if ((cluster as unknown as Record<string, unknown>).proxyUrl) throw new CLIError('This build cannot honor kubeconfig proxy-url. Use a directly reachable cluster endpoint.', 3, 'AUTH');
    if (!path.startsWith('/') || path.startsWith('//')) throw new CLIError('Invalid cluster request path.', 2, 'USAGE');
    const url = new URL(cluster.server.replace(/\/$/, '') + path);
    for (const [key, value] of Object.entries(options.query || {})) if (value !== undefined) url.searchParams.set(key, String(value));
    const authContext = new k8s.RequestContext(url.toString(), method as k8s.HttpMethod);
    await this.kubeConfig.applySecurityAuthentication(authContext);
    const headers = new Headers(authContext.getHeaders());
    headers.set('Accept', 'application/json');
    if (body !== undefined) headers.set('Content-Type', options.contentType || 'application/json');
    const connectionDeadline = new AbortController();
    const timer = setTimeout(() => connectionDeadline.abort(), 30000);
    const signals = [options.signal, this.signal, connectionDeadline.signal].filter((s): s is AbortSignal => Boolean(s));
    let response: Response;
    try {
      response = await fetch(url, { headers, method, body: body === undefined ? undefined : typeof body === 'string' ? body : JSON.stringify(body), redirect: 'error', signal: AbortSignal.any(signals), tls: await kubeConfigToBunTls(this.kubeConfig) });
    } catch {
      clearTimeout(timer);
      if (signals.some(s => s.aborted)) throw new CLIError('Cluster request canceled or timed out.', this.signal?.aborted ? 130 : 4, 'TIMEOUT');
      throw new CLIError('Cannot connect to the selected cluster. Check connectivity and credentials.', 3, 'CONNECTION');
    }
    clearTimeout(timer);
    if (!response.ok) {
      // Do not print raw server bodies: admission errors may echo credentials.
      const code = response.status;
      const message = code === 401 ? 'Cluster credentials were rejected.' : code === 403 ? 'Your identity does not have permission for this operation.' : code === 404 ? 'The requested resource or API is not installed.' : code === 409 ? 'The resource already exists or changed concurrently. Read it again before updating.' : code === 422 ? 'The cluster rejected the resource. Check its fields and provider compatibility.' : `Cluster request failed with HTTP ${code}.`;
      throw new CLIError(message, code === 401 || code === 403 ? 3 : code === 409 ? 5 : 1, `HTTP_${code}`);
    }
    return response;
  }
  async request<T = JsonObject>(method: string, path: string, body?: unknown, options?: RequestOptions): Promise<T> {
    const signal = AbortSignal.any([options?.signal, this.signal, AbortSignal.timeout(30000)].filter((s): s is AbortSignal => Boolean(s)));
    try {
      const response = await this.raw(method, path, body, { ...options, signal });
      if (response.status === 204) return undefined as T;
      const value = await response.text();
      try { return value ? JSON.parse(value) as T : undefined as T; }
      catch { throw new CLIError('The cluster returned an invalid JSON response.', 1, 'RESPONSE'); }
    } catch (error) {
      if (error instanceof CLIError) throw error;
      if (signal.aborted) throw new CLIError('Cluster request canceled or timed out.', this.signal?.aborted ? 130 : 4, this.signal?.aborted ? 'INTERRUPTED' : 'TIMEOUT');
      throw new CLIError('Cannot read the cluster response. Check connectivity and try again.', 3, 'CONNECTION');
    }
  }
  async list(type: ResourceType, namespace = type.namespaced ? this.namespace : undefined, query: RequestOptions['query'] = {}): Promise<Resource[]> {
    const items: Resource[] = []; let continuation: string | undefined;
    do {
      const page = await this.request<{ items: Resource[]; metadata?: { continue?: string } }>('GET', resourcePath(type, namespace || undefined), undefined, { query: { limit: 500, ...query, continue: continuation } });
      items.push(...page.items); continuation = page.metadata?.continue;
    } while (continuation);
    return items;
  }
  get(type: ResourceType, name: string, namespace = this.namespace): Promise<Resource> { return this.request('GET', resourcePath(type, namespace, name)); }
  create(resource: Resource, dryRun = false): Promise<Resource> {
    const type = typeFor(resource);
    return this.request('POST', resourcePath(type, resource.metadata.namespace || this.namespace), resource, { query: { dryRun: dryRun ? 'All' : undefined, fieldManager: 'airunway-cli', fieldValidation: 'Strict' } });
  }
  patch(type: ResourceType, name: string, patch: unknown, namespace = this.namespace, dryRun = false): Promise<Resource> {
    return this.request('PATCH', resourcePath(type, namespace, name), patch, { contentType: 'application/merge-patch+json', query: { dryRun: dryRun ? 'All' : undefined, fieldManager: 'airunway-cli', fieldValidation: 'Strict' } });
  }
  async delete(type: ResourceType, name: string, namespace = this.namespace, uid?: string): Promise<void> {
    await this.request('DELETE', resourcePath(type, namespace, name), { apiVersion: 'v1', kind: 'DeleteOptions', propagationPolicy: 'Background', ...(uid ? { preconditions: { uid } } : {}) });
  }
}
