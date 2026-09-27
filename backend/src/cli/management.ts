import { readdir, lstat, readFile } from 'node:fs/promises';
import { extname, join } from 'node:path';
import { CORE_SCHEMA, loadAll } from 'js-yaml';
import bundledModels from '../data/models.json';
import { duration, required, text } from './args';
import { resourcePath } from './client';
import { output } from './output';
import { CLIError, resourceTypes, type ClusterClient, type CommandContext, type Flags, type Resource } from './types';

const MANAGED_BY = 'app.kubernetes.io/managed-by';
const CREDENTIAL_TYPE = 'airunway.ai/credential-type';
const MANAGER = 'airunway-cli';
const CREDENTIAL_KEYS = { huggingface: 'HF_TOKEN', 'api-key': 'API_KEY', artifact: 'credentials' } as const;
const MAX_INPUT = 4 * 1024 * 1024;
const MAX_ARTIFACT_CREDENTIALS = 64 * 1024; // Matches the downloader credential-document limit.
const globalFlags = ['kubeconfig', 'context', 'namespace', 'output', 'timeout', 'help', 'version'];
type CredentialType = keyof typeof CREDENTIAL_KEYS;
type ObjectValue = Record<string, unknown>;

function fail(message: string): never { throw new CLIError(message, 2, 'USAGE'); }
function object(value: unknown): value is ObjectValue {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}
function identifier(value: unknown, label = 'name', max = 253): string {
  if (typeof value !== 'string' || value.length > max || !/^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/.test(value)) {
    fail(`Provide a valid ${label}.`);
  }
  return value;
}
function namespace(ctx: CommandContext): string {
  const ns = identifier(ctx.namespace, 'namespace', 63);
  if (ns.includes('.') || ctx.flags['all-namespaces']) fail('This command requires one namespace.');
  return ns;
}
function options(ctx: CommandContext, allowed: string[]): void {
  if (Object.keys(ctx.flags).some(key => !globalFlags.includes(key) && !allowed.includes(key))) fail('Unsupported option for this command. Run with --help.');
  if (!['text', 'json', 'yaml'].includes(text(ctx.flags, 'output') || 'text')) fail('--output must be text, json, or yaml.');
}
function arity(words: string[], count: number): void {
  if (words.length !== count) fail('Unexpected or missing command arguments. Run with --help.');
}
function dryRun(flags: Flags): string | undefined {
  const value = text(flags, 'dry-run');
  if (value !== undefined && value !== 'client' && value !== 'server') fail('--dry-run must be client or server.');
  return value;
}
function canceled(ctx: CommandContext): void {
  if (ctx.signal.aborted) throw new CLIError('Interrupted. Already submitted resources were not rolled back.', 130, 'INTERRUPTED');
}
function credentialType(value: unknown): CredentialType {
  if (value !== 'huggingface' && value !== 'api-key' && value !== 'artifact') fail('--type must be huggingface, api-key, or artifact.');
  return value;
}
function ownedCredential(resource: Resource, ns: string): CredentialType {
  if (resource.kind !== 'Secret' || resource.apiVersion !== 'v1' || resource.metadata.namespace !== ns ||
      resource.metadata.labels?.[MANAGED_BY] !== MANAGER || resource.type !== 'Opaque' || resource.metadata.ownerReferences?.length) {
    fail('This secret is not a CLI-managed credential in the selected namespace.');
  }
  return credentialType(resource.metadata.labels?.[CREDENTIAL_TYPE]);
}
// Deliberately allowlist metadata. Annotations (including last-applied), arbitrary
// labels, data and stringData can contain credentials, even on a dry-run response.
function credentialMetadata(resource: Resource): Resource {
  const { name, namespace, uid, resourceVersion, creationTimestamp } = resource.metadata;
  return {
    apiVersion: 'v1', kind: 'Secret',
    metadata: { name, namespace, uid, resourceVersion, creationTimestamp, labels: {
      [MANAGED_BY]: MANAGER, [CREDENTIAL_TYPE]: resource.metadata.labels?.[CREDENTIAL_TYPE] || '',
    } },
  };
}
async function readCredential(ctx: CommandContext, type: CredentialType): Promise<string> {
  const path = required(ctx.flags, 'from-file');
  const limit = type === 'artifact' ? MAX_ARTIFACT_CREDENTIALS : MAX_INPUT;
  const limitLabel = type === 'artifact' ? '64 KiB' : '4 MiB';
  let value: string;
  try {
    if (path === '-') value = await ctx.io.input();
    else {
      const stat = await lstat(path);
      if (!stat.isFile() || stat.size > limit) fail(`Credential input must be a regular file of at most ${limitLabel}.`);
      value = await readFile(path, 'utf8');
    }
  } catch (error) {
    if (error instanceof CLIError) throw error;
    fail('Cannot read credential input.');
  }
  if (Buffer.byteLength(value) > limit) fail(`Credential input is limited to ${limitLabel}.`);
  if (type === 'artifact') {
    let encoded: string;
    try {
      const credentials: unknown = JSON.parse(value);
      if (!object(credentials) || !Object.keys(credentials).length) fail('Artifact credentials must be a nonempty JSON object.');
      validateJSON(credentials);
      encoded = JSON.stringify(credentials);
    } catch {
      // The loader owns the source-specific credential schema. Never include
      // parser excerpts or values from this deliberately secret-bearing input.
      fail('Artifact credentials must be a nonempty JSON object with safe keys.');
    }
    if (Buffer.byteLength(encoded) > limit) fail(`Artifact credentials are limited to ${limitLabel}.`);
    return encoded;
  }
  // A token file commonly ends in a newline. Do not silently trim other bytes.
  value = value.replace(/\r?\n$/, '');
  if (!value || /[\s\x00-\x1f\x7f]/.test(value)) fail('Credential input must contain one nonempty token without whitespace.');
  return value;
}
function referencesCredential(resource: Resource, credential: string): boolean {
  if (resource.spec?.secrets?.huggingFaceToken === credential) return true;
  const scan = (value: unknown): boolean => {
    if (Array.isArray(value)) return value.some(scan);
    if (!object(value)) return false;
    for (const [key, child] of Object.entries(value)) {
      if (['secretKeyRef', 'secretRef', 'credentialsRef', 'authSecretRef'].includes(key) && object(child) && child.name === credential) return true;
      if (key === 'imagePullSecrets' && Array.isArray(child) && child.some(ref => object(ref) && ref.name === credential)) return true;
      if (scan(child)) return true;
    }
    return false;
  };
  return scan(resource.spec) || scan(resource.status?.modelBinding);
}
async function credential(words: string[], ctx: CommandContext): Promise<void> {
  const action = words[1];
  if (!['create', 'list', 'get', 'update', 'delete'].includes(action)) fail('Use credential create, list, get, update, or delete.');
  arity(words, action === 'list' ? 2 : 3);
  options(ctx, action === 'create' || action === 'update' ? ['type', 'from-file', 'dry-run'] : []);
  const ns = namespace(ctx);
  const resourceName = action === 'list' ? undefined : identifier(words[2], 'credential name');
  const dry = dryRun(ctx.flags);
  canceled(ctx);
  if (action === 'create') {
    const type = credentialType(required(ctx.flags, 'type'));
    const value = await readCredential(ctx, type);
    const desired: Resource = { apiVersion: 'v1', kind: 'Secret', metadata: {
      name: resourceName!, namespace: ns, labels: { [MANAGED_BY]: MANAGER, [CREDENTIAL_TYPE]: type },
    }, type: 'Opaque', data: { [CREDENTIAL_KEYS[type]]: Buffer.from(value).toString('base64') } };
    canceled(ctx);
    const result = dry === 'client' ? desired : await ctx.client().create(desired, dry === 'server');
    output(ctx.io, ctx.flags, credentialMetadata(result));
    return;
  }
  const client = ctx.client();
  if (action === 'list') {
    const resources = await client.list(resourceTypes.credential, ns, { labelSelector: `${MANAGED_BY}=${MANAGER},${CREDENTIAL_TYPE}` });
    const owned = resources.filter(item => {
      try { ownedCredential(item, ns); return true; } catch { return false; }
    });
    output(ctx.io, ctx.flags, owned.map(credentialMetadata));
    return;
  }
  const existing = await client.get(resourceTypes.credential, resourceName!, ns);
  const type = ownedCredential(existing, ns);
  if (action === 'get') { output(ctx.io, ctx.flags, credentialMetadata(existing)); return; }
  if (action === 'update') {
    if (text(ctx.flags, 'type') !== undefined && credentialType(text(ctx.flags, 'type')) !== type) fail('Credential type cannot change. Create a new credential instead.');
    if (!existing.metadata.resourceVersion) fail('Cannot update a credential without its resourceVersion.');
    const value = await readCredential(ctx, type);
    canceled(ctx);
    const patch = { metadata: { resourceVersion: existing.metadata.resourceVersion }, data: { [CREDENTIAL_KEYS[type]]: Buffer.from(value).toString('base64') } };
    const result = dry === 'client' ? existing : await client.patch(resourceTypes.credential, resourceName!, patch, ns, dry === 'server');
    output(ctx.io, ctx.flags, credentialMetadata(result));
    return;
  }
  if (!existing.metadata.uid) fail('Cannot delete a credential without its UID.');
  // Fail closed if either collection cannot be read: lack of RBAC or an absent
  // API is not proof that no deployment references the credential.
  for (const type of [resourceTypes.model, resourceTypes.agent]) {
    const resources = await client.list(type, ns);
    if (resources.some(item => item.metadata.namespace === ns && referencesCredential(item, resourceName!))) {
      throw new CLIError('Credential is referenced by a deployment in this namespace. Remove the reference before deleting it.', 5, 'IN_USE');
    }
  }
  canceled(ctx);
  await client.delete(resourceTypes.credential, resourceName!, ns, existing.metadata.uid);
  output(ctx.io, ctx.flags, credentialMetadata(existing));
}

function discoveryMetadata(resource: Resource): Resource {
  return { apiVersion: resource.apiVersion, kind: resource.kind, metadata: { name: resource.metadata.name },
    spec: { capabilities: resource.spec?.capabilities, selectionRules: resource.spec?.selectionRules },
    status: { ready: resource.status?.ready, version: resource.status?.version } };
}
async function discovery(words: string[], ctx: CommandContext): Promise<void> {
  const action = words[1];
  if (action !== 'list' && action !== 'get') fail('Use list or get for installed providers and frameworks.');
  arity(words, action === 'list' ? 2 : 3);
  options(ctx, []);
  const type = resourceTypes[words[0] as 'provider' | 'framework'];
  canceled(ctx);
  const client = ctx.client();
  if (action === 'list') output(ctx.io, ctx.flags, (await client.list(type)).map(discoveryMetadata));
  else output(ctx.io, ctx.flags, discoveryMetadata(await client.get(type, identifier(words[2]))));
}

function modelID(value: unknown): string {
  if (typeof value !== 'string') fail('Provide a Hugging Face identifier or hf:// identifier.');
  const id = value.startsWith('hf://') ? value.slice(5) : value;
  const segments = id.split('/');
  if (segments.length > 2 || segments.some(segment => !/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,95}$/.test(segment) || segment.endsWith('.') || segment.endsWith('-') || segment.includes('..') || segment.includes('--'))) {
    fail('Provide a Hugging Face identifier or hf:// identifier, not a URL or file path.');
  }
  return id;
}
async function fetchModels(url: URL, ctx: CommandContext): Promise<unknown> {
  const signal = AbortSignal.any([ctx.signal, AbortSignal.timeout(Math.min(duration(text(ctx.flags, 'timeout')), 15000))]);
  try {
    const response = await fetch(url, { headers: { Accept: 'application/json' }, redirect: 'error', signal });
    if (!response.ok) throw new CLIError(response.status === 404 ? 'Model not found in Hugging Face.' : 'Hugging Face catalog request failed.', 1, response.status === 404 ? 'NOT_FOUND' : 'CATALOG');
    if (!response.body) throw new Error('Empty response');
    const reader = response.body.getReader();
    const chunks: Uint8Array[] = [];
    let bytes = 0;
    try {
      while (true) {
        const result = await reader.read();
        if (result.done) break;
        bytes += result.value.byteLength;
        if (bytes > MAX_INPUT) throw new Error('Response limit');
        chunks.push(result.value);
      }
    } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
    return JSON.parse(Buffer.concat(chunks).toString('utf8'));
  } catch (error) {
    if (ctx.signal.aborted) throw new CLIError('Catalog request interrupted.', 130, 'INTERRUPTED');
    if (signal.aborted) throw new CLIError('Hugging Face catalog request timed out.', 4, 'TIMEOUT');
    if (error instanceof CLIError) throw error;
    throw new CLIError('Cannot read the Hugging Face catalog response.', 1, 'CATALOG');
  }
}
function remoteModel(value: unknown): ObjectValue {
  if (!object(value)) throw new CLIError('Invalid Hugging Face catalog entry.', 1, 'CATALOG');
  let id: string;
  try { id = modelID(value.id ?? value.modelId); } catch { throw new CLIError('Invalid Hugging Face model identifier in response.', 1, 'CATALOG'); }
  const result: ObjectValue = { id, source: 'huggingface' };
  // Do not forward cardData, instructions, endpoints or arbitrary config into
  // either command output or a later deployment request.
  if (typeof value.pipeline_tag === 'string') result.task = value.pipeline_tag.slice(0, 100);
  for (const key of ['downloads', 'likes']) if (typeof value[key] === 'number' && Number.isFinite(value[key])) result[key] = value[key];
  if (typeof value.gated === 'boolean' || value.gated === 'auto' || value.gated === 'manual') result.gated = value.gated;
  return result;
}
async function modelCatalog(words: string[], ctx: CommandContext): Promise<void> {
  if (words[2] !== 'search' && words[2] !== 'get') fail('Use catalog model search QUERY or get ID.');
  arity(words, 4);
  const id = modelID(words[3]);
  canceled(ctx);
  if (words[2] === 'get') {
    const bundled = bundledModels.models.find(model => model.id === id);
    if (bundled) { output(ctx.io, ctx.flags, { ...bundled, source: 'bundled' }); return; }
    const value = remoteModel(await fetchModels(new URL(`https://huggingface.co/api/models/${id.split('/').map(encodeURIComponent).join('/')}`), ctx));
    if (value.id !== id) throw new CLIError('Hugging Face returned a different model identifier.', 1, 'CATALOG');
    output(ctx.io, ctx.flags, value);
  } else {
    const url = new URL('https://huggingface.co/api/models');
    url.searchParams.set('search', id); url.searchParams.set('limit', '20');
    const response = await fetchModels(url, ctx);
    if (!Array.isArray(response) || response.length > 20) throw new CLIError('Invalid Hugging Face search response.', 1, 'CATALOG');
    const results = new Map<string, ObjectValue>();
    for (const model of response) {
      const result = remoteModel(model); results.set(result.id as string, result);
    }
    for (const model of bundledModels.models) {
      if (`${model.id} ${model.name}`.toLowerCase().includes(id.toLowerCase())) results.set(model.id, { ...model, source: 'bundled' });
    }
    output(ctx.io, ctx.flags, [...results.values()]);
  }
}

interface Preset extends ObjectValue { id: string; name: string; framework: string; title: string; config: ObjectValue }
// Bound walks before JSON serialization also reject YAML cycles/alias expansion
// and prototype keys. Keep valid, unknown desired fields rather than UI coercion.
function validateJSON(value: unknown, rejectSecrets = false): void {
  let nodes = 0;
  const active = new Set<object>();
  const secretKey = /(?:^|[_-])(?:api[_-]?key|access[_-]?key|account[_-]?key|token|password|passwd|private[_-]?key|client[_-]?secret|authorization|secret|credentials)(?:$|[_-])/i;
  const sensitive = (key: string) => secretKey.test(key.replace(/([a-z])([A-Z])/g, '$1_$2'));
  const visit = (item: unknown, depth: number, path: string[] = []): void => {
    if (++nodes > 50000 || depth > 64) fail('Document is too complex.');
    if (item === null || typeof item === 'boolean') return;
    if (typeof item === 'string') {
      if (rejectSecrets && /(?:\bhf_[A-Za-z0-9]{16,}|\bsk-[A-Za-z0-9_-]{16,}|-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----|\bBearer\s+\S+|[a-z][a-z0-9+.-]*:\/\/[^\s/?#]*@|[?&](?:token|api[_-]?key|sig|signature|x-amz-credential|x-amz-signature)=)/i.test(item)) fail('Inline credentials are not allowed. Use a credential reference.');
      return;
    }
    if (typeof item === 'number' && Number.isFinite(item)) return;
    if (!Array.isArray(item) && !object(item)) fail('Documents must contain only JSON-compatible values.');
    if (active.has(item)) fail('Cyclic YAML aliases are not supported.');
    active.add(item);
    if (Array.isArray(item)) for (const child of item) visit(child, depth + 1, path);
    else {
      const record = item as ObjectValue;
      for (const [key, child] of Object.entries(record)) {
        if (['__proto__', 'constructor', 'prototype', '<<'].includes(key)) fail('Unsafe document key.');
        // References are safe, but scalar secrets and env-style name/value pairs
        // are not. This is a guardrail, not a claim to detect every possible secret.
        const reference = /(?:Ref|Refs)$/.test(key) || (key === 'huggingFaceToken' && path.join('.') === 'spec.secrets');
        if (rejectSecrets && sensitive(key) && child !== null && child !== '' && typeof child !== 'boolean' && !reference) fail('Inline credential fields are not allowed. Use a credential reference.');
        visit(child, depth + 1, [...path, key]);
      }
      if (rejectSecrets && typeof record.name === 'string' && sensitive(record.name) && record.value !== undefined && record.value !== '') fail('Inline credential environment values are not allowed. Use secretKeyRef.');
    }
    active.delete(item);
  };
  visit(value, 0);
}
async function presets(client: ClusterClient): Promise<Preset[]> {
  const result: Preset[] = [];
  for (const provider of await client.list(resourceTypes.framework)) {
    const framework = identifier(provider.metadata.name, 'framework name', 63);
    // The current Go API uses agent-catalog. Accept catalog only as a fallback
    // for older registrations, never combine two competing catalogs.
    const raw = provider.metadata.annotations?.['airunway.ai/agent-catalog'] ?? provider.metadata.annotations?.['airunway.ai/catalog'];
    if (!raw) continue;
    let entries: unknown;
    try {
      if (Buffer.byteLength(raw) > 256 * 1024) throw new Error('Catalog limit');
      entries = JSON.parse(raw);
      validateJSON(entries, true);
      if (!Array.isArray(entries) || entries.length > 200) throw new Error('Invalid catalog');
      const seen = new Set<string>();
      for (const entry of entries) {
        if (!object(entry)) throw new Error('Invalid entry');
        const name = identifier(entry.name, 'preset name', 63);
        if (name.includes('.') || seen.has(name) || typeof entry.title !== 'string' || !entry.title.trim()) throw new Error('Invalid preset');
        seen.add(name);
        if (entry.template !== undefined && !object(entry.template)) throw new Error('Invalid template');
        const template = (entry.template || {}) as ObjectValue;
        if (template.config !== undefined && !object(template.config)) throw new Error('Invalid config');
        if (template.framework !== undefined && (!object(template.framework) || template.framework.name !== framework)) throw new Error('Conflicting framework');
        if (entry.image !== undefined && typeof entry.image !== 'string') throw new Error('Invalid image');
        const config: ObjectValue = { ...(template.config as ObjectValue || {}) };
        if (entry.image) {
          if (config.image !== undefined && config.image !== entry.image) throw new Error('Conflicting image');
          config.image = entry.image;
        }
        const preset: Preset = { ...template, id: `${framework}/${name}`, name, framework, title: entry.title, config };
        if (typeof entry.description === 'string') preset.description = entry.description;
        if (Array.isArray(entry.tags) && entry.tags.every(tag => typeof tag === 'string')) preset.tags = entry.tags;
        result.push(preset);
      }
    } catch {
      // No JSON parser snippets: a malformed annotation can contain credentials.
      throw new CLIError(`Invalid agent catalog for framework "${framework}".`, 1, 'CATALOG');
    }
  }
  return result;
}
function presetMetadata(preset: Preset): ObjectValue {
  const { id, name, framework, title, description, tags } = preset;
  return { id, name, framework, title, description, tags };
}
export async function resolvePreset(client: ClusterClient, id: string): Promise<{ framework: string; config: Record<string, unknown>; [key: string]: unknown }> {
  const parts = id.split('/');
  if (parts.length > 2) fail('Use PRESET or FRAMEWORK/PRESET.');
  for (const part of parts) identifier(part, 'preset identifier', 63);
  const matches = (await presets(client)).filter(preset => parts.length === 2 ? preset.id === id : preset.name === id);
  if (matches.length > 1) fail('Preset name is ambiguous. Use FRAMEWORK/PRESET.');
  if (!matches.length) throw new CLIError('Preset not found in the installed frameworks.', 1, 'NOT_FOUND');
  return matches[0];
}

async function manifestFiles(path: string): Promise<string[]> {
  try {
    const stat = await lstat(path);
    if (stat.isFile()) {
      if (!['.yaml', '.yml', '.json'].includes(extname(path).toLowerCase())) fail('Apply accepts only .yaml, .yml, or .json files.');
      return [path];
    }
    if (!stat.isDirectory()) fail('Apply requires a regular file or directory, not a symlink.');
    const files = (await readdir(path, { withFileTypes: true }))
      .filter(entry => ['.yaml', '.yml', '.json'].includes(extname(entry.name).toLowerCase()))
      .sort((a, b) => a.name.localeCompare(b.name));
    if (files.some(entry => !entry.isFile())) fail('Manifest entries must be regular files.');
    if (!files.length || files.length > 100) fail('Apply requires between 1 and 100 manifest files.');
    return files.map(entry => join(path, entry.name));
  } catch (error) {
    if (error instanceof CLIError) throw error;
    fail('Cannot read the apply path.');
  }
}
function manifest(value: unknown, ns: string): Resource {
  validateJSON(value, true);
  if (!object(value) || !['ModelDeployment', 'AgentDeployment'].includes(value.kind as string) || value.apiVersion !== 'airunway.ai/v1alpha1') fail('Apply accepts only airunway.ai/v1alpha1 ModelDeployment and AgentDeployment documents.');
  if (Object.keys(value).some(key => !['apiVersion', 'kind', 'metadata', 'spec'].includes(key))) fail('Apply accepts desired state only, without status or other top-level fields.');
  if (!object(value.metadata) || !object(value.spec)) fail('Each document requires metadata and spec objects.');
  identifier(value.metadata.name, 'resource name');
  if (Object.keys(value.metadata).some(key => !['name', 'namespace', 'labels', 'annotations'].includes(key))) fail('Apply metadata accepts only name, namespace, labels, and annotations. Remove server-owned metadata.');
  if (value.metadata.namespace !== undefined && value.metadata.namespace !== ns) fail('Document namespace differs from the selected namespace.');
  for (const key of ['labels', 'annotations']) {
    const map = value.metadata[key];
    if (map !== undefined && (!object(map) || Object.values(map).some(item => typeof item !== 'string'))) fail('Labels and annotations must be string maps.');
  }
  if (object(value.metadata.annotations) && 'kubectl.kubernetes.io/last-applied-configuration' in value.metadata.annotations) fail('Remove last-applied configuration before applying desired state.');
  return { ...value, metadata: { ...value.metadata, namespace: ns } } as Resource;
}
async function apply(words: string[], ctx: CommandContext): Promise<void> {
  arity(words, 1); options(ctx, ['file', 'dry-run']);
  const ns = namespace(ctx), dry = dryRun(ctx.flags);
  const files = await manifestFiles(required(ctx.flags, 'file'));
  const resources: Resource[] = [];
  const names = new Set<string>();
  let totalBytes = 0;
  // Read and validate the *entire* batch before acquiring a client or writing.
  for (const file of files) {
    canceled(ctx);
    let docs: unknown[];
    try {
      const stat = await lstat(file);
      if (!stat.isFile() || stat.size + totalBytes > MAX_INPUT) fail('Apply input is limited to 4 MiB of regular files.');
      const data = await readFile(file, 'utf8');
      totalBytes += Buffer.byteLength(data);
      if (totalBytes > MAX_INPUT) fail('Apply input is limited to 4 MiB.');
      docs = extname(file).toLowerCase() === '.json' ? [JSON.parse(data)] : loadAll(data, { schema: CORE_SCHEMA, maxDepth: 64 });
    } catch (error) {
      if (error instanceof CLIError) throw error;
      fail('Cannot parse apply input as YAML or JSON.');
    }
    for (const doc of docs) {
      if (doc === null) continue; // Empty YAML separators are harmless.
      const desired = manifest(doc, ns);
      const key = `${desired.kind}/${desired.metadata.name}`;
      if (names.has(key)) fail('Apply input contains duplicate resources.');
      names.add(key); resources.push(desired);
      if (resources.length > 200) fail('Apply is limited to 200 documents.');
    }
  }
  if (!resources.length) fail('No deployment documents found.');
  if (dry === 'client') { output(ctx.io, ctx.flags, resources); return; }
  canceled(ctx);
  const client = ctx.client();
  // Preflight every agent before the first write. Existing one-shot workloads
  // are never patched, deleted or recreated, even if this is a server dry run.
  const agentVersions = new Map<string, string>();
  for (const desired of resources) {
    if (desired.kind !== 'AgentDeployment') continue;
    let existing: Resource;
    try { existing = await client.get(resourceTypes.agent, desired.metadata.name, ns); }
    catch (error) { if (error instanceof CLIError && error.code === 'HTTP_404') continue; throw error; }
    if (existing.spec?.lifecycle === 'job' || desired.spec?.lifecycle === 'job') fail('Apply cannot modify an existing one-shot agent. Create an agent with a new name.');
    if (!existing.metadata.resourceVersion) fail('Cannot apply an existing agent without its resourceVersion.');
    agentVersions.set(desired.metadata.name, existing.metadata.resourceVersion);
  }
  const results: Resource[] = [];
  try {
    for (const desired of resources) {
      canceled(ctx);
      const type = desired.kind === 'ModelDeployment' ? resourceTypes.model : resourceTypes.agent;
      const version = desired.kind === 'AgentDeployment' ? agentVersions.get(desired.metadata.name) : undefined;
      const body = version ? { ...desired, metadata: { ...desired.metadata, resourceVersion: version } } : desired;
      const result = await client.request<Resource>('PATCH', resourcePath(type, ns, desired.metadata.name), body, {
        contentType: 'application/apply-patch+yaml', signal: ctx.signal,
        query: { fieldManager: MANAGER, force: false, fieldValidation: 'Strict', dryRun: dry === 'server' ? 'All' : undefined },
      });
      // Server defaults/status may contain provider-generated secrets. A concise
      // receipt is safer than echoing the full response; client dry-run shows intent.
      results.push({ apiVersion: result.apiVersion, kind: result.kind, metadata: { name: result.metadata.name, namespace: result.metadata.namespace } });
    }
  } catch (error) {
    if (results.length) {
      output(ctx.io, ctx.flags, results);
      ctx.io.err(dry === 'server'
        ? `Validation stopped after ${results.length} successful documents. No resources were persisted.\n`
        : `Apply stopped after ${results.length} successful documents. The resources listed on stdout were not rolled back.\n`);
    }
    throw error;
  }
  output(ctx.io, ctx.flags, results);
}

export async function runManagement(words: string[], ctx: CommandContext): Promise<boolean> {
  switch (words[0]) {
    case 'credential': await credential(words, ctx); return true;
    case 'provider': case 'framework': await discovery(words, ctx); return true;
    case 'apply': await apply(words, ctx); return true;
    case 'catalog':
      options(ctx, []);
      if (words[1] === 'model') await modelCatalog(words, ctx);
      else if (words[1] === 'agent') {
        if (words[2] !== 'list' && words[2] !== 'get') fail('Use catalog agent list or get FRAMEWORK/PRESET.');
        arity(words, words[2] === 'list' ? 3 : 4);
        canceled(ctx);
        if (words[2] === 'list') output(ctx.io, ctx.flags, (await presets(ctx.client())).map(presetMetadata));
        else output(ctx.io, ctx.flags, presetMetadata(await resolvePreset(ctx.client(), words[3]) as Preset));
      } else fail('Use catalog model or catalog agent.');
      return true;
    default: return false;
  }
}
