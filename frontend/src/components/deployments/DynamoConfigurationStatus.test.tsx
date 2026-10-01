import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defaultDynamoIntent, type DeploymentStatus } from '@airunway/shared'
import { createWrapper } from '@/test/test-utils'
import { DynamoConfigurationStatus } from './DynamoConfigurationStatus'

const reconfigure = vi.hoisted(() => vi.fn())
vi.mock('@/lib/api', async importOriginal => ({ ...await importOriginal<typeof import('@/lib/api')>(), deploymentsApi: { reconfigure } }))
const deployment: DeploymentStatus = {
  name: 'auto', namespace: 'models', resourceVersion: '42', modelId: 'Qwen/Qwen3-0.6B', engine: 'vllm',
  provider: 'dynamo', configurationMode: 'automatic', intent: defaultDynamoIntent(), mode: 'aggregated', phase: 'Failed',
  createdAt: '2026-09-25', replicas: { desired: 0, ready: 0, available: 0 }, pods: [],
  providerStatus: { intent: { phase: 'Failed', profilingPhase: 'Searching' }, workloadRef: { name: 'generated-custom', namespace: 'serving' } },
}

describe('Dynamo configuration status', () => {
  beforeEach(() => { reconfigure.mockReset() })
  it('prefers the stable gateway over a generated frontend address', () => {
    render(<DynamoConfigurationStatus deployment={{ ...deployment, frontendService: 'request-hash-dgd-frontend', gateway: { endpoint: 'https://models.example.test', modelName: 'Qwen/Qwen3-0.6B' } }} />, { wrapper: createWrapper() })
    expect(screen.getByText('Gateway address')).toBeInTheDocument()
    expect(screen.getByText('https://models.example.test')).toBeInTheDocument()
    expect(screen.getByText('Model name for requests')).toBeInTheDocument()
    expect(screen.queryByText(/request-hash-dgd-frontend/)).not.toBeInTheDocument()
  })

  it('preserves saved tool settings and the opened revision, sends null to clear and false to disable', async () => {
    reconfigure.mockResolvedValue({ attempt: 'new' })
    const saved = { ...deployment, toolCalling: true, toolCallParser: 'hermes', reasoningParser: 'basic' }
    const view = render(<DynamoConfigurationStatus deployment={saved} />, { wrapper: createWrapper() })
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    expect(screen.getByRole('checkbox', { name: 'Enable tool calling' })).toBeChecked()
    expect(screen.getByLabelText('Tool parser')).toHaveValue('hermes')
    expect(screen.getByLabelText('Reasoning parser')).toHaveValue('basic')
    view.rerender(<DynamoConfigurationStatus deployment={{ ...saved, resourceVersion: '43', toolCallParser: 'changed_elsewhere' }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Confirm reconfiguration' }))
    await waitFor(() => expect(reconfigure).toHaveBeenCalledTimes(1))
    expect(reconfigure.mock.calls[0][2].resourceVersion).toBe('42')
    expect(reconfigure.mock.calls[0][2]).not.toHaveProperty('toolCallParser')
    expect(reconfigure.mock.calls[0][2]).not.toHaveProperty('toolCalling')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    fireEvent.change(screen.getByLabelText('Tool parser'), { target: { value: '' } })
    fireEvent.change(screen.getByLabelText('Reasoning parser'), { target: { value: '' } })
    fireEvent.click(screen.getByRole('button', { name: 'Confirm reconfiguration' }))
    await waitFor(() => expect(reconfigure).toHaveBeenCalledTimes(2))
    expect(reconfigure.mock.calls[1][2]).toMatchObject({ resourceVersion: '43', toolCallParser: null, reasoningParser: null })
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    fireEvent.click(screen.getByRole('checkbox', { name: 'Enable tool calling' }))
    expect(screen.queryByLabelText('Tool parser')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Confirm reconfiguration' }))
    await waitFor(() => expect(reconfigure).toHaveBeenCalledTimes(3))
    expect(reconfigure.mock.calls[2][2]).toMatchObject({ toolCalling: false })
    expect(reconfigure.mock.calls[2][2]).not.toHaveProperty('toolCallParser')
    expect(reconfigure.mock.calls[2][2]).not.toHaveProperty('reasoningParser')
  })

  it('validates new tool settings against the edited model and conflicting native overrides', async () => {
    reconfigure.mockResolvedValue({ attempt: 'new' })
    render(<DynamoConfigurationStatus deployment={deployment} />, { wrapper: createWrapper() })
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    fireEvent.click(screen.getByRole('checkbox', { name: 'Enable tool calling' }))
    fireEvent.change(screen.getByLabelText('Model ID'), { target: { value: 'acme/model' } })
    const submit = screen.getByRole('button', { name: 'Confirm reconfiguration' })
    expect(submit).toBeDisabled()
    fireEvent.submit(submit.closest('form')!)
    expect(reconfigure).not.toHaveBeenCalled()
    fireEvent.change(screen.getByLabelText('Tool parser'), { target: { value: 'auto' } })
    expect(submit).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Tool parser'), { target: { value: 'hermes' } })
    expect(submit).toBeEnabled()
    fireEvent.change(screen.getByRole('textbox', { name: /Advanced configuration/ }), { target: { value: '{"profilingJob":{"args":["--dyn-chat-processor=custom"]}}' } })
    expect(submit).toBeDisabled()
    expect(screen.getByRole('alert')).toHaveTextContent('conflicts')
    fireEvent.change(screen.getByRole('textbox', { name: /Advanced configuration/ }), { target: { value: '' } })
    fireEvent.click(submit)
    await waitFor(() => expect(reconfigure).toHaveBeenCalledTimes(1))
    expect(reconfigure.mock.calls[0][2]).toMatchObject({ toolCalling: true, toolCallParser: 'hermes', modelId: 'acme/model' })
  })

  it('shows progress, actual serving identity and no performance guarantee', () => {
    render(<DynamoConfigurationStatus deployment={deployment} />, { wrapper: createWrapper() })
    expect(screen.getByText('Searching')).toBeInTheDocument()
    expect(screen.getByText('serving/generated-custom')).toBeInTheDocument()
    expect(screen.getByText(/does not confirm that these targets were met/)).toBeInTheDocument()
    expect(screen.queryByRole('spinbutton')).not.toBeInTheDocument()
  })
  it('requires confirmation before retry and submits only the displayed revision', async () => {
    reconfigure.mockResolvedValue({ attempt: 'new' })
    render(<DynamoConfigurationStatus deployment={deployment} />, { wrapper: createWrapper() })
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(screen.getByText(/requests may be interrupted/)).toBeInTheDocument()
    expect(reconfigure).not.toHaveBeenCalled()
    expect(screen.queryByRole('spinbutton')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Confirm retry' }))
    await waitFor(() => expect(reconfigure).toHaveBeenCalledWith('auto', 'models', { resourceVersion: '42' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })
  it('switches exclusive traffic and latency targets without switching modes when a field is cleared', async () => {
    reconfigure.mockResolvedValue({ attempt: 'new' })
    render(<DynamoConfigurationStatus deployment={deployment} />, { wrapper: createWrapper() })
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    fireEvent.change(screen.getByLabelText('Expected traffic'), { target: { value: 'concurrency' } })
    fireEvent.change(screen.getByRole('spinbutton', { name: /Simultaneous requests/ }), { target: { value: '' } })
    expect(screen.getByLabelText('Expected traffic')).toHaveValue('concurrency')
    fireEvent.change(screen.getByRole('spinbutton', { name: /Simultaneous requests/ }), { target: { value: '8' } })
    fireEvent.change(screen.getByLabelText('Latency targets'), { target: { value: 'total' } })
    fireEvent.change(screen.getByRole('spinbutton', { name: /Total response target/ }), { target: { value: '' } })
    expect(screen.getByLabelText('Latency targets')).toHaveValue('total')
    fireEvent.change(screen.getByRole('spinbutton', { name: /Total response target/ }), { target: { value: '5000' } })
    fireEvent.click(screen.getByRole('button', { name: 'Confirm reconfiguration' }))
    await waitFor(() => expect(reconfigure).toHaveBeenCalledTimes(1))
    expect(reconfigure.mock.calls[0][2].intent.workload).toEqual({ isl: 1024, osl: 256, concurrency: 8 })
    expect(reconfigure.mock.calls[0][2].intent.sla).toEqual({ e2eLatency: 5000 })
  })

  it('preserves saved advanced overrides through ordinary reconfiguration edits and refreshes', async () => {
    reconfigure.mockResolvedValue({ attempt: 'new' })
    const overrides = {
      profilingJob: { activeDeadlineSeconds: 1800 },
      dgd: { apiVersion: 'nvidia.com/v1beta1' as const, kind: 'DynamoGraphDeployment' as const,
        spec: { components: [{ name: 'VllmDecodeWorker', podTemplate: { spec: { containers: [{
          name: 'main', $patch: { args: 'append' }, args: ['--dyn-tool-call-parser', 'hermes', '--dyn-reasoning-parser', 'qwen3'],
        }] } } }] },
      },
    }
    const saved = { ...deployment, intent: { ...defaultDynamoIntent(), overrides } }
    const view = render(<DynamoConfigurationStatus deployment={saved} />, { wrapper: createWrapper() })
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    const editor = screen.getByRole('textbox', { name: /Advanced configuration/ })
    expect(JSON.parse((editor as HTMLTextAreaElement).value)).toEqual(overrides)
    fireEvent.change(screen.getByRole('spinbutton', { name: /GPU budget/ }), { target: { value: '2' } })
    fireEvent.change(screen.getByLabelText('Model ID'), { target: { value: 'Qwen/Qwen3-8B' } })
    view.rerender(<DynamoConfigurationStatus deployment={{ ...saved, resourceVersion: '43', intent: defaultDynamoIntent() }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Confirm reconfiguration' }))
    await waitFor(() => expect(reconfigure).toHaveBeenCalledTimes(1))
    expect(reconfigure.mock.calls[0][2]).toMatchObject({ resourceVersion: '42', modelId: 'Qwen/Qwen3-8B',
      intent: { hardware: { totalGpus: 2 }, overrides },
    })
  })

  it.each(['{', '{"profilingJob":{"activeDeadlineSeconds":1e400}}'])('blocks invalid reconfiguration without submitting old overrides, and clears them: %s', async invalidText => {
    reconfigure.mockResolvedValue({ attempt: 'new' })
    render(<DynamoConfigurationStatus deployment={{ ...deployment, intent: { ...defaultDynamoIntent(), overrides: { profilingJob: { activeDeadlineSeconds: 1800 } } } }} />, { wrapper: createWrapper() })
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    const editor = screen.getByRole('textbox', { name: /Advanced configuration/ })
    const submit = screen.getByRole('button', { name: 'Confirm reconfiguration' })
    fireEvent.change(editor, { target: { value: invalidText } })
    fireEvent.change(screen.getByRole('spinbutton', { name: /GPU budget/ }), { target: { value: '2' } })
    expect(editor).toHaveValue(invalidText)
    expect(submit).toBeDisabled()
    expect(submit.closest('form')!.checkValidity()).toBe(false)
    fireEvent.submit(submit.closest('form')!)
    expect(reconfigure).not.toHaveBeenCalled()
    fireEvent.change(editor, { target: { value: '' } })
    expect(submit).toBeEnabled()
    fireEvent.click(submit)
    await waitFor(() => expect(reconfigure).toHaveBeenCalledTimes(1))
    expect(reconfigure.mock.calls[0][2].intent).not.toHaveProperty('overrides')
    expect(reconfigure.mock.calls[0][2].intent.hardware.totalGpus).toBe(2)
  })

  it('discards invalid draft text on cancel without blocking retry or the next reconfiguration', async () => {
    reconfigure.mockResolvedValue({ attempt: 'new' })
    render(<DynamoConfigurationStatus deployment={deployment} />, { wrapper: createWrapper() })
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    fireEvent.change(screen.getByRole('textbox', { name: /Advanced configuration/ }), { target: { value: '{' } })
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    expect(screen.getByRole('textbox', { name: /Advanced configuration/ })).toHaveValue('')
    expect(screen.getByRole('button', { name: 'Confirm reconfiguration' })).toBeEnabled()
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    fireEvent.click(screen.getByRole('button', { name: 'Confirm retry' }))
    await waitFor(() => expect(reconfigure).toHaveBeenCalledWith('auto', 'models', { resourceVersion: '42' }))
  })

  it('keeps the opened revision when live status refreshes and reports a conflict without auto-retry', async () => {
    reconfigure.mockRejectedValue(new Error('Deployment changed. Refresh before reconfiguring.'))
    const view = render(<DynamoConfigurationStatus deployment={deployment} />, { wrapper: createWrapper() })
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    fireEvent.change(screen.getByRole('spinbutton', { name: /GPU budget/ }), { target: { value: '4' } })
    fireEvent.change(screen.getByLabelText('Model ID'), { target: { value: 'Qwen/Qwen3-8B' } })
    view.rerender(<DynamoConfigurationStatus deployment={{ ...deployment, resourceVersion: '43' }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Confirm reconfiguration' }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Deployment changed'))
    expect(reconfigure).toHaveBeenCalledTimes(1)
    expect(reconfigure.mock.calls[0][2]).toMatchObject({ resourceVersion: '42', modelId: 'Qwen/Qwen3-8B', intent: { hardware: { totalGpus: 4 } } })
  })
})
