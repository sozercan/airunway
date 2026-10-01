package v1alpha1

import (
	"context"
	"strings"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDynamoToolCallingAdmission(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	config := &api.InferenceProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: "dynamo"}, Spec: api.InferenceProviderConfigSpec{Capabilities: &api.ProviderCapabilities{Engines: []api.EngineCapability{{Name: api.EngineTypeVLLM, GPUSupport: true, ServingModes: []api.ServingMode{api.ServingModeAggregated}}}}}}
	validator := ModelDeploymentCustomValidator{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()}
	md := &api.ModelDeployment{ObjectMeta: metav1.ObjectMeta{Name: "tools", Namespace: "default"}, Spec: api.ModelDeploymentSpec{Model: api.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: api.ModelSourceHuggingFace}, Engine: api.EngineSpec{Type: api.EngineTypeVLLM, ToolCalling: true}, Provider: &api.ProviderSpec{Name: "dynamo", Overrides: &runtime.RawExtension{Raw: []byte(`{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":1}}}`)}}}}
	if _, err := validator.ValidateCreate(context.Background(), md); err != nil {
		t.Fatalf("valid tool configuration rejected: %v", err)
	}
	unknown := md.DeepCopy()
	unknown.Spec.Model.ID = "other/model"
	if _, err := validator.ValidateCreate(context.Background(), unknown); err == nil || !strings.Contains(err.Error(), "spec.engine.toolCalling") {
		t.Fatalf("expected engine field error, got %v", err)
	}
	unknown.Spec.Engine.ToolCallParser = "hermes"
	if _, err := validator.ValidateCreate(context.Background(), unknown); err != nil {
		t.Fatalf("explicit parser rejected: %v", err)
	}
	old := md.DeepCopy()
	old.Status.Provider = &api.ProviderStatus{RequestRef: &api.ProviderResourceReference{Name: "request"}, Intent: &api.ProviderIntentStatus{Phase: "Deployed"}}
	updated := old.DeepCopy()
	updated.Spec.Engine.ReasoningParser = "basic"
	if _, err := validator.ValidateUpdate(context.Background(), old, updated); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("expected profiling lock, got %v", err)
	}
	updated.Annotations = map[string]string{"airunway.ai/dynamo-attempt": "new-tools"}
	if _, err := validator.ValidateUpdate(context.Background(), old, updated); err != nil {
		t.Fatalf("explicit reconfiguration rejected: %v", err)
	}
}
