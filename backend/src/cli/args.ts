import { parseArgs } from 'node:util';
import { CLIError, type Flags, type ParsedArgs } from './types';

const booleanFlags = ['help', 'version', 'all-namespaces', 'follow', 'check', 'wait', 'trust-remote-code', 'gateway', 'timestamps'];
const valueFlags = ['kubeconfig', 'context', 'namespace', 'output', 'timeout', 'id', 'gpus', 'cpu', 'memory', 'provider', 'engine', 'image', 'model-path', 'served-name', 'context-length', 'replicas', 'credential', 'revision', 'file', 'storage-size', 'storage-class', 'artifact-image', 'service-account', 'framework', 'model-ref', 'prompt', 'prompt-file', 'model-url', 'model-api', 'model-id', 'model-credential', 'model-gateway', 'gateway-listener', 'mode', 'task', 'task-file', 'config-file', 'preset', 'dry-run', 'for', 'tail', 'pod', 'container', 'port', 'message', 'message-file', 'type', 'from-file', 'server', 'temperature', 'max-tokens'];
const short: Record<string, string> = { output: 'o', namespace: 'n', help: 'h', follow: 'f', version: 'v' };
export const flagNames = [...booleanFlags, ...valueFlags, 'engine-arg'];
// Recover only the requested error format when strict parsing fails. This does
// not authorize a command or relax its validation.
export function requestedOutput(argv: string[]): string | undefined {
  let value: string | undefined;
  for (let i = 0; i < argv.length && argv[i] !== '--'; i++) {
    const arg = argv[i];
    if (arg === '--output' || arg === '-o') value = argv[++i];
    else if (arg.startsWith('--output=')) value = arg.slice('--output='.length);
    else if (arg.startsWith('-o') && !arg.startsWith('--')) value = arg.slice(2).replace(/^=/, '');
  }
  return value;
}
export function parseCLIArgs(argv: string[]): ParsedArgs {
  // Normalize assigned booleans without changing token indexes. Reprocess the
  // option tokens below so repeated flags remain last-wins and values after --
  // are left untouched.
  let positionalOnly = false;
  const args = argv.map(arg => {
    if (arg === '--') positionalOnly = true;
    const match = !positionalOnly && /^--([a-z-]+)=(true|false)$/.exec(arg);
    return match && booleanFlags.includes(match[1]) ? `--${match[1]}` : arg;
  });
  try {
    const options: Record<string, { type: 'boolean' | 'string'; short?: string; multiple?: boolean }> = {};
    for (const name of booleanFlags) options[name] = { type: 'boolean', ...(short[name] ? { short: short[name] } : {}) };
    for (const name of valueFlags) options[name] = { type: 'string', ...(short[name] ? { short: short[name] } : {}) };
    options['engine-arg'] = { type: 'string', multiple: true };
    const parsed = parseArgs({ args, options, strict: true, allowPositionals: true, tokens: true });
    const flags: Flags = {};
    for (const [key, value] of Object.entries(parsed.values)) flags[key] = Array.isArray(value) ? value.map(String) : value;
    for (const token of parsed.tokens) {
      if (token.kind === 'option' && booleanFlags.includes(token.name)) flags[token.name] = !argv[token.index].endsWith('=false');
    }
    return { words: parsed.positionals, flags };
  } catch (error) { throw new CLIError(error instanceof Error ? error.message : 'Invalid arguments.', 2, 'USAGE'); }
}
export function text(flags: Flags, name: string): string | undefined { const value = flags[name]; return typeof value === 'string' ? value : undefined; }
export function required(flags: Flags, name: string): string { const value = text(flags, name); if (!value) throw new CLIError(`Provide --${name}.`, 2, 'USAGE'); return value; }
export function integer(flags: Flags, name: string, fallback?: number, min = 0, max = Number.MAX_SAFE_INTEGER): number | undefined {
  const raw = text(flags, name); if (raw === undefined) return fallback;
  if (!/^\d+$/.test(raw) || !Number.isSafeInteger(Number(raw)) || Number(raw) < min || Number(raw) > max) throw new CLIError(`--${name} must be an integer between ${min} and ${max}.`, 2, 'USAGE');
  return Number(raw);
}
export function duration(value = '10m'): number {
  const match = /^(\d+)(ms|s|m|h)$/.exec(value);
  if (!match) throw new CLIError('Use a positive timeout such as 30s, 10m, or 1h.', 2, 'USAGE');
  const ms = Number(match[1]) * ({ ms: 1, s: 1000, m: 60000, h: 3600000 }[match[2]]!);
  if (!Number.isSafeInteger(ms) || ms < 1 || ms > 86400000) throw new CLIError('Timeout must be between 1ms and 24h.', 2, 'USAGE');
  return ms;
}
export function name(value: string | undefined, label = 'name'): string {
  if (!value || !/^[a-z]([-a-z0-9]*[a-z0-9])?$/.test(value) || value.length > 63) throw new CLIError(`Provide a ${label} of at most 63 lowercase letters, numbers, or hyphens, starting with a letter.`, 2, 'USAGE');
  return value;
}
export async function inputValue(flags: Flags, inline: string, file: string, input: () => Promise<string>): Promise<string | undefined> {
  const value = text(flags, inline), path = text(flags, file);
  if (value !== undefined && path !== undefined) throw new CLIError(`Use --${inline} or --${file}, not both.`, 2, 'USAGE');
  if (path === undefined) return value;
  try {
    if (path === '-') return await input();
    const content = Bun.file(path);
    if (content.size > 4 * 1024 * 1024) throw new CLIError(`--${file} is limited to 4 MiB.`, 2, 'USAGE');
    return await content.text();
  } catch (error) { if (error instanceof CLIError) throw error; throw new CLIError(`Cannot read --${file} file.`, 2, 'USAGE'); }
}
