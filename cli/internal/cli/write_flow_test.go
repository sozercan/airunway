package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func writeFlowRun(t *testing.T, client ClusterClient, args ...string) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	args = append(args, "--namespace=team", "--output=json")
	code := Run(context.Background(), args, RunOptions{
		IO: &IO{Out: &out, Err: &stderr}, Client: client,
		Config: &CLIConfig{Version: 1, Contexts: map[string]*ContextDefaults{}},
	})
	return code, out.String(), stderr.String()
}

type writeFlowZeroCase struct {
	name, action string
	existing     any
	flags        []string
	result       int
	wantCode     int
	wantWrite    bool
	wantPoll     bool
}

func TestModelWriteZeroReplicaWaitFlow(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	for _, tc := range []writeFlowZeroCase{
		{"create zero default", "create", nil, []string{"--replicas=0"}, 0, 0, true, false},
		{"create zero no wait", "create", nil, []string{"--replicas=0", "--wait=false"}, 0, 0, true, false},
		{"create zero explicit wait rejected", "create", nil, []string{"--replicas=0", "--wait=true"}, 0, 2, false, false},
		{"update to zero default", "update", 1, []string{"--replicas=0"}, 0, 0, true, false},
		{"update zero no wait", "update", 1, []string{"--replicas=0", "--wait=false"}, 0, 0, true, false},
		{"update zero explicit wait rejected", "update", 1, []string{"--replicas=0", "--wait=true"}, 0, 2, false, false},
		{"update existing zero default", "update", json.Number("0"), []string{"--context-length=4096"}, 0, 0, true, false},
		{"update existing zero explicit wait rejected", "update", 0, []string{"--context-length=4096", "--wait=true"}, 0, 2, false, false},
		{"create positive default waits", "create", nil, []string{"--replicas=1"}, 1, 0, true, true},
		{"create omitted replicas waits", "create", nil, nil, 1, 0, true, true},
		{"update positive default waits", "update", 0, []string{"--replicas=1"}, 1, 0, true, true},
		{"update positive explicit waits", "update", 1, []string{"--context-length=4096", "--wait=true"}, 1, 0, true, true},
		{"update omitted existing replicas waits", "update", nil, []string{"--context-length=4096"}, 1, 0, true, true},
		{"positive no wait preserved", "create", nil, []string{"--replicas=1", "--wait=false"}, 1, 0, true, false},
		{"zero server preview ignores wait", "create", nil, []string{"--replicas=0", "--wait=true", "--dry-run=server"}, 0, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runWriteFlowZeroCase(t, tc)
		})
	}
}

type writeFlowFrameworkCase struct {
	name       string
	flags      []string
	denyKind   string
	framework  error
	wantCode   int
	wantCreate bool
}

func TestAgentForbiddenFrameworkPreflightFlow(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	for _, tc := range []writeFlowFrameworkCase{
		{"plain external API", nil, "", cliError(3, "HTTP_403", "forbidden"), 0, true},
		{"image and once remain controller validated", []string{"--image=example.test/agent:v1", "--mode=once", "--task=hello"}, "", cliError(3, "HTTP_403", "forbidden"), 0, true},
		{"credential read retained", []string{"--model-credential=api-key/API_KEY"}, "", cliError(3, "HTTP_403", "forbidden"), 0, true},
		{"credential denial blocks create", []string{"--model-credential=api-key/API_KEY"}, "Secret", cliError(3, "HTTP_403", "forbidden"), 3, false},
		{"model reference read retained", []string{"--model-ref=llama"}, "", cliError(3, "HTTP_403", "forbidden"), 0, true},
		{"model reference denial blocks create", []string{"--model-ref=llama"}, "ModelDeployment", cliError(3, "HTTP_403", "forbidden"), 3, false},
		{"gateway reference denial blocks create", []string{"--model-gateway=edge", "--model-id=model"}, "Gateway", cliError(3, "HTTP_403", "forbidden"), 3, false},
		{"missing framework blocks create", nil, "", cliError(1, "HTTP_404", "not found"), 1, false},
		{"other framework error blocks create", nil, "", errors.New("transport failure"), 1, false},
		{"preset catalog read remains required", []string{"--preset=langgraph/example"}, "AgentProviderConfig", nil, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runWriteFlowFrameworkCase(t, tc)
		})
	}
}

func runWriteFlowZeroCase(t *testing.T, tc writeFlowZeroCase) {
	t.Helper()
	model := accessTestModel()
	object(model["metadata"])["name"], object(model["metadata"])["namespace"] = "demo", "team"
	if tc.existing != nil {
		object(model["spec"])["scaling"] = Object{"replicas": tc.existing}
	}
	provider := Object{"kind": "InferenceProviderConfig", "metadata": Object{"name": "vllm"}, "status": Object{"ready": true}}
	client := &managementFakeClient{resources: []Object{model, provider}}
	client.response = func(call managementCall, resource Object) Object {
		pending := cloneObject(resource)
		object(pending["metadata"])["generation"] = 3
		object(pending["metadata"])["uid"] = "demo-uid"
		object(pending["spec"])["scaling"] = Object{"replicas": tc.result}
		pending["status"] = Object{"phase": "Pending", "observedGeneration": 3}
		ready := cloneObject(pending)
		ready["status"] = Object{"phase": "Running", "observedGeneration": 3, "conditions": accessTestObjects(Object{"type": "Ready", "status": "True", "observedGeneration": 3})}
		client.resources = []Object{ready, provider}
		return pending
	}
	args := []string{"model", tc.action, "demo", "--timeout=50ms"}
	if tc.wantPoll {
		args[len(args)-1] = "--timeout=1s"
	}
	if tc.action == "create" {
		args = append(args, "--id=hf://org/model", "--provider=vllm")
	}
	code, out, stderr := writeFlowRun(t, client, append(args, tc.flags...)...)
	if code != tc.wantCode {
		t.Fatalf("code=%d want=%d: %s", code, tc.wantCode, stderr)
	}
	if (len(client.writes()) == 1) != tc.wantWrite {
		t.Fatalf("writes=%d wantWrite=%v", len(client.writes()), tc.wantWrite)
	}
	polls := writeFlowReadinessPolls(client.calls)
	if (polls > 0) != tc.wantPoll {
		t.Fatalf("readiness polls=%d wantPoll=%v", polls, tc.wantPoll)
	}
	if tc.wantCode == 2 {
		if out != "" || !strings.Contains(stderr, "zero") || !strings.Contains(stderr, "termination") {
			t.Fatalf("explicit wait rejection must explain semantics: %s", stderr)
		}
	} else if !tc.wantPoll && !strings.Contains(out, `"Pending"`) {
		t.Fatalf("must return submitted state, not claim ready or terminated: %s", out)
	}
}

func runWriteFlowFrameworkCase(t *testing.T, tc writeFlowFrameworkCase) {
	t.Helper()
	model := accessTestModel()
	object(model["metadata"])["namespace"] = "team"
	secret := Object{"kind": "Secret", "metadata": Object{"name": "api-key", "namespace": "team"}}
	client := &managementFakeClient{resources: []Object{model, secret}, failure: func(call managementCall) error {
		if tc.denyKind != "" && call.typ.Kind == tc.denyKind {
			return cliError(3, "HTTP_403", "forbidden")
		}
		if call.typ.Kind == "AgentProviderConfig" {
			return tc.framework
		}
		return nil
	}}
	args := []string{"agent", "create", "helper", "--framework=langgraph", "--wait=false"}
	if !strings.Contains(strings.Join(tc.flags, " "), "--model-ref=") && !strings.Contains(strings.Join(tc.flags, " "), "--model-gateway=") {
		args = append(args, "--model-url=http://model.test/v1", "--model-api=openai", "--model-id=model")
	}
	code, _, stderr := writeFlowRun(t, client, append(args, tc.flags...)...)
	if code != tc.wantCode {
		t.Fatalf("code=%d want=%d: %s", code, tc.wantCode, stderr)
	}
	if (len(client.writes()) == 1) != tc.wantCreate {
		t.Fatalf("writes=%d wantCreate=%v", len(client.writes()), tc.wantCreate)
	}
	if tc.wantCreate && stringAt(client.writes()[0].body, "metadata", "namespace") != "team" {
		t.Fatal("create escaped the caller's namespace")
	}
	assertWriteFlowRequiredRead(t, tc.name, client.calls)
}

func writeFlowReadinessPolls(calls []managementCall) int {
	written, polls := false, 0
	for _, call := range calls {
		if call.op == "create" || call.op == "patch" {
			written = true
		} else if written && call.typ.Kind == "ModelDeployment" {
			polls++
		}
	}
	return polls
}

func assertWriteFlowRequiredRead(t *testing.T, name string, calls []managementCall) {
	t.Helper()
	if !strings.Contains(name, "read retained") {
		return
	}
	kind := "Secret"
	if strings.Contains(name, "model reference") {
		kind = "ModelDeployment"
	}
	for _, call := range calls {
		if call.op == "get" && call.typ.Kind == kind && call.namespace == "team" {
			return
		}
	}
	t.Fatalf("required %s read was skipped", kind)
}
