package kuberay

import (
	"context"
	"reflect"
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// This is the build_openai_app / LLMConfig contract validated against
// rayproject/ray-llm:2.55.0-py311-cu128, not a user-provided vllm_serve module.
type nativeServeConfig struct {
	Applications []struct {
		Name        string `json:"name"`
		RoutePrefix string `json:"route_prefix"`
		ImportPath  string `json:"import_path"`
		Args        struct {
			LLMConfigs []struct {
				ModelLoadingConfig map[string]interface{} `json:"model_loading_config"`
				EngineKwargs       map[string]interface{} `json:"engine_kwargs"`
				DeploymentConfig   map[string]interface{} `json:"deployment_config"`
			} `json:"llm_configs"`
		} `json:"args"`
	} `json:"applications"`
}

func renderNativeServeConfig(t *testing.T, md *airunwayv1alpha1.ModelDeployment) nativeServeConfig {
	t.Helper()
	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	text, found, err := unstructured.NestedString(resources[0].Object, "spec", "serveConfigV2")
	if err != nil || !found {
		t.Fatalf("missing Serve config: found=%v, err=%v", found, err)
	}
	if strings.Contains(text, "vllm_serve") || strings.Contains(text, "VLLMDeployment") {
		t.Fatalf("Serve config still references an external wrapper: %s", text)
	}
	var config nativeServeConfig
	if err := yaml.UnmarshalStrict([]byte(text), &config); err != nil {
		t.Fatalf("invalid native Serve config: %v\n%s", err, text)
	}
	return config
}

func TestNativeServeConfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		scaling    *airunwayv1alpha1.ScalingSpec
		servedName string
		want       int32
	}{
		{name: "default", want: 1},
		{name: "stopped", scaling: &airunwayv1alpha1.ScalingSpec{Replicas: 0}},
		{name: "one", scaling: &airunwayv1alpha1.ScalingSpec{Replicas: 1}, want: 1},
		{name: "multiple", scaling: &airunwayv1alpha1.ScalingSpec{Replicas: 3}, want: 3, servedName: "qwen-api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newTestMD("native", "default")
			md.Spec.Model.ID = "Qwen/Qwen3-0.6B"
			md.Spec.Model.ServedName = tc.servedName
			md.Spec.Scaling = tc.scaling
			config := renderNativeServeConfig(t, md)
			if tc.want == 0 {
				// The builder rejects num_replicas=0. No application means no
				// LLM or ingress, including no autoscaler that could restart it.
				if len(config.Applications) != 0 {
					t.Fatal("stopped deployment must remove all Serve applications")
				}
				return
			}
			if len(config.Applications) != 1 {
				t.Fatalf("expected one application, got %d", len(config.Applications))
			}
			app := config.Applications[0]
			if app.Name != "llm" || app.RoutePrefix != "/" || app.ImportPath != "ray.serve.llm:build_openai_app" {
				t.Fatalf("unexpected native application: %+v", app)
			}
			if len(app.Args.LLMConfigs) != 1 {
				t.Fatalf("expected one LLMConfig, got %d", len(app.Args.LLMConfigs))
			}
			llm := app.Args.LLMConfigs[0]
			modelID := md.Spec.Model.ID
			if tc.servedName != "" {
				modelID = tc.servedName
			}
			wantModel := map[string]interface{}{"model_id": modelID, "model_source": md.Spec.Model.ID}
			if !reflect.DeepEqual(llm.ModelLoadingConfig, wantModel) {
				t.Errorf("model loading config = %v, want %v", llm.ModelLoadingConfig, wantModel)
			}
			if !reflect.DeepEqual(llm.DeploymentConfig, map[string]interface{}{"num_replicas": float64(tc.want)}) {
				t.Errorf("replicas must be fixed without autoscaling: %v", llm.DeploymentConfig)
			}
			wantKwargs := map[string]interface{}{"tensor_parallel_size": float64(1), "trust_remote_code": false}
			if !reflect.DeepEqual(llm.EngineKwargs, wantKwargs) {
				t.Errorf("engine kwargs = %v, want %v", llm.EngineKwargs, wantKwargs)
			}
		})
	}
}

func TestNativeServeConfigEngineOverrides(t *testing.T) {
	md := newTestMD("native", "default")
	contextLength := int32(4096)
	md.Spec.Engine.ContextLength = &contextLength
	md.Spec.Engine.TrustRemoteCode = true
	md.Spec.Resources.GPU.Count = 4
	llm := renderNativeServeConfig(t, md).Applications[0].Args.LLMConfigs[0]
	if llm.EngineKwargs["tensor_parallel_size"] != float64(4) ||
		llm.EngineKwargs["max_model_len"] != float64(4096) || llm.EngineKwargs["trust_remote_code"] != true {
		t.Fatalf("structured engine settings lost: %v", llm.EngineKwargs)
	}

	md.Spec.Engine.Args = map[string]string{
		"max-model-len":          "2048",
		"tensor-parallel-size":   "2",
		"trust-remote-code":      "false",
		"gpu-memory-utilization": "0.85",
		"dtype":                  "half",
		"enforce-eager":          "",
		"hf-overrides":           `{"architectures":["Qwen3ForCausalLM"]}`,
		"model":                  "/models/local: weights",
		"served-model-name":      "001",
	}
	llm = renderNativeServeConfig(t, md).Applications[0].Args.LLMConfigs[0]
	want := map[string]interface{}{
		"max_model_len":          float64(2048),
		"tensor_parallel_size":   float64(2),
		"trust_remote_code":      false,
		"gpu_memory_utilization": 0.85,
		"dtype":                  "half",
		"enforce_eager":          true,
		"hf_overrides":           map[string]interface{}{"architectures": []interface{}{"Qwen3ForCausalLM"}},
	}
	if !reflect.DeepEqual(llm.EngineKwargs, want) {
		t.Errorf("engine overrides = %#v, want %#v", llm.EngineKwargs, want)
	}
	wantModel := map[string]interface{}{"model_id": "001", "model_source": "/models/local: weights"}
	if !reflect.DeepEqual(llm.ModelLoadingConfig, wantModel) {
		t.Errorf("model overrides = %v, want %v", llm.ModelLoadingConfig, wantModel)
	}
}

func TestNativeServeConfigQuotesModelNames(t *testing.T) {
	md := newTestMD("native", "default")
	md.Spec.Model.ID = "model: source\nwith a newline"
	md.Spec.Model.ServedName = "model: alias\nwith a newline"
	llm := renderNativeServeConfig(t, md).Applications[0].Args.LLMConfigs[0]
	if llm.ModelLoadingConfig["model_source"] != md.Spec.Model.ID ||
		llm.ModelLoadingConfig["model_id"] != md.Spec.Model.ServedName {
		t.Fatalf("YAML must preserve model strings: %v", llm.ModelLoadingConfig)
	}
}

func TestNativeServeWorkerEnvironment(t *testing.T) {
	md := newTestMD("native", "default")
	md.Spec.Image = "legacy-image"
	md.Spec.Engine.Image = "custom-ray-image"
	md.Spec.Secrets = &airunwayv1alpha1.SecretsSpec{HuggingFaceToken: "hf-credentials"}
	md.Spec.Env = []corev1.EnvVar{
		{Name: "HF_HOME", Value: "/model-cache"},
		{Name: "CUSTOM_TOKEN", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "custom-credentials"}, Key: "token",
			},
		}},
	}
	tr := NewTransformer()
	groups := tr.buildAggregatedWorkerGroup(md)
	for _, rawGroup := range groups {
		group := rawGroup.(map[string]interface{})
		containers, _, err := unstructured.NestedSlice(group, "template", "spec", "containers")
		if err != nil {
			t.Fatal(err)
		}
		container := containers[0].(map[string]interface{})
		if container["image"] != md.Spec.Engine.Image {
			t.Errorf("engine image override lost: %v", container["image"])
		}
		if !reflect.DeepEqual(container["env"], tr.buildEnvVars(md)) {
			t.Errorf("native LLM worker must receive env and secret references: %v", container["env"])
		}
		limits, _, _ := unstructured.NestedMap(container, "resources", "limits")
		if limits["memory"] != DefaultWorkerMemory || limits["nvidia.com/gpu"] != "1" {
			t.Errorf("worker resources changed: %v", limits)
		}
	}
}
