import { mkdir, readFile, rename, writeFile, chmod } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { homedir } from 'node:os';
import { randomUUID } from 'node:crypto';
import { CLIError, type Flags } from './types';
import { name } from './args';

export interface CLIConfig { version: 1; context?: string; contexts: Record<string, { namespace?: string; namespaces: Record<string, { 'agent.framework'?: string; 'agent.model-ref'?: string }> }> }
const empty = (): CLIConfig => ({ version: 1, contexts: {} });
export function configPath(): string { return process.env.AIRUNWAY_CONFIG || join(process.env.XDG_CONFIG_HOME || join(homedir(), '.config'), 'airunway', 'cli.json'); }
export async function readConfig(path = configPath()): Promise<CLIConfig> {
  try {
    const stat = Bun.file(path); if (stat.size > 1024 * 1024) throw new Error('too large');
    const value = JSON.parse(await readFile(path, 'utf8'));
    if (value.version !== 1 || !value.contexts || typeof value.contexts !== 'object' || Array.isArray(value.contexts)) throw new Error('schema');
    return value;
  } catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return empty(); throw new CLIError('Cannot read AI Runway configuration. Check AIRUNWAY_CONFIG.', 2, 'CONFIG'); }
}
export async function writeConfig(config: CLIConfig, path = configPath()): Promise<void> {
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const temporary = `${path}.${randomUUID()}.tmp`;
  await writeFile(temporary, JSON.stringify(config, null, 2) + '\n', { mode: 0o600, flag: 'wx' });
  await rename(temporary, path); await chmod(path, 0o600);
}
export function effectiveDefaults(config: CLIConfig, context: string, namespace: string): Flags {
  const values = config.contexts[context]?.namespaces?.[namespace] || {};
  return { framework: values['agent.framework'], 'model-ref': values['agent.model-ref'] };
}
export function changeConfig(config: CLIConfig, context: string, namespace: string, key: string, value?: string): void {
  if (!['namespace', 'agent.framework', 'agent.model-ref'].includes(key)) throw new CLIError('Supported settings: namespace, agent.framework, agent.model-ref.', 2, 'USAGE');
  if (['__proto__', 'constructor', 'prototype'].includes(context)) throw new CLIError('Unsupported context name.', 2, 'USAGE');
  if (value !== undefined) name(value, key);
  config.contexts[context] ||= { namespaces: {} };
  const target = config.contexts[context];
  target.namespaces ||= {};
  if (key === 'namespace') { if (value === undefined) delete target.namespace; else target.namespace = value; return; }
  target.namespaces[namespace] ||= {};
  if (value === undefined) delete target.namespaces[namespace][key as 'agent.framework' | 'agent.model-ref'];
  else target.namespaces[namespace][key as 'agent.framework' | 'agent.model-ref'] = value;
}
