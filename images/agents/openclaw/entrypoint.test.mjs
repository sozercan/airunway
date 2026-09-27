import assert from "node:assert/strict";
import test from "node:test";

import { oneShotAnswer } from "./entrypoint.mjs";

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
