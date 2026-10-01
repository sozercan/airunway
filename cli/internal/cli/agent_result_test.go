package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func agentResultTestRecord(answer string) string {
	data, _ := json.Marshal(Object{"output": answer})
	return agentResultPrefix + string(data) + "\n"
}

func TestAgentRunReturnsOnlyFramedAnswer(t *testing.T) {
	for _, format := range []string{"text", "json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			client, command, out, stderr := agentRunTestSetup(t)
			command.Flags["output"] = []string{format}
			answer := "Answer line 1\nAnswer line 2"
			client.onRaw = func(context.Context, string, string, any, RequestOptions) (*http.Response, error) {
				logs := "diagnostic before\n" + agentResultTestRecord(answer) + "diagnostic after\n"
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(logs))}, nil
			}
			if err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false); err != nil {
				t.Fatal(err)
			}
			var expected bytes.Buffer
			if err := writeOutput(&IO{Out: &expected}, command.Flags, answer); err != nil {
				t.Fatal(err)
			}
			if out.String() != expected.String() || strings.Contains(out.String(), "diagnostic") {
				t.Fatalf("wrong result: %q", out)
			}
			if !strings.Contains(stderr.String(), "completed") || len(client.created) != 1 {
				t.Fatal("missing progress or duplicate submission")
			}
			if stringAt(client.created[0], "spec", "config", "resultFormat") != agentResultFormat {
				t.Fatal("result format was not requested")
			}
		})
	}
}

func TestAgentResultParser(t *testing.T) {
	for _, tc := range []struct {
		name, logs, answer string
		success            bool
	}{
		{"plain", "5\n", "", false},
		{"unframed-json", `{"output":"5"}`, "", false},
		{"malformed", agentResultPrefix + "{\n", "", false},
		{"empty", agentResultTestRecord(" \n"), "", false},
		{"non-text", agentResultPrefix + `{"output":3}`, "", false},
		{"unknown-fields", agentResultPrefix + `{"output":"5","private":"no"}`, "", false},
		{"duplicate-key", agentResultPrefix + `{"output":"5","output":"6"}`, "", false},
		{"duplicate-record", agentResultTestRecord("5") + agentResultTestRecord("6"), "", false},
		{"multiline", agentResultTestRecord("first\nsecond"), "first\nsecond", true},
		{"marker-in-answer", agentResultTestRecord("one\n" + agentResultPrefix + "two"), "one\n" + agentResultPrefix + "two", true},
		{"diagnostics", "before\n" + agentResultTestRecord("5") + "after\n", "5", true},
		{"size-bound", strings.Repeat("x", maxInput+1), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer, err := agentRunDecodeResult(strings.NewReader(tc.logs))
			if tc.success {
				if err != nil || answer != tc.answer {
					t.Fatalf("result %q: %v", answer, err)
				}
			} else if err == nil || answer != "" {
				t.Fatal("invalid result accepted")
			}
		})
	}
}

func TestAgentRunResultFormatConflictDoesNotSubmit(t *testing.T) {
	client, command, _, _ := agentRunTestSetup(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"resultFormat":"some-other-format"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	command.Flags["config-file"] = []string{path}
	err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
	agentRunTestError(t, err, "USAGE", 2)
	if len(client.created) != 0 {
		t.Fatal("conflicting format submitted a task")
	}
}

func TestAgentRunWithReplacedPodDoesNotPrintResult(t *testing.T) {
	client, command, out, _ := agentRunTestSetup(t)
	client.onRaw = func(context.Context, string, string, any, RequestOptions) (*http.Response, error) {
		object(client.resources[2]["metadata"])["uid"] = "replacement-uid"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(agentResultTestRecord("5")))}, nil
	}
	err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
	agentRunTestError(t, err, "DELETED", 1)
	if out.Len() != 0 {
		t.Fatal("result from replaced pod was printed")
	}
}

func TestAgentRunPublicCommandDispatch(t *testing.T) {
	client, _, _, _ := agentRunTestSetup(t)
	client.onRaw = func(context.Context, string, string, any, RequestOptions) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(agentResultTestRecord("5")))}, nil
	}
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"agent", "run", "helper", "-n", "team", "--framework", "langgraph",
		"--model-url", "https://model.example/v1", "--model-api", "openai", "--model-id", "test-model", "--task", "2+3"},
		RunOptions{Client: client, Config: &CLIConfig{}, IO: &IO{Out: &out, Err: &stderr}})
	if code != 0 || out.String() != "5\n" {
		t.Fatalf("run dispatch: %d %q %s", code, &out, &stderr)
	}
	if get(client.created[0], "spec", "config", "image") != nil {
		t.Fatal("run bypassed server image defaulting")
	}
}

func TestAgentRunIgnoresUnrelatedJobPermissions(t *testing.T) {
	for _, unrelatedFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated-last", true: "unrelated-first"}[unrelatedFirst], func(t *testing.T) {
			client, _, _, _ := agentRunTestSetup(t)
			job, target := client.resources[1], client.resources[2]
			other := cloneObject(target)
			object(other["metadata"])["name"], object(other["metadata"])["uid"] = "other-pod", "other-pod-uid"
			ref := objects(get(other, "metadata", "ownerReferences"))[0]
			ref["name"], ref["uid"] = "other-job", "other-job-uid"
			client.resources = []Object{target, other}
			if unrelatedFirst {
				client.resources = []Object{other, target}
			}
			client.onGet = func(context.Context, ResourceType, string, string) (Object, error) {
				return nil, cliError(3, "HTTP_403", "unrelated Job is forbidden")
			}
			selected, err := agentRunSelectPod(context.Background(), client, job, "team")
			if err != nil || stringAt(selected, "metadata", "uid") != "pod-uid" {
				t.Fatalf("unrelated Job blocked result: %v", err)
			}
			for _, call := range client.snapshot() {
				if call.Method == "get" {
					t.Fatalf("read unrelated owner: %+v", call)
				}
			}
		})
	}
}

func TestAgentRunDirectOwnerCheckIgnoresOtherReferences(t *testing.T) {
	client, _, _, _ := agentRunTestSetup(t)
	job, pod := client.resources[1], client.resources[2]
	refs := array(get(pod, "metadata", "ownerReferences"))
	other := Object{"apiVersion": "batch/v1", "kind": agentJobKind, "name": "private-job", "uid": "other-uid"}
	object(pod["metadata"])["ownerReferences"] = append([]any{other}, refs...)
	client.onGet = func(context.Context, ResourceType, string, string) (Object, error) {
		return nil, cliError(3, "HTTP_403", "unrelated owner is forbidden")
	}
	selected, err := agentRunSelectPod(context.Background(), client, job, "team")
	if err != nil || stringAt(selected, "metadata", "uid") != "pod-uid" {
		t.Fatalf("unrelated owner blocked result: %v", err)
	}
}

func TestAgentRunRejectsMalformedDirectJobIdentity(t *testing.T) {
	for _, field := range []string{"apiVersion", "kind", "name", "uid"} {
		t.Run(field, func(t *testing.T) {
			client, _, _, _ := agentRunTestSetup(t)
			objects(get(client.resources[2], "metadata", "ownerReferences"))[0][field] = "not-the-target"
			_, err := agentRunSelectPod(context.Background(), client, client.resources[1], "team")
			agentRunTestError(t, err, "RESULT_UNAVAILABLE", 1)
		})
	}
}

func TestAgentRunWithChangedPodOwnershipDoesNotPrintResult(t *testing.T) {
	client, command, out, _ := agentRunTestSetup(t)
	client.onRaw = func(context.Context, string, string, any, RequestOptions) (*http.Response, error) {
		objects(get(client.resources[2], "metadata", "ownerReferences"))[0]["uid"] = "another-job-uid"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(agentResultTestRecord("5")))}, nil
	}
	err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
	agentRunTestError(t, err, "DELETED", 1)
	if out.Len() != 0 {
		t.Fatal("result from reassigned pod was printed")
	}
}
