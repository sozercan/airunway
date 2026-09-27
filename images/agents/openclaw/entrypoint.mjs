import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { spawn } from "node:child_process";
import { pathToFileURL } from "node:url";

import { chatOnlyToolPolicy, modelProvider, required } from "./model_provider.mjs";
import { createBoundedShutdown, createRuntimeProxy } from "./runtime_proxy.mjs";

// The pinned CLI may exit zero for failed turns. Its JSON metadata, not the
// assistant's text, is the authority for whether a one-shot task completed.
export function oneShotAnswer(raw, secretValues = []) {
  let result;
  try {
    result = JSON.parse(raw);
  } catch {
    throw new Error("OpenClaw returned an invalid task result.");
  }
  if (!result || typeof result !== "object" || Array.isArray(result) ||
      !result.meta || typeof result.meta !== "object" || Array.isArray(result.meta) ||
      !Array.isArray(result.payloads)) {
    throw new Error("OpenClaw returned an invalid task result.");
  }
  if (result.meta.error != null || result.meta.aborted === true ||
      result.meta.livenessState === "blocked") {
    throw new Error("OpenClaw task failed according to native status.");
  }
  const texts = [];
  for (const payload of result.payloads) {
    if (!payload || typeof payload !== "object" || Array.isArray(payload) ||
        (payload.text != null && typeof payload.text !== "string")) {
      throw new Error("OpenClaw returned an invalid task result.");
    }
    if (typeof payload.text === "string" && payload.text.trim()) texts.push(payload.text);
  }
  if (texts.length === 0) throw new Error("OpenClaw returned no text answer.");
  const answer = texts.join("\n");
  if (secretValues.some((value) => typeof value === "string" && value && answer.includes(value))) {
    throw new Error("OpenClaw returned sensitive runtime data.");
  }
  return answer;
}

function runOneShot(task, model) {
  return new Promise((resolve, reject) => {
    const child = spawn("node", [
      "/app/openclaw.mjs", "agent", "--local", "--agent", "main",
      "--json", "--message", task, "--model", model,
    ], { stdio: ["ignore", "pipe", "ignore"], env: process.env });
    const chunks = [];
    let bytes = 0;
    let oversized = false;
    child.stdout.on("data", (chunk) => {
      bytes += chunk.length;
      if (bytes > 4 * 1024 * 1024) {
        oversized = true;
        child.kill("SIGKILL");
      } else {
        chunks.push(chunk);
      }
    });
    child.once("error", () => reject(new Error("OpenClaw task process could not start.")));
    child.once("close", (code, signal) => {
      if (oversized) {
        reject(new Error("OpenClaw task result exceeds 4 MiB."));
        return;
      }
      if (signal) {
        process.kill(process.pid, signal);
        return;
      }
      if (code !== 0) {
        reject(new Error("OpenClaw task process failed."));
        return;
      }
      try {
        resolve(oneShotAnswer(Buffer.concat(chunks).toString("utf8"), [
          process.env.OPENAI_API_KEY,
          process.env.ANTHROPIC_API_KEY,
          process.env.AZURE_OPENAI_API_KEY,
          process.env.AIRUNWAY_AGENT_API_KEY,
          process.env.OPENCLAW_GATEWAY_TOKEN,
        ]));
      } catch (error) {
        reject(error);
      }
    });
  });
}

async function main() {
  const mountedConfig = process.env.AIRUNWAY_AGENT_CONFIG || "/etc/airunway/agent.json";
  const runtimeConfig = JSON.parse(fs.readFileSync(mountedConfig, "utf8"));
  const stateDir = "/tmp/airunway-openclaw";
  const workspaceDir = path.join(stateDir, "workspace");
  const externalPort = Number(process.env.AIRUNWAY_AGENT_PORT || "18789");
  if (!Number.isInteger(externalPort) || externalPort < 1 || externalPort > 65535) {
    throw new Error("AIRUNWAY_AGENT_PORT must be an integer between 1 and 65535");
  }
  const internalPort = externalPort === 65535 ? 65534 : externalPort + 1;
  const gatewayToken = crypto.randomBytes(32).toString("hex");
  const maxRequestBytes = 4 * 1024 * 1024;

  const provider = modelProvider();
  fs.mkdirSync(workspaceDir, { recursive: true });
  const systemPrompt = runtimeConfig.systemPrompt;
  if (typeof systemPrompt === "string" && systemPrompt.trim()) {
    fs.writeFileSync(path.join(workspaceDir, "SOUL.md"), `${systemPrompt.trim()}\n`, { mode: 0o600 });
  }

  const openclawConfig = {
    // The image is release-pinned. Runtime update traffic and in-place package
    // mutation would bypass that supply-chain boundary.
    update: { checkOnStart: false, auto: { enabled: false } },
    // The public runtime contract is chat-only. Keep host filesystem, process,
    // browser, messaging, model-switching, and control tools out of the model's
    // tool surface.
    tools: chatOnlyToolPolicy(),
    gateway: {
      mode: "local",
      bind: "loopback",
      port: internalPort,
      auth: { mode: "token", token: "${OPENCLAW_GATEWAY_TOKEN}" },
      http: { endpoints: { chatCompletions: { enabled: true } } },
      controlUi: { enabled: false },
    },
    agents: {
      defaults: {
        workspace: workspaceDir,
        model: { primary: `${provider.id}/${provider.model}` },
      },
    },
    models: {
      mode: "merge",
      providers: {
        [provider.id]: {
          baseUrl: provider.baseUrl,
          apiKey: provider.apiKey,
          api: provider.api,
          ...(provider.authHeader !== undefined ? { authHeader: provider.authHeader } : {}),
          ...(provider.headers ? { headers: provider.headers } : {}),
          models: [{ id: provider.model, name: provider.model }],
        },
      },
    },
  };

  const configPath = path.join(stateDir, "openclaw.json");
  fs.writeFileSync(configPath, JSON.stringify(openclawConfig), { mode: 0o600 });
  Object.assign(process.env, {
    HOME: stateDir,
    OPENCLAW_HOME: stateDir,
    OPENCLAW_STATE_DIR: stateDir,
    OPENCLAW_CONFIG_PATH: configPath,
    OPENCLAW_WORKSPACE_DIR: workspaceDir,
    OPENCLAW_GATEWAY_TOKEN: gatewayToken,
    OPENCLAW_DISABLE_BONJOUR: "1",
    OPENCLAW_NO_AUTO_UPDATE: "1",
  });

  const mode = process.env.AIRUNWAY_AGENT_MODE || "server";
  if (mode === "job") {
    const task = runtimeConfig.task || runtimeConfig.prompt;
    if (typeof task !== "string" || !task.trim()) {
      throw new Error("job lifecycle requires spec.config.task or spec.config.prompt");
    }
    const answer = await runOneShot(task, `${provider.id}/${provider.model}`);
    process.stdout.write(`${answer}\n`);
  } else {
    const accessToken = required("AIRUNWAY_AGENT_API_KEY");
    const gateway = spawn("node", ["/app/openclaw.mjs", "gateway"], {
      stdio: "inherit",
      env: process.env,
    });

    const proxy = createRuntimeProxy({
      accessToken,
      gatewayToken,
      internalPort,
      maxRequestBytes,
    });
    proxy.listen(externalPort, "0.0.0.0", () => {
      console.log(`AI Runway OpenClaw proxy listening on :${externalPort}`);
    });

    const shutdown = createBoundedShutdown({ server: proxy, child: gateway });
    let termination;
    function terminate(signal, exitCode) {
      if (termination) return termination;
      termination = shutdown(signal).finally(() => process.exit(exitCode));
      return termination;
    }

    gateway.once("exit", (code, signal) => {
      if (termination) return;
      terminate(undefined, signal ? 1 : (code ?? 1));
    });
    for (const signal of ["SIGINT", "SIGTERM"]) {
      process.once(signal, () => terminate(signal, 0));
    }
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
