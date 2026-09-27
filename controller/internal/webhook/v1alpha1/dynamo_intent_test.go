package v1alpha1

import (
	"context"
	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	"k8s.io/apimachinery/pkg/runtime"
	"strings"
	"testing"
)

func TestDynamoIntentDoesNotReceiveManualDefaults(t *testing.T) {
	md := &api.ModelDeployment{Spec: api.ModelDeploymentSpec{Model: api.ModelSpec{ID: "Qwen/Qwen3-0.6B"}, Engine: api.EngineSpec{Type: api.EngineTypeVLLM}, Provider: &api.ProviderSpec{Name: "dynamo", Overrides: &runtime.RawExtension{Raw: []byte(`{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":2}}}`)}}}}
	if err := (&ModelDeploymentCustomDefaulter{}).Default(context.Background(), md); err != nil {
		t.Fatal(err)
	}
	if md.Spec.Scaling != nil || md.Spec.Resources != nil {
		t.Fatal("automatic configuration acquired manual sizing defaults")
	}
	if err := dynamointent.Validate(md); err != nil {
		t.Fatal(err)
	}
	if _, err := (&ModelDeploymentCustomValidator{}).ValidateCreate(context.Background(), md); err != nil {
		t.Fatal(err)
	}
	old := md.DeepCopy()
	old.Status.Provider = &api.ProviderStatus{RequestRef: &api.ProviderResourceReference{Name: "request"}, Intent: &api.ProviderIntentStatus{Phase: "Profiling"}}
	next := old.DeepCopy()
	next.Spec.Model.ID = "Qwen/Qwen3-8B"
	if _, err := (&ModelDeploymentCustomValidator{}).ValidateUpdate(context.Background(), old, next); err == nil {
		t.Fatal("ordinary edit to locked model accepted")
	}
	next.Annotations = map[string]string{dynamointent.AttemptAnnotation: "next"}
	if _, err := (&ModelDeploymentCustomValidator{}).ValidateUpdate(context.Background(), old, next); err != nil {
		t.Fatalf("explicit reconfigure rejected: %v", err)
	}
}

func TestDynamoNativeIntentOverridesAdmission(t *testing.T) {
	md := &api.ModelDeployment{Spec: api.ModelDeploymentSpec{
		Model:    api.ModelSpec{ID: "Qwen/Qwen3-0.6B"},
		Engine:   api.EngineSpec{Type: api.EngineTypeVLLM},
		Provider: &api.ProviderSpec{Name: "dynamo", Overrides: &runtime.RawExtension{Raw: []byte(`{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":1},"overrides":{"profilingJob":{"activeDeadlineSeconds":1800},"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"DynamoGraphDeployment","spec":{"components":[{"name":"VllmDecodeWorker","podTemplate":{"spec":{"containers":[{"name":"main","$patch":{"args":"append"},"args":["--dyn-tool-call-parser","hermes"]}]}}}]}}}}}`)}},
	}}
	if err := (&ModelDeploymentCustomDefaulter{}).Default(context.Background(), md); err != nil {
		t.Fatal(err)
	}
	validator := &ModelDeploymentCustomValidator{}
	if _, err := validator.ValidateCreate(context.Background(), md); err != nil {
		t.Fatalf("native override rejected by admission: %v", err)
	}
	old := md.DeepCopy()
	old.Status.Provider = &api.ProviderStatus{RequestRef: &api.ProviderResourceReference{Name: "request"}, Intent: &api.ProviderIntentStatus{Phase: "Profiling"}}
	next := old.DeepCopy()
	next.Spec.Provider.Overrides.Raw = []byte(strings.ReplaceAll(string(next.Spec.Provider.Overrides.Raw), "hermes", "qwen3_coder"))
	if _, err := validator.ValidateUpdate(context.Background(), old, next); err == nil {
		t.Fatal("parser edit accepted without a new attempt")
	}
	next.Annotations = map[string]string{dynamointent.AttemptAnnotation: "parser-2"}
	if _, err := validator.ValidateUpdate(context.Background(), old, next); err != nil {
		t.Fatalf("explicit native override reconfiguration rejected: %v", err)
	}
	for _, change := range []func(*api.ModelDeployment){
		func(md *api.ModelDeployment) { md.Spec.Engine.ExtraArgs = []string{"--dyn-tool-call-parser", "hermes"} },
		func(md *api.ModelDeployment) {
			md.Spec.Engine.Args = map[string]string{"dyn-tool-call-parser": "hermes"}
		},
		func(md *api.ModelDeployment) {
			md.Spec.Provider.Overrides.Raw = []byte(strings.ReplaceAll(string(md.Spec.Provider.Overrides.Raw), `"activeDeadlineSeconds":1800`, `"template":{"spec":{"hostNetwork":true}}`))
		},
		func(md *api.ModelDeployment) {
			md.Spec.Provider.Overrides.Raw = []byte(strings.ReplaceAll(string(md.Spec.Provider.Overrides.Raw), `"$patch":{"args":"append"}`, `"resources":{"limits":{"nvidia.com/gpu":"8"}}`))
		},
	} {
		invalid := md.DeepCopy()
		change(invalid)
		if _, err := validator.ValidateCreate(context.Background(), invalid); err == nil {
			t.Fatal("unsupported engine setting or forbidden override accepted")
		}
	}
}
