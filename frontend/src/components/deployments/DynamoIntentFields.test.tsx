import { useState } from 'react'
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { defaultDynamoIntent, type DynamoIntent } from '@airunway/shared'
import { DynamoIntentFields } from './DynamoIntentFields'

const betaOverrides: DynamoIntent['overrides'] = {
  profilingJob: { activeDeadlineSeconds: 1800 },
  dgd: {
    apiVersion: 'nvidia.com/v1beta1', kind: 'DynamoGraphDeployment',
    spec: { components: [{ name: 'VllmDecodeWorker', podTemplate: { spec: { containers: [{
      name: 'main', $patch: { args: 'append' },
      args: ['--dyn-tool-call-parser', 'hermes', '--dyn-reasoning-parser', 'qwen3'],
    }] } } }] },
  },
}

function Editor({ initial = defaultDynamoIntent(), onChange = vi.fn(), onValidityChange = vi.fn() }: {
  initial?: DynamoIntent; onChange?: (intent: DynamoIntent) => void; onValidityChange?: (valid: boolean) => void
}) {
  const [value, setValue] = useState(initial)
  return <form><DynamoIntentFields value={value} onValidityChange={onValidityChange} onChange={next => {
    setValue(next)
    onChange(next)
  }} /></form>
}

const editor = () => screen.getByRole('textbox', { name: /Advanced configuration/ })

describe('Dynamo advanced configuration', () => {
  it('preserves beta append directives and JSON formatting through ordinary intent edits', () => {
    const onChange = vi.fn()
    render(<Editor onChange={onChange} />)
    const text = JSON.stringify(betaOverrides, null, 4)
    fireEvent.change(editor(), { target: { value: text } })
    fireEvent.change(screen.getByRole('spinbutton', { name: /GPU budget/ }), { target: { value: '2' } })
    fireEvent.change(screen.getByLabelText('Expected traffic'), { target: { value: 'concurrency' } })
    fireEvent.change(screen.getByLabelText('Latency targets'), { target: { value: 'total' } })
    expect(editor()).toHaveValue(text)
    expect(onChange.mock.lastCall?.[0]).toMatchObject({ hardware: { totalGpus: 2 }, overrides: betaOverrides })
    expect(editor()).toBeValid()
  })

  it('shows existing overrides and removes them entirely when cleared', () => {
    const onChange = vi.fn()
    render(<Editor initial={{ ...defaultDynamoIntent(), overrides: betaOverrides }} onChange={onChange} />)
    expect(JSON.parse((editor() as HTMLTextAreaElement).value)).toEqual(betaOverrides)
    fireEvent.change(editor(), { target: { value: '  ' } })
    expect(onChange.mock.lastCall?.[0]).not.toHaveProperty('overrides')
    expect(editor()).toBeValid()
  })

  it.each([
    ['{ broken', 'Enter valid JSON'],
    ['{"profilingJob":{"activeDeadlineSeconds":1e400}}', 'finite'],
    ['{"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"DynamoGraphDeployment","spec":{"components":[{"limit":-1e400}]}}}', 'finite'],
  ])('keeps invalid text across ordinary edits, blocks native submission and recovers: %s', (invalidText, error) => {
    const onChange = vi.fn()
    const onValidityChange = vi.fn()
    render(<Editor initial={{ ...defaultDynamoIntent(), overrides: betaOverrides }} onChange={onChange} onValidityChange={onValidityChange} />)
    fireEvent.change(editor(), { target: { value: invalidText } })
    expect(onChange).not.toHaveBeenCalled()
    expect(onValidityChange).toHaveBeenLastCalledWith(false)
    expect(screen.getByRole('alert')).toHaveTextContent(error)
    expect(editor()).toHaveAttribute('aria-invalid', 'true')
    expect(editor().closest('form')!.checkValidity()).toBe(false)
    fireEvent.change(screen.getByRole('spinbutton', { name: /Input tokens/ }), { target: { value: '2048' } })
    expect(editor()).toHaveValue(invalidText)
    expect(editor()).toBeInvalid()
    fireEvent.change(editor(), { target: { value: '{ "profilingJob": { "activeDeadlineSeconds": 900 } }' } })
    expect(onChange.mock.lastCall?.[0]).toMatchObject({ overrides: { profilingJob: { activeDeadlineSeconds: 900 } } })
    expect(onValidityChange).toHaveBeenLastCalledWith(true)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(editor()).toBeValid()
  })

  it.each([
    'null', '[]', 'true', '1', '"text"', '{"spec":{}}',
    '{"profilingJob":null}', '{"profilingJob":[]}', '{"profilingJob":1}',
    '{"dgd":null}', '{"dgd":[]}', '{"dgd":{}}',
    '{"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"DynamoGraphDeployment","spec":[]}}',
    '{"dgd":{"apiVersion":["nvidia.com/v1beta1"],"kind":"DynamoGraphDeployment","spec":{}}}',
    '{"dgd":{"apiVersion":"nvidia.com/v1","kind":"DynamoGraphDeployment","spec":{}}}',
    '{"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"Other","spec":{}}}',
    '{"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"DynamoGraphDeployment","spec":{},"metadata":[]}}',
  ])('rejects malformed root or child objects: %s', text => {
    const onChange = vi.fn()
    render(<Editor onChange={onChange} />)
    fireEvent.change(editor(), { target: { value: text } })
    expect(screen.getByRole('alert')).toBeInTheDocument()
    expect(editor()).toBeInvalid()
    expect(onChange).not.toHaveBeenCalled()
  })

  it('accepts the alpha shape without translating or interpreting its fields', () => {
    const onChange = vi.fn()
    const overrides = { dgd: { apiVersion: 'nvidia.com/v1alpha1', kind: 'DynamoGraphDeployment',
      metadata: { labels: { team: 'inference' } },
      spec: { services: { VllmDecodeWorker: { extraPodSpec: { mainContainer: { args: ['--dyn-tool-call-parser', 'hermes'] } } } } },
    } }
    render(<Editor onChange={onChange} />)
    fireEvent.change(editor(), { target: { value: JSON.stringify(overrides) } })
    expect(editor()).toBeValid()
    expect(onChange.mock.lastCall?.[0].overrides).toEqual(overrides)
  })

  it('resets the editor when a different saved configuration is supplied', () => {
    const onValidityChange = vi.fn()
    const view = render(<DynamoIntentFields value={defaultDynamoIntent()} onChange={vi.fn()} onValidityChange={onValidityChange} />)
    fireEvent.change(editor(), { target: { value: '{broken' } })
    view.rerender(<DynamoIntentFields value={{ ...defaultDynamoIntent(), overrides: betaOverrides }} onChange={vi.fn()} onValidityChange={onValidityChange} />)
    expect(JSON.parse((editor() as HTMLTextAreaElement).value)).toEqual(betaOverrides)
    expect(editor()).toBeValid()
    expect(onValidityChange).toHaveBeenLastCalledWith(true)
  })
})
