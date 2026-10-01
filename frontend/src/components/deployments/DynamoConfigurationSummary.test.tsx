import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { DynamoConfigurationSummary } from './DynamoConfigurationSummary'

describe('Dynamo configuration summary', () => {
  it.each([
    ['provided', 'Provided by you'], ['discovered', 'Detected from available hardware'], ['mixed', 'Provided and detected values'],
  ] as const)('labels %s hardware without substituting requested values', (source, label) => {
    render(<DynamoConfigurationSummary progress={{ hardware: { source, gpuSku: 'H100', vramMb: 81920, numGpusPerNode: 8 } }} />)
    expect(screen.getByText(label)).toBeInTheDocument()
    expect(screen.getByText('H100')).toBeInTheDocument()
    expect(screen.getByText('81,920 MiB')).toBeInTheDocument()
    expect(screen.getByText('GPUs per machine').nextSibling).toHaveTextContent('8')
    if (source === 'provided') expect(screen.queryByText(/detected/i)).not.toBeInTheDocument()
  })
  it.each(['selectedConfig', 'workload'] as const)('shows %s layout and actual workers without inventing missing metrics or totals', source => {
    render(<DynamoConfigurationSummary progress={{ phase: 'Profiling', profilingPhase: 'SweepingDecode',
      plan: { source, engine: 'vllm', servingMode: 'disaggregated', workers: [
        { name: 'prefill-a', role: 'prefill', replicas: 2, gpusPerReplica: 4, tensorParallelism: 4, pipelineParallelism: 1 },
        { name: 'decode-b', role: 'decode', replicas: 0 },
      ] }, diagnostic: 'Only the first 32 workers are included.' }} />)
    expect(screen.getByText('Finding a configuration')).toBeInTheDocument()
    expect(screen.getByText('Evaluating response generation')).toBeInTheDocument()
    expect(screen.getByText(source === 'selectedConfig' ? 'From the configuration search' : 'From the serving configuration')).toBeInTheDocument()
    expect(screen.getByText('Separate prompt and response processing')).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('Only the first 32 workers are included.')
    const rows = screen.getAllByRole('row')
    expect(within(rows[1]).getByText('Tensor: 4')).toBeInTheDocument()
    expect(within(rows[2]).getByText('0')).toBeInTheDocument()
    expect(within(rows[2]).getAllByText('Not reported')).toHaveLength(2)
    expect(screen.queryByText(/total GPUs|ETA|elapsed|%|minutes|complete totals/i)).not.toBeInTheDocument()
  })
  it('does not manufacture hardware, layout, timing or workers when status is absent', () => {
    render(<DynamoConfigurationSummary />)
    expect(screen.getByText('Waiting to start')).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    expect(screen.queryByRole('region')).not.toBeInTheDocument()
  })
})
