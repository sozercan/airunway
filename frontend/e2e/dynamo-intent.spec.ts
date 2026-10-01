import { mkdir, writeFile } from 'node:fs/promises'
import { test, expect } from './fixtures'
import { defaultDynamoIntent, toModelDeploymentSpec, type DeploymentConfig, type DeploymentStatus, type DynamoReconfigureRequest, type DynamoIntent } from '@airunway/shared'

// Optional local-browser override avoids downloading a second Chromium build for UI proof.
if (process.env.DYNAMO_UI_CHROMIUM_PATH) test.use({ launchOptions: { executablePath: process.env.DYNAMO_UI_CHROMIUM_PATH } })
const proofDir = process.env.DYNAMO_UI_PROOF_DIR
test.beforeEach(async () => { if (proofDir) await mkdir(proofDir, { recursive: true }) })
const runtime = { id: 'dynamo', name: 'Dynamo', installed: true, healthy: true, version: '1.5.0',
  capabilities: { engines: ['vllm', 'sglang', 'trtllm'], modes: ['aggregated', 'disaggregated'], modelSources: ['huggingface'], routerModes: ['default'], features: {} } }

const advancedOverrides: DynamoIntent['overrides'] = {
  profilingJob: { activeDeadlineSeconds: 1800 },
  dgd: {
    apiVersion: 'nvidia.com/v1beta1', kind: 'DynamoGraphDeployment',
    spec: { components: [{ name: 'VllmDecodeWorker', podTemplate: { spec: { containers: [{
      name: 'main', $patch: { args: 'append' },
      args: ['--dyn-tool-call-parser', 'hermes', '--dyn-reasoning-parser', 'qwen3'],
    }] } } }] },
  },
}

test('automatic creation validates advanced overrides and emits intent without manual sizing', async ({ mockedPage: page }) => {
  let submitted: DeploymentConfig | undefined
  const previews: DeploymentConfig[] = []
  await page.route(/\/api\/(?:installation\/)?runtimes\/status$/, route => route.fulfill({ json: { runtimes: [runtime] } }))
  await page.route(/\/api\/deployments\/-\/pvcs/, route => route.fulfill({ json: { pvcs: [] } }))
  await page.route(/\/api\/deployments\/preview$/, route => {
    const config = route.request().postDataJSON() as DeploymentConfig
    previews.push(config)
    return route.fulfill({ json: { resources: [{ kind: 'ModelDeployment', apiVersion: 'airunway.ai/v1alpha1', name: config.name,
      manifest: { apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment', metadata: { name: config.name, namespace: config.namespace }, spec: toModelDeploymentSpec(config) } }], primaryResource: { kind: 'ModelDeployment', apiVersion: 'airunway.ai/v1alpha1' } } })
  })
  await page.route(/\/api\/deployments\/?$/, route => {
    if (route.request().method() !== 'POST') return route.fallback()
    submitted = route.request().postDataJSON()
    return route.fulfill({ status: 201, json: { message: 'Created', name: submitted!.name, namespace: submitted!.namespace } })
  })
  await page.goto('/deploy/Qwen%2FQwen3-0.6B')
  await page.getByRole('radio', { name: /Automatic configuration/ }).click()
  const editor = page.getByRole('textbox', { name: /Advanced configuration/ })
  const submit = page.getByRole('button', { name: /Deploy Model/ })
  await editor.fill(JSON.stringify(advancedOverrides, null, 2))
  await Promise.all([
    page.waitForResponse(/\/api\/deployments\/preview$/),
    page.getByText('Manifest Preview', { exact: true }).click(),
  ])
  expect(previews.at(-1)?.providerOverrides).toMatchObject({ intent: { overrides: advancedOverrides } })
  const previewCount = previews.length
  await editor.fill('{"profilingJob":')
  await page.getByRole('spinbutton', { name: /GPU budget/ }).fill('2')
  await expect(editor).toHaveValue('{"profilingJob":')
  await expect(editor).toHaveAttribute('aria-invalid', 'true')
  await expect(page.getByRole('alert')).toContainText('Enter valid JSON')
  await expect(submit).toBeDisabled()
  await expect(page.getByText('Manifest Preview', { exact: true })).toHaveCount(0)
  await expect(page.getByText('Fix advanced configuration to preview or deploy.')).toBeVisible()
  expect(await editor.evaluate(el => (el as HTMLTextAreaElement).form!.checkValidity())).toBe(false)
  // Exercise both the native keyboard-submit path and the submit-handler guard.
  await editor.press('Control+Enter')
  await editor.evaluate(el => (el as HTMLTextAreaElement).form!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })))
  expect(submitted).toBeUndefined()
  expect(previews).toHaveLength(previewCount)
  if (proofDir) {
    await mkdir(proofDir, { recursive: true })
    await page.screenshot({ path: `${proofDir}/automatic-invalid-overrides.png`, fullPage: true })
  }
  const correctedOverrides = { ...advancedOverrides, profilingJob: { activeDeadlineSeconds: 900 } }
  await editor.fill(JSON.stringify(correctedOverrides, null, 2))
  await expect(submit).toBeEnabled()
  await Promise.all([
    page.waitForResponse(/\/api\/deployments\/preview$/),
    page.getByText('Manifest Preview', { exact: true }).click(),
  ])
  expect(previews.at(-1)?.providerOverrides).toMatchObject({ intent: { overrides: correctedOverrides } })
  await expect(page.getByText('Deployment Options', { exact: true })).toHaveCount(0)
  await expect(page.getByText('Deployment Mode', { exact: true })).toHaveCount(0)
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  if (proofDir) {
    await mkdir(proofDir, { recursive: true })
    await page.screenshot({ path: `${proofDir}/automatic-create.png`, fullPage: true })
  }
  await page.getByRole('button', { name: /Deploy Model/ }).click()
  await expect.poll(() => submitted).toBeDefined()
  expect(submitted!.modelId).toBe('Qwen/Qwen3-0.6B')
  expect(submitted!.providerOverrides).toMatchObject({ deploymentMode: 'intent', intent: { hardware: { totalGpus: 2 }, searchStrategy: 'rapid', overrides: correctedOverrides } })
  for (const field of ['resources', 'scaling', 'replicas', 'mode', 'prefillReplicas', 'decodeReplicas', 'prefillGpus', 'decodeGpus']) expect(submitted).not.toHaveProperty(field)
  const spec = toModelDeploymentSpec(submitted!)
  expect(spec.provider?.overrides).toMatchObject({ intent: { overrides: correctedOverrides } })
  expect(spec.provider?.overrides).not.toHaveProperty('spec')
  expect(spec).not.toHaveProperty('resources'); expect(spec).not.toHaveProperty('scaling'); expect(spec).not.toHaveProperty('serving')
  if (proofDir) await writeFile(`${proofDir}/automatic-create-payload.json`, JSON.stringify({ submitted, spec }, null, 2))
})

test('failed automatic deployment confirms retry and validates advanced reconfiguration', async ({ mockedPage: page }) => {
  const deployment = { name: 'qwen-auto', namespace: 'models', resourceVersion: '42', modelId: 'Qwen/Qwen3-0.6B', engine: 'vllm', mode: 'aggregated', phase: 'Failed', provider: 'dynamo', configurationMode: 'automatic', intent: { ...defaultDynamoIntent(), overrides: advancedOverrides },
    replicas: { desired: 0, ready: 0, available: 0 }, pods: [], createdAt: '2026-09-25T10:00:00Z',
    providerStatus: { intent: { phase: 'Failed', profilingPhase: 'Searching', attempt: 'previous' } }, message: 'No configuration found for the requested hardware.' }
  const writes: unknown[] = []
  await page.route(/\/api\/deployments\/qwen-auto(?:\?.*)?$/, route => route.fulfill({ json: deployment }))
  await page.route(/\/api\/deployments\/models\/qwen-auto\/reconfigure$/, route => {
    writes.push(route.request().postDataJSON())
    return route.fulfill({ json: { message: 'Configuration requested', attempt: `attempt-${writes.length}` } })
  })
  await page.goto('/deployments/qwen-auto?namespace=models')
  await expect(page.getByText('Searching', { exact: true })).toBeVisible()
  await expect(page.getByText('Automatic', { exact: true })).toBeVisible()
  await expect(page.getByRole('spinbutton')).toHaveCount(0)
  await page.getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(page.getByRole('dialog')).toContainText('requests may be interrupted')
  expect(writes).toHaveLength(0)
  if (proofDir) {
    await mkdir(proofDir, { recursive: true })
    await page.screenshot({ path: `${proofDir}/retry-confirmation.png`, fullPage: true })
  }
  await page.getByRole('button', { name: 'Confirm retry' }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(writes).toEqual([{ resourceVersion: '42' }])
  await page.getByRole('button', { name: 'Reconfigure', exact: true }).click()
  const editor = page.getByRole('textbox', { name: /Advanced configuration/ })
  const submit = page.getByRole('button', { name: 'Confirm reconfiguration' })
  expect(JSON.parse(await editor.inputValue())).toEqual(advancedOverrides)
  await editor.fill('{"dgd":')
  await page.getByRole('spinbutton', { name: /GPU budget/ }).fill('4')
  await expect(editor).toHaveValue('{"dgd":')
  await expect(page.getByRole('dialog').getByRole('alert')).toContainText('Enter valid JSON')
  await expect(submit).toBeDisabled()
  expect(await editor.evaluate(el => (el as HTMLTextAreaElement).form!.checkValidity())).toBe(false)
  await editor.evaluate(el => {
    const form = (el as HTMLTextAreaElement).form!
    form.requestSubmit()
    form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
  })
  expect(writes).toEqual([{ resourceVersion: '42' }])
  if (proofDir) await page.screenshot({ path: `${proofDir}/reconfigure-invalid-overrides.png`, fullPage: true })
  const correctedOverrides = { ...advancedOverrides, profilingJob: { activeDeadlineSeconds: 900 } }
  await editor.fill(JSON.stringify(correctedOverrides, null, 2))
  await expect(submit).toBeEnabled()
  await page.getByLabel('Model ID').fill('Qwen/Qwen3-8B')
  const dialogBounds = await page.getByRole('dialog').boundingBox()
  expect(dialogBounds!.x).toBeGreaterThanOrEqual(0)
  expect(dialogBounds!.width).toBeLessThanOrEqual(page.viewportSize()!.width)
  expect(dialogBounds!.height).toBeLessThanOrEqual(page.viewportSize()!.height)
  if (proofDir) await page.screenshot({ path: `${proofDir}/reconfigure-dialog.png`, fullPage: true })
  await page.getByRole('button', { name: 'Confirm reconfiguration' }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(writes[1]).toMatchObject({ resourceVersion: '42', modelId: 'Qwen/Qwen3-8B', intent: { hardware: { totalGpus: 4 }, overrides: correctedOverrides } })
  if (proofDir) await writeFile(`${proofDir}/reconfigure-requests.json`, JSON.stringify(writes, null, 2))
})

test('gateway-only Dynamo deployment exposes working chat without a frontend service', async ({ mockedPage: page }) => {
  const deployment = { name: 'qwen-epp', namespace: 'models', resourceVersion: '7', modelId: 'Qwen/Qwen3-0.6B',
    engine: 'vllm', mode: 'aggregated', phase: 'Running', provider: 'dynamo', configurationMode: 'manual',
    replicas: { desired: 1, ready: 1, available: 1 }, pods: [], createdAt: '2026-09-25T10:00:00Z',
    gateway: { endpoint: 'http://gateway.invalid', modelName: 'Qwen/Qwen3-0.6B' } }
  let submitted: unknown
  await page.route(/\/api\/deployments\/qwen-epp(?:\?.*)?$/, route => route.fulfill({ json: deployment }))
  await page.route(/\/api\/deployments\/models\/qwen-epp\/chat$/, route => {
    submitted = route.request().postDataJSON()
    return route.fulfill({ contentType: 'text/event-stream', body: 'data: {"choices":[{"index":0,"delta":{"content":"Gateway-only reply"},"finish_reason":null}]}\n\ndata: [DONE]\n\n' })
  })
  await page.goto('/deployments/qwen-epp?namespace=models')
  await expect(page.getByRole('heading', { name: 'Chat with model' })).toBeVisible()
  await page.getByLabel('Message').fill('Hello through the gateway')
  await page.getByRole('button', { name: /send/i }).click()
  await expect(page.getByTestId('chat-transcript')).toContainText('Gateway-only reply')
  expect(submitted).toMatchObject({ messages: [{ role: 'user', content: 'Hello through the gateway' }] })
  if (proofDir) await page.screenshot({ path: `${proofDir}/gateway-only-chat.png`, fullPage: true })
})

for (const configuration of ['Manual', 'Automatic']) {
  test(`${configuration.toLowerCase()} tool calling preserves create and preview payloads and clears disabled parsers`, async ({ mockedPage: page }) => {
    const previews: DeploymentConfig[] = []
    let submitted: DeploymentConfig | undefined
    await page.route(/\/api\/(?:installation\/)?runtimes\/status$/, route => route.fulfill({ json: { runtimes: [runtime] } }))
    await page.route(/\/api\/deployments\/-\/pvcs/, route => route.fulfill({ json: { pvcs: [] } }))
    await page.route(/\/api\/deployments\/preview$/, route => {
      const config = route.request().postDataJSON() as DeploymentConfig
      previews.push(config)
      return route.fulfill({ json: { resources: [{ kind: 'ModelDeployment', apiVersion: 'airunway.ai/v1alpha1', name: config.name,
        manifest: { spec: toModelDeploymentSpec(config) } }], primaryResource: { kind: 'ModelDeployment', apiVersion: 'airunway.ai/v1alpha1' } } })
    })
    await page.route(/\/api\/deployments\/?$/, route => {
      if (route.request().method() !== 'POST') return route.fallback()
      submitted = route.request().postDataJSON()
      return route.fulfill({ status: 201, json: { message: 'Created', name: submitted!.name, namespace: submitted!.namespace } })
    })
    await page.goto('/deploy/Qwen%2FQwen3-0.6B')
    await page.getByRole('radio', { name: new RegExp(`${configuration} configuration`) }).click()
    const checkbox = page.getByRole('checkbox', { name: 'Enable tool calling' })
    await expect(checkbox).not.toBeChecked()
    await checkbox.check()
    await expect(page.getByLabel('Tool parser', { exact: true })).toHaveValue('')
    await expect(page.getByLabel('Tool parser', { exact: true })).toHaveAttribute('placeholder', 'hermes')
    await Promise.all([
      page.waitForResponse(/\/api\/deployments\/preview$/),
      page.getByText('Manifest Preview', { exact: true }).click(),
    ])
    expect(previews.at(-1)?.toolCalling).toBe(true)
    expect(previews.at(-1)).not.toHaveProperty('toolCallParser')
    expect(previews.at(-1)).not.toHaveProperty('reasoningParser')
    // Invalid text must block both submission paths, not just native form validation.
    await page.getByLabel('Tool parser', { exact: true }).fill('auto')
    const submit = page.getByRole('button', { name: /Deploy Model/ })
    await expect(submit).toBeDisabled()
    await expect(page.getByText('Manifest Preview', { exact: true })).toHaveCount(0)
    await submit.evaluate(el => el.closest('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })))
    expect(submitted).toBeUndefined()
    await page.getByLabel('Tool parser', { exact: true }).fill('custom_parser')
    await page.getByLabel('Reasoning parser', { exact: true }).fill('none')
    await expect(page.getByRole('alert').filter({ hasText: 'not a supported Dynamo disable value' })).toBeVisible()
    await expect(submit).toBeDisabled()
    await page.getByLabel('Reasoning parser', { exact: true }).fill('basic')
    await Promise.all([
      page.waitForResponse(/\/api\/deployments\/preview$/),
      page.getByText('Manifest Preview', { exact: true }).click(),
    ])
    expect(previews.at(-1)).toMatchObject({ toolCalling: true, toolCallParser: 'custom_parser', reasoningParser: 'basic' })
    await checkbox.uncheck()
    await expect(page.getByLabel('Tool parser', { exact: true })).toHaveCount(0)
    await expect.poll(() => previews.at(-1)?.toolCalling).toBe(false)
    expect(previews.at(-1)).not.toHaveProperty('toolCallParser')
    expect(previews.at(-1)).not.toHaveProperty('reasoningParser')
    await checkbox.check()
    await expect(page.getByLabel('Tool parser', { exact: true })).toHaveValue('')
    await expect(page.getByLabel('Reasoning parser', { exact: true })).toHaveValue('')
    await page.getByLabel('Tool parser', { exact: true }).fill('hermes')
    await page.getByLabel('Reasoning parser', { exact: true }).fill('qwen3')
    await expect.poll(() => previews.at(-1)?.reasoningParser).toBe('qwen3')
    if (proofDir) await page.screenshot({ path: `${proofDir}/${configuration.toLowerCase()}-tool-calling.png`, fullPage: true })
    await Promise.all([page.waitForResponse(response => response.url().endsWith('/api/deployments') && response.request().method() === 'POST'), submit.click()])
    expect(submitted).toMatchObject({ provider: 'dynamo', toolCalling: true, toolCallParser: 'hermes', reasoningParser: 'qwen3' })
    if (configuration === 'Automatic') {
      expect(submitted?.providerOverrides).toMatchObject({ deploymentMode: 'intent' })
      for (const field of ['resources', 'replicas', 'mode', 'prefillGpus', 'decodeGpus']) expect(submitted).not.toHaveProperty(field)
    }
  })
}

test('tool calling reconfiguration preserves values and sends sparse flags, nullable clears and disabling', async ({ mockedPage: page }) => {
  const deployment: DeploymentStatus = { name: 'qwen-tools', namespace: 'models', resourceVersion: '42', modelId: 'Qwen/Qwen3-0.6B', engine: 'vllm', mode: 'aggregated', phase: 'Deploying', provider: 'dynamo', configurationMode: 'automatic', intent: defaultDynamoIntent(),
    replicas: { desired: 0, ready: 0, available: 0 }, pods: [], createdAt: '2026-09-25T10:00:00Z',
    providerStatus: { intent: { phase: 'Profiling', profilingPhase: 'SelectingConfig' } } }
  const writes: DynamoReconfigureRequest[] = []
  const readUrl = /\/api\/deployments\/qwen-tools(?:\?.*)?$/
  await page.route(readUrl, route => route.fulfill({ json: deployment }))
  await page.route(/\/api\/deployments\/models\/qwen-tools\/reconfigure$/, route => {
    const payload = route.request().postDataJSON() as DynamoReconfigureRequest
    writes.push(payload)
    if (payload.toolCalling !== undefined) deployment.toolCalling = payload.toolCalling
    for (const field of ['toolCallParser', 'reasoningParser'] as const) {
      if (payload.toolCalling === false || payload[field] === null) delete deployment[field]
      else if (payload[field] !== undefined) deployment[field] = payload[field]
    }
    if (payload.intent) deployment.intent = payload.intent
    deployment.resourceVersion = String(Number(deployment.resourceVersion) + 1)
    return route.fulfill({ json: { message: 'Configuration requested', attempt: `attempt-${writes.length}` } })
  })
  await page.goto('/deployments/qwen-tools?namespace=models')
  const open = () => page.getByRole('button', { name: 'Reconfigure', exact: true }).click()
  const confirm = async () => {
    await Promise.all([page.waitForResponse(readUrl), page.getByRole('button', { name: 'Confirm reconfiguration' }).click()])
    await expect(page.getByRole('dialog')).toHaveCount(0)
  }
  await open()
  await page.getByRole('checkbox', { name: 'Enable tool calling' }).check()
  await page.getByLabel('Tool parser', { exact: true }).fill('hermes')
  await page.getByLabel('Reasoning parser', { exact: true }).fill('basic')
  await confirm()
  expect(writes[0]).toMatchObject({ resourceVersion: '42', toolCalling: true, toolCallParser: 'hermes', reasoningParser: 'basic' })
  await open()
  await expect(page.getByRole('checkbox', { name: 'Enable tool calling' })).toBeChecked()
  await expect(page.getByLabel('Tool parser', { exact: true })).toHaveValue('hermes')
  await expect(page.getByLabel('Reasoning parser', { exact: true })).toHaveValue('basic')
  await page.getByRole('spinbutton', { name: /GPU budget/ }).fill('2')
  await confirm()
  for (const field of ['toolCalling', 'toolCallParser', 'reasoningParser']) expect(writes[1]).not.toHaveProperty(field)
  await open()
  await page.getByLabel('Tool parser', { exact: true }).fill('')
  await page.getByLabel('Reasoning parser', { exact: true }).fill('')
  await confirm()
  expect(writes[2]).toMatchObject({ resourceVersion: '44', toolCallParser: null, reasoningParser: null })
  expect(writes[2]).not.toHaveProperty('toolCalling')
  await open()
  await expect(page.getByLabel('Tool parser', { exact: true })).toHaveValue('')
  await expect(page.getByLabel('Reasoning parser', { exact: true })).toHaveValue('')
  await page.getByLabel('Tool parser', { exact: true }).fill('hermes')
  await page.getByLabel('Reasoning parser', { exact: true }).fill('basic')
  await page.getByRole('checkbox', { name: 'Enable tool calling' }).uncheck()
  await expect(page.getByLabel('Tool parser', { exact: true })).toHaveCount(0)
  await confirm()
  expect(writes[3]).toMatchObject({ resourceVersion: '45', toolCalling: false })
  expect(writes[3]).not.toHaveProperty('toolCallParser')
  expect(writes[3]).not.toHaveProperty('reasoningParser')
  await open()
  await expect(page.getByRole('checkbox', { name: 'Enable tool calling' })).not.toBeChecked()
  await page.getByRole('checkbox', { name: 'Enable tool calling' }).check()
  await expect(page.getByLabel('Tool parser', { exact: true })).toHaveValue('')
  if (proofDir) await writeFile(`${proofDir}/tool-reconfigure-requests.json`, JSON.stringify(writes, null, 2))
})

for (const source of ['provided', 'discovered', 'mixed'] as const) {
  test(`automatic status labels ${source} hardware, selected plan, diagnostics and progress without totals or estimates`, async ({ mockedPage: page }) => {
    const planSource = source === 'provided' ? 'selectedConfig' : 'workload'
    const deployment: DeploymentStatus = { name: 'qwen-summary', namespace: 'models', resourceVersion: '42', modelId: 'Qwen/Qwen3-0.6B', engine: 'vllm', mode: 'aggregated', phase: 'Deploying', provider: 'dynamo', configurationMode: 'automatic', intent: { ...defaultDynamoIntent(), hardware: { totalGpus: 64, gpuSku: 'requested-only' } },
      replicas: { desired: 0, ready: 0, available: 0 }, pods: [], createdAt: '2026-09-25T10:00:00Z',
      providerStatus: { intent: { phase: 'Profiling', profilingPhase: 'SweepingDecode',
        hardware: { source, gpuSku: 'H100', vramMb: 81920, numGpusPerNode: 8 },
        plan: { source: planSource, engine: 'vllm', servingMode: 'disaggregated', workers: [
          { name: 'prefill-worker', role: 'prefill', replicas: 2, gpusPerReplica: 4, tensorParallelism: 4, pipelineParallelism: 1 },
          { name: 'decode-worker', role: 'decode' },
        ] }, diagnostic: 'Only the first 32 workers are included. Some hardware details were not reported.' } } }
    await page.route(/\/api\/deployments\/qwen-summary(?:\?.*)?$/, route => route.fulfill({ json: deployment }))
    await page.goto('/deployments/qwen-summary?namespace=models')
    const panel = page.getByRole('region', { name: 'Automatic configuration status' })
    await expect(panel.getByText('Finding a configuration', { exact: true })).toBeVisible()
    await expect(panel.getByText('Evaluating response generation', { exact: true })).toBeVisible()
    const hardware = panel.getByRole('region', { name: 'Configuration hardware' })
    await expect(hardware).toContainText(source === 'provided' ? 'Provided by you' : source === 'discovered' ? 'Detected from available hardware' : 'Provided and detected values')
    await expect(hardware).toContainText('H100')
    await expect(hardware).toContainText('81,920 MiB')
    await expect(hardware).not.toContainText('requested-only')
    if (source === 'provided') await expect(hardware).not.toContainText(/detected/i)
    const plan = panel.getByRole('region', { name: 'Selected serving configuration' })
    await expect(plan).toContainText(planSource === 'selectedConfig' ? 'From the configuration search' : 'From the serving configuration')
    await expect(plan).toContainText('Separate prompt and response processing')
    await expect(plan.getByRole('table')).toContainText('prefill-worker')
    await expect(plan.getByRole('table')).toContainText('Tensor: 4')
    await expect(plan.getByRole('table')).toContainText('Pipeline: 1')
    await expect(plan.getByRole('row').filter({ hasText: 'decode-worker' })).toContainText('Not reported')
    await expect(panel.getByRole('status')).toContainText('Only the first 32 workers are included.')
    await expect(panel).not.toContainText(/total GPUs|estimated completion|\bETA\b|elapsed|\d+%/i)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
    if (proofDir) await page.screenshot({ path: `${proofDir}/configuration-summary-${source}.png`, fullPage: true })
  })
}
