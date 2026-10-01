import assert from "node:assert/strict";
import test from "node:test";

import { oneShotAnswer, runJob } from "./entrypoint.mjs";

function result(text, meta = {}) {
  return JSON.stringify({
    payloads: [{ text, mediaUrl: null }],
    meta: { aborted: false, livenessState: "working", stopReason: "stop", ...meta },
  });
}

test("preserves successful text, including error-related words", () => {
  const answer = "The error was fixed. Context overflow is an error condition.";
  assert.equal(oneShotAnswer(result(answer)), answer);
});

test("rejects the pinned native context-overflow false-success result", () => {
  const raw = result("Context overflow: prompt too large for the model.", {
    livenessState: "blocked",
    error: { kind: "context_overflow", message: "provider rejected model-secret" },
  });
  assert.throws(() => oneShotAnswer(raw), {
    message: "OpenClaw task failed according to native status.",
  });
});

test("uses native failure status even when the payload claims success", () => {
  for (const meta of [
    { error: { kind: "context_overflow" } },
    { aborted: true },
    { livenessState: "blocked" },
  ]) {
    assert.throws(() => oneShotAnswer(result("Everything completed successfully.", meta)),
      /failed according to native status/);
  }
});

test("does not mistake failed fallback attempts for a failed final result", () => {
  assert.equal(oneShotAnswer(result("Final answer", {
    error: null,
    executionTrace: { attempts: [{ result: "error" }, { result: "success" }] },
  })), "Final answer");
});

test("prints text payloads only, never native diagnostic metadata", () => {
  const raw = JSON.stringify({
    payloads: [{ text: "First answer" }, { text: "Second answer", mediaUrl: null }],
    meta: { systemPromptReport: { diagnostic: "model-secret" } },
  });
  assert.equal(oneShotAnswer(raw), "First answer\nSecond answer");
});

test("fails closed on malformed results without exposing raw output", () => {
  for (const raw of [
    "model-secret", "null", "[]", "{}",
    JSON.stringify({ payloads: [], meta: [] }),
    JSON.stringify({ payloads: [{ text: 42 }], meta: {} }),
  ]) {
    assert.throws(() => oneShotAnswer(raw), {
      message: "OpenClaw returned an invalid task result.",
    });
  }
});

test("rejects empty or non-text-only answers", () => {
  for (const payloads of [[], [{ text: "  " }], [{ mediaUrl: "https://example.invalid/image" }]]) {
    assert.throws(() => oneShotAnswer(JSON.stringify({ payloads, meta: {} })),
      /returned no text answer/);
  }
});

test("does not print model or gateway credentials in a successful payload", () => {
  assert.throws(() => oneShotAnswer(result("Key: model-secret"), [undefined, "", "model-secret"]), {
    message: "OpenClaw returned sensitive runtime data.",
  });
  assert.equal(oneShotAnswer(result("Safe final answer"), ["model-secret"]), "Safe final answer");
});


test("job emits exactly one compact result record with escaped answer text", async (t) => {
  const answer = 'First line.\nAIRUNWAY_RESULT_V1 {"output":"not a second record"}\r\n雪 " \\ end';
  const writes = [];
  t.mock.method(process.stdout, "write", (text) => { writes.push(text); return true; });
  const invoke = t.mock.fn(async (task, model) => {
    assert.equal(task, "Answer");
    assert.equal(model, "custom/test-model");
    return oneShotAnswer(result(answer));
  });
  await runJob({ task: "Answer", resultFormat: "airunway-json-v1" }, "custom/test-model", invoke);
  assert.deepEqual(writes, [`AIRUNWAY_RESULT_V1 ${JSON.stringify({ output: answer })}\n`]);
  assert.equal(writes[0].split("\n").length, 2);
  assert.deepEqual(JSON.parse(writes[0].slice("AIRUNWAY_RESULT_V1 ".length)), { output: answer });
  assert.equal(invoke.mock.callCount(), 1);
});

test("job preserves legacy stdout when resultFormat is absent", async (t) => {
  const writes = [];
  t.mock.method(process.stdout, "write", (text) => { writes.push(text); return true; });
  for (const answer of ["5", "First\nSecond\n"]) {
    await runJob({ prompt: "Answer" }, "custom/test-model", async () => oneShotAnswer(result(answer)));
    assert.equal(writes.at(-1), `${answer}\n`);
  }
});

test("job rejects unknown result formats before native invocation", async (t) => {
  const writes = [];
  t.mock.method(process.stdout, "write", (text) => { writes.push(text); return true; });
  const invoke = t.mock.fn(async () => "must not be called");
  for (const resultFormat of ["private-config-value", "", undefined, null, true, 1, [], {}]) {
    await assert.rejects(runJob({ task: "Answer", resultFormat }, "custom/test-model", invoke), {
      message: "spec.config.resultFormat must be airunway-json-v1 when set",
    });
  }
  assert.equal(invoke.mock.callCount(), 0);
  assert.deepEqual(writes, []);
});

test("job rejects empty or non-string structured output without printing it", async (t) => {
  const writes = [];
  t.mock.method(process.stdout, "write", (text) => { writes.push(text); return true; });
  for (const answer of ["", " \n\t", undefined, null, true, 42, { privateOutput: "value" }, []]) {
    await assert.rejects(runJob({ task: "Answer", resultFormat: "airunway-json-v1" }, "custom/test-model", async () => answer), {
      message: "OpenClaw returned no text answer.",
    });
  }
  assert.deepEqual(writes, []);
});

test("failed native tasks never emit a result marker", async (t) => {
  const writes = [];
  t.mock.method(process.stdout, "write", (text) => { writes.push(text); return true; });
  for (const config of [{ task: "Answer" }, { task: "Answer", resultFormat: "airunway-json-v1" }]) {
    for (const meta of [{ error: { message: "private-provider-error" } }, { aborted: true }, { livenessState: "blocked" }]) {
      await assert.rejects(runJob(config, "custom/test-model", async () => oneShotAnswer(result("Everything succeeded", meta))), {
        message: "OpenClaw task failed according to native status.",
      });
    }
    await assert.rejects(runJob(config, "custom/test-model", async () => { throw new Error("OpenClaw task process failed."); }), {
      message: "OpenClaw task process failed.",
    });
  }
  assert.deepEqual(writes, []);
});
