import { z } from 'zod';
import { DYNAMO_ATTEMPT_ANNOTATION, type ModelDeployment, type DynamoReconfigureRequest } from '@airunway/shared';
import { HTTPException } from 'hono/http-exception';

const positive = () => z.number().finite().positive();
const tokens = () => positive().int().max(2_147_483_647);

const forbiddenOverrideKeys = new Set([
  'securityContext', 'serviceAccountName', 'serviceAccount', 'hostNetwork', 'hostPID', 'hostIPC',
  'automountServiceAccountToken', 'nodeName', 'priorityClassName', 'runtimeClassName', 'resources', 'replicas',
].map(key => key.toLowerCase()));

function isJsonObject(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object'
    && (Object.getPrototypeOf(value) === Object.prototype || Object.getPrototypeOf(value) === null);
}

// Validate raw JSON before any object parser can strip fields such as __proto__.
// Dynamo owns the native schema, including unknown fields and merge directives.
const nativeOverrideObjectSchema = z.custom<Record<string, unknown>>(isJsonObject, {
  message: 'Native overrides must be JSON objects',
}).superRefine((object, ctx) => {
  const ancestors = new Set<object>();
  const check = (value: unknown, path: Array<string | number>) => {
    if (value === null || typeof value === 'string' || typeof value === 'boolean'
      || (typeof value === 'number' && Number.isFinite(value))) return;
    if (!Array.isArray(value) && !isJsonObject(value)) {
      ctx.addIssue({ code: 'custom', message: 'Native overrides must contain only finite JSON values', path });
      return;
    }
    if (ancestors.has(value)) {
      ctx.addIssue({ code: 'custom', message: 'Native overrides must not contain circular references', path });
      return;
    }
    ancestors.add(value);
    if (Array.isArray(value)) {
      for (const [index, item] of value.entries()) check(item, [...path, index]);
    } else {
      for (const [key, child] of Object.entries(value)) {
        const childPath = [...path, key];
        if (forbiddenOverrideKeys.has(key.toLowerCase())) {
          ctx.addIssue({ code: 'custom', message: `${key} is not allowed in automatic configuration overrides`, path: childPath });
        } else {
          check(child, childPath);
        }
      }
    }
    ancestors.delete(value);
  };
  check(object, []);
}).transform(object => structuredClone(object));

const dynamoIntentOverridesSchema = z.object({
  profilingJob: nativeOverrideObjectSchema.optional(),
  dgd: z.object({
    apiVersion: z.enum(['nvidia.com/v1alpha1', 'nvidia.com/v1beta1']),
    kind: z.literal('DynamoGraphDeployment'),
    metadata: nativeOverrideObjectSchema.optional(),
    spec: nativeOverrideObjectSchema,
  }).strict().optional(),
}).strict();

export const dynamoIntentSchema = z.object({
  hardware: z.object({
    totalGpus: z.number().int().min(1).max(64),
    gpuSku: z.string().trim().min(1).max(128).optional(),
    vramMb: positive().max(10_000_000).optional(),
    numGpusPerNode: z.number().int().min(1).max(64).optional(),
  }).strict(),
  searchStrategy: z.literal('rapid').optional().default('rapid'),
  overrides: dynamoIntentOverridesSchema.optional(),
  workload: z.object({
    isl: tokens().optional(),
    osl: tokens().optional(),
    requestRate: positive().max(1_000_000).optional(),
    concurrency: positive().max(1_000_000).optional(),
  }).strict().refine(w => w.requestRate === undefined || w.concurrency === undefined, {
    message: 'Specify requestRate or concurrency, not both',
  }).optional(),
  sla: z.object({
    ttft: positive().max(86_400_000).optional(),
    itl: positive().max(86_400_000).optional(),
    e2eLatency: positive().max(86_400_000).optional(),
  }).strict().refine(s => s.e2eLatency === undefined || (s.ttft === undefined && s.itl === undefined), {
    message: 'Specify e2eLatency or ttft/itl, not both',
  }).optional(),
}).strict();

export const dynamoOverridesSchema = z.object({
  deploymentMode: z.literal('intent'),
  intent: dynamoIntentSchema,
}).strict();

export const dynamoReconfigureSchema = z.object({
  resourceVersion: z.string().min(1).max(256),
  intent: dynamoIntentSchema.optional(),
  modelId: z.string().trim().min(1).max(512).optional(),
  engine: z.enum(['vllm', 'sglang', 'trtllm']).optional(),
}).strict();

/** Prepare one optimistic update. Never retry a stale read or drop unrelated fields. */
export function reconfigureDynamoDeployment(
  current: ModelDeployment, request: DynamoReconfigureRequest, attempt = crypto.randomUUID(),
): ModelDeployment {
  if (current.spec.provider?.name !== 'dynamo' || current.spec.provider.overrides?.deploymentMode !== 'intent') {
    throw new HTTPException(422, { message: 'Only automatic Dynamo deployments can be reconfigured' });
  }
  if (current.metadata.resourceVersion !== request.resourceVersion) {
    throw new HTTPException(409, { message: 'Deployment changed. Refresh it before retrying or reconfiguring.' });
  }
  const spec = current.spec;
  const extra = spec as unknown as Record<string, unknown>;
  const engine = spec.engine;
  if (spec.image || engine.image || engine.contextLength || engine.trustRemoteCode || engine.enforceEager
    || Object.keys(engine.args || {}).length || engine.extraArgs?.length || spec.model.servedName
    || spec.env?.length || spec.model.storage?.volumes?.length
    || Object.keys((extra.nodeSelector as object) || {}).length
    || (Array.isArray(extra.tolerations) && extra.tolerations.length)
    || (spec.podTemplate && Object.keys(spec.podTemplate).length)
    || (spec.secrets?.huggingFaceToken && spec.secrets.huggingFaceToken !== 'hf-token-secret')
    || current.metadata.annotations?.['airunway.ai/dynamo-test-backend'] === 'mocker') {
    throw new HTTPException(422, { message: 'This deployment has custom runtime, storage or access settings that require manual configuration' });
  }
  const next = structuredClone(current);
  const intent = request.intent ?? current.spec.provider.overrides.intent;
  const result = dynamoIntentSchema.safeParse(intent);
  if (!result.success) {
    throw new HTTPException(422, { message: 'A valid typed intent is required to reconfigure this deployment' });
  }
  next.metadata.annotations = { ...next.metadata.annotations, [DYNAMO_ATTEMPT_ANNOTATION]: attempt };
  // Keep unrelated provider options. Legacy spec and typed intent cannot coexist.
  const overrides = { ...next.spec.provider!.overrides, deploymentMode: 'intent', intent: result.data };
  delete (overrides as Record<string, unknown>).spec;
  next.spec.provider!.overrides = overrides;
  if (request.modelId !== undefined) next.spec.model.id = request.modelId;
  if (request.engine !== undefined) next.spec.engine.type = request.engine;
  delete next.spec.resources;
  delete next.spec.scaling;
  delete next.spec.serving;
  return next;
}
