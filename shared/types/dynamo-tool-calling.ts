/** Structured Dynamo parser settings. Omission requests the known model-family default. */
export interface DynamoToolCallingConfig {
  toolCalling?: boolean;
  toolCallParser?: string;
  reasoningParser?: string;
}

export const DYNAMO_PARSER_IDENTIFIER = /^[a-z][a-z0-9_]*$/;

export function getDynamoParserDefaults(modelId: string): Pick<DynamoToolCallingConfig, 'toolCallParser' | 'reasoningParser'> {
  const id = modelId.toLowerCase();
  // Ordered, exact family prefixes. In particular, Coder must not get reasoning.
  if (id.startsWith('qwen/qwen3-coder')) return { toolCallParser: 'qwen3_coder' };
  if (id.startsWith('qwen/qwen3.5-')) return { toolCallParser: 'qwen3_coder', reasoningParser: 'qwen3' };
  if (id.startsWith('qwen/qwen3-')) return { toolCallParser: 'hermes', reasoningParser: 'qwen3' };
  return {};
}

function parserSetting(value: string): string | undefined {
  for (const name of ['DYN_TOOL_CALL_PARSER', 'DYN_REASONING_PARSER', 'DYN_CHAT_PROCESSOR']) {
    if (value.includes(name)) return name;
  }
  for (const name of ['dyn-tool-call-parser', 'dyn-reasoning-parser', 'dyn-chat-processor', 'tool-call-parser', 'reasoning-parser', 'enable-auto-tool-choice']) {
    if (value === name || value.includes(`--${name}`)) return name;
  }
}

/** Mirrors controller/pkg/dynamointent/toolcalling.go, including native override trees. */
function conflictingParserSetting(value: unknown): string | undefined {
  if (typeof value === 'string') return parserSetting(value);
  if (Array.isArray(value)) {
    for (const child of value) {
      const conflict = conflictingParserSetting(child);
      if (conflict) return conflict;
    }
  } else if (value && typeof value === 'object') {
    const object = value as Record<string, unknown>;
    for (const key of Object.keys(object).sort()) {
      const conflict = parserSetting(key) || conflictingParserSetting(object[key]);
      if (conflict) return conflict;
    }
  }
}

export function getDynamoToolCallingError(config: DynamoToolCallingConfig & {
  modelId: string;
  provider?: string;
  engine?: string;
  engineArgs?: Record<string, unknown>;
  engineExtraArgs?: string[];
  env?: unknown;
  providerOverrides?: Record<string, unknown>;
}, resolvedProvider?: string): string | undefined {
  if (!config.toolCalling) {
    if (config.toolCallParser !== undefined || config.reasoningParser !== undefined) {
      return 'Tool and reasoning parsers require tool calling to be enabled.';
    }
    return;
  }
  for (const [label, value] of [['Tool parser', config.toolCallParser], ['Reasoning parser', config.reasoningParser]]) {
    if (value === undefined) continue;
    if (value === 'none') return `${label}: none is not a supported Dynamo disable value. Leave the field empty for model defaults.`;
    if (value === 'auto') return `${label}: leave the field empty to use automatic selection, not "auto".`;
    if (value.length > 64 || !DYNAMO_PARSER_IDENTIFIER.test(value)) {
      return `${label} must be a lowercase parser identifier of at most 64 characters.`;
    }
  }
  if (!config.toolCallParser && !getDynamoParserDefaults(config.modelId).toolCallParser) {
    return 'No automatic tool parser is known for this model. Enter a compatible Dynamo tool parser.';
  }
  const provider = config.provider || resolvedProvider;
  if ((provider !== 'dynamo' && !(provider === undefined && config.providerOverrides?.deploymentMode === 'intent'))
    || (resolvedProvider && resolvedProvider !== 'dynamo')) {
    return 'Tool calling currently requires the Dynamo deployment method.';
  }
  if (config.engine === 'llamacpp') return 'Tool calling requires a Dynamo vLLM, SGLang or TensorRT-LLM model server.';
  for (const value of [config.engineArgs, config.engineExtraArgs, config.env, config.providerOverrides]) {
    const conflict = conflictingParserSetting(value);
    if (conflict) return `Tool calling conflicts with raw parser or chat processor setting ${conflict}. Use the tool and reasoning parser fields, or disable tool calling.`;
  }
}
