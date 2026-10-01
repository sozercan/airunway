package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func textTestResource(noun string) Object {
	resource := Object{"apiVersion": "airunway.ai/v1alpha1", "kind": resourceTypes[noun].Kind,
		"metadata": Object{"name": "demo", "namespace": "team", "generation": 1,
			"managedFields": []any{Object{"manager": "hidden-manager"}}, "annotations": Object{"hidden": "hidden-value"}},
		"spec": Object{"provider": Object{"name": "dynamo"}, "framework": Object{"name": "crewai"},
			"model":        Object{"id": "Qwen/Qwen3-8B", "deploymentRef": Object{"name": "reasoning-gpu"}},
			"config":       Object{"systemPrompt": "hidden-prompt"},
			"capabilities": Object{"backend": "container", "engines": []any{Object{"name": "vllm"}}}},
		"status": Object{"phase": "Running", "observedGeneration": 1, "ready": true,
			"replicas": Object{"ready": 2, "desired": 3}, "version": "v1"}}
	return resource
}

func TestResourceTextTables(t *testing.T) {
	for _, tc := range []struct {
		noun            string
		headers, values []string
	}{
		{"model", []string{"NAME", "NAMESPACE", "STATUS", "PROVIDER", "ENGINE", "READY"}, []string{"demo", "team", "Running", "dynamo", "2/3"}},
		{"agent", []string{"NAME", "NAMESPACE", "STATUS", "FRAMEWORK", "MODEL", "READY"}, []string{"demo", "crewai", "reasoning-gpu"}},
		{"provider", []string{"NAME", "STATUS", "ENGINES", "VERSION"}, []string{"demo", "Ready", "vllm", "v1"}},
		{"framework", []string{"NAME", "STATUS", "BACKEND", "VERSION"}, []string{"demo", "Ready", "container", "v1"}},
	} {
		t.Run(tc.noun, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeResourceOutput(&IO{Out: &out}, Flags{}, tc.noun, "list", []Object{textTestResource(tc.noun)}); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) != 2 || !reflect.DeepEqual(strings.Fields(lines[0]), tc.headers) {
				t.Fatalf("table: %s", &out)
			}
			for _, value := range tc.values {
				if !strings.Contains(lines[1], value) {
					t.Errorf("missing %s in %s", value, &out)
				}
			}
			if strings.Contains(out.String(), "\t") {
				t.Fatal("table was not aligned")
			}
		})
	}
}

func TestResourceSummaryAndWaitText(t *testing.T) {
	for _, noun := range []string{"model", "agent"} {
		for _, action := range []string{"get", "wait", "created", "updated"} {
			t.Run(noun+"/"+action, func(t *testing.T) { checkResourceSummary(t, noun, action) })
		}
	}
}

func checkResourceSummary(t *testing.T, noun, action string) {
	t.Helper()
	var out bytes.Buffer
	if err := writeResourceOutput(&IO{Out: &out}, Flags{}, noun, action, textTestResource(noun)); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "demo") || !strings.Contains(text, "2/3") {
		t.Fatalf("missing summary: %s", text)
	}
	for _, hidden := range []string{"managedFields", "hidden-", "systemPrompt", "GPUs", "pods"} {
		if strings.Contains(text, hidden) {
			t.Fatalf("summary disclosed or inferred %s", hidden)
		}
	}
	if action != "get" && strings.Count(text, "\n") != 1 {
		t.Fatalf("verbose receipt: %s", text)
	}
}

func TestStructuredResourceOutputUnchanged(t *testing.T) {
	resource := textTestResource("agent")
	for _, format := range []string{"json", "yaml"} {
		var out, expected bytes.Buffer
		flags := Flags{"output": {format}}
		if err := writeResourceOutput(&IO{Out: &out}, flags, "agent", "get", resource); err != nil {
			t.Fatal(err)
		}
		if err := writeOutput(&IO{Out: &expected}, flags, resource); err != nil {
			t.Fatal(err)
		}
		if out.String() != expected.String() || !strings.Contains(out.String(), "managedFields") {
			t.Fatal("structured object changed")
		}
	}
}

func TestApplyTextReceipts(t *testing.T) {
	for _, tc := range []struct{ dry, want string }{{"", "applied"}, {"server", "validated (dry run)"}} {
		var out bytes.Buffer
		flags := Flags{}
		if tc.dry != "" {
			flags["dry-run"] = []string{tc.dry}
		}
		if err := writeApplyOutput(&IO{Out: &out}, flags, []Object{textTestResource("model")}); err != nil {
			t.Fatal(err)
		}
		if out.String() != "modeldeployment/demo "+tc.want+"\n" {
			t.Fatalf("receipt: %q", &out)
		}
	}
}

func TestDoctorTextDiagnostics(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var out, stderr bytes.Buffer
		client := &managementFakeClient{response: func(call managementCall, value Object) Object {
			return Object{"status": Object{"allowed": true}}
		}}
		if fail {
			client.failure = func(managementCall) error { return cliError(3, "HTTP_403", "Access denied") }
		}
		code := Run(context.Background(), []string{"doctor", "-n", "team", "--context", "test-context"}, RunOptions{
			Client: client, Config: &CLIConfig{}, IO: &IO{Out: &out, Err: &stderr}})
		if fail {
			if code != 1 || !strings.Contains(out.String(), "0/6 access checks passed") || strings.Count(out.String(), "FAIL ") != 6 {
				t.Fatalf("missing failures: %d %s", code, &out)
			}
		} else if code != 0 || strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), "6/6 access checks passed") {
			t.Fatalf("success summary: %d %s", code, &out)
		}
	}
}

func TestDoctorJSONPreservesCheckDetails(t *testing.T) {
	var out bytes.Buffer
	c := &CommandContext{IO: &IO{Out: &out}, Flags: Flags{"output": {"json"}}, ContextName: "test-context", Namespace: "team"}
	checks := []Object{{"check": "model", "ok": false, "detail": "Access denied"}}
	if err := writeDoctorOutput(c, checks); err != nil {
		t.Fatal(err)
	}
	var result Object
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if stringAt(result, "context") != c.ContextName || len(objects(result["checks"])) != 1 {
		t.Fatal("diagnostic schema changed")
	}
}

func TestSummarySanitizesEndpointAndControlCharacters(t *testing.T) {
	resource := textTestResource("agent")
	object(resource["metadata"])["name"] = "demo\nforged\trow\x1b"
	// Construct a synthetic credential-bearing URL; it must never reach the summary.
	address := url.URL{Scheme: "https", Host: "example.test", Path: "/agent",
		User: url.UserPassword("user", "secret"), RawQuery: "token=hidden", Fragment: "fragment"}
	object(resource["status"])["runtime"] = Object{"address": address.String()}
	var out bytes.Buffer
	if err := writeResourceSummary(&IO{Out: &out}, "agent", resource); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret", "token=", "hidden", "\x1b"} {
		if strings.Contains(out.String(), forbidden) {
			t.Fatalf("unsafe summary %q", forbidden)
		}
	}
	if !strings.Contains(out.String(), "https://example.test/agent") {
		t.Fatal("missing safe endpoint")
	}
}

func TestWaitFailureIncludesFreshReason(t *testing.T) {
	resource := textTestResource("agent")
	object(resource["status"])["conditions"] = []any{
		Object{"type": "Ready", "status": "False", "reason": "StaleFailure", "observedGeneration": 0},
		Object{"type": "ProviderReady", "status": "False", "reason": "MissingImage", "observedGeneration": 1},
	}
	err := waitFailure(resource)
	if !strings.Contains(err.Error(), "MissingImage") || strings.Contains(err.Error(), "StaleFailure") {
		t.Fatalf("failure reason: %v", err)
	}
}

func TestOneShotSummaryDoesNotImplyServingReplicas(t *testing.T) {
	resource := textTestResource("agent")
	object(resource["spec"])["lifecycle"] = "job"
	object(resource["status"])["phase"] = "Completed"
	var out bytes.Buffer
	if err := writeResourceOutput(&IO{Out: &out}, Flags{}, "agent", "get", resource); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "replicas") || !strings.Contains(out.String(), "once") {
		t.Fatalf("job summary: %s", &out)
	}
}

func TestModelSummaryShowsReportedPlan(t *testing.T) {
	resource := textTestResource("model")
	status := object(resource["status"])
	status["provider"] = Object{"name": "dynamo", "intent": Object{"plan": Object{
		"source": "selectedConfig", "engine": "vllm", "servingMode": "disaggregated",
		"workers": []any{
			Object{"name": "VllmDecodeWorker", "role": "decode", "replicas": 2, "gpusPerReplica": 1},
			Object{"name": "VllmPrefillWorker", "role": "prefill", "replicas": 1, "gpusPerReplica": 1,
				"tensorParallelism": 2},
			Object{"name": "Custom\x1bWorker"},
		}}}}
	var out bytes.Buffer
	if err := writeResourceOutput(&IO{Out: &out}, Flags{}, "model", "get", resource); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"Plan:            disaggregated, selected by Dynamo",
		"Decode worker:   VllmDecodeWorker, 2 replicas, 1 GPU each",
		"Prefill worker:  VllmPrefillWorker, 1 replica, 1 GPU, tensor parallel 2",
		"Worker:          Custom Worker\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}

	object(object(status["provider"])["intent"])["plan"] = Object{"source": "workload"}
	out.Reset()
	if err := writeResourceOutput(&IO{Out: &out}, Flags{}, "model", "get", resource); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Plan:            layout not reported, read from the serving workload") {
		t.Fatalf("fallback plan not labeled:\n%s", &out)
	}
}

func TestSummaryWithoutPlanHasNoPlanRows(t *testing.T) {
	for _, noun := range []string{"model", "agent"} {
		var out bytes.Buffer
		if err := writeResourceOutput(&IO{Out: &out}, Flags{}, noun, "get", textTestResource(noun)); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "Plan:") || strings.Contains(out.String(), "worker:") {
			t.Fatalf("%s summary invented a plan:\n%s", noun, &out)
		}
	}
}

func TestSummaryOmitsDeliberatelyDisabledGateway(t *testing.T) {
	resource := textTestResource("model")
	object(resource["status"])["conditions"] = []any{
		Object{"type": "GatewayReady", "status": "False", "reason": "GatewayDisabled"},
		Object{"type": "ProviderReady", "status": "False", "reason": "ImagePullBackOff"},
	}
	var out bytes.Buffer
	if err := writeResourceOutput(&IO{Out: &out}, Flags{}, "model", "get", resource); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "GatewayDisabled") || !strings.Contains(out.String(), "ImagePullBackOff") {
		t.Fatalf("expected only real problems in the summary:\n%s", &out)
	}
}
