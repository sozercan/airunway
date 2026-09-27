import { dump } from 'js-yaml';
import { CLIError, type Flags, type IO } from './types';
import { text } from './args';

export function output(io: IO, flags: Flags, value: unknown): void {
  const format = text(flags, 'output') || 'text';
  if (format === 'json') { io.out(JSON.stringify(value, null, 2) + '\n'); return; }
  if (format === 'yaml') { io.out(dump(value, { noRefs: true, lineWidth: 100 })); return; }
  if (format !== 'text') throw new CLIError('--output must be text, json, or yaml.', 2, 'USAGE');
  if (typeof value === 'string') { io.out(value + (value.endsWith('\n') ? '' : '\n')); return; }
  if (Array.isArray(value)) {
    if (!value.length) { io.out('No results.\n'); return; }
    const rows = value.map(item => {
      if (item?.metadata) return [item.metadata.name, item.metadata.namespace || '-', item.status?.phase ?? (item.status?.ready === undefined ? '' : item.status.ready ? 'Ready' : 'Not ready')].join('\t');
      return typeof item === 'object' ? Object.entries(item).map(([key, val]) => `${key}=${typeof val === 'object' ? JSON.stringify(val) : val}`).join('  ') : String(item);
    });
    io.out(rows.join('\n') + '\n'); return;
  }
  io.out(dump(value, { noRefs: true, lineWidth: 100 }));
}
