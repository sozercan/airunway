package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type agentRunTestClient struct {
	*accessFakeClient
	created  []Object
	dryRuns  []bool
	onCreate func(context.Context, Object, bool) (Object, error)
}

func (c *agentRunTestClient) Create(ctx context.Context, resource Object, dry bool) (Object, error) {
	c.created = append(c.created, cloneObject(resource))
	c.dryRuns = append(c.dryRuns, dry)
	if c.onCreate != nil {
		return c.onCreate(ctx, resource, dry)
	}
	result := cloneObject(resource)
	result["metadata"] = Object{"name": "helper", "namespace": "team", "uid": "agent-uid", "generation": 1}
	result["status"] = agentRunTestStatus("Completed")
	c.resources = append(c.resources, result)
	return result, nil
}

func agentRunTestStatus(phase string) Object {
	status := Object{"phase": phase, "observedGeneration": 1,
		"runtime": Object{"workloadRef": Object{"apiVersion": "batch/v1", "kind": "Job", "name": "helper", "namespace": "team"}}}
	if phase == "Completed" {
		status["conditions"] = []any{Object{"type": "ProviderReady", "status": "True", "reason": "JobCompleted", "observedGeneration": 1}}
	}
	return status
}

func agentRunTestSetup(t *testing.T) (*agentRunTestClient, *CommandContext, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	job := Object{"apiVersion": "batch/v1", "kind": "Job", "metadata": Object{"name": "helper", "namespace": "team", "uid": "job-uid",
		"ownerReferences": []any{Object{"apiVersion": "airunway.ai/v1alpha1", "kind": "AgentDeployment", "name": "helper", "uid": "agent-uid", "controller": true}}}}
	pod := Object{"apiVersion": "v1", "kind": "Pod", "metadata": Object{"name": "helper-pod", "namespace": "team", "uid": "pod-uid",
		"ownerReferences": []any{Object{"apiVersion": "batch/v1", "kind": "Job", "name": "helper", "uid": "job-uid", "controller": true}}},
		"spec":   Object{"containers": []any{Object{"name": "agent"}}},
		"status": Object{"phase": "Succeeded", "containerStatuses": []any{Object{"name": "agent", "state": Object{"terminated": Object{"exitCode": 0}}}}}}
	client := &agentRunTestClient{accessFakeClient: &accessFakeClient{resources: []Object{managementTestFramework("langgraph", nil), job, pod}}}
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	command := &CommandContext{
		Context: context.Background(), Namespace: "team", ContextName: "test",
		Flags:  Flags{"framework": {"langgraph"}, "model-url": {"https://model.example/v1"}, "model-id": {"test-model"}, "model-api": {"openai"}, "task": {"What is 2 + 3?"}, "image": {"registry.example/agent:v1"}, "timeout": {"1s"}},
		IO:     &IO{In: strings.NewReader(""), Out: out, Err: stderr},
		Client: func() (ClusterClient, error) { return client, nil },
	}
	return client, command, out, stderr
}

func agentRunTestError(t *testing.T, err error, code string, exit int) {
	t.Helper()
	var failure *CLIError
	if !errors.As(err, &failure) || failure.Code != code || failure.ExitCode != exit {
		t.Fatalf("error = %v, want %s, exit %d", err, code, exit)
	}
}

func agentRunTestLogCalls(client *agentRunTestClient) []accessFakeCall {
	var calls []accessFakeCall
	for _, call := range client.snapshot() {
		if strings.HasSuffix(call.Path, "/log") {
			calls = append(calls, call)
		}
	}
	return calls
}

func TestAgentRunDryRun(t *testing.T) {
	for _, dry := range []string{"client", "server"} {
		t.Run(dry, func(t *testing.T) { checkAgentRunDryRun(t, dry) })
	}
}

func checkAgentRunDryRun(t *testing.T, dry string) {
	t.Helper()
	client, command, out, stderr := agentRunTestSetup(t)
	command.Flags["dry-run"] = []string{dry}
	command.Flags["output"] = []string{"json"}
	command.Flags["wait"] = []string{"false"}
	original := command.Flags.Copy()
	err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, dry == "client")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"lifecycle": "job"`) || !strings.Contains(out.String(), `"image": "registry.example/agent:v1"`) {
		t.Fatalf("not a one-shot manifest with the explicit image: %s", out)
	}
	if !reflect.DeepEqual(command.Flags, original) {
		t.Fatalf("caller flags changed: %v", command.Flags)
	}
	if stderr.Len() != 0 || len(agentRunTestLogCalls(client)) != 0 {
		t.Fatalf("dry run must not wait or retrieve results: %s, %+v", stderr, client.snapshot())
	}
	if dry == "client" {
		if len(client.created) != 0 || len(client.snapshot()) != 0 {
			t.Fatalf("client preview contacted cluster: %+v", client.snapshot())
		}
	} else if len(client.created) != 1 || !client.dryRuns[0] {
		t.Fatalf("server preview writes = %d, dry = %v", len(client.created), client.dryRuns)
	}
}

func TestAgentRunValidatesBeforeSubmission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		words []string
		flags Flags
	}{
		{"missing name", []string{"agent", "run"}, nil},
		{"extra name", []string{"agent", "run", "helper", "another"}, nil},
		{"invalid name", []string{"agent", "run", "Not-Valid"}, nil},
		{"unsupported flag", nil, Flags{"tail": {"100"}}},
		{"long-running mode", nil, Flags{"mode": {"deployment"}}},
		{"invalid mode", nil, Flags{"mode": {"something"}}},
		{"no waiting", nil, Flags{"wait": {"false"}}},
		{"invalid dry run", nil, Flags{"dry-run": {"yes"}}},
		{"missing task", nil, Flags{"task": {""}}},
		{"bad timeout", nil, Flags{"timeout": {"0s"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, command, out, _ := agentRunTestSetup(t)
			words := tc.words
			if words == nil {
				words = []string{"agent", "run", "helper"}
			}
			maps.Copy(command.Flags, tc.flags)
			err := runAgentOnce(words, command, &CLIConfig{}, false)
			agentRunTestError(t, err, "USAGE", 2)
			if len(client.created) != 0 || out.Len() != 0 {
				t.Fatalf("invalid run submitted or printed output: %+v, %s", client.created, out)
			}
		})
	}
}

func TestAgentRunFailureDoesNotReadLogsOrResubmit(t *testing.T) {
	for _, phase := range []string{"Failed", "Error"} {
		t.Run(phase, func(t *testing.T) {
			client, command, out, _ := agentRunTestSetup(t)
			client.onCreate = func(_ context.Context, resource Object, _ bool) (Object, error) {
				resource = cloneObject(resource)
				resource["metadata"] = Object{"name": "helper", "namespace": "team", "uid": "agent-uid", "generation": 1}
				resource["status"] = agentRunTestStatus(phase)
				return resource, nil
			}
			err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
			agentRunTestError(t, err, "FAILED", 1)
			if len(client.created) != 1 || len(agentRunTestLogCalls(client)) != 0 || out.Len() != 0 {
				t.Fatalf("failure submitted %d times, logs=%v, stdout=%q", len(client.created), agentRunTestLogCalls(client), out)
			}
		})
	}
}

func TestAgentRunNoDuplicateSubmissionOnCreateError(t *testing.T) {
	for _, code := range []string{"HTTP_409", "HTTP_500"} {
		t.Run(code, func(t *testing.T) {
			client, command, out, _ := agentRunTestSetup(t)
			client.onCreate = func(context.Context, Object, bool) (Object, error) {
				return nil, cliError(1, code, "Creation failed or its outcome is unknown.")
			}
			err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
			agentRunTestError(t, err, code, 1)
			if len(client.created) != 1 || len(agentRunTestLogCalls(client)) != 0 || out.Len() != 0 {
				t.Fatalf("create error retried or produced output: %+v, %s", client.created, out)
			}
		})
	}
}

func TestAgentRunCanceledBeforeSubmission(t *testing.T) {
	client, command, out, _ := agentRunTestSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command.Context = ctx
	err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
	agentRunTestError(t, err, "CANCELED", 130)
	if len(client.created) != 0 || len(client.snapshot()) != 0 || out.Len() != 0 {
		t.Fatalf("canceled run performed work: %+v, %s", client.snapshot(), out)
	}
}

func TestAgentRunTimeoutAndCancellationAfterSubmission(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "canceled"}[canceled], func(t *testing.T) {
			client, command, out, _ := agentRunTestSetup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			command.Context = ctx
			command.Flags["timeout"] = []string{"10ms"}
			client.onCreate = func(_ context.Context, resource Object, _ bool) (Object, error) {
				resource = cloneObject(resource)
				resource["metadata"] = Object{"name": "helper", "namespace": "team", "uid": "agent-uid", "generation": 1}
				resource["status"] = agentRunTestStatus("Pending")
				if canceled {
					cancel()
				}
				return resource, nil
			}
			err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
			if canceled {
				agentRunTestError(t, err, "CANCELED", 130)
			} else {
				agentRunTestError(t, err, "TIMEOUT", 4)
			}
			if len(client.created) != 1 || len(agentRunTestLogCalls(client)) != 0 || out.Len() != 0 {
				t.Fatalf("interrupted task repeated or returned output: %+v, %s", client.created, out)
			}
		})
	}
}

func TestAgentRunOwnershipChecks(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(job, pod Object)
		code string
		exit int
	}{
		{"missing job UID", func(job, _ Object) { delete(object(job["metadata"]), "uid") }, "UNSUPPORTED", 2},
		{"different agent UID", func(job, _ Object) { objects(get(job, "metadata", "ownerReferences"))[0]["uid"] = "old-agent-uid" }, "UNSUPPORTED", 2},
		{"missing pod UID", func(_, pod Object) { delete(object(pod["metadata"]), "uid") }, "UNSUPPORTED", 2},
		{"different job UID", func(_, pod Object) { objects(get(pod, "metadata", "ownerReferences"))[0]["uid"] = "old-job-uid" }, "RESULT_UNAVAILABLE", 1},
		{"no ownership", func(_, pod Object) { delete(object(pod["metadata"]), "ownerReferences") }, "RESULT_UNAVAILABLE", 1},
		{"nonzero agent exit", func(_, pod Object) {
			object(get(objects(get(pod, "status", "containerStatuses"))[0], "state", "terminated"))["exitCode"] = 1
		}, "RESULT_UNAVAILABLE", 1},
		{"missing agent exit", func(_, pod Object) {
			delete(object(get(objects(get(pod, "status", "containerStatuses"))[0], "state", "terminated")), "exitCode")
		}, "RESULT_UNAVAILABLE", 1},
		{"failed pod", func(_, pod Object) { object(pod["status"])["phase"] = "Failed" }, "RESULT_UNAVAILABLE", 1},
		{"only a sidecar", func(_, pod Object) { objects(get(pod, "spec", "containers"))[0]["name"] = "sidecar" }, "RESULT_UNAVAILABLE", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, command, out, _ := agentRunTestSetup(t)
			tc.edit(client.resources[1], client.resources[2])
			err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
			agentRunTestError(t, err, tc.code, tc.exit)
			if len(agentRunTestLogCalls(client)) != 0 || out.Len() != 0 || len(client.created) != 1 {
				t.Fatalf("unsafe log access or duplicate submission: %+v, %s", client.snapshot(), out)
			}
		})
	}
}

func TestAgentRunUnframedLogsAreNotAResult(t *testing.T) {
	for _, logs := range []string{"", "5\n", "diagnostic\n5\ncleanup\n", `{"result":"looks like an answer"}`, "{malformed"} {
		client, command, out, _ := agentRunTestSetup(t)
		client.onRaw = func(context.Context, string, string, any, RequestOptions) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(logs))}, nil
		}
		err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
		agentRunTestError(t, err, "RESULT_UNAVAILABLE", 1)
		if out.Len() != 0 || len(client.created) != 1 {
			t.Fatalf("unframed logs were printed or task resubmitted: %q, %s", logs, out)
		}
	}
}

func TestAgentRunPreflightRemainsMandatory(t *testing.T) {
	client, command, out, _ := agentRunTestSetup(t)
	command.Flags["model-credential"] = []string{"missing/API_KEY"}
	err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
	agentRunTestError(t, err, "HTTP_404", 1)
	if len(client.created) != 0 || out.Len() != 0 {
		t.Fatalf("run skipped preflight: %+v, %s", client.created, out)
	}
}

func TestAgentRunTaskFileReadOnce(t *testing.T) {
	client, command, _, _ := agentRunTestSetup(t)
	delete(command.Flags, "task")
	command.Flags["task-file"] = []string{"-"}
	command.IO.In = strings.NewReader("task from stdin")
	err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
	agentRunTestError(t, err, "RESULT_UNAVAILABLE", 1)
	if len(client.created) != 1 || stringAt(client.created[0], "spec", "config", "task") != "task from stdin" {
		t.Fatalf("stdin or submission was duplicated: %+v", client.created)
	}
}

func TestAgentRunClientPreviewWithoutKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	client, command, _, _ := agentRunTestSetup(t)
	command.Flags["dry-run"] = []string{"client"}
	command.Client = func() (ClusterClient, error) { t.Fatal("client preview requested a client"); return nil, nil }
	if err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, true); err != nil {
		t.Fatal(err)
	}
	if len(client.created) != 0 {
		t.Fatal("preview submitted a resource")
	}
}

func TestAgentRunSelectsOnlySuccessfulOwnedAgentLogs(t *testing.T) {
	client, command, out, stderr := agentRunTestSetup(t)
	original := command.Flags.Copy()
	pod := client.resources[2]
	object(pod["spec"])["containers"] = []any{Object{"name": "agent"}, Object{"name": "sidecar"}}
	failed := cloneObject(pod)
	object(failed["metadata"])["name"] = "failed-attempt"
	object(failed["metadata"])["uid"] = "failed-pod-uid"
	object(failed["status"])["phase"] = "Failed"
	unowned := cloneObject(pod)
	object(unowned["metadata"])["name"] = "someone-elses-task"
	object(unowned["metadata"])["uid"] = "unowned-pod-uid"
	delete(object(unowned["metadata"]), "ownerReferences")
	client.resources = append(client.resources, failed, unowned)
	err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
	agentRunTestError(t, err, "RESULT_UNAVAILABLE", 1)
	calls := agentRunTestLogCalls(client)
	if len(calls) != 1 || calls[0].Path != "/api/v1/namespaces/team/pods/helper-pod/log" {
		t.Fatalf("read the wrong logs: %+v", calls)
	}
	query := calls[0].Options.Query
	if query.Get("container") != "agent" || query.Get("follow") != "false" || query.Get("timestamps") != "false" || query.Has("tailLines") {
		t.Fatalf("task logs were tailed or from the wrong container: %v", query)
	}
	if !reflect.DeepEqual(command.Flags, original) {
		t.Fatalf("run changed caller flags: %v", command.Flags)
	}
	if len(client.created) != 1 || stringAt(client.created[0], "spec", "lifecycle") != "job" || out.Len() != 0 {
		t.Fatalf("submission or output contract failed: %+v, %s", client.created, out)
	}
	if !strings.Contains(stderr.String(), "Created agent") || !strings.Contains(stderr.String(), "completed") {
		t.Fatalf("missing stderr progress: %s", stderr)
	}
}

func TestAgentRunRejectsAmbiguousSuccessfulAttempts(t *testing.T) {
	client, command, out, _ := agentRunTestSetup(t)
	duplicate := cloneObject(client.resources[2])
	object(duplicate["metadata"])["name"] = "second-success"
	object(duplicate["metadata"])["uid"] = "second-pod-uid"
	client.resources = append(client.resources, duplicate)
	err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
	agentRunTestError(t, err, "RESULT_UNAVAILABLE", 1)
	if len(agentRunTestLogCalls(client)) != 0 || out.Len() != 0 {
		t.Fatalf("ambiguous result read logs or printed output: %+v, %s", client.snapshot(), out)
	}
}

func TestAgentRunPublishedWorkloadChecks(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(Object)
	}{
		{"missing agent UID", func(resource Object) { delete(object(resource["metadata"]), "uid") }},
		{"cross namespace job", func(resource Object) {
			object(get(resource, "status", "runtime", "workloadRef"))["namespace"] = "other-team"
		}},
		{"replaced job", func(resource Object) {
			object(get(resource, "status", "runtime", "workloadRef"))["uid"] = "old-job-uid"
		}},
		{"not a job", func(resource Object) {
			object(get(resource, "status", "runtime", "workloadRef"))["kind"] = "Deployment"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, command, out, _ := agentRunTestSetup(t)
			client.onCreate = func(_ context.Context, resource Object, _ bool) (Object, error) {
				resource = cloneObject(resource)
				resource["metadata"] = Object{"name": "helper", "namespace": "team", "uid": "agent-uid", "generation": 1}
				resource["status"] = agentRunTestStatus("Completed")
				tc.edit(resource)
				return resource, nil
			}
			err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
			agentRunTestError(t, err, "UNSUPPORTED", 2)
			if len(agentRunTestLogCalls(client)) != 0 || out.Len() != 0 {
				t.Fatalf("unsafe workload read: %+v, %s", client.snapshot(), out)
			}
		})
	}
}

func TestAgentRunWaitsForCompletedNotMerelyReady(t *testing.T) {
	client, command, out, _ := agentRunTestSetup(t)
	command.Flags["timeout"] = []string{"10ms"}
	client.onCreate = func(_ context.Context, resource Object, _ bool) (Object, error) {
		resource = cloneObject(resource)
		resource["metadata"] = Object{"name": "helper", "namespace": "team", "uid": "agent-uid", "generation": 1}
		resource["status"] = Object{"phase": "Running", "observedGeneration": 1, "conditions": []any{Object{"type": "Ready", "status": "True", "observedGeneration": 1}}}
		return resource, nil
	}
	err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
	agentRunTestError(t, err, "TIMEOUT", 4)
	if out.Len() != 0 || len(agentRunTestLogCalls(client)) != 0 || len(client.created) != 1 {
		t.Fatalf("ready Job was treated as completed: %+v, %s", client.snapshot(), out)
	}
}

func TestAgentRunLogRequestCancellationAndTimeout(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "canceled"}[canceled], func(t *testing.T) {
			client, command, out, _ := agentRunTestSetup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			command.Context = ctx
			command.Flags["timeout"] = []string{"50ms"}
			client.onRaw = func(ctx context.Context, _ string, _ string, _ any, _ RequestOptions) (*http.Response, error) {
				if canceled {
					cancel()
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}
			err := runAgentOnce([]string{"agent", "run", "helper"}, command, &CLIConfig{}, false)
			if canceled {
				agentRunTestError(t, err, "CANCELED", 130)
			} else {
				agentRunTestError(t, err, "TIMEOUT", 4)
			}
			if out.Len() != 0 || len(agentRunTestLogCalls(client)) != 1 || len(client.created) != 1 {
				t.Fatalf("interrupted result duplicated work or produced output: %+v, %s", client.snapshot(), out)
			}
		})
	}
}
