package dynamo

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestIntentDiagnosticRequiresFailureEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, phase, status, message, want string
	}{
		{"known discovery failure", "Failed", "False", "GPU hardware info required but auto-discovery failed. Verify DCGM exporter is reachable.", gpuDiscoveryDiagnostic},
		{"GFD failure", "Pending", "False", "auto-discovery failed: no nodes with NVIDIA GPU Feature Discovery labels found (checked 3 nodes)", gpuDiscoveryDiagnostic},
		{"DCGM failure", "Profiling", "False", "failed to scrape any DCGM exporter pod: connection refused", gpuDiscoveryDiagnostic},
		{"GPU profiler unsupported", "Failed", "False", "AI Configurator: GPU SKU h100_pcie is not supported", gpuProfilerDiagnostic},
		{"missing performance data", "Failed", "False", "Profiler: missing performance data for GPU h100_pcie", gpuProfilerDiagnostic},
		{"profiler takes precedence", "Failed", "False", "GPU discovery failed; profiler GPU SKU not supported", gpuProfilerDiagnostic},
		{"missing fields not proof", "Pending", "", "", ""},
		{"generic failure", "Failed", "False", "Profiling job failed: backoff limit exceeded", ""},
		{"OOM not discovery", "Failed", "False", "CUDA out of memory on GPU", ""},
		{"scheduling not discovery", "Failed", "False", "Insufficient nvidia.com/gpu", ""},
		{"model unsupported", "Failed", "False", "Profiler model architecture is not supported", ""},
		{"in progress", "Profiling", "False", "Discovering GPU hardware and preparing profiling job", ""},
		{"success condition ignored", "Profiling", "True", "GPU discovery failed previously; recovered", ""},
		{"unknown condition ignored", "Profiling", "Unknown", "GPU discovery failed previously", ""},
		{"recovered phase", "Deployed", "False", "GPU discovery failed previously", ""},
		{"invalid oversized message", "Failed", "False", "GPU discovery failed " + strings.Repeat("x", 32768), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{
				"phase": tc.phase, "conditions": []any{map[string]any{"type": "Validation", "status": tc.status, "reason": "ValidationFailed", "message": tc.message}},
			}}}
			if got := intentDiagnostic(request); got != tc.want {
				t.Fatalf("diagnostic %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIntentDiagnosticLegacyAndStaleConditions(t *testing.T) {
	for _, field := range []string{"message", "error"} {
		request := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"phase": "Failed", field: "GPU discovery failed"}}}
		if got := intentDiagnostic(request); got != gpuDiscoveryDiagnostic {
			t.Fatalf("%s evidence was ignored: %q", field, got)
		}
		request.SetGeneration(2)
		_ = unstructured.SetNestedField(request.Object, int64(1), "status", "observedGeneration")
		if got := intentDiagnostic(request); got != "" {
			t.Fatalf("stale evidence was used: %q", got)
		}
	}
	request := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{
		"phase": "Failed", "conditions": []any{map[string]any{
			"type": "Validation", "status": "False", "reason": "GPUDiscoveryFailed", "observedGeneration": int64(1),
		}},
	}}}
	if got := intentDiagnostic(request); got != gpuDiscoveryDiagnostic {
		t.Fatalf("reason evidence was ignored: %q", got)
	}
	request.SetGeneration(2)
	if got := intentDiagnostic(request); got != "" {
		t.Fatalf("stale condition was used: %q", got)
	}
}
