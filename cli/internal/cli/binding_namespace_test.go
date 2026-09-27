package cli

import "testing"

const (
	bindingTestNamespace   = "team"
	bindingTestGatewayFlag = "model-gateway"
)

func bindingNamespaceFlags(mode, reference string) Flags {
	if mode == bindingTestGatewayFlag {
		return Flags{mode: {reference}, "model-id": {"served-model"}}
	}
	return Flags{mode: {reference}}
}

func bindingNamespaceRef(model Object, mode string) Object {
	if mode == bindingTestGatewayFlag {
		return object(get(model, "gatewayEndpoint", "gatewayRef"))
	}
	return object(model["deploymentRef"])
}

func checkAgentBindingNamespace(t *testing.T, mode, operation, reference string) {
	t.Helper()
	flags := bindingNamespaceFlags(mode, reference)
	streams, _, _ := manifestTestIO("")
	var result Object
	var err error
	if operation == "create" {
		flags["framework"] = []string{"langgraph"}
		result, err = buildAgent("helper", flags, bindingTestNamespace, streams)
	} else {
		existing := manifestTestAgent(t, Flags{})
		before := cloneObject(existing)
		result, err = updateResource("agent", existing, flags, streams)
		manifestTestEqual(t, existing, before)
	}
	if reference == "other/demo" {
		manifestTestUsage(t, err, "--"+mode+" must reference the agent namespace")
		if result != nil {
			t.Fatal("cross-namespace reference produced a submission payload")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	want := Object{"name": "demo"}
	if reference == bindingTestNamespace+"/demo" {
		want["namespace"] = bindingTestNamespace
	}
	manifestTestEqual(t, bindingNamespaceRef(object(get(result, "spec", "model")), mode), want)
}

func TestAgentBindingReferenceNamespaces(t *testing.T) {
	for _, mode := range []string{"model-ref", bindingTestGatewayFlag} {
		for _, operation := range []string{"create", "update"} {
			for _, reference := range []string{"demo", bindingTestNamespace + "/demo", "other/demo"} {
				t.Run(mode+"/"+operation+"/"+reference, func(t *testing.T) {
					checkAgentBindingNamespace(t, mode, operation, reference)
				})
			}
		}
	}
}

func TestAgentBindingUpdateUsesResourceNamespace(t *testing.T) {
	existing := manifestTestAgent(t, Flags{})
	object(existing["metadata"])["namespace"] = "agents"
	streams, _, _ := manifestTestIO("")
	for _, mode := range []string{"model-ref", bindingTestGatewayFlag} {
		flags := bindingNamespaceFlags(mode, "agents/demo")
		flags["namespace"] = []string{bindingTestNamespace}
		if _, err := updateResource("agent", existing, flags, streams); err != nil {
			t.Fatalf("same-namespace update rejected: %v", err)
		}
		flags[mode] = []string{bindingTestNamespace + "/demo"}
		_, err := updateResource("agent", existing, flags, streams)
		manifestTestUsage(t, err, "cross-namespace references are not supported")
	}
}

func TestAgentBindingRepairsOldCrossNamespaceReferences(t *testing.T) {
	for _, mode := range []string{"model-ref", bindingTestGatewayFlag} {
		t.Run(mode, func(t *testing.T) {
			flags := bindingNamespaceFlags(mode, "demo")
			flags["framework"] = []string{"langgraph"}
			streams, _, _ := manifestTestIO("")
			existing, err := buildAgent("helper", flags, bindingTestNamespace, streams)
			if err != nil {
				t.Fatal(err)
			}
			bindingNamespaceRef(object(get(existing, "spec", "model")), mode)["namespace"] = "other"
			patch, err := updateResource("agent", existing, bindingNamespaceFlags(mode, "local"), streams)
			if err != nil {
				t.Fatal(err)
			}
			merged := object(manifestTestMergePatch(existing, patch))
			manifestTestEqual(t, bindingNamespaceRef(object(get(merged, "spec", "model")), mode), Object{"name": "local"})
			if mode == bindingTestGatewayFlag {
				_, err = updateResource("agent", existing, Flags{"gateway-listener": {"http"}}, streams)
				manifestTestUsage(t, err, "cross-namespace references are not supported")
			}
		})
	}
}

func TestAgentBindingExternalURLAndCredentialKeyUnchanged(t *testing.T) {
	flags := Flags{
		"model-url": {"https://models.example.test/v1"}, "model-api": {"openai"}, "model-id": {"remote-model"},
		"model-credential": {"other/API_KEY"},
	}
	streams, _, _ := manifestTestIO("")
	createFlags := flags.Copy()
	createFlags["framework"] = []string{"langgraph"}
	created, err := buildAgent("helper", createFlags, bindingTestNamespace, streams)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := updateResource("agent", manifestTestAgent(t, Flags{}), flags, streams)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range []Object{created, patch} {
		api := object(get(result, "spec", "model", "externalAPI"))
		manifestTestEqual(t, api["baseURL"], "https://models.example.test/v1")
		manifestTestEqual(t, api["credentialsRef"], Object{"name": "other", "key": "API_KEY"})
	}
}
