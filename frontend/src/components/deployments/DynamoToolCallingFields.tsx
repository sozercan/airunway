import { getDynamoParserDefaults, type DynamoToolCallingConfig } from '@airunway/shared'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { InfoHint } from '@/components/ui/InfoHint'

export function DynamoToolCallingFields({ value, modelId, onChange, error, prefix = 'dynamo' }: {
  value: DynamoToolCallingConfig
  modelId: string
  onChange: (value: DynamoToolCallingConfig) => void
  error?: string
  prefix?: string
}) {
  const defaults = getDynamoParserDefaults(modelId)
  return <div className="space-y-3">
    <div className="flex items-center gap-2">
      <input id={`${prefix}-tool-calling`} type="checkbox" checked={value.toolCalling === true}
        onChange={e => onChange({ toolCalling: e.target.checked, toolCallParser: undefined, reasoningParser: undefined })} />
      <Label htmlFor={`${prefix}-tool-calling`}>Enable tool calling</Label>
      <InfoHint text="Let the model request actions from tools connected to your chat app. Your app decides whether to run each action." />
    </div>
    {value.toolCalling && <div className="grid gap-3 sm:grid-cols-2">
      <div className="space-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${prefix}-tool-parser`}>Tool parser</Label>
          <InfoHint text="A parser reads the model's tool requests. Leave this empty for a known Qwen family, or enter the Dynamo parser name that matches your model." />
        </div>
        <Input id={`${prefix}-tool-parser`} value={value.toolCallParser ?? ''} maxLength={64}
          pattern="[a-z][a-z0-9_]*" placeholder={defaults.toolCallParser ?? 'Enter a parser name'}
          onChange={e => onChange({ ...value, toolCallParser: e.target.value || undefined })} />
        <p className="text-xs text-muted-foreground">{defaults.toolCallParser
          ? `Optional. Leave empty to use ${defaults.toolCallParser}.`
          : 'A parser name is required for this model. Automatic selection is not available.'}</p>
      </div>
      <div className="space-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${prefix}-reasoning-parser`}>Reasoning parser</Label>
          <InfoHint text="Separates the model's reasoning text from its answer. Leave this empty for the model-family default. This does not disable a parser configured elsewhere." />
        </div>
        <Input id={`${prefix}-reasoning-parser`} value={value.reasoningParser ?? ''} maxLength={64}
          pattern="[a-z][a-z0-9_]*" placeholder={defaults.reasoningParser ?? 'No model default'}
          onChange={e => onChange({ ...value, reasoningParser: e.target.value || undefined })} />
        <p className="text-xs text-muted-foreground">Optional. {defaults.reasoningParser
          ? `Leave empty to use ${defaults.reasoningParser}.`
          : 'Leave empty to keep the runtime default.'}</p>
      </div>
    </div>}
    {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
  </div>
}
