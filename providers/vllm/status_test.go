package vllm

import (
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newTestDeployment(name, namespace string) *unstructured.Unstructured {
	d := &unstructured.Unstructured{}
	d.SetAPIVersion("apps/v1")
	d.SetKind("Deployment")
	d.SetName(name)
	d.SetNamespace(namespace)
	return d
}

func setDeploymentConditions(d *unstructured.Unstructured, conditions []map[string]interface{}) {
	condSlice := make([]interface{}, len(conditions))
	for i, c := range conditions {
		condSlice[i] = c
	}
	_ = unstructured.SetNestedSlice(d.Object, condSlice, "status", "conditions")
}

func setDeploymentReplicas(d *unstructured.Unstructured, desired, ready, available int64) {
	_ = unstructured.SetNestedField(d.Object, desired, "spec", "replicas")
	_ = unstructured.SetNestedField(d.Object, ready, "status", "readyReplicas")
	_ = unstructured.SetNestedField(d.Object, available, "status", "availableReplicas")
}

func TestTranslateStatusNilUpstream(t *testing.T) {
	st := NewStatusTranslator()
	_, err := st.TranslateStatus(nil)
	if err == nil {
		t.Fatal("expected error for nil upstream")
	}
}

func TestTranslateStatusNoConditions(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("test", "default")

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhasePending {
		t.Errorf("expected Pending phase, got %s", result.Phase)
	}
}

func TestTranslateStatusAvailableTrue(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("test", "default")
	setDeploymentReplicas(d, 2, 2, 2)
	d.SetGeneration(1)
	_ = unstructured.SetNestedField(d.Object, int64(1), "status", "observedGeneration")
	_ = unstructured.SetNestedField(d.Object, int64(2), "status", "replicas")
	_ = unstructured.SetNestedField(d.Object, int64(2), "status", "updatedReplicas")
	setDeploymentConditions(d, []map[string]interface{}{
		{"type": "Available", "status": "True"},
		{"type": "Progressing", "status": "True"},
	})

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
		t.Errorf("expected Running phase, got %s", result.Phase)
	}
	if result.Replicas == nil || result.Replicas.Desired != 2 {
		t.Errorf("expected 2 desired replicas, got %v", result.Replicas)
	}
	if result.Replicas.Ready != 2 {
		t.Errorf("expected 2 ready replicas, got %v", result.Replicas.Ready)
	}
}

func TestTranslateStatusAvailableTrueRequiresCurrentReplicas(t *testing.T) {
	tests := []struct {
		name        string
		desired     int64
		ready       int64
		available   int64
		setReplicas bool
	}{
		{name: "ready count is stale", desired: 1, ready: 0, available: 1, setReplicas: true},
		{name: "available count is stale", desired: 1, ready: 1, available: 0, setReplicas: true},
		{name: "both counts are stale", desired: 1, ready: 0, available: 0, setReplicas: true},
		{name: "counts are below desired", desired: 2, ready: 1, available: 1, setReplicas: true},
		{name: "scaled to zero is not serving", desired: 0, ready: 0, available: 0, setReplicas: true},
		{name: "replica fields are missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := NewStatusTranslator()
			d := newTestDeployment("test", "default")
			if tt.setReplicas {
				setDeploymentReplicas(d, tt.desired, tt.ready, tt.available)
				d.SetGeneration(1)
				_ = unstructured.SetNestedField(d.Object, int64(1), "status", "observedGeneration")
				_ = unstructured.SetNestedField(d.Object, tt.desired, "status", "replicas")
				_ = unstructured.SetNestedField(d.Object, tt.desired, "status", "updatedReplicas")
			}
			setDeploymentConditions(d, []map[string]interface{}{
				{"type": "Available", "status": "True"},
				{"type": "Progressing", "status": "True"},
			})

			result, err := st.TranslateStatus(d)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
				t.Errorf("expected Deploying phase, got %s", result.Phase)
			}
		})
	}
}

func TestTranslateStatusProgressingDeadlineExceeded(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("test", "default")
	setDeploymentConditions(d, []map[string]interface{}{
		{
			"type":    "Progressing",
			"status":  "False",
			"reason":  "ProgressDeadlineExceeded",
			"message": "deployment timed out",
		},
	})

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed phase, got %s", result.Phase)
	}
	if result.Message == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestTranslateStatusProgressing(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("test", "default")
	setDeploymentReplicas(d, 3, 1, 1)
	setDeploymentConditions(d, []map[string]interface{}{
		{"type": "Available", "status": "False"},
		{"type": "Progressing", "status": "True"},
	})

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
		t.Errorf("expected Deploying phase, got %s", result.Phase)
	}
}

func TestTranslateStatusEndpoint(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("my-deployment", "default")

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Endpoint == nil {
		t.Fatal("expected endpoint")
	}
	if result.Endpoint.Service != "my-deployment" {
		t.Errorf("expected service name 'my-deployment', got %s", result.Endpoint.Service)
	}
	if result.Endpoint.Port != int32(DefaultVLLMPort) {
		t.Errorf("expected port %d, got %d", DefaultVLLMPort, result.Endpoint.Port)
	}
}

func TestTranslateStatusResourceName(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("my-deployment", "default")

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ResourceName != "my-deployment" {
		t.Errorf("expected resource name 'my-deployment', got %s", result.ResourceName)
	}
	if result.ResourceKind != "Deployment" {
		t.Errorf("expected resource kind 'Deployment', got %s", result.ResourceKind)
	}
}

func TestTranslateStatusAvailableFalseWithMessage(t *testing.T) {
	st := NewStatusTranslator()
	d := newTestDeployment("test", "default")
	setDeploymentConditions(d, []map[string]interface{}{
		{
			"type":    "Available",
			"status":  "False",
			"message": "insufficient replicas",
		},
	})

	result, err := st.TranslateStatus(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		t.Errorf("expected Failed phase when Available=False with message, got %s", result.Phase)
	}
	if result.Message != "insufficient replicas" {
		t.Errorf("expected message 'insufficient replicas', got %s", result.Message)
	}
}

func TestTranslateStatusRequiresCompleteCurrentRollout(t *testing.T) {
	for _, tt := range []struct {
		name        string
		observed    int64
		desired     int64
		total       int64
		updated     int64
		ready       int64
		available   int64
		missing     string
		progressing bool
		want        airunwayv1alpha1.DeploymentPhase
	}{
		{
			name: "spec not yet observed", observed: 1, desired: 1, total: 1, updated: 1, ready: 1, available: 1,
			progressing: true, want: airunwayv1alpha1.DeploymentPhaseDeploying,
		},
		{
			name: "old ready pod and pending replacement", observed: 2, desired: 1, total: 2, updated: 1, ready: 1, available: 1,
			progressing: true, want: airunwayv1alpha1.DeploymentPhaseDeploying,
		},
		{
			name: "not all replicas updated", observed: 2, desired: 2, total: 2, updated: 1, ready: 2, available: 2,
			progressing: true, want: airunwayv1alpha1.DeploymentPhaseDeploying,
		},
		{
			name: "old replica still present after replacement is ready", observed: 2, desired: 1, total: 2, updated: 1, ready: 2, available: 2,
			progressing: true, want: airunwayv1alpha1.DeploymentPhaseDeploying,
		},
		{
			name: "replacement not ready", observed: 2, desired: 1, total: 1, updated: 1,
			progressing: true, want: airunwayv1alpha1.DeploymentPhaseDeploying,
		},
		{
			name: "complete actual rollout", observed: 2, desired: 1, total: 1, updated: 1, ready: 1, available: 1,
			progressing: true, want: airunwayv1alpha1.DeploymentPhaseRunning,
		},
		{
			name: "complete multi replica rollout", observed: 2, desired: 3, total: 3, updated: 3, ready: 3, available: 3,
			want: airunwayv1alpha1.DeploymentPhaseRunning,
		},
		{
			name: "available old revision without progressing condition", observed: 2, desired: 1, total: 2, updated: 1, ready: 1, available: 1,
			want: airunwayv1alpha1.DeploymentPhaseDeploying,
		},
		{
			name: "zero desired is not serving", observed: 2, want: airunwayv1alpha1.DeploymentPhaseDeploying,
		},
		{
			name: "zero desired while old pod drains", observed: 2, total: 1, ready: 1, available: 1,
			want: airunwayv1alpha1.DeploymentPhaseDeploying,
		},
		{
			name: "missing observed generation", desired: 1, total: 1, updated: 1, ready: 1, available: 1,
			missing: "observedGeneration", want: airunwayv1alpha1.DeploymentPhaseDeploying,
		},
		{
			name: "missing total replicas", observed: 2, desired: 1, updated: 1, ready: 1, available: 1,
			missing: "replicas", want: airunwayv1alpha1.DeploymentPhaseDeploying,
		},
		{
			name: "missing updated replicas", observed: 2, desired: 1, total: 1, ready: 1, available: 1,
			missing: "updatedReplicas", want: airunwayv1alpha1.DeploymentPhaseDeploying,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDeployment("test", "default")
			d.SetGeneration(2)
			setDeploymentReplicas(d, tt.desired, tt.ready, tt.available)
			_ = unstructured.SetNestedField(d.Object, tt.observed, "status", "observedGeneration")
			_ = unstructured.SetNestedField(d.Object, tt.total, "status", "replicas")
			_ = unstructured.SetNestedField(d.Object, tt.updated, "status", "updatedReplicas")
			if tt.missing != "" {
				unstructured.RemoveNestedField(d.Object, "status", tt.missing)
			}
			conditions := []map[string]any{{"type": "Available", "status": "True"}}
			if tt.progressing {
				conditions = append(conditions, map[string]any{"type": "Progressing", "status": "True"})
			}
			setDeploymentConditions(d, conditions)
			result, err := NewStatusTranslator().TranslateStatus(d)
			if err != nil {
				t.Fatal(err)
			}
			if result.Phase != tt.want {
				t.Errorf("phase = %s; want %s", result.Phase, tt.want)
			}
			wantReplicas := airunwayv1alpha1.ReplicaStatus{
				Desired: int32(tt.desired), Ready: int32(tt.ready), Available: int32(tt.available),
			}
			if result.Replicas == nil || *result.Replicas != wantReplicas {
				t.Errorf("replica status = %+v; want %+v", result.Replicas, wantReplicas)
			}
		})
	}
}
