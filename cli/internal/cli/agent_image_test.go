package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func agentImageTestFramework(backend, catalog string) Object {
	framework := managementTestFramework("langgraph", nil)
	object(get(framework, "spec", "capabilities"))["backend"] = backend
	object(framework["status"])["version"] = "agent-container-provider:main-aaaaaaaaaaaa"
	object(get(framework, "metadata", "annotations"))["airunway.ai/agent-catalog"] = catalog
	return framework
}

func agentImageTestArgs(once bool) []string {
	args := []string{resourceAgent, "create", "helper", "--framework=langgraph", "--model-url=http://model.test/v1", "--model-api=openai", "--model-id=model", "--timeout=2s"}
	if once {
		args = append(args, "--mode=once", "--task=Explain the result")
	}
	return args
}

func agentImageTestStatus(phase, reason, message string) Object {
	status := "True"
	if phase == "Failed" {
		status = "False"
	}
	return Object{"phase": phase, "observedGeneration": 1, "conditions": []any{Object{
		"type": "Ready", "status": status, "reason": reason, "message": message, "observedGeneration": 1,
	}}}
}

func agentImageTestReconcile(client *managementFakeClient, status Object) {
	// Only simulate the API response and observed status. Catalog selection and
	// version expansion belong to ContainerProviderReconciler, not this fake.
	client.response = func(call managementCall, resource Object) Object {
		object(resource["metadata"])["uid"] = "agent-image-test"
		object(resource["metadata"])["generation"] = 1
		observed := cloneObject(resource)
		observed["status"] = cloneObject(status)
		client.resources = append(client.resources, observed)
		resource["status"] = Object{"phase": "Pending"}
		return resource
	}
}

func TestAgentImageCreationLeavesCatalogDefaultToServer(t *testing.T) {
	for name, catalog := range map[string]string{
		"versioned image":  `[{"name":"default","title":"Default","image":"registry.invalid/agent:${AIRUNWAY_VERSION}"}]`,
		"not first recipe": `[{"name":"empty","title":"Empty"},{"name":"runtime","title":"Runtime","image":"registry.invalid/agent:v1","template":{"config":{"systemPrompt":"Recipe prompt must not be selected"}}}]`,
	} {
		for _, once := range []bool{false, true} {
			mode, phase := "deployment", "Running"
			if once {
				mode, phase = agentLifecycleJob, "Completed"
			}
			for _, dry := range []string{"", dryRunServer, dryRunClient} {
				t.Run(name+"/"+mode+"/"+dry, func(t *testing.T) {
					runAgentImageCatalogDefaultCase(t, catalog, once, mode, phase, dry)
				})
			}
		}
	}
}

func runAgentImageCatalogDefaultCase(t *testing.T, catalog string, once bool, mode, phase, dry string) {
	t.Helper()
	client := &managementFakeClient{resources: []Object{agentImageTestFramework("container", catalog)}}
	agentImageTestReconcile(client, agentImageTestStatus(phase, "Reconciled", ""))
	args := agentImageTestArgs(once)
	if dry != "" {
		args = append(args, "--dry-run="+dry)
	}
	code, out, stderr := writeFlowRun(t, client, args...)
	if code != 0 {
		t.Fatalf("code=%d: %s", code, stderr)
	}
	var returned Object
	if err := json.Unmarshal([]byte(out), &returned); err != nil {
		t.Fatal(err)
	}
	wantSpec := Object{"framework": Object{"name": "langgraph"}, "lifecycle": mode, "model": Object{
		"externalAPI": Object{"baseURL": "http://model.test/v1", "type": "openai", "modelName": "model"},
	}}
	if once {
		wantSpec[keyConfig] = Object{"task": "Explain the result"}
	}
	if !reflect.DeepEqual(returned["spec"], wantSpec) {
		t.Fatalf("CLI changed the image-less spec: got %#v, want %#v", returned["spec"], wantSpec)
	}
	if dry == dryRunClient {
		if len(client.calls) != 0 {
			t.Fatalf("client preview contacted the cluster: %#v", client.calls)
		}
		return
	}
	assertAgentImageCatalogSubmission(t, client, dry, wantSpec)
	if dry == "" && stringAt(returned, "status", "phase") != phase {
		t.Fatalf("did not wait for the server's result: %s", out)
	}
}

func assertAgentImageCatalogSubmission(t *testing.T, client *managementFakeClient, dry string, wantSpec Object) {
	t.Helper()
	writes := client.writes()
	if len(writes) != 1 || writes[0].op != "create" || writes[0].dry != (dry == dryRunServer) {
		t.Fatalf("unexpected writes: %#v", writes)
	}
	if !reflect.DeepEqual(writes[0].body["spec"], wantSpec) {
		t.Fatalf("submitted spec differs: %#v", writes[0].body["spec"])
	}
	for _, call := range client.calls {
		if call.op == "list" {
			t.Fatal("creating without a preset must not require a catalog list")
		}
	}
}

func TestAgentImageCreationPreservesExplicitImage(t *testing.T) {
	for _, source := range []string{"flag", keyConfig, "preset"} {
		for _, once := range []bool{false, true} {
			t.Run(source+"/"+map[bool]string{false: "deployment", true: agentLifecycleJob}[once], func(t *testing.T) {
				runAgentImageExplicitCase(t, source, once)
			})
		}
	}
}

func runAgentImageExplicitCase(t *testing.T, source string, once bool) {
	t.Helper()
	const image = "registry.invalid/custom-agent:v2"
	// A broken catalog must not invalidate an explicit image. A preset
	// requires catalog validation, but chooses the requested recipe.
	catalog := "invalid JSON"
	args := append(agentImageTestArgs(once), "--wait=false")
	switch source {
	case "flag":
		args = append(args, "--image="+image)
	case keyConfig:
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"image":"`+image+`","systemPrompt":"Keep this prompt"}`), 0600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--config-file="+path)
	case "preset":
		catalog = `[{"name":"other","title":"Other","image":"registry.invalid/other:v1"},{"name":"chosen","title":"Chosen","image":"` + image + `"}]`
		args = append(args, "--preset=langgraph/chosen")
	}
	client := &managementFakeClient{resources: []Object{agentImageTestFramework("container", catalog)}}
	code, _, stderr := writeFlowRun(t, client, args...)
	if code != 0 {
		t.Fatalf("code=%d: %s", code, stderr)
	}
	writes := client.writes()
	if len(writes) != 1 || stringAt(writes[0].body, "spec", keyConfig, "image") != image {
		t.Fatalf("explicit image was not preserved: %#v", writes)
	}
	if source == keyConfig && stringAt(writes[0].body, "spec", keyConfig, "systemPrompt") != "Keep this prompt" {
		t.Fatal("configuration was overwritten")
	}
}

func TestAgentImageCreationLeavesCatalogFailuresToServer(t *testing.T) {
	for name, catalog := range map[string]string{
		"missing":          "",
		"no image":         `[{"name":"empty","title":"Empty"}]`,
		"ambiguous":        `[{"name":"first","title":"First","image":"registry.invalid/first:v1"},{"name":"second","title":"Second","image":"registry.invalid/second:v1"}]`,
		"same image twice": `[{"name":"first","title":"First","image":"registry.invalid/shared:v1"},{"name":"second","title":"Second","image":"registry.invalid/shared:v1"}]`,
		"malformed JSON":   "not JSON",
		"invalid entry":    `[{"name":"no-title","image":"registry.invalid/agent:v1"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, wait := range []bool{false, true} {
				runAgentImageCatalogFailureCase(t, catalog, wait)
			}
		})
	}
}

func runAgentImageCatalogFailureCase(t *testing.T, catalog string, wait bool) {
	t.Helper()
	client := &managementFakeClient{resources: []Object{agentImageTestFramework("container", catalog)}}
	agentImageTestReconcile(client, agentImageTestStatus("Failed", "MissingImage", "Set spec.config.image or repair the framework catalog."))
	args := agentImageTestArgs(false)
	wantCode := 1
	if !wait {
		args = append(args, "--wait=false")
		wantCode = 0
	}
	code, out, stderr := writeFlowRun(t, client, args...)
	if code != wantCode {
		t.Fatalf("wait=%v code=%d: %s", wait, code, stderr)
	}
	writes := client.writes()
	if len(writes) != 1 || writes[0].op != "create" || get(writes[0].body, "spec", keyConfig, "image") != nil {
		t.Fatalf("CLI must not guess an image or delete a failed submission: %#v", writes)
	}
	if wait && (out != "" || !strings.Contains(stderr, `"FAILED"`)) {
		t.Fatalf("server failure was not reported: out=%s err=%s", out, stderr)
	}
}

func TestAgentImageCreationWithCRDBackend(t *testing.T) {
	for _, backend := range []string{"crd", ""} {
		for _, image := range []string{"", "registry.invalid/custom-agent:v1"} {
			t.Run(backend+"/"+image, func(t *testing.T) {
				runAgentImageCRDCase(t, backend, image)
			})
		}
	}
}

func runAgentImageCRDCase(t *testing.T, backend, image string) {
	t.Helper()
	client := &managementFakeClient{resources: []Object{agentImageTestFramework(backend, `[{"name":"recipe","title":"Recipe","image":"registry.invalid/ignored:v1"}]`)}}
	args := append(agentImageTestArgs(false), "--wait=false")
	wantCode := 0
	if image != "" {
		args = append(args, "--image="+image)
		wantCode = 2
	}
	code, _, stderr := writeFlowRun(t, client, args...)
	if code != wantCode {
		t.Fatalf("code=%d: %s", code, stderr)
	}
	writes := client.writes()
	if image != "" {
		if len(writes) != 0 || !strings.Contains(stderr, `"UNSUPPORTED"`) {
			t.Fatalf("preflight did not reject a CRD image override: %s", stderr)
		}
	} else if len(writes) != 1 || get(writes[0].body, "spec", keyConfig, "image") != nil {
		t.Fatalf("CRD framework acquired a container image: %#v", writes)
	}
}

func TestAgentImageCreationPreservesAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, op, kind string
		args           []string
		failure        *CLIError
		wantCode       int
		wantCreate     bool
	}{
		{"advisory framework denial", "get", "AgentProviderConfig", nil, &CLIError{"framework denied", 3, "HTTP_403"}, 0, true},
		{"framework unauthenticated", "get", "AgentProviderConfig", nil, &CLIError{"login required", 3, "HTTP_401"}, 3, false},
		{"framework missing", "get", "AgentProviderConfig", nil, &CLIError{"framework missing", 1, "HTTP_404"}, 1, false},
		{"credential denial", "get", "Secret", []string{"--model-credential=api-key/token"}, &CLIError{"credential denied", 3, "HTTP_403"}, 3, false},
		{"preset discovery denial", "list", "AgentProviderConfig", []string{"--preset=langgraph/chosen"}, &CLIError{"catalog denied", 3, "HTTP_403"}, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &managementFakeClient{resources: []Object{agentImageTestFramework("container", "")}, failure: func(call managementCall) error {
				if call.op == tc.op && call.typ.Kind == tc.kind {
					return tc.failure
				}
				return nil
			}}
			args := append(agentImageTestArgs(false), "--wait=false")
			code, _, stderr := writeFlowRun(t, client, append(args, tc.args...)...)
			if code != tc.wantCode {
				t.Fatalf("code=%d: %s", code, stderr)
			}
			writes := client.writes()
			if tc.wantCreate {
				if len(writes) != 1 || get(writes[0].body, "spec", keyConfig, "image") != nil {
					t.Fatalf("advisory denial must still submit without an image: %#v", writes)
				}
			} else if len(writes) != 0 || !strings.Contains(stderr, tc.failure.Code) || !strings.Contains(stderr, tc.failure.Message) {
				t.Fatalf("authorization error was replaced or ignored: writes=%#v err=%s", writes, stderr)
			}
		})
	}
}

func TestAgentImageRunPreviewLeavesCatalogDefaultToServer(t *testing.T) {
	for _, dry := range []string{dryRunClient, dryRunServer} {
		t.Run(dry, func(t *testing.T) {
			runAgentImageRunPreviewCase(t, dry)
		})
	}
}
func runAgentImageRunPreviewCase(t *testing.T, dry string) {
	t.Helper()
	client, command, out, _ := agentRunTestSetup(t)
	client.resources[0] = agentImageTestFramework("container", `[{"name":"runtime","title":"Runtime","image":"registry.invalid/agent:${AIRUNWAY_VERSION}"}]`)
	delete(command.Flags, "image")
	command.Flags["dry-run"] = []string{dry}
	command.Flags["output"] = []string{"json"}
	if err := runAgentOnce([]string{resourceAgent, "run", "helper"}, command, &CLIConfig{}, dry == dryRunClient); err != nil {
		t.Fatal(err)
	}
	var preview Object
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if stringAt(preview, "spec", "lifecycle") != agentLifecycleJob || get(preview, "spec", keyConfig, "image") != nil {
		t.Fatalf("run must leave the one-shot image to the server: %s", out)
	}
	if dry == dryRunClient {
		if len(client.created) != 0 || len(client.snapshot()) != 0 {
			t.Fatal("client preview contacted the cluster")
		}
	} else if len(client.created) != 1 || !client.dryRuns[0] || get(client.created[0], "spec", keyConfig, "image") != nil {
		t.Fatalf("server preview must submit without an image: %#v", client.created)
	}
}
