import { useEffect, useRef, useState } from 'react'
import { type DynamoIntent } from '@airunway/shared'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { InfoHint } from '@/components/ui/InfoHint'

function parseOverrides(text: string): { overrides?: DynamoIntent['overrides']; error?: string } {
  if (!text.trim()) return {}
  let parsed: unknown
  let hasNonFiniteNumber = false
  try {
    parsed = JSON.parse(text, (_key, value: unknown) => {
      // Reject overflow before JSON.stringify can silently turn it into null.
      if (typeof value === 'number' && !Number.isFinite(value)) hasNonFiniteNumber = true
      return value
    })
  } catch {
    return { error: 'Enter valid JSON or clear advanced configuration to use the defaults.' }
  }
  if (hasNonFiniteNumber) return { error: 'Numbers in advanced configuration must be finite.' }
  const isObject = (value: unknown): value is Record<string, unknown> =>
    value !== null && typeof value === 'object' && !Array.isArray(value)
  if (!isObject(parsed)) return { error: 'Advanced configuration must be a JSON object.' }
  if (Object.keys(parsed).some(key => key !== 'profilingJob' && key !== 'dgd')) {
    return { error: 'Only "profilingJob" and "dgd" are supported in advanced configuration.' }
  }
  if ('profilingJob' in parsed && !isObject(parsed.profilingJob)) {
    return { error: 'The "profilingJob" setting must be a JSON object.' }
  }
  if ('dgd' in parsed) {
    const dgd = parsed.dgd
    if (!isObject(dgd)) return { error: 'The "dgd" setting must be a JSON object.' }
    if ((dgd.apiVersion !== 'nvidia.com/v1alpha1' && dgd.apiVersion !== 'nvidia.com/v1beta1') ||
      dgd.kind !== 'DynamoGraphDeployment' || !isObject(dgd.spec)) {
      return { error: 'The "dgd" setting needs a supported "apiVersion", "kind": "DynamoGraphDeployment", and a "spec" JSON object.' }
    }
    if ('metadata' in dgd && !isObject(dgd.metadata)) {
      return { error: 'The "metadata" setting must be a JSON object.' }
    }
  }
  // Detailed field and policy validation belongs to the server. Preserve nested
  // data verbatim, including the beta container $patch.args append directive.
  return { overrides: parsed as DynamoIntent['overrides'] }
}

export function DynamoIntentFields({ value, onChange, onValidityChange, disabled = false, prefix = 'intent' }: {
  value: DynamoIntent; onChange: (intent: DynamoIntent) => void; onValidityChange?: (valid: boolean) => void; disabled?: boolean; prefix?: string
}) {
  const savedText = JSON.stringify(value.overrides, null, 2) ?? ''
  const [draft, setDraft] = useState({ savedText, text: savedText })
  // Ordinary intent edits must not discard invalid JSON or change its formatting.
  // Replace the draft only when the supplied overrides themselves change.
  if (draft.savedText !== savedText) setDraft({ savedText, text: savedText })
  const { error } = parseOverrides(draft.text)
  const editorRef = useRef<HTMLTextAreaElement>(null)
  useEffect(() => {
    editorRef.current?.setCustomValidity(error ?? '')
    onValidityChange?.(!error)
  }, [error, onValidityChange])
  const workload = value.workload || {}
  const sla = value.sla || {}
  const concurrency = Object.prototype.hasOwnProperty.call(workload, 'concurrency')
  const totalLatency = Object.prototype.hasOwnProperty.call(sla, 'e2eLatency')
  const numberField = (key: string, label: string, help: string, current: number | undefined,
    update: (n: number | undefined) => void, options: { max?: number; integer?: boolean; required?: boolean } = {}) => (
    <div className="space-y-2" key={key}>
      <Label htmlFor={`${prefix}-${key}`} className="flex items-center gap-2">{label}<InfoHint text={help} /></Label>
      <Input id={`${prefix}-${key}`} type="number" min={options.integer ? 1 : 0.001}
        max={options.max} step={options.integer ? 1 : 'any'} required={options.required}
        value={current ?? ''} onChange={e => update(e.target.value === '' ? undefined : Number(e.target.value))} />
    </div>
  )
  return <fieldset disabled={disabled} className="space-y-4">
    <div className="grid gap-4 sm:grid-cols-2">
      {numberField('gpus', 'GPU budget', 'Maximum GPUs Dynamo can use to choose a serving configuration. This is a total budget, not GPUs per copy.', value.hardware.totalGpus,
        n => onChange({ ...value, hardware: { ...value.hardware, totalGpus: n ?? 0 } }), { integer: true, max: 64, required: true })}
      <div className="space-y-2">
        <Label htmlFor={`${prefix}-gpuSku`} className="flex items-center gap-2">GPU model (optional)<InfoHint text="Leave blank to let Dynamo discover available hardware. Otherwise enter the GPU model supported by your installation." /></Label>
        <Input id={`${prefix}-gpuSku`} maxLength={128} value={value.hardware.gpuSku || ''}
          onChange={e => onChange({ ...value, hardware: { ...value.hardware, gpuSku: e.target.value || undefined } })} />
      </div>
      {numberField('input', 'Input tokens', 'Typical length of the prompt and conversation sent with each request.', workload.isl,
        n => onChange({ ...value, workload: { ...workload, isl: n } }), { integer: true, max: 2147483647 })}
      {numberField('output', 'Output tokens', 'Typical number of tokens you expect in each generated answer.', workload.osl,
        n => onChange({ ...value, workload: { ...workload, osl: n } }), { integer: true, max: 2147483647 })}
      <div className="space-y-2">
        <Label htmlFor={`${prefix}-traffic`}>Expected traffic</Label>
        <select id={`${prefix}-traffic`} className="w-full rounded-md border bg-background p-2" value={concurrency ? 'concurrency' : 'rate'} onChange={e => {
          const { requestRate: _r, concurrency: _c, ...rest } = workload
          onChange({ ...value, workload: { ...rest, ...(e.target.value === 'rate' ? { requestRate: 1 } : { concurrency: 1 }) } })
        }}><option value="rate">Requests per second</option><option value="concurrency">Simultaneous requests</option></select>
      </div>
      {numberField('traffic-value', concurrency ? 'Simultaneous requests' : 'Requests per second', 'Estimate your typical traffic. Specify either a request rate or simultaneous requests, not both.', concurrency ? workload.concurrency : workload.requestRate,
        n => onChange({ ...value, workload: { ...workload, [concurrency ? 'concurrency' : 'requestRate']: n } }), { max: 1000000 })}
      <div className="space-y-2 sm:col-span-2">
        <Label htmlFor={`${prefix}-latency`}>Latency targets</Label>
        <select id={`${prefix}-latency`} className="w-full rounded-md border bg-background p-2" value={totalLatency ? 'total' : 'stream'} onChange={e => onChange({ ...value, sla: e.target.value === 'total' ? { e2eLatency: 10000 } : { ttft: 1000, itl: 50 } })}>
          <option value="stream">First response and streaming speed</option><option value="total">Total response time</option>
        </select>
      </div>
      {totalLatency ? numberField('total', 'Total response target (ms)', 'Desired time to finish a complete answer. This is a target, not a guarantee.', sla.e2eLatency,
        n => onChange({ ...value, sla: { e2eLatency: n } }), { max: 86400000 }) : <>
        {numberField('first', 'First response target (ms)', 'Desired delay before the first generated token arrives. This is a target, not a guarantee.', sla.ttft,
          n => onChange({ ...value, sla: { ...sla, ttft: n } }), { max: 86400000 })}
        {numberField('between', 'Time between tokens (ms)', 'Desired delay between streamed tokens. Lower values mean faster streaming, not guaranteed performance.', sla.itl,
          n => onChange({ ...value, sla: { ...sla, itl: n } }), { max: 86400000 })}
      </>}
    </div>
    <div className="space-y-2">
      <Label htmlFor={`${prefix}-overrides`} className="flex items-center gap-2">Advanced configuration (optional)<InfoHint text="Add JSON settings for the configuration search or generated model server. Leave blank to use the defaults. The server checks which settings your installation supports." /></Label>
      <textarea ref={editorRef} id={`${prefix}-overrides`} rows={6} spellCheck={false}
        className="w-full rounded-md border bg-background p-2 font-mono text-sm"
        value={draft.text} aria-invalid={!!error}
        aria-describedby={`${prefix}-overrides-help${error ? ` ${prefix}-overrides-error` : ''}`}
        placeholder={'{ "profilingJob": { "activeDeadlineSeconds": 1800 } }'}
        onChange={e => {
          const text = e.target.value
          const result = parseOverrides(text)
          e.target.setCustomValidity(result.error ?? '')
          onValidityChange?.(!result.error)
          if (result.error) {
            setDraft({ savedText, text })
          } else {
            setDraft({ savedText: JSON.stringify(result.overrides, null, 2) ?? '', text })
            const { overrides: _overrides, ...rest } = value
            onChange(result.overrides === undefined ? rest : { ...rest, overrides: result.overrides })
          }
        }} />
      <p id={`${prefix}-overrides-help`} className="text-sm text-muted-foreground">Enter only the advanced settings here, not the whole deployment. Clear this field to remove them. GPU allocation and copy counts remain managed automatically.</p>
      {error && <p id={`${prefix}-overrides-error`} role="alert" className="text-sm text-destructive">{error}</p>}
    </div>
    <p className="text-sm text-muted-foreground">The standard runtime options, placement rules, and storage controls are available in manual configuration. Automatic configuration uses the installation's default Hugging Face access.</p>
    <p className="text-sm text-muted-foreground">Dynamo uses a rapid configuration search and starts the selected configuration automatically. These performance targets are not guarantees.</p>
  </fieldset>
}
