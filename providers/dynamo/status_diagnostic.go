package dynamo

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	gpuDiscoveryDiagnostic = "Dynamo could not discover required GPU details. Check operator access to DCGM metrics and, if enabled, GPU Feature Discovery labels nvidia.com/gpu.product, nvidia.com/gpu.count, and nvidia.com/gpu.memory. Alternatively, provide the exact intent.hardware.gpuSku, vramMb (MiB), and numGpusPerNode. This does not confirm that labels are missing."
	gpuProfilerDiagnostic  = "The profiler reports unsupported GPU hardware or missing GPU performance data. Check support for the exact GPU SKU in this Dynamo profiler version; do not substitute another GPU SKU. Discovery settings alone will not add profiler support."
)

// intentDiagnostic adds prerequisites only when upstream failure evidence points
// to GPU discovery or profiler hardware support. Missing hardware fields alone,
// slow profiling, generic job failures and successful conditions prove neither.
func intentDiagnostic(request *unstructured.Unstructured) string {
	if !summaryStatusCurrent(request) {
		return ""
	}
	phase, _, _ := unstructured.NestedString(request.Object, "status", "phase")
	if phase == "Ready" || phase == "Deploying" || phase == "Deployed" {
		return "" // Discovery is finished; do not repeat a recovered failure.
	}
	var messages []string
	for _, key := range []string{"message", "error"} {
		message, _, _ := unstructured.NestedString(request.Object, "status", key)
		messages = append(messages, message)
	}
	conditions, _, _ := unstructured.NestedSlice(request.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["status"] != "False" {
			continue
		}
		if observed, ok := condition["observedGeneration"].(int64); ok && observed < request.GetGeneration() {
			continue
		}
		message, _ := condition["message"].(string)
		reason, _ := condition["reason"].(string)
		messages = append(messages, reason+" "+message)
	}
	discoveryFailed := false
	for _, raw := range messages {
		if len(raw) > 32768 {
			continue
		}
		message := strings.ToLower(raw)
		hardware := strings.Contains(message, "gpu") || strings.Contains(message, "hardware")
		profiler := strings.Contains(message, "profiler") || strings.Contains(message, "aiconfigurator") || strings.Contains(message, "ai configurator")
		if hardware && profiler && containsSummaryPhrase(message, "unsupported", "not supported", "no performance data", "missing performance data", "no profiling data") {
			return gpuProfilerDiagnostic
		}
		if containsSummaryPhrase(message,
			"gpu hardware info required but auto-discovery failed",
			"gpu discovery failed", "gpudiscoveryfailed", "failed to discover gpu", "unable to discover gpu",
			"no nodes with nvidia gpu feature discovery labels",
			"no dcgm exporter pods found", "failed to scrape any dcgm exporter pod",
			"no gpu metrics could be parsed", "no gpus detected from dcgm",
			"dcgm is not enabled", "listing dcgm exporter pods failed") {
			discoveryFailed = true
		}
	}
	if discoveryFailed {
		return gpuDiscoveryDiagnostic
	}
	return ""
}

func containsSummaryPhrase(message string, phrases ...string) bool {
	for _, phrase := range phrases {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}
