import { BUILD_INFO } from '../build-info';
import { parseCLIArgs, requestedOutput, text, name, duration } from './args';
import { KubernetesCLIClient, loadClientConfig } from './client';
import { changeConfig, effectiveDefaults, readConfig, writeConfig, type CLIConfig } from './config';
import { help, completion } from './help';
import { output } from './output';
import { CLIError, resourceTypes, type ClusterClient, type CommandContext, type Flags, type IO, type Resource, type ResourceNoun } from './types';

const cliNouns = new Set(['model', 'agent', 'context', 'config', 'doctor', 'credential', 'provider', 'framework', 'catalog', 'apply', 'completion', 'version']);
export function isCLICommand(args: string[]): boolean {
  return args.length > 0 && !['serve', 'login', 'logout'].includes(args[0]);
}
const globalOptions = ['kubeconfig', 'context', 'namespace', 'output', 'timeout', 'help', 'version'];
const createOptions = ['id', 'gpus', 'cpu', 'memory', 'provider', 'engine', 'image', 'model-path', 'served-name', 'context-length', 'replicas', 'credential', 'revision', 'file', 'storage-size', 'storage-class', 'artifact-image', 'service-account', 'engine-arg', 'trust-remote-code', 'gateway', 'framework', 'model-ref', 'prompt', 'prompt-file', 'model-url', 'model-api', 'model-id', 'model-credential', 'model-gateway', 'gateway-listener', 'mode', 'task', 'task-file', 'config-file', 'preset', 'dry-run', 'wait'];
function assertFlags(flags: Flags, allowed: string[]): void {
  const extras = Object.keys(flags).filter(key => !globalOptions.includes(key) && !allowed.includes(key));
  if (extras.length) throw new CLIError(`Unsupported option --${extras[0]} for this command.`, 2, 'USAGE');
}
function assertPositionals(words: string[], count: number): void { if (words.length !== count) throw new CLIError('Unexpected or missing command arguments. Run with --help.', 2, 'USAGE'); }
export function mergeAgentDefaults(flags: Flags, defaults: Flags): Flags {
  const merged = { ...defaults, ...flags };
  const explicitBinding = ['model-ref', 'model-url', 'model-gateway', 'model-id', 'model-api', 'model-credential', 'gateway-listener'].some(key => flags[key] !== undefined);
  if (explicitBinding) for (const key of ['model-ref', 'model-url', 'model-gateway', 'model-id', 'model-api', 'model-credential', 'gateway-listener']) if (flags[key] === undefined) delete merged[key];
  return merged;
}
function dryRun(flags: Flags): string | undefined {
  const value = text(flags, 'dry-run');
  if (value !== undefined && value !== 'client' && value !== 'server') throw new CLIError('--dry-run must be client or server.', 2, 'USAGE');
  return value;
}
async function pause(ms: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) throw new CLIError('Interrupted. Submitted resources were not deleted.', 130, 'INTERRUPTED');
  await new Promise<void>((resolve, reject) => {
    const onAbort = () => { clearTimeout(timer); reject(new CLIError('Interrupted. Submitted resources were not deleted.', 130, 'INTERRUPTED')); };
    const timer = setTimeout(() => { signal.removeEventListener('abort', onAbort); resolve(); }, ms);
    signal.addEventListener('abort', onAbort, { once: true });
  });
}
async function preflight(resource: Resource, noun: ResourceNoun, client: ClusterClient): Promise<void> {
  // A readable resource collection proves the CRD exists, without requiring
  // cluster-admin permission to enumerate CRDs themselves.
  await client.list(resourceTypes[noun]);
  if (noun === 'agent') {
    const framework = await client.get(resourceTypes.framework, resource.spec!.framework.name);
    if (framework.status?.ready !== true) throw new CLIError('The selected agent framework is not ready. Run airunway framework get NAME.', 1, 'NOT_READY');
    const caps = framework.spec?.capabilities;
    if (resource.spec?.lifecycle === 'job' && caps?.backend !== 'container') throw new CLIError('One-shot mode requires a container-backed framework.', 2, 'UNSUPPORTED');
    if ((resource.spec?.resources || resource.spec?.config?.image) && caps?.backend !== 'container') throw new CLIError('Image and resource overrides require a container-backed framework.', 2, 'UNSUPPORTED');
    const binding = resource.spec?.model || {};
    const bindingMode = Object.keys(binding)[0];
    if (caps?.modelBindingModes && !caps.modelBindingModes.includes(bindingMode)) throw new CLIError('The selected framework does not support this model binding.', 2, 'UNSUPPORTED');
    if (binding.deploymentRef) await client.get(resourceTypes.model, binding.deploymentRef.name, binding.deploymentRef.namespace);
    if (binding.gatewayEndpoint) await client.get(resourceTypes.gateway, binding.gatewayEndpoint.gatewayRef.name, binding.gatewayEndpoint.gatewayRef.namespace);
    if (binding.externalAPI?.credentialsRef) await client.get(resourceTypes.credential, binding.externalAPI.credentialsRef.name);
  } else {
    const providers = await client.list(resourceTypes.provider);
    const requested = resource.spec?.provider?.name;
    if (!providers.some(p => p.status?.ready === true && (!requested || p.metadata.name === requested))) throw new CLIError('No matching model provider is ready. Run airunway provider list.', 1, 'NOT_READY');
    if (resource.spec?.secrets?.huggingFaceToken) await client.get(resourceTypes.credential, resource.spec.secrets.huggingFaceToken);
    if (resource.spec?.model?.artifact?.credentialsRef) await client.get(resourceTypes.credential, resource.spec.model.artifact.credentialsRef.name);
  }
}
export interface RunOptions { io?: IO; client?: ClusterClient; signal?: AbortSignal; config?: CLIConfig }
export async function runCLI(argv: string[], options: RunOptions = {}): Promise<number> {
  let flags: Flags = { output: requestedOutput(argv) };
  const io: IO = options.io || { out: value => process.stdout.write(value), err: value => process.stderr.write(value), interactive: Boolean(process.stdin.isTTY && process.stdout.isTTY), input: async () => {
    let size = 0; const parts: Buffer[] = [];
    for await (const part of process.stdin) { const bytes = Buffer.from(part); size += bytes.length; if (size > 4 * 1024 * 1024) throw new CLIError('Input is limited to 4 MiB.', 2, 'USAGE'); parts.push(bytes); }
    return Buffer.concat(parts).toString('utf8');
  } };
  const signal = options.signal || new AbortController().signal;
  try {
    const parsed = parseCLIArgs(argv); flags = parsed.flags;
    const words = parsed.words;
    if (flags.help || words[0] === 'help') { io.out(help); return 0; }
    if (!['text', 'json', 'yaml'].includes(text(flags, 'output') || 'text')) throw new CLIError('--output must be text, json, or yaml.', 2, 'USAGE');
    if (flags.version || words[0] === 'version') {
      if (!flags.version) { assertPositionals(words, 1); assertFlags(flags, []); }
      output(io, flags, text(flags, 'output') && text(flags, 'output') !== 'text' ? BUILD_INFO : `AI Runway ${BUILD_INFO.version} (${BUILD_INFO.gitCommit})`);
      return 0;
    }
    if (!words.length || !cliNouns.has(words[0])) throw new CLIError('Unknown or missing command. Run airunway --help.', 2, 'USAGE');
    if (text(flags, 'timeout')) duration(text(flags, 'timeout'));
    if (words[0] === 'completion') { assertPositionals(words, 2); assertFlags(flags, []); output(io, flags, completion(words[1])); return 0; }
    const config = options.config || await readConfig();
    let kubeConfig: ReturnType<typeof loadClientConfig> | undefined;
    const getKubeConfig = () => kubeConfig ||= loadClientConfig(text(flags, 'kubeconfig'), text(flags, 'context') || config.context);
    // Previews may read local defaults, but never require kubeconfig or invoke authentication.
    const localPreview = text(flags, 'dry-run') === 'client' || (words[0] === 'catalog' && words[1] === 'model');
    if (localPreview && !options.client) {
      try { kubeConfig = loadClientConfig(text(flags, 'kubeconfig')); } catch { /* A local preview also works without cluster configuration. */ }
    }
    const selectedContext = options.client?.context || text(flags, 'context') || config.context || (localPreview ? kubeConfig?.getCurrentContext() || '' : getKubeConfig().getCurrentContext());
    const currentKubeContext = !options.client ? (localPreview ? kubeConfig : getKubeConfig())?.getContexts().find(c => c.name === selectedContext) : undefined;
    const namespace = name(text(flags, 'namespace') || options.client?.namespace || config.contexts[selectedContext]?.namespace || currentKubeContext?.namespace || 'default', 'namespace');
    let client = options.client;
    const commandIO: IO = {
      interactive: io.interactive, input: () => io.input(), out: value => io.out(value),
      err: value => io.err(text(flags, 'output') === 'json' ? JSON.stringify({ progress: { message: value.trimEnd() } }) + '\n' : value),
    };
    const ctx: CommandContext = { flags, io: commandIO, signal, context: selectedContext, namespace, client: () => client ||= new KubernetesCLIClient(getKubeConfig(), namespace, signal) };
    if (words[0] === 'context') {
      assertFlags(flags, []);
      if (words[1] === 'list') { assertPositionals(words, 2); output(io, flags, getKubeConfig().getContexts().map(c => ({ name: c.name, cluster: c.cluster, namespace: c.namespace || 'default', selected: c.name === selectedContext }))); }
      else if (words[1] === 'current') { assertPositionals(words, 2); output(io, flags, { context: selectedContext, namespace }); }
      else if (words[1] === 'use') { assertPositionals(words, 3); if (!getKubeConfig().getContexts().some(c => c.name === words[2])) throw new CLIError('That context does not exist in kubeconfig.', 2, 'USAGE'); config.context = words[2]; await writeConfig(config); output(io, flags, { context: words[2] }); }
      else throw new CLIError('Use context list, current, or use NAME.', 2, 'USAGE');
      return 0;
    }
    if (words[0] === 'config') {
      assertFlags(flags, []);
      if (words[1] === 'get') { assertPositionals(words, 2); output(io, flags, { context: selectedContext, namespace, ...effectiveDefaults(config, selectedContext, namespace) }); }
      else if (words[1] === 'set' || words[1] === 'unset') { assertPositionals(words, words[1] === 'set' ? 4 : 3); changeConfig(config, selectedContext, namespace, words[2], words[1] === 'set' ? words[3] : undefined); await writeConfig(config); output(io, flags, { setting: words[2], value: words[1] === 'set' ? words[3] : null, context: selectedContext, namespace }); }
      else throw new CLIError('Use config get, set KEY VALUE, or unset KEY.', 2, 'USAGE');
      return 0;
    }
    if (words[0] === 'doctor') {
      assertPositionals(words, 1); assertFlags(flags, []);
      const checks: Array<{ check: string; ok: boolean; detail: string }> = [];
      for (const noun of ['model', 'agent', 'provider', 'framework'] as const) {
        try { const found = await ctx.client().list(resourceTypes[noun]); checks.push({ check: noun, ok: true, detail: `${found.length} visible` }); }
        catch (error) { checks.push({ check: noun, ok: false, detail: error instanceof Error ? error.message : 'Not available' }); }
      }
      for (const noun of ['model', 'agent'] as const) {
        const result = await ctx.client().request<{ status?: { allowed?: boolean } }>('POST', '/apis/authorization.k8s.io/v1/selfsubjectaccessreviews', { apiVersion: 'authorization.k8s.io/v1', kind: 'SelfSubjectAccessReview', spec: { resourceAttributes: { namespace, group: 'airunway.ai', resource: resourceTypes[noun].plural, verb: 'create' } } });
        checks.push({ check: `create ${noun}`, ok: result.status?.allowed === true, detail: result.status?.allowed ? 'Allowed' : 'Not allowed' });
      }
      output(io, flags, { context: selectedContext, namespace, checks }); return checks.every(c => c.ok) ? 0 : 1;
    }
    if (words[0] !== 'model' && words[0] !== 'agent') {
      const { runManagement } = await import('./management');
      if (await runManagement(words, ctx)) return 0;
      throw new CLIError('Unknown command. Run airunway --help.', 2, 'USAGE');
    }
    const noun = words[0] as ResourceNoun, action = words[1];
    const type = resourceTypes[noun];
    if (action === 'list') { assertPositionals(words, 2); assertFlags(flags, ['all-namespaces']); output(io, flags, await ctx.client().list(type, flags['all-namespaces'] ? '' : namespace)); return 0; }
    assertPositionals(words, 3); const resourceName = name(words[2]);
    if (action === 'get') { assertFlags(flags, []); output(io, flags, await ctx.client().get(type, resourceName)); return 0; }
    if (action === 'create') {
      assertFlags(flags, createOptions); const dry = dryRun(flags);
      const { buildModel, buildAgent } = await import('./manifests');
      let creationFlags = noun === 'agent' ? mergeAgentDefaults(flags, effectiveDefaults(config, selectedContext, namespace)) : flags;
      if (noun === 'agent' && text(flags, 'preset')) {
        if (localPreview) throw new CLIError('Preset resolution needs a cluster. Use --dry-run server or provide framework configuration directly.', 2, 'USAGE');
        const { resolvePreset } = await import('./management');
        const preset = await resolvePreset(ctx.client(), text(flags, 'preset')!);
        if (text(flags, 'framework') && text(flags, 'framework') !== preset.framework) throw new CLIError('The preset and --framework disagree.', 2, 'USAGE');
        creationFlags = { ...creationFlags, framework: preset.framework, '__preset-config': JSON.stringify(preset.config) };
      }
      const resource = await (noun === 'model' ? buildModel : buildAgent)(resourceName, creationFlags, namespace, io);
      if (dry === 'client') { output(io, flags, resource); return 0; }
      await preflight(resource, noun, ctx.client());
      const created = await ctx.client().create(resource, dry === 'server');
      if (dry || flags.wait === false) { output(io, flags, created); return 0; }
      ctx.io.err(`Created ${noun} "${resourceName}" in ${namespace}. Waiting; timeout or interruption will not delete it.\n`);
      const { waitForResource } = await import('./access');
      output(io, flags, await waitForResource(ctx.client(), noun, created, { ...flags, for: resource.spec?.lifecycle === 'job' ? 'completed' : 'ready' }, io, signal)); return 0;
    }
    if (action === 'update') {
      assertFlags(flags, createOptions.filter(key => !['preset', 'dry-run', 'wait'].includes(key)).concat(['wait', 'dry-run'])); const dry = dryRun(flags);
      if (dry === 'client') throw new CLIError('Updates need the existing resource. Use --dry-run server.', 2, 'USAGE');
      const existing = await ctx.client().get(type, resourceName);
      const { updateResource } = await import('./manifests');
      const patch = await updateResource(noun, existing, flags, io);
      const updated = await ctx.client().patch(type, resourceName, patch, namespace, dry === 'server');
      if (dry || flags.wait === false) output(io, flags, updated);
      else { const { waitForResource } = await import('./access'); output(io, flags, await waitForResource(ctx.client(), noun, updated, flags, io, signal)); }
      return 0;
    }
    if (action === 'delete') {
      assertFlags(flags, ['wait']); const existing = await ctx.client().get(type, resourceName);
      await ctx.client().delete(type, resourceName, namespace, existing.metadata.uid);
      if (flags.wait !== false) {
        const deadline = Date.now() + duration(text(flags, 'timeout'));
        while (true) {
          try { const current = await ctx.client().get(type, resourceName); if (current.metadata.uid !== existing.metadata.uid) break; }
          catch (error) { if (error instanceof CLIError && error.code === 'HTTP_404') break; throw error; }
          if (Date.now() >= deadline) throw new CLIError(`Deletion is still pending for ${resourceName}. Inspect its events; no other resources were deleted.`, 4, 'TIMEOUT');
          await pause(1000, signal);
        }
      }
      output(io, flags, { name: resourceName, namespace, deletionRequested: true }); return 0;
    }
    const { waitForResource, runAccess } = await import('./access');
    if (action === 'wait') { assertFlags(flags, ['for']); const resource = await ctx.client().get(type, resourceName); output(io, flags, await waitForResource(ctx.client(), noun, resource, flags, io, signal)); return 0; }
    if (['endpoint', 'connect', 'chat', 'logs', 'events'].includes(action)) {
      const allowed: Record<string, string[]> = { endpoint: ['check', 'gateway', 'gateway-listener', 'server'], connect: ['port', 'gateway', 'gateway-listener'], chat: ['message', 'message-file', 'temperature', 'max-tokens', 'gateway', 'gateway-listener', 'server', 'credential'], logs: ['follow', 'tail', 'pod', 'container', 'timestamps'], events: [] };
      assertFlags(flags, allowed[action]); await runAccess(noun, action as 'endpoint' | 'connect' | 'chat' | 'logs' | 'events', resourceName, ctx); return 0;
    }
    throw new CLIError('Unknown action. Run airunway --help.', 2, 'USAGE');
  } catch (error) {
    const failure = error instanceof CLIError ? error : new CLIError(error instanceof Error ? error.message : 'Command failed.');
    if (text(flags, 'output') === 'json') io.err(JSON.stringify({ error: { code: failure.code, message: failure.message } }) + '\n');
    else io.err(`Error: ${failure.message}\n`);
    return signal.aborted ? 130 : failure.exitCode;
  }
}
