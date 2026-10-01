import type { ProviderIntentStatus } from '@airunway/shared'
import { InfoHint } from '@/components/ui/InfoHint'
import { getEngineDisplayName } from '@/lib/deploymentDisplay'

const phases: Record<string, { label: string; description: string }> = {
  Pending: { label: 'Waiting to start', description: 'Dynamo is waiting to begin configuration.' },
  Profiling: { label: 'Finding a configuration', description: 'Dynamo is evaluating serving options against your GPU budget and performance targets.' },
  Ready: { label: 'Configuration selected', description: 'Dynamo has selected a configuration and is preparing to start the model.' },
  Deploying: { label: 'Starting model servers', description: 'Dynamo is applying the selected configuration. The model may not be ready for requests yet.' },
  Deployed: { label: 'Configuration applied', description: 'Check the ready serving copies and serving address below before sending requests.' },
  Failed: { label: 'Configuration failed', description: 'Review the details below before retrying or changing the settings.' },
}
const profilingSteps: Record<string, string> = {
  Initializing: 'Preparing the configuration search',
  SweepingPrefill: 'Evaluating prompt processing',
  SweepingDecode: 'Evaluating response generation',
  SelectingConfig: 'Choosing a serving configuration',
  BuildingCurves: 'Comparing performance results',
  GeneratingDGD: 'Preparing model servers',
  Done: 'Configuration search complete',
}
const hardwareSources = { provided: 'Provided by you', discovered: 'Detected from available hardware', mixed: 'Provided and detected values' }
const workerRoles: Record<string, string> = { prefill: 'Prompt processing', decode: 'Response generation', aggregated: 'Prompt processing and response generation' }

export function DynamoConfigurationSummary({ progress }: { progress?: ProviderIntentStatus }) {
  const phase = phases[progress?.phase || 'Pending']
  const hardware = progress?.hardware
  const plan = progress?.plan
  return <div className="space-y-4 text-sm">
    <div>
      <p className="text-muted-foreground">Configuration progress</p>
      <p>{phase?.label || progress?.phase}</p>
      {phase && <p className="text-muted-foreground">{phase.description}</p>}
      {progress?.profilingPhase && <p>Current step: <span>{profilingSteps[progress.profilingPhase] || progress.profilingPhase}</span></p>}
    </div>
    {hardware && <section aria-label="Configuration hardware" className="space-y-2">
      <h3 className="flex items-center gap-2 font-medium">Hardware used for configuration
        <InfoHint text="Hardware discovery can include inferred values. These details describe the hardware used for configuration, not a measured model benchmark." />
      </h3>
      <p className="text-muted-foreground">{hardware.source ? hardwareSources[hardware.source] : 'Source not reported'}</p>
      <dl className="grid gap-2 sm:grid-cols-3">
        {hardware.gpuSku !== undefined && <div><dt className="text-muted-foreground">GPU model</dt><dd>{hardware.gpuSku}</dd></div>}
        {hardware.vramMb !== undefined && <div><dt className="text-muted-foreground">Memory per GPU</dt><dd>{hardware.vramMb.toLocaleString()} MiB</dd></div>}
        {hardware.numGpusPerNode !== undefined && <div><dt className="text-muted-foreground">GPUs per machine</dt><dd>{hardware.numGpusPerNode}</dd></div>}
      </dl>
    </section>}
    {plan && <section aria-label="Selected serving configuration" className="space-y-2">
      <h3 className="font-medium">Selected serving configuration</h3>
      <p className="text-muted-foreground">{plan.source === 'selectedConfig' ? 'From the configuration search' : 'From the serving configuration'}</p>
      <dl className="grid gap-2 sm:grid-cols-2">
        {plan.engine && <div><dt className="text-muted-foreground">Model server</dt><dd>{getEngineDisplayName(plan.engine)}</dd></div>}
        {plan.servingMode && <div><dt className="text-muted-foreground">Serving layout</dt><dd>{plan.servingMode === 'aggregated'
          ? 'Prompt and response processing together'
          : plan.servingMode === 'disaggregated' ? 'Separate prompt and response processing' : plan.servingMode}</dd></div>}
      </dl>
      {!!plan.workers?.length && <div className="overflow-x-auto"><table className="w-full text-left">
        <caption className="sr-only">Selected workers</caption>
        <thead><tr>
          <th className="p-2">Worker</th><th className="p-2">Role</th><th className="p-2">Copies</th><th className="p-2">GPUs per copy</th>
          <th className="p-2">Parallel processing <InfoHint text="Tensor parallelism splits each model layer across GPUs. Pipeline parallelism splits groups of layers across GPUs. These values describe the selected layout, not measured performance." /></th>
        </tr></thead>
        <tbody>{plan.workers.map((worker, index) => <tr key={`${worker.name}-${index}`} className="border-t">
          <td className="p-2 break-all">{worker.name}</td>
          <td className="p-2">{worker.role ? workerRoles[worker.role] || worker.role : 'Not reported'}</td>
          <td className="p-2">{worker.replicas ?? 'Not reported'}</td>
          <td className="p-2">{worker.gpusPerReplica ?? 'Not reported'}</td>
          <td className="p-2">
            {worker.tensorParallelism !== undefined && <div>Tensor: {worker.tensorParallelism}</div>}
            {worker.pipelineParallelism !== undefined && <div>Pipeline: {worker.pipelineParallelism}</div>}
            {worker.tensorParallelism === undefined && worker.pipelineParallelism === undefined && 'Not reported'}
          </td>
        </tr>)}</tbody>
      </table></div>}
    </section>}
    {progress?.diagnostic && <p role="status" className="whitespace-pre-wrap break-words">{progress.diagnostic}</p>}
  </div>
}
