package dynamo

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestToolCallingReleasedRenderingContracts(t *testing.T) {
	for _, tc := range []struct{ version, release, envKey string }{{"v1alpha1", "1.1.1", "envs"}, {"v1alpha1", "1.5.0", "envs"}, {"v1beta1", "1.5.0", "env"}} {
		for _, engine := range []api.EngineType{api.EngineTypeVLLM, api.EngineTypeSGLang, api.EngineTypeTRTLLM} {
			for _, mode := range []string{"manual", "typed", "legacy"} {
				t.Run(tc.release+"/"+tc.version+"/"+string(engine)+"/"+mode, func(t *testing.T) {
					md := newMDForController("model", "models")
					md.Spec.Model.ID = "Qwen/Qwen3-0.6B"
					md.Spec.Engine.Type = engine
					md.Spec.Engine.ToolCalling = true
					md.Spec.Provider = &api.ProviderSpec{Name: "dynamo"}
					envKey := tc.envKey
					if mode != "manual" {
						md.Spec.Resources = nil
						md.Spec.Scaling = nil
						md.Spec.Serving = nil
						if tc.release == "1.5.0" {
							envKey = "env"
						}
						if mode == "typed" {
							setRenderingOverrides(t, md, map[string]any{"deploymentMode": "intent", "intent": map[string]any{"hardware": map[string]any{"totalGpus": 1}}})
						} else {
							setRenderingOverrides(t, md, map[string]any{"deploymentMode": "intent", "spec": map[string]any{"hardware": map[string]any{"totalGpus": 1}, "searchStrategy": "rapid"}})
						}
					}
					before := md.DeepCopy()
					objects, err := NewTransformer().TransformForVersion(context.Background(), md, tc.version, tc.release)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before, md) {
						t.Fatal("rendering mutated ModelDeployment")
					}
					obj := objects[0]
					var env []any
					if mode == "manual" {
						assertContract(t, readReleasedContract(t, "v"+tc.release, "dynamographdeployments", tc.version), obj)
						env, _, err = unstructured.NestedSlice(obj.Object, "spec", envKey)
					} else {
						assertContract(t, readReleasedContract(t, "v"+tc.release, "dynamographdeploymentrequests", "v1beta1"), obj)
						env, _, err = unstructured.NestedSlice(obj.Object, "spec", "overrides", "dgd", "spec", envKey)
					}
					if err != nil {
						t.Fatal(err)
					}
					want := []any{map[string]any{"name": "DYN_TOOL_CALL_PARSER", "value": "hermes"}, map[string]any{"name": "DYN_REASONING_PARSER", "value": "qwen3"}}
					if !reflect.DeepEqual(env, want) {
						t.Fatalf("env=%#v", env)
					}
				})
			}
		}
	}
}

func TestToolCallingPreservesNativeOverrides(t *testing.T) {
	for _, automatic := range []bool{true, false} {
		md := newMDForController("model", "models")
		md.Spec.Model.ID = "Qwen/Qwen3-Coder-30B-A3B-Instruct"
		md.Spec.Engine.ToolCalling = true
		md.Spec.Provider = &api.ProviderSpec{Name: "dynamo"}
		spec := map[string]any{"env": []any{map[string]any{"name": "CUSTOM_ENV", "value": "kept"}}}
		if automatic {
			md.Spec.Resources = nil
			md.Spec.Scaling = nil
			md.Spec.Serving = nil
			setRenderingOverrides(t, md, map[string]any{"deploymentMode": "intent", "intent": map[string]any{"hardware": map[string]any{"totalGpus": 1}, "overrides": map[string]any{"profilingJob": map[string]any{"activeDeadlineSeconds": int64(1800)}, "dgd": map[string]any{"apiVersion": "nvidia.com/v1beta1", "kind": DynamoGraphDeploymentKind, "metadata": map[string]any{"name": "stable-model"}, "spec": spec}}}})
		} else {
			setRenderingOverrides(t, md, map[string]any{"spec": spec})
		}
		objects, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.5.0")
		if err != nil {
			t.Fatal(err)
		}
		obj := objects[0]
		path := []string{"spec", "env"}
		if automatic {
			path = []string{"spec", "overrides", "dgd", "spec", "env"}
			name, _, _ := unstructured.NestedString(obj.Object, "spec", "overrides", "dgd", "metadata", "name")
			if name != "stable-model" {
				t.Fatal("lost explicit DGD name")
			}
		}
		env, _, err := unstructured.NestedSlice(obj.Object, path...)
		if err != nil {
			t.Fatal(err)
		}
		want := []any{map[string]any{"name": "CUSTOM_ENV", "value": "kept"}, map[string]any{"name": "DYN_TOOL_CALL_PARSER", "value": "qwen3_coder"}}
		if !reflect.DeepEqual(env, want) {
			t.Fatalf("env=%#v", env)
		}
	}
}

func TestToolCallingRejectsConflictingNativeSettings(t *testing.T) {
	md := newMDForController("model", "models")
	md.Spec.Model.ID = "Qwen/Qwen3-0.6B"
	md.Spec.Engine.ToolCalling = true
	md.Spec.Provider = &api.ProviderSpec{Name: "dynamo"}
	setRenderingOverrides(t, md, map[string]any{"spec": map[string]any{"env": []any{map[string]any{"name": "DYN_TOOL_CALL_PARSER", "value": "other"}}}})
	if _, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.5.0"); err == nil {
		t.Fatal("conflicting raw env must fail, not override structured parser")
	}
}

func TestToolCallingPreservesAlphaIntentOverride(t *testing.T) {
	for _, release := range []string{"1.1.1", "1.5.0"} {
		md := newMDForController("model", "models")
		md.Spec.Model.ID = "Qwen/Qwen3-Coder-30B-A3B-Instruct"
		md.Spec.Engine.ToolCalling = true
		md.Spec.Resources = nil
		md.Spec.Scaling = nil
		md.Spec.Serving = nil
		md.Spec.Provider = &api.ProviderSpec{Name: "dynamo"}
		setRenderingOverrides(t, md, map[string]any{"deploymentMode": "intent", "intent": map[string]any{"hardware": map[string]any{"totalGpus": 1}, "overrides": map[string]any{"dgd": map[string]any{"apiVersion": "nvidia.com/v1alpha1", "kind": DynamoGraphDeploymentKind, "spec": map[string]any{"envs": []any{map[string]any{"name": "USER_ENV", "value": "preserved"}}}}}}})
		objects, err := NewTransformer().TransformForVersion(context.Background(), md, "v1alpha1", release)
		if err != nil {
			t.Fatal(err)
		}
		env, _, _ := unstructured.NestedSlice(objects[0].Object, "spec", "overrides", "dgd", "spec", "envs")
		if len(env) != 2 || env[1].(map[string]any)["name"] != "DYN_TOOL_CALL_PARSER" {
			t.Fatalf("unexpected env on %s: %#v", release, env)
		}
		assertContract(t, readReleasedContract(t, "v"+release, "dynamographdeploymentrequests", "v1beta1"), objects[0])
	}
}

func TestToolCallingDocumentedSample(t *testing.T) {
	data, err := os.ReadFile("../../controller/config/samples/airunway_v1alpha1_modeldeployment_dynamo_intent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var md api.ModelDeployment
	if err := yaml.UnmarshalStrict(data, &md); err != nil {
		t.Fatal(err)
	}
	if !md.Spec.Engine.ToolCalling {
		t.Fatal("sample must exercise structured tool calling")
	}
	for _, release := range []string{"1.1.1", "1.5.0"} {
		objects, err := NewTransformer().TransformForVersion(context.Background(), &md, "v1alpha1", release)
		if err != nil {
			t.Fatal(err)
		}
		assertContract(t, readReleasedContract(t, "v"+release, "dynamographdeploymentrequests", "v1beta1"), objects[0])
	}
}

func TestToolCallingRejectsLegacyBetaOverrideOnAlphaRuntime(t *testing.T) {
	md := newMDForController("model", "models")
	md.Spec.Model.ID = "Qwen/Qwen3-0.6B"
	md.Spec.Engine.ToolCalling = true
	md.Spec.Provider = &api.ProviderSpec{Name: "dynamo"}
	setRenderingOverrides(t, md, map[string]any{"deploymentMode": "intent", "spec": map[string]any{"hardware": map[string]any{"totalGpus": 1}, "overrides": map[string]any{"dgd": map[string]any{"apiVersion": "nvidia.com/v1beta1", "kind": DynamoGraphDeploymentKind, "spec": map[string]any{}}}}})
	if _, err := NewTransformer().TransformForVersion(context.Background(), md, "v1alpha1", "1.1.1"); err == nil || !strings.Contains(err.Error(), "1.5.0 or newer") {
		t.Fatalf("legacy beta override must fail before losing parser env: %v", err)
	}
	if _, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.5.0"); err != nil {
		t.Fatalf("supported beta override rejected: %v", err)
	}
}

func TestToolCallingRejectsUnsupportedReasoningDisable(t *testing.T) {
	md := newMDForController("model", "models")
	md.Spec.Model.ID = "Qwen/Qwen3-0.6B"
	md.Spec.Engine.ToolCalling = true
	md.Spec.Engine.ReasoningParser = "none"
	md.Spec.Provider = &api.ProviderSpec{Name: "dynamo"}
	container := map[string]any{"name": "main", "envFrom": []any{map[string]any{"configMapRef": map[string]any{"name": "parser-defaults"}}}}
	component := map[string]any{"name": "VllmWorker", "podTemplate": map[string]any{"spec": map[string]any{"containers": []any{container}}}}
	setRenderingOverrides(t, md, map[string]any{"spec": map[string]any{"components": []any{component}}})
	// Neither an absent nor empty EnvVar can reliably disable an imported native
	// parser. Reject the invented sentinel rather than promise an ineffective disable.
	if _, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.5.0"); err == nil || !strings.Contains(err.Error(), "not a supported Dynamo disable value") {
		t.Fatalf("expected unsupported disable rejection, got %v", err)
	}
}
