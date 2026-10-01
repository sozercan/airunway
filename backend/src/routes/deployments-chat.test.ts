import { afterEach, beforeEach, describe, expect, test } from 'bun:test';
import type { DeploymentStatus } from '@airunway/shared';
import app from '../hono-app';
import { authService } from '../services/auth';
import { configService } from '../services/config';
import { kubernetesService } from '../services/kubernetes';
import { mockDeployment } from '../test/fixtures';
import { mockServiceMethod } from '../test/helpers';

const restores: Array<() => void> = [];
const gatewaySse = 'data: {"choices":[{"delta":{"content":"Hello from gateway"}}]}\n\ndata: [DONE]\n\n';
let deployment: DeploymentStatus | null;
let gatewayCalls: Array<{ url: string; init?: RequestInit }>;
let directCalls: unknown[][];
let modelLookups: number;
let deploymentLookups: unknown[][];

const request = (body: Record<string, unknown> = {}, path = '/models/test-deploy/chat', headers = {}) =>
  app.request(`/api/deployments${path}`, {
    method: 'POST', headers: { 'Content-Type': 'application/json', ...headers },
    body: JSON.stringify({ messages: [{ role: 'user', content: 'Hello' }], ...body }),
  });

beforeEach(() => {
  deployment = {
    ...mockDeployment, name: 'test-deploy', namespace: 'models', phase: 'Running', provider: 'dynamo',
    frontendService: undefined, gateway: { endpoint: 'gateway.example:8080', modelName: 'gateway-alias' },
  };
  gatewayCalls = [];
  directCalls = [];
  modelLookups = 0;
  deploymentLookups = [];
  restores.push(mockServiceMethod(authService, 'isAuthEnabled', () => false));
  restores.push(mockServiceMethod(configService, 'getDefaultNamespace', async () => 'models'));
  restores.push(mockServiceMethod(kubernetesService, 'getDeployment', async (...args: unknown[]) => {
    deploymentLookups.push(args);
    return deployment;
  }));
  restores.push(mockServiceMethod(kubernetesService, 'proxyServiceGet', async () => {
    modelLookups++;
    return JSON.stringify({ data: [{ id: 'direct-model' }] });
  }));
  restores.push(mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
    directCalls.push(args);
    return new Response('data: [DONE]\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
  }));
  const originalFetch = globalThis.fetch;
  restores.push(() => { globalThis.fetch = originalFetch; });
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    gatewayCalls.push({ url: input.toString(), init });
    return new Response(gatewaySse, { headers: { 'Content-Type': 'text/event-stream' } });
  }) as typeof fetch;
});
afterEach(() => { restores.reverse().forEach(restore => restore()); restores.length = 0; });

describe('Gateway-only deployment chat', () => {
  test.each(['/models/test-deploy/chat', '/test-deploy/chat?namespace=models'])(
    'streams via the trusted gateway for %s without a direct binding', async path => {
      const response = await request({}, path);
      expect(response.status).toBe(200);
      expect(response.headers.get('Content-Type')).toBe('text/event-stream');
      expect(await response.text()).toBe(gatewaySse);
      expect(gatewayCalls).toHaveLength(1);
      expect(gatewayCalls[0].url).toBe('http://gateway.example:8080/v1/chat/completions');
      expect(JSON.parse(String(gatewayCalls[0].init?.body))).toEqual({
        messages: [{ role: 'user', content: 'Hello' }], model: 'gateway-alias', stream: true,
      });
      expect(modelLookups).toBe(0);
      expect(directCalls).toHaveLength(0);
    },
  );

  test('uses authenticated deployment status for the target and model, not request destinations', async () => {
    restores.push(mockServiceMethod(authService, 'isAuthEnabled', () => true));
    restores.push(mockServiceMethod(authService, 'validateToken', async () => ({ valid: true, user: { username: 'reader' } })));
    const response = await request({
      model: 'untrusted-model', endpoint: 'https://untrusted.example', namespace: 'another-namespace',
      gateway: { endpoint: 'https://untrusted.example', modelName: 'untrusted-alias' },
    }, '/models/test-deploy/chat', { Authorization: 'Bearer user-token' });
    expect(response.status).toBe(200);
    expect(deploymentLookups).toEqual([['test-deploy', 'models', 'user-token']]);
    expect(gatewayCalls[0].url).toBe('http://gateway.example:8080/v1/chat/completions');
    const headers = new Headers(gatewayCalls[0].init?.headers);
    expect(headers.get('X-Gateway-Model-Name')).toBe('gateway-alias');
    expect(headers.get('Authorization')).toBeNull();
    expect(gatewayCalls[0].init?.signal).toBeInstanceOf(AbortSignal);
    expect(JSON.parse(String(gatewayCalls[0].init?.body)).model).toBe('gateway-alias');
  });

  test.each(['served-name', undefined])('uses served name or model ID without attempting discovery when no gateway alias exists: %s', async servedModelName => {
    deployment = { ...deployment!, servedModelName, gateway: { endpoint: 'https://gateway.example/v1?ignored=1#fragment' } };
    expect((await request()).status).toBe(200);
    expect(gatewayCalls[0].url).toBe('https://gateway.example/v1/chat/completions');
    expect(JSON.parse(String(gatewayCalls[0].init?.body)).model).toBe(servedModelName || deployment.modelId);
    expect(modelLookups).toBe(0);
  });

  test('forwards stream chunks without waiting for completion', async () => {
    let controller!: ReadableStreamDefaultController<Uint8Array>;
    const encoder = new TextEncoder();
    const first = 'data: {"choices":[{"delta":{"content":"First"}}]}\n\n';
    const last = 'data: [DONE]\n\n';
    globalThis.fetch = (async () => new Response(new ReadableStream<Uint8Array>({
      start(streamController) { controller = streamController; controller.enqueue(encoder.encode(first)); },
    }), { headers: { 'Content-Type': 'text/event-stream' } })) as typeof fetch;
    const response = await request();
    expect(response.status).toBe(200);
    const reader = response.body!.getReader();
    expect(new TextDecoder().decode((await reader.read()).value)).toBe(first);
    controller.enqueue(encoder.encode(last));
    controller.close();
    expect(new TextDecoder().decode((await reader.read()).value)).toBe(last);
    expect((await reader.read()).done).toBe(true);
  });

  test.each([401, 403, 404, 503])('preserves non-streaming gateway error status %s', async status => {
    globalThis.fetch = (async () => new Response(JSON.stringify({ error: { message: 'Gateway rejected the model' } }), {
      status, headers: { 'Content-Type': 'application/json' },
    })) as typeof fetch;
    const response = await request();
    expect(response.status).toBe(status);
    expect(response.headers.get('Content-Type')).toContain('application/json');
    expect(await response.json()).toMatchObject({ error: { message: 'Gateway rejected the model', statusCode: status } });
  });

  test('returns an error when the gateway provides no response body', async () => {
    globalThis.fetch = (async () => new Response(null, { status: 200 })) as typeof fetch;
    const response = await request();
    expect(response.status).toBe(502);
    expect(await response.json()).toMatchObject({ error: { message: 'Gateway chat response did not include a stream body' } });
  });

  test('rejects missing endpoints and non-running or missing deployments without contacting a target', async () => {
    const current = deployment!;
    for (const [value, status] of [
      [{ ...current, gateway: undefined }, 409],
      [{ ...current, gateway: { endpoint: '' } }, 409],
      [{ ...current, phase: 'Pending' as const }, 409],
      [null, 404],
    ] as const) {
      deployment = value;
      expect((await request()).status).toBe(status);
    }
    expect(gatewayCalls).toHaveLength(0);
    expect(directCalls).toHaveLength(0);
    expect(modelLookups).toBe(0);
  });

  test('retains authentication and namespace validation before resolving the gateway', async () => {
    restores.push(mockServiceMethod(authService, 'isAuthEnabled', () => true));
    expect((await request()).status).toBe(401);
    restores.push(mockServiceMethod(authService, 'validateToken', async () => ({ valid: true, user: { username: 'reader' } })));
    expect((await request({}, '/bad%20namespace/test-deploy/chat', { Authorization: 'Bearer user-token' })).status).toBe(400);
    expect(deploymentLookups).toHaveLength(0);
    expect(gatewayCalls).toHaveLength(0);
  });

  test('prefers a direct Service when both bindings exist', async () => {
    deployment = { ...deployment!, frontendService: 'direct-service:9000', frontendNamespace: 'serving' };
    const response = await request();
    expect(response.status).toBe(200);
    expect(await response.text()).toBe('data: [DONE]\n\n');
    expect(directCalls[0].slice(0, 5)).toEqual(['direct-service', 'serving', 9000, 'v1/chat/completions', {
      messages: [{ role: 'user', content: 'Hello' }], model: 'direct-model', stream: true,
    }]);
    expect(gatewayCalls).toHaveLength(0);
  });

  test('does not fall back to the gateway on direct model or authorization errors', async () => {
    deployment = { ...deployment!, frontendService: 'direct-service:9000' };
    for (const status of [401, 403, 404]) {
      restores.push(mockServiceMethod(kubernetesService, 'proxyServicePostStream', async () => new Response(JSON.stringify({
        error: { message: 'Direct endpoint rejected the model' },
      }), { status, headers: { 'Content-Type': 'application/json' } })));
      expect((await request()).status).toBe(status);
    }
    expect(gatewayCalls).toHaveLength(0);
  });
});
