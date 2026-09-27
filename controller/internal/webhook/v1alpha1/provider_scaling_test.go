package v1alpha1

import (
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

const scalingTestKAITO = "kaito"

func TestProviderScalingLimits(t *testing.T) {
	for _, provider := range []string{scalingTestKAITO, "vllm", "dynamo", "kuberay", "llmd", ""} {
		for _, selected := range []bool{false, true} {
			obj := &airunwayv1alpha1.ModelDeployment{Spec: airunwayv1alpha1.ModelDeploymentSpec{Scaling: &airunwayv1alpha1.ScalingSpec{Replicas: 0}}}
			if selected {
				obj.Status.Provider = &airunwayv1alpha1.ProviderStatus{Name: provider}
			} else {
				obj.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: provider}
			}
			errs := validateProviderScaling(obj, nil)
			if (len(errs) > 0) != (provider == scalingTestKAITO) {
				t.Fatalf("provider=%s selected=%t errors=%v", provider, selected, errs)
			}
			old := obj.DeepCopy()
			old.Spec.Scaling.Replicas = 1
			if (len(validateProviderScaling(obj, old)) > 0) != (provider == scalingTestKAITO) {
				t.Fatal("update did not enforce provider limit")
			}
			if errs := validateProviderScaling(obj, obj.DeepCopy()); len(errs) > 0 {
				t.Fatalf("pre-existing zero blocks bookkeeping: %v", errs)
			}
			obj.Spec.Scaling = nil
			if errs := validateProviderScaling(obj, nil); len(errs) > 0 {
				t.Fatalf("default scaling rejected: %v", errs)
			}
		}
	}
	obj := &airunwayv1alpha1.ModelDeployment{Spec: airunwayv1alpha1.ModelDeploymentSpec{Provider: &airunwayv1alpha1.ProviderSpec{Name: "vllm"}, Scaling: &airunwayv1alpha1.ScalingSpec{Replicas: 0}}, Status: airunwayv1alpha1.ModelDeploymentStatus{Provider: &airunwayv1alpha1.ProviderStatus{Name: scalingTestKAITO}}}
	if errs := validateProviderScaling(obj, nil); len(errs) > 0 {
		t.Fatalf("stale selection overrides explicit provider: %v", errs)
	}
}
