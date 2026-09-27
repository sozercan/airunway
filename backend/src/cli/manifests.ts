import { isDeepStrictEqual } from 'node:util';
import { inputValue, integer, name as resourceName, required, text } from './args';
import { CLIError, resourceTypes, type Flags, type IO, type Resource, type ResourceNoun } from './types';

const commonFlags = ['kubeconfig', 'context', 'namespace', 'output', 'timeout', 'help', 'version', 'dry-run', 'wait'];
const modelMutableFlags = ['gpus', 'cpu', 'memory', 'replicas', 'served-name', 'context-length', 'credential', 'image', 'engine-arg', 'trust-remote-code', 'gateway'];
const sourceFlags = ['id', 'model-path', 'revision', 'file', 'storage-size', 'storage-class', 'artifact-image', 'service-account'];
const bindingFlags = ['model-ref', 'model-url', 'model-api', 'model-id', 'model-credential', 'model-gateway', 'gateway-listener'];
const promptFlags = ['prompt', 'prompt-file'];
const immutableFlags = ['id', 'provider', 'engine', 'model-source', 'framework', 'mode', 'model-path', 'revision', 'file', 'storage-size', 'storage-class', 'artifact-image', 'service-account'];
const artifactRoot = '/model-cache/artifacts';
const int32Max = 2147483647;
type ObjectValue = NonNullable<Resource['spec']>;

function usage(message: string): never { throw new CLIError(message, 2, 'USAGE'); }
function supplied(flags: Flags, key: string): boolean { return flags[key] !== undefined; }
function anySupplied(flags: Flags, keys: string[]): boolean { return keys.some(key => supplied(flags, key)); }

function checkFlags(flags: Flags, allowed: string[], operation: string): void {
  for (const [key, value] of Object.entries(flags)) {
    if (value === undefined || commonFlags.includes(key)) continue;
    if (!allowed.includes(key)) usage(`--${key} is not supported for ${operation}.`);
    if (key === 'engine-arg') {
      if (!Array.isArray(value) || !value.every(item => typeof item === 'string' && item.length > 0 && !item.includes('\0'))) usage('--engine-arg must contain nonempty raw arguments.');
    } else if (key === 'gateway' || key === 'trust-remote-code') {
      if (typeof value !== 'boolean') usage(`--${key} must be true or false.`);
    } else if (typeof value !== 'string') usage(`--${key} requires a value.`);
  }
}

function nonempty(flags: Flags, key: string): string {
  const value = required(flags, key);
  if (!value.trim() || value !== value.trim() || /[\x00-\x1f\x7f]/.test(value)) usage(`--${key} must be a nonempty value without surrounding whitespace or control characters.`);
  return value;
}

function dnsName(value: string, label: string, max = 253): string {
  if (value.length > max || !/^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]*[a-z0-9])?)*$/.test(value)) usage(`${label} must be a valid lowercase name.`);
  return value;
}

function dnsLabel(value: string, label: string): string {
  if (value.includes('.')) usage(`${label} must be a valid lowercase name without dots.`);
  return dnsName(value, label, 63);
}

function quantity(flags: Flags, key: string): string | undefined {
  if (!supplied(flags, key)) return undefined;
  const value = nonempty(flags, key);
  const match = /^(\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+|[numkMGTPE]|[KMGTPE]i)?$/.exec(value);
  if (!match || !Number.isFinite(Number(match[1])) || Number(match[1]) <= 0) usage(`--${key} must be a positive resource quantity, such as 500m, 4, or 8Gi.`);
  return value;
}

function relativePath(value: string, label: string): string {
  if (!value || value.length > 1024 || /[\\%\x00-\x1f\x7f]/.test(value) || value.split('/').some(part => !part || part === '.' || part === '..')) usage(`${label} must be a clean relative path without traversal or encoded characters.`);
  return value;
}

function secretRef(value: string, label: string, keyRequired = false): { name: string; key?: string } {
  const parts = value.split('/');
  if (parts.length > 2 || (keyRequired && parts.length !== 2)) usage(`${label} must be ${keyRequired ? 'NAME/KEY' : 'NAME or NAME/KEY'}.`);
  const name = dnsName(parts[0], label);
  if (parts.length === 1) return { name };
  const key = parts[1];
  if (!key || key.length > 253 || !/^[A-Za-z0-9._-]+$/.test(key)) usage(`${label} has an invalid credential key.`);
  return { name, key };
}

function imageReference(value: string, label: string, pinned = false): string {
  if (!value || value.length > 512 || /[\\%?#\s]/.test(value)) usage(`${label} must be a container image reference.`);
  const [withTag, digest, ...extra] = value.split('@');
  if (extra.length || (digest !== undefined && !/^sha256:[a-f0-9]{64}$/.test(digest))) usage(`${label} has an invalid image digest.`);
  let image = withTag;
  const colon = image.lastIndexOf(':');
  const hasTag = colon > image.lastIndexOf('/');
  if (hasTag) {
    if (!/^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$/.test(image.slice(colon + 1))) usage(`${label} has an invalid image tag.`);
    image = image.slice(0, colon);
  }
  if (pinned && !hasTag && digest === undefined) usage(`${label} requires an explicit tag or sha256 digest.`);
  const parts = image.split('/');
  if (parts.length > 1 && /[.:]|^localhost$/.test(parts[0])) {
    const registry = parts.shift()!;
    if (!/^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::\d+)?$/.test(registry)) usage(`${label} has an invalid registry.`);
    dnsName(registry.split(':')[0], `${label} registry`);
  }
  if (!parts.every(part => /^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$/.test(part))) usage(`${label} has an invalid image repository.`);
  return value;
}

function cleanURL(value: string, label: string, protocols: string[]): { host: string; pathname: string } {
  // Check before URL parsing: URL normalisation would hide dot segments, and
  // rejected credentials must never be echoed into diagnostics.
  if (value.length > 4096 || /[\\%?#\s\x00-\x1f\x7f]/.test(value)) usage(`${label} must not contain credentials, query parameters, fragments, or encoded paths.`);
  const match = /^([a-z][a-z0-9+.-]*):\/\/([^/]+)(\/.*)?$/.exec(value);
  if (!match || !protocols.includes(match[1]) || match[2].includes('@')) usage(`${label} must use ${protocols.join(' or ')} without inline credentials.`);
  if (match[3] && match[3] !== '/') relativePath(match[3].slice(1).replace(/\/$/, ''), label);
  try {
    const url = new URL(value);
    if (!url.hostname || url.username || url.password) usage(`${label} must not contain inline credentials.`);
    return { host: url.host, pathname: match[3] ?? '' };
  } catch (error) {
    if (error instanceof CLIError) throw error;
    usage(`${label} is not a valid URL.`);
  }
}

function manifest(noun: ResourceNoun, name: string, namespace: string, spec: ObjectValue): Resource {
  const type = resourceTypes[noun];
  return {
    apiVersion: `${type.group}/${type.version}`, kind: type.kind,
    metadata: {
      name: resourceName(name), namespace: resourceName(namespace, 'namespace'),
      annotations: { 'airunway.ai/managed-by': 'cli' },
    },
    spec,
  };
}

function modelOptions(flags: Flags): ObjectValue {
  const spec: ObjectValue = {};
  const resources: ObjectValue = {};
  if (supplied(flags, 'gpus')) resources.gpu = { count: integer(flags, 'gpus', undefined, 0, int32Max) };
  for (const key of ['cpu', 'memory']) if (supplied(flags, key)) resources[key] = quantity(flags, key);
  if (Object.keys(resources).length) spec.resources = resources;
  if (supplied(flags, 'replicas')) spec.scaling = { replicas: integer(flags, 'replicas', undefined, 0, int32Max) };
  if (supplied(flags, 'served-name')) spec.model = { servedName: nonempty(flags, 'served-name') };
  const engine: ObjectValue = {};
  if (supplied(flags, 'context-length')) engine.contextLength = integer(flags, 'context-length', undefined, 1, int32Max);
  if (supplied(flags, 'image')) engine.image = imageReference(nonempty(flags, 'image'), '--image');
  if (supplied(flags, 'engine-arg')) engine.extraArgs = [...flags['engine-arg'] as string[]];
  if (supplied(flags, 'trust-remote-code')) engine.trustRemoteCode = flags['trust-remote-code'];
  if (Object.keys(engine).length) spec.engine = engine;
  if (supplied(flags, 'gateway')) spec.gateway = { enabled: flags.gateway };
  return spec;
}

function hfCredential(flags: Flags): ObjectValue | undefined {
  if (!supplied(flags, 'credential')) return undefined;
  const ref = secretRef(nonempty(flags, 'credential'), '--credential');
  if (ref.key !== undefined && ref.key !== 'HF_TOKEN') usage('Unstaged Hugging Face models require the HF_TOKEN credential key.');
  return { huggingFaceToken: ref.name };
}

function requireVllm(spec: ObjectValue, description: string): void {
  if ((spec.provider?.name && spec.provider.name !== 'vllm') || (spec.engine?.type && spec.engine.type !== 'vllm')) usage(`${description} currently requires --provider vllm and --engine vllm.`);
  spec.provider = { name: 'vllm' };
  spec.engine = { ...spec.engine, type: 'vllm' };
}

function rejectSourceOptions(flags: Flags, keys: string[], description: string): void {
  const key = keys.find(key => supplied(flags, key));
  if (key) usage(`--${key} is not supported for ${description}.`);
}

export async function buildModel(name: string, flags: Flags, namespace: string, _io: IO): Promise<Resource> {
  checkFlags(flags, [...modelMutableFlags, ...sourceFlags, 'provider', 'engine'], 'model creation');
  const spec = modelOptions(flags);
  spec.resources = { ...spec.resources, gpu: { count: integer(flags, 'gpus', 1, 0, int32Max) } };
  spec.scaling = { replicas: integer(flags, 'replicas', 1, 0, int32Max) };
  if (supplied(flags, 'provider')) spec.provider = { name: dnsName(nonempty(flags, 'provider'), '--provider') };
  if (supplied(flags, 'engine')) {
    const engine = nonempty(flags, 'engine');
    if (!['vllm', 'sglang', 'trtllm', 'llamacpp'].includes(engine)) usage('--engine must be vllm, sglang, trtllm, or llamacpp.');
    spec.engine = { ...spec.engine, type: engine };
  }

  if (supplied(flags, 'model-path')) {
    rejectSourceOptions(flags, ['id', 'revision', 'file', 'credential', 'storage-size', 'storage-class', 'artifact-image', 'service-account'], 'bundled models');
    nonempty(flags, 'image');
    const path = nonempty(flags, 'model-path');
    if (!path.startsWith('/')) usage('--model-path must be an absolute path inside the image.');
    relativePath(path.slice(1), '--model-path');
    rejectSourceOptions(flags, ['served-name'], 'bundled models');
    requireVllm(spec, 'Bundled models');
    spec.model = { ...spec.model, source: 'custom', id: path };
    return manifest('model', name, namespace, spec);
  }

  const source = nonempty(flags, 'id');
  const scheme = /^([a-z][a-z0-9+.-]*):\/\//.exec(source)?.[1] ?? 'hf';
  let uri = source;
  if (scheme === 'hf') {
    const repository = source.startsWith('hf://') ? source.slice(5) : source;
    if (repository.length > 4091 || repository.split('/').length > 2 || !repository.split('/').every(part => /^[A-Za-z0-9_][A-Za-z0-9_.-]*$/.test(part) && !part.includes('..'))) usage('--id must identify a Hugging Face repository; select files with --file.');
    uri = `hf://${repository}`;
    if (!anySupplied(flags, ['revision', 'file'])) {
      rejectSourceOptions(flags, ['storage-size', 'storage-class', 'artifact-image', 'service-account'], 'unstaged Hugging Face models');
      spec.model = { ...spec.model, source: 'huggingface', id: repository };
      const secrets = hfCredential(flags);
      if (secrets) spec.secrets = secrets;
      return manifest('model', name, namespace, spec);
    }
  }

  if (scheme === 'pvc') {
    const url = cleanURL(uri, '--id', ['pvc']);
    rejectSourceOptions(flags, ['revision', 'file', 'credential', 'storage-size', 'storage-class', 'artifact-image', 'service-account'], 'existing storage references');
    const claimName = dnsName(url.host, 'PVC claim');
    const path = url.pathname.replace(/^\//, '').replace(/\/$/, '');
    rejectSourceOptions(flags, ['served-name'], 'existing storage references');
    requireVllm(spec, 'Existing storage references');
    spec.model = {
      ...spec.model, source: 'custom', id: `/model-cache${path ? `/${path}` : ''}`,
      storage: { volumes: [{ name: 'model-cache', purpose: 'modelCache', claimName, readOnly: true, mountPath: '/model-cache' }] },
    };
    return manifest('model', name, namespace, spec);
  }

  const url = scheme === 'oci' ? undefined : cleanURL(uri, '--id', ['hf', 's3', 'gs', 'https']);
  if (scheme === 'oci') {
    const reference = uri.slice(6);
    if (!reference.includes('/')) usage('OCI sources require a registry and repository.');
    imageReference(reference, '--id OCI source', true);
  }
  if (scheme === 's3' || scheme === 'gs') dnsName(url!.host, 'Artifact bucket');
  if (scheme === 'https' && (!url!.pathname || url!.pathname === '/' || url!.pathname.endsWith('/'))) usage('HTTPS sources must identify a single file.');
  requireVllm(spec, 'Staged model artifacts');
  const artifact: ObjectValue = { uri };
  if (supplied(flags, 'revision')) {
    if (scheme !== 'hf') usage('--revision is supported only for Hugging Face sources.');
    const revision = nonempty(flags, 'revision');
    if (revision.length > 256) usage('--revision must be at most 256 characters.');
    artifact.revision = relativePath(revision, '--revision');
  }
  if (supplied(flags, 'file')) artifact.file = relativePath(nonempty(flags, 'file'), '--file');
  if (supplied(flags, 'credential')) {
    const ref = secretRef(nonempty(flags, 'credential'), '--credential');
    if (scheme === 'hf' && (ref.key === undefined || ref.key === 'HF_TOKEN')) spec.secrets = hfCredential(flags);
    else artifact.credentialsRef = ref;
  }
  if (supplied(flags, 'artifact-image')) artifact.image = imageReference(nonempty(flags, 'artifact-image'), '--artifact-image', true);
  if (supplied(flags, 'service-account')) artifact.serviceAccountName = dnsName(nonempty(flags, 'service-account'), '--service-account');
  const volume: ObjectValue = { name: 'model-cache', purpose: 'modelCache', mountPath: '/model-cache', readOnly: false, size: quantity(flags, 'storage-size') ?? '100Gi' };
  if (supplied(flags, 'storage-class')) {
    const storageClass = text(flags, 'storage-class')!;
    volume.storageClassName = storageClass === '' ? '' : dnsName(storageClass, '--storage-class');
  }
  const file = artifact.file ?? (scheme === 'https' ? url!.pathname.slice(url!.pathname.lastIndexOf('/') + 1) : undefined);
  spec.model = { ...spec.model, source: 'custom', id: `${artifactRoot}${file ? `/${file}` : ''}`, artifact, storage: { volumes: [volume] } };
  return manifest('model', name, namespace, spec);
}

function objectRef(value: string, label: string): ObjectValue {
  const parts = value.split('/');
  if (parts.length > 2) usage(`${label} must be NAME or NAMESPACE/NAME.`);
  const name = dnsName(parts[parts.length - 1], label);
  return parts.length === 2 ? { name, namespace: dnsLabel(parts[0], label) } : { name };
}

function binding(flags: Flags, existing?: ObjectValue): ObjectValue {
  const selectors = ['model-ref', 'model-url', 'model-gateway'].filter(key => supplied(flags, key));
  if (selectors.length > 1) usage('Choose only one of --model-ref, --model-url, or --model-gateway.');
  const kinds: Record<string, string> = { 'model-ref': 'deploymentRef', 'model-url': 'externalAPI', 'model-gateway': 'gatewayEndpoint' };
  const oldKinds = Object.values(kinds).filter(kind => existing?.[kind] != null);
  const kind = selectors.length ? kinds[selectors[0]] : oldKinds.length === 1 ? oldKinds[0] : undefined;
  if (!kind) usage('Provide --model-ref, --model-url, or --model-gateway.');
  const old = existing?.[kind];
  const patch: ObjectValue = {};
  if (kind === 'deploymentRef') {
    rejectSourceOptions(flags, ['model-url', 'model-api', 'model-id', 'model-credential', 'model-gateway', 'gateway-listener'], 'a model deployment binding');
    const ref = objectRef(nonempty(flags, 'model-ref'), '--model-ref');
    if (old?.namespace && ref.namespace === undefined) ref.namespace = null;
    patch.deploymentRef = ref;
  } else if (kind === 'externalAPI') {
    rejectSourceOptions(flags, ['model-ref', 'model-gateway', 'gateway-listener'], 'an external API binding');
    const api: ObjectValue = {};
    if (supplied(flags, 'model-url') || !old) {
      const value = nonempty(flags, 'model-url');
      cleanURL(value, '--model-url', ['http', 'https']);
      api.baseURL = value;
    }
    if (supplied(flags, 'model-api') || !old) {
      const type = nonempty(flags, 'model-api');
      if (!['openai', 'anthropic', 'azure-openai', 'azureOpenAI', 'custom'].includes(type)) usage('--model-api must be openai, anthropic, azure-openai, or custom.');
      api.type = type === 'azure-openai' ? 'azureOpenAI' : type;
    }
    if (supplied(flags, 'model-id') || !old) api.modelName = nonempty(flags, 'model-id');
    if (supplied(flags, 'model-credential')) api.credentialsRef = secretRef(nonempty(flags, 'model-credential'), '--model-credential', true);
    patch.externalAPI = api;
  } else {
    rejectSourceOptions(flags, ['model-ref', 'model-url', 'model-api', 'model-credential'], 'a gateway binding');
    const gateway: ObjectValue = {};
    if (supplied(flags, 'model-gateway') || !old) {
      const ref = objectRef(nonempty(flags, 'model-gateway'), '--model-gateway');
      if (old?.gatewayRef?.namespace && ref.namespace === undefined) ref.namespace = null;
      if (old?.gatewayRef?.listenerName && !supplied(flags, 'gateway-listener')) ref.listenerName = null;
      gateway.gatewayRef = ref;
    }
    if (supplied(flags, 'gateway-listener')) gateway.gatewayRef = { ...gateway.gatewayRef, listenerName: dnsName(nonempty(flags, 'gateway-listener'), '--gateway-listener') };
    if (supplied(flags, 'model-id') || !old) gateway.modelName = nonempty(flags, 'model-id');
    patch.gatewayEndpoint = gateway;
  }
  // JSON merge patch retains omitted union members. Explicit nulls remove the
  // old binding when changing modes instead of leaving an invalid union.
  for (const oldKind of oldKinds) if (oldKind !== kind) patch[oldKind] = null;
  return patch;
}

function jsonObject(raw: string, label: string): ObjectValue {
  let parsed: unknown;
  try { parsed = JSON.parse(raw); } catch { usage(`${label} must contain valid JSON.`); }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) usage(`${label} must contain a JSON object.`);
  return parsed as ObjectValue;
}

function mergeConfig(target: ObjectValue, source: ObjectValue, label: string): ObjectValue {
  const result = { ...target };
  for (const [key, value] of Object.entries(source)) {
    const present = Object.prototype.hasOwnProperty.call(result, key);
    const old = result[key];
    const objects = old && value && typeof old === 'object' && typeof value === 'object' && !Array.isArray(old) && !Array.isArray(value);
    if (present && objects) {
      Object.defineProperty(result, key, { value: mergeConfig(old, value, label), enumerable: true, configurable: true, writable: true });
    } else {
      if (present && !isDeepStrictEqual(old, value)) usage(`${label} conflicts with configuration field ${key}. Supply that field in only one place.`);
      Object.defineProperty(result, key, { value, enumerable: true, configurable: true, writable: true });
    }
  }
  return result;
}

function fileInput(flags: Flags, io: IO): () => Promise<string> {
  if (['prompt-file', 'task-file', 'config-file'].filter(key => text(flags, key) === '-').length > 1) usage('Only one input file can read from stdin.');
  return () => io.input();
}

export async function buildAgent(name: string, flags: Flags, namespace: string, io: IO): Promise<Resource> {
  checkFlags(flags, ['framework', ...bindingFlags, ...promptFlags, 'task', 'task-file', 'config-file', '__preset-config', 'preset', 'mode', 'image', 'cpu', 'memory'], 'agent creation');
  const framework = dnsLabel(nonempty(flags, 'framework'), '--framework');
  const mode = text(flags, 'mode') ?? 'deployment';
  if (!['deployment', 'once', 'job'].includes(mode)) usage('--mode must be deployment or once.');
  const lifecycle = mode === 'once' ? 'job' : mode;
  const model = binding(flags);
  const input = fileInput(flags, io);
  let config: ObjectValue = supplied(flags, '__preset-config') ? jsonObject(nonempty(flags, '__preset-config'), 'Preset configuration') : {};
  const fromFile = await inputValue(flags, '__unused-config', 'config-file', input);
  if (fromFile !== undefined) config = mergeConfig(config, jsonObject(fromFile, '--config-file'), '--config-file');
  const prompt = await inputValue(flags, 'prompt', 'prompt-file', input);
  const task = await inputValue(flags, 'task', 'task-file', input);
  if (prompt !== undefined) config = mergeConfig(config, { systemPrompt: prompt }, '--prompt');
  if (task !== undefined) config = mergeConfig(config, { task }, '--task');
  if (supplied(flags, 'image')) config = mergeConfig(config, { image: imageReference(nonempty(flags, 'image'), '--image') }, '--image');
  for (const key of ['systemPrompt', 'task', 'image']) {
    if (Object.prototype.hasOwnProperty.call(config, key) && typeof config[key] !== 'string') usage(`Configuration field ${key} must be a string.`);
  }
  if (typeof config.image === 'string') imageReference(config.image, 'Configuration image');
  if (lifecycle === 'job' && (typeof (config.task ?? config.prompt) !== 'string' || !(config.task ?? config.prompt).trim())) usage('--mode once requires a task, using --task, --task-file, or config.task.');
  if (lifecycle !== 'job' && task !== undefined) usage('--task and --task-file require --mode once.');
  const spec: ObjectValue = { framework: { name: framework }, lifecycle, model };
  if (Object.keys(config).length) spec.config = config;
  const requests: ObjectValue = {};
  for (const key of ['cpu', 'memory']) if (supplied(flags, key)) requests[key] = quantity(flags, key);
  if (Object.keys(requests).length) spec.resources = { requests };
  return manifest('agent', name, namespace, spec);
}

export async function updateResource(noun: ResourceNoun, existing: Resource, flags: Flags, io: IO): Promise<Resource> {
  if (existing.kind !== resourceTypes[noun].kind) usage(`The existing resource is not a ${noun}.`);
  const immutable = immutableFlags.find(key => supplied(flags, key));
  if (immutable) usage(`--${immutable} is immutable. Create a new ${noun} instead.`);
  if (noun === 'agent' && existing.spec?.lifecycle === 'job') usage('One-shot agents cannot be updated. Create a new agent to run another task.');
  checkFlags(flags, noun === 'model' ? modelMutableFlags : [...promptFlags, ...bindingFlags], `${noun} updates`);
  let spec: ObjectValue = {};
  if (noun === 'model') {
    spec = modelOptions(flags);
    if (supplied(flags, 'served-name') && existing.spec?.model?.source === 'custom' && !existing.spec.model.artifact) usage('--served-name is not supported for unstaged custom models.');
    if (supplied(flags, 'credential')) {
      if (existing.spec?.model?.artifact) usage('Staged artifact credentials are immutable. Rotate the existing credential or create a new model.');
      if (existing.spec?.model?.source === 'custom') usage('--credential is only supported for Hugging Face model updates.');
      spec.secrets = hfCredential(flags);
    }
  } else {
    const prompt = await inputValue(flags, 'prompt', 'prompt-file', fileInput(flags, io));
    if (prompt !== undefined) spec.config = { systemPrompt: prompt };
    if (anySupplied(flags, bindingFlags)) spec.model = binding(flags, existing.spec?.model);
  }
  if (!Object.keys(spec).length) usage(`Supply at least one mutable field for the ${noun} update.`);
  const metadata: Resource['metadata'] = { name: existing.metadata.name };
  if (existing.metadata.namespace !== undefined) metadata.namespace = existing.metadata.namespace;
  if (existing.metadata.resourceVersion !== undefined) metadata.resourceVersion = existing.metadata.resourceVersion;
  return { apiVersion: existing.apiVersion, kind: existing.kind, metadata, spec };
}
