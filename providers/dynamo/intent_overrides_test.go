package dynamo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

func intentOverridesMD(t *testing.T, version string) *api.ModelDeployment {
	t.Helper()
	md := typedRenderingMD(t)
	args := []any{"--dyn-tool-call-parser", "hermes", "--dyn-reasoning-parser", "qwen3"}
	var spec map[string]any
	if version == "v1alpha1" {
		spec = map[string]any{"services": map[string]any{"VllmDecodeWorker": map[string]any{"extraPodSpec": map[string]any{"mainContainer": map[string]any{"args": args}}}}}
	} else {
		spec = map[string]any{"components": []any{map[string]any{"name": "VllmDecodeWorker", "podTemplate": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "main", "$patch": map[string]any{"args": "append"}, "args": args}}}}}}}
	}
	var overrides map[string]any
	if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &overrides); err != nil {
		t.Fatal(err)
	}
	overrides["intent"].(map[string]any)["overrides"] = map[string]any{
		"profilingJob": map[string]any{"activeDeadlineSeconds": 1800},
		"dgd":          map[string]any{"apiVersion": "nvidia.com/" + version, "kind": DynamoGraphDeploymentKind, "spec": spec},
	}
	setRenderingOverrides(t, md, overrides)
	return md
}

func TestTypedNativeOverridesRenderingContracts(t *testing.T) {
	for _, target := range []struct{ runtime, dgdAPI, overrideAPI string }{
		{"1.1.1", "v1alpha1", "v1alpha1"},
		{"1.5.0", "v1beta1", "v1alpha1"},
		{"1.5.0", "v1beta1", "v1beta1"},
	} {
		t.Run(target.runtime+"/"+target.overrideAPI, func(t *testing.T) {
			md := intentOverridesMD(t, target.overrideAPI)
			before := md.DeepCopy()
			objects, err := NewTransformer().TransformForVersion(context.Background(), md, target.dgdAPI, target.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, md) {
				t.Fatal("rendering mutated ModelDeployment")
			}
			var supplied map[string]any
			if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &supplied); err != nil {
				t.Fatal(err)
			}
			spec := objects[0].Object["spec"].(map[string]any)
			expected := supplied["intent"].(map[string]any)["overrides"].(map[string]any)
			expected["profilingJob"].(map[string]any)["template"] = map[string]any{"spec": map[string]any{"containers": []any{}}}
			if !reflect.DeepEqual(expected, spec["overrides"]) {
				t.Fatal("native overrides or append directive changed during rendering")
			}
			if spec["model"] != md.Spec.Model.ID || spec["backend"] != "vllm" || spec["autoApply"] != true || spec["searchStrategy"] != "rapid" {
				t.Fatal("authoritative profiling inputs changed")
			}
			assertContract(t, readReleasedContract(t, "v"+target.runtime, "dynamographdeploymentrequests", "v1beta1"), objects[0])
		})
	}
	md := intentOverridesMD(t, "v1beta1")
	if _, err := NewTransformer().TransformForVersion(context.Background(), md, "v1alpha1", "1.1.1"); err == nil || !strings.Contains(err.Error(), "requires Dynamo 1.5.0") {
		t.Fatalf("beta override accepted by alpha-only runtime: %v", err)
	}
}

func TestTypedOverrideChangesRespectRequestLifecycle(t *testing.T) {
	for _, phase := range []string{"Pending", "Profiling", "Ready", "Deploying", "Deployed"} {
		t.Run(phase, func(t *testing.T) {
			md := intentOverridesMD(t, "v1beta1")
			request := requestFixture(t, md, phase)
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(request).Build()
			r := NewDynamoProviderReconciler(c, newScheme(), "")
			md.Spec.Provider.Overrides.Raw = []byte(strings.ReplaceAll(string(md.Spec.Provider.Overrides.Raw), "hermes", "qwen3_coder"))
			desired, err := r.Transformer.Transform(context.Background(), md)
			if err != nil {
				t.Fatal(err)
			}
			err = r.createOrUpdateResource(context.Background(), desired[0], md)
			if phase == "Pending" {
				if err != nil {
					t.Fatal(err)
				}
				got := request.DeepCopy()
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(got), got); err != nil {
					t.Fatal(err)
				}
				if !sameJSON(t, got.Object["spec"], desired[0].Object["spec"]) {
					t.Fatal("pending override change lost")
				}
			} else {
				var locked *intentLockedError
				if !errors.As(err, &locked) {
					t.Fatalf("active override edit not locked: %v", err)
				}
				got := request.DeepCopy()
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(got), got); err != nil {
					t.Fatal(err)
				}
				if !sameJSON(t, got.Object["spec"], request.Object["spec"]) {
					t.Fatal("active request mutated without a new attempt")
				}
			}
		})
	}
}

func TestTypedOverridePolicyWithoutAdmission(t *testing.T) {
	md := intentOverridesMD(t, "v1beta1")
	md.Spec.Provider.Overrides = &runtime.RawExtension{Raw: []byte(strings.ReplaceAll(string(md.Spec.Provider.Overrides.Raw), `"activeDeadlineSeconds":1800`, `"template":{"spec":{"hostNetwork":true}}`))}
	if err := (&DynamoProviderReconciler{}).validateCompatibility(md); err == nil {
		t.Fatal("provider accepted forbidden native override")
	}
	// Auto-selected providers still receive the same validation during rendering.
	md.Spec.Provider.Name = ""
	if _, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.5.0"); err == nil {
		t.Fatal("auto-selected provider bypassed override validation")
	}
	clean := intentOverridesMD(t, "v1beta1")
	if err := dynamointent.Validate(clean); err != nil {
		t.Fatal(err)
	}
}

// Fake API storage normalizes JSON integers; compare the wire representation.
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(left) == string(right)
}

func TestTypedPartialProfilingPodOverride(t *testing.T) {
	md := typedRenderingMD(t)
	var root map[string]any
	if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &root); err != nil {
		t.Fatal(err)
	}
	root["intent"].(map[string]any)["overrides"] = map[string]any{"profilingJob": map[string]any{"template": map[string]any{"spec": map[string]any{"nodeSelector": map[string]any{"pool": "gpu"}}}}}
	setRenderingOverrides(t, md, root)
	before := md.DeepCopy()
	for _, release := range []string{"1.1.1", "1.5.0"} {
		objects, err := NewTransformer().TransformForVersion(context.Background(), md, "v1alpha1", release)
		if err != nil {
			t.Fatal(err)
		}
		assertContract(t, readReleasedContract(t, "v"+release, "dynamographdeploymentrequests", "v1beta1"), objects[0])
	}
	if !reflect.DeepEqual(before, md) {
		t.Fatal("structural padding mutated user intent")
	}
}

func TestDocumentedNativeIntentOverridesSample(t *testing.T) {
	data, err := os.ReadFile("../../controller/config/samples/airunway_v1alpha1_modeldeployment_dynamo_intent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var md api.ModelDeployment
	if err := yaml.Unmarshal(data, &md); err != nil {
		t.Fatal(err)
	}
	if err := dynamointent.Validate(&md); err != nil {
		t.Fatal(err)
	}
	objects, err := NewTransformer().TransformForVersion(context.Background(), &md, "v1beta1", "1.5.0")
	if err != nil {
		t.Fatal(err)
	}
	assertContract(t, readReleasedContract(t, "v1.5.0", "dynamographdeploymentrequests", "v1beta1"), objects[0])
	spec := objects[0].Object["spec"].(map[string]any)
	overrides, ok := spec["overrides"].(map[string]any)
	if !ok || overrides["dgd"] == nil || overrides["profilingJob"] == nil {
		t.Fatal("sample must demonstrate both native overrides")
	}
}

func TestPartialProfilingJobSurvivesTypedRoundTrip(t *testing.T) {
	for _, job := range []map[string]any{
		{"activeDeadlineSeconds": 1800},
		{"template": map[string]any{}},
		{"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"example.com/profile": "test"}}}},
		{"template": map[string]any{"spec": map[string]any{"nodeSelector": map[string]any{"pool": "gpu"}}}},
	} {
		for _, release := range []string{"1.1.1", "1.5.0"} {
			md := typedRenderingMD(t)
			var root map[string]any
			if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &root); err != nil {
				t.Fatal(err)
			}
			root["intent"].(map[string]any)["overrides"] = map[string]any{"profilingJob": job}
			setRenderingOverrides(t, md, root)
			objects, err := NewTransformer().TransformForVersion(context.Background(), md, "v1alpha1", release)
			if err != nil {
				t.Fatal(err)
			}
			resource := objects[0]
			contract := readReleasedContract(t, "v"+release, "dynamographdeploymentrequests", "v1beta1")
			assertContract(t, contract, resource)
			overrides := resource.Object["spec"].(map[string]any)["overrides"].(map[string]any)
			encoded, err := json.Marshal(overrides["profilingJob"])
			if err != nil {
				t.Fatal(err)
			}
			// Admission and later operator updates decode a native JobSpec. Its
			// required containers field must survive as [] instead of nil/null.
			var typed batchv1.JobSpec
			if err := json.Unmarshal(encoded, &typed); err != nil {
				t.Fatal(err)
			}
			if typed.Template.Spec.Containers == nil {
				t.Fatal("partial job will emit containers:null")
			}
			encoded, err = json.Marshal(typed)
			if err != nil {
				t.Fatal(err)
			}
			var roundTripped map[string]any
			if err := json.Unmarshal(encoded, &roundTripped); err != nil {
				t.Fatal(err)
			}
			overrides["profilingJob"] = roundTripped
			assertContract(t, contract, resource)
		}
	}
}
