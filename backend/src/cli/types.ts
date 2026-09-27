import type { KubeConfig } from '@kubernetes/client-node';

// JSON object compatibility at the Kubernetes unstructured-resource boundary.
// Callers validate the fields they consume; unknown fields must survive patches.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type JsonObject = Record<string, any>;

// Kubernetes resources are extensible JSON documents. Keep their unknown fields
// intact when reading and patching instead of round-tripping through UI types.
export interface Resource {
  apiVersion: string;
  kind: string;
  metadata: {
    name: string;
    namespace?: string;
    uid?: string;
    generation?: number;
    resourceVersion?: string;
    creationTimestamp?: string;
    deletionTimestamp?: string;
    labels?: Record<string, string>;
    annotations?: Record<string, string>;
    ownerReferences?: Array<{ apiVersion: string; kind: string; name: string; uid: string; controller?: boolean }>;
    [key: string]: unknown;
  };
  spec?: JsonObject;
  status?: JsonObject;
  data?: Record<string, string>;
  stringData?: Record<string, string>;
  type?: string;
  [key: string]: unknown;
}

export interface ResourceType { group: string; version: string; plural: string; kind: string; namespaced: boolean }
export const resourceTypes = {
  model: { group: 'airunway.ai', version: 'v1alpha1', plural: 'modeldeployments', kind: 'ModelDeployment', namespaced: true },
  agent: { group: 'airunway.ai', version: 'v1alpha1', plural: 'agentdeployments', kind: 'AgentDeployment', namespaced: true },
  provider: { group: 'airunway.ai', version: 'v1alpha1', plural: 'inferenceproviderconfigs', kind: 'InferenceProviderConfig', namespaced: false },
  framework: { group: 'airunway.ai', version: 'v1alpha1', plural: 'agentproviderconfigs', kind: 'AgentProviderConfig', namespaced: false },
  credential: { group: '', version: 'v1', plural: 'secrets', kind: 'Secret', namespaced: true },
  service: { group: '', version: 'v1', plural: 'services', kind: 'Service', namespaced: true },
  pod: { group: '', version: 'v1', plural: 'pods', kind: 'Pod', namespaced: true },
  event: { group: '', version: 'v1', plural: 'events', kind: 'Event', namespaced: true },
  gateway: { group: 'gateway.networking.k8s.io', version: 'v1', plural: 'gateways', kind: 'Gateway', namespaced: true },
} as const satisfies Record<string, ResourceType>;
export type ResourceNoun = 'model' | 'agent';
export type Flags = Record<string, string | boolean | string[] | undefined>;
export interface ParsedArgs { words: string[]; flags: Flags }
export interface IO { out(text: string): void; err(text: string): void; input(): Promise<string>; interactive: boolean }
export interface RequestOptions { query?: Record<string, string | number | boolean | undefined>; contentType?: string; signal?: AbortSignal }
export interface ClusterClient {
  readonly namespace: string;
  readonly context: string;
  readonly kubeConfig?: KubeConfig;
  request<T = JsonObject>(method: string, path: string, body?: unknown, options?: RequestOptions): Promise<T>;
  raw(method: string, path: string, body?: unknown, options?: RequestOptions): Promise<Response>;
  list(type: ResourceType, namespace?: string, query?: RequestOptions['query']): Promise<Resource[]>;
  get(type: ResourceType, name: string, namespace?: string): Promise<Resource>;
  create(resource: Resource, dryRun?: boolean): Promise<Resource>;
  patch(type: ResourceType, name: string, patch: unknown, namespace?: string, dryRun?: boolean): Promise<Resource>;
  delete(type: ResourceType, name: string, namespace?: string, uid?: string): Promise<void>;
}
export interface CommandContext { flags: Flags; io: IO; signal: AbortSignal; client(): ClusterClient; namespace: string; context: string }
export class CLIError extends Error {
  constructor(message: string, readonly exitCode = 1, readonly code = 'ERROR') { super(message); this.name = 'CLIError'; }
}
