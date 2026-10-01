package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	dgdrTestAPIs    = "/apis"
	dgdrTestGroup   = "nvidia.com"
	dgdrTestKind    = "DynamoGraphDeployment"
	dgdrTestVersion = "v1beta1"
)

// The b7ed2f9 reference stores requestRef/workloadRef under status.provider.
// Its lifecycle_test.go workloadFixture links the generated DGD to the DGDR
// with labels, not ownerReferences: deleting a request must not delete serving.
func dgdrLogFixture(version string) (*accessFakeClient, Object, Object, Object) {
	model := accessTestModel()
	request := accessTestResource("DynamoGraphDeploymentRequest", "profiling-request")
	request["apiVersion"] = dgdrTestGroup + "/" + dgdrTestVersion
	accessTestOwn(request, model)
	workload := accessTestResource(dgdrTestKind, "generated-serving")
	workload["apiVersion"] = dgdrTestGroup + "/" + version
	object(workload["metadata"])["labels"] = Object{
		"dgdr.nvidia.com/name": "profiling-request", "dgdr.nvidia.com/namespace": "test",
	}
	ref := Object{
		"apiVersion": workload["apiVersion"], "kind": workload["kind"], "name": "generated-serving",
		"namespace": "test", "uid": get(workload, "metadata", "uid"),
	}
	object(model["status"])["provider"] = Object{
		"name": "dynamo", "resourceKind": request["kind"], "resourceName": "profiling-request",
		"requestRef": Object{
			"apiVersion": request["apiVersion"], "kind": request["kind"], "name": "profiling-request",
			"namespace": "test", "uid": get(request, "metadata", "uid"),
		},
		"workloadRef": ref,
	}
	component := accessTestResource("DynamoComponentDeployment", "serving-component")
	component["apiVersion"] = workload["apiVersion"]
	accessTestOwn(component, workload)
	pod := accessTestPod(accessTestService("actual-api", 8000), component)
	client := &accessFakeClient{
		resources: []Object{model, request, workload, component, pod},
		onRequest: dgdrLogDiscovery,
	}
	return client, model, workload, pod
}

func dgdrLogDiscovery(_ context.Context, method, path string, _ any, _ RequestOptions) (Object, error) {
	if method != "GET" {
		return nil, fmt.Errorf("unexpected method %s", method)
	}
	switch path {
	case dgdrTestAPIs:
		return Object{"groups": accessTestObjects(Object{
			"name": dgdrTestGroup, "preferredVersion": Object{"groupVersion": dgdrTestGroup + "/v1"},
			"versions": accessTestObjects(
				Object{"groupVersion": dgdrTestGroup + "/v1"}, Object{"groupVersion": dgdrTestGroup + "/" + dgdrTestVersion},
			),
		})}, nil
	case "/apis/nvidia.com/v1":
		return Object{"resources": accessTestObjects(Object{"name": "clusterpolicies", "kind": "ClusterPolicy"})}, nil
	case "/apis/nvidia.com/v1alpha1", "/apis/nvidia.com/v1beta1":
		return Object{"resources": accessTestObjects(
			Object{"name": "dynamographdeployments", "kind": dgdrTestKind, "namespaced": true},
			Object{"name": "dynamographdeploymentrequests", "kind": "DynamoGraphDeploymentRequest", "namespaced": true},
			Object{"name": "dynamocomponentdeployments", "kind": "DynamoComponentDeployment", "namespaced": true},
		)}, nil
	default:
		return nil, fmt.Errorf("unexpected discovery path %s", path)
	}
}

func requireDGDRLogRequest(t *testing.T, client *accessFakeClient, want bool) {
	t.Helper()
	found := false
	for _, call := range client.snapshot() {
		if strings.HasSuffix(call.Path, "/log") {
			found = true
			if call.Path != "/api/v1/namespaces/test/pods/selected-pod/log" || call.Options.Query.Get("container") != "main" {
				t.Fatalf("unexpected pod log request: %+v", call)
			}
		}
	}
	if found != want {
		t.Fatalf("log subresource requested=%v, want %v", found, want)
	}
}

func TestDGDRGeneratedWorkloadLogs(t *testing.T) {
	for _, version := range []string{"v1alpha1", dgdrTestVersion} {
		t.Run(version, func(t *testing.T) {
			client, model, _, _ := dgdrLogFixture(version)
			// A conflicting old location must not override the authoritative nested ref.
			object(model["status"])["workloadRef"] = Object{
				"apiVersion": dgdrTestGroup + "/" + dgdrTestVersion,
				"kind":       "DynamoGraphDeploymentRequest", "name": "profiling-request",
			}
			before := cloneObject(model)
			c, out, _ := accessTestContext(client, Flags{"pod": {"selected-pod"}, "container": {"main"}})
			accessTestCode(t, runAccess("model", "logs", "llama", c), "")
			requireDGDRLogRequest(t, client, true)
			var text string
			if err := json.Unmarshal(out.Bytes(), &text); err != nil || text != "line one\nline two\n" {
				t.Fatalf("log output: %q, %v", out, err)
			}
			manifestTestEqual(t, model, before)
			for _, call := range client.snapshot() {
				if call.Type.Kind == "DynamoGraphDeploymentRequest" {
					t.Fatal("serving logs resolved the profiling request")
				}
				if call.Type.Kind == dgdrTestKind && call.Type.Version != version {
					t.Fatalf("ignored published API version: %+v", call.Type)
				}
			}
		})
	}
}

func TestDGDRWorkloadReferenceLegacyControls(t *testing.T) {
	for _, location := range []string{"legacy-provider-fields", "legacy-top-level", "optional-uid-and-namespace"} {
		t.Run(location, func(t *testing.T) {
			client, model, workload, _ := dgdrLogFixture(dgdrTestVersion)
			provider := object(get(model, "status", "provider"))
			ref := object(provider["workloadRef"])
			switch location {
			case "legacy-provider-fields":
				delete(provider, "workloadRef")
				provider["resourceKind"], provider["resourceName"] = workload["kind"], get(workload, "metadata", "name")
				accessTestOwn(workload, model)
			case "legacy-top-level":
				object(model["status"])["workloadRef"] = ref
				delete(provider, "workloadRef")
			case "optional-uid-and-namespace":
				delete(ref, "uid")
				delete(ref, "namespace")
			}
			c, _, _ := accessTestContext(client, Flags{"pod": {"selected-pod"}, "container": {"main"}})
			accessTestCode(t, runAccess("model", "logs", "llama", c), "")
			requireDGDRLogRequest(t, client, true)
		})
	}
	t.Run("agent runtime reference unchanged", func(t *testing.T) {
		agent, service, pod, workload, _ := accessTestAgent()
		client := &accessFakeClient{resources: []Object{agent, service, pod, workload}}
		c, _, _ := accessTestContext(client, Flags{"pod": {"selected-pod"}, "container": {"main"}})
		accessTestCode(t, runAccess("agent", "logs", "helper", c), "")
		requireDGDRLogRequest(t, client, true)
	})
}

func TestDGDRWorkloadReferenceRejectsUnsafeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, field, value, message string
	}{
		{"replaced workload", "uid", "old-workload-uid", "UID"},
		{"cross namespace", "namespace", "other", "namespace"},
		{"path namespace", "namespace", "../test", "namespace"},
		{"missing name", "name", "", "workload reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, model, _, _ := dgdrLogFixture(dgdrTestVersion)
			ref := object(get(model, "status", "provider", "workloadRef"))
			ref[tc.field] = tc.value
			c, _, _ := accessTestContext(client, Flags{"pod": {"selected-pod"}, "container": {"main"}})
			err := runAccess("model", "logs", "llama", c)
			accessTestCode(t, err, "UNSUPPORTED")
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("missing identity diagnostic: %v", err)
			}
			requireDGDRLogRequest(t, client, false)
			for _, call := range client.snapshot() {
				if call.Type.Kind == "Pod" || call.Type.Kind == "DynamoGraphDeploymentRequest" || call.Namespace == "other" {
					t.Fatalf("unsafe reference reached pod lookup or request fallback: %+v", call)
				}
			}
		})
	}
}

func TestDGDRWorkloadReferenceStillRequiresPodOwnership(t *testing.T) {
	client, _, _, pod := dgdrLogFixture(dgdrTestVersion)
	delete(object(pod["metadata"]), "ownerReferences")
	object(pod["metadata"])["labels"] = Object{"dgdr.nvidia.com/name": "profiling-request"}
	c, _, _ := accessTestContext(client, Flags{"pod": {"selected-pod"}, "container": {"main"}})
	accessTestCode(t, runAccess("model", "logs", "llama", c), "UNSUPPORTED")
	requireDGDRLogRequest(t, client, false)
}
