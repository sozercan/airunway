package dynamo

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const maxIntentWorkers = 32

// selectedConfig is a full DGD manifest in the supported Dynamo releases. Do
// not interpret arbitrary optimizer records or merge fields from two sources.
func intentPlan(request, workload *unstructured.Unstructured) *api.ProviderIntentPlanStatus {
	if request != nil && summaryStatusCurrent(request) {
		selected, _, _ := unstructured.NestedMap(request.Object, "status", "profilingResults", "selectedConfig")
		if plan := summarizeDynamoPlan(selected, "selectedConfig"); plan != nil {
			return plan
		}
	}
	if workload != nil {
		return summarizeDynamoPlan(workload.Object, "workload")
	}
	return nil
}

func summaryStatusCurrent(object *unstructured.Unstructured) bool {
	observed, found, _ := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	return !found || observed >= object.GetGeneration()
}

func summarizeDynamoPlan(object map[string]any, source string) *api.ProviderIntentPlanStatus {
	if kind, exists := object["kind"]; exists && kind != DynamoGraphDeploymentKind {
		return nil
	}
	if version, exists := object["apiVersion"]; exists && version != "nvidia.com/v1alpha1" && version != "nvidia.com/v1beta1" {
		return nil
	}
	plan := &api.ProviderIntentPlanStatus{Source: source}
	engine, _, _ := unstructured.NestedString(object, "spec", "backendFramework")
	if summaryEngine(engine) {
		plan.Engine = engine
	}
	declaredEngine := plan.Engine
	components := dynamoComponents(object, "spec")
	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)
	unknownEngine, conflictingEngine, conflictingRole := false, false, false
	allAggregated, anyAggregated := true, false
	for _, name := range names {
		component, ok := components[name].(map[string]any)
		if !ok || summaryText(name) == "" {
			continue
		}
		container := summaryMainContainer(component)
		launchEngine, args := summaryLaunch(container)
		role := strings.ToLower(dynamoComponentType(component))
		switch role {
		case "worker":
			if sub, _ := component["subComponentType"].(string); sub == "prefill" || sub == "decode" {
				role = sub
			}
		case "prefill", "decode":
		case "":
			if launchEngine == "" {
				continue
			}
		default:
			continue
		}
		worker := api.ProviderIntentWorkerStatus{
			Name: name, Role: role,
			Replicas:       summaryInt32(component["replicas"], 0),
			GPUsPerReplica: summaryWorkerGPUs(component, container),
		}
		mode, present, valid := summaryOption(args, "--disaggregation-mode")
		aggregated := summaryAggregatedLaunch(object, component, container, launchEngine, args)
		if present {
			if !valid || (mode != "prefill" && mode != "decode" && !aggregated) ||
				(role == "prefill" || role == "decode") && !aggregated && role != mode {
				worker.Role = ""
				conflictingRole = true
			} else if !aggregated {
				worker.Role = mode
				plan.ServingMode = "disaggregated"
			}
		}
		if aggregated && role == "prefill" {
			worker.Role = ""
			conflictingRole = true
		}
		if worker.Role == "prefill" {
			plan.ServingMode = "disaggregated"
		}
		allAggregated = allAggregated && aggregated
		anyAggregated = anyAggregated || aggregated
		worker.TensorParallelism, worker.PipelineParallelism = summaryParallelism(launchEngine, args)
		if launchEngine == "" {
			unknownEngine = true
		} else if plan.Engine == "" {
			plan.Engine = launchEngine
		} else if plan.Engine != launchEngine {
			conflictingEngine = true
		}
		if len(plan.Workers) < maxIntentWorkers {
			plan.Workers = append(plan.Workers, worker)
		}
	}
	if conflictingEngine || unknownEngine && declaredEngine == "" {
		plan.Engine = ""
	}
	if allAggregated && len(plan.Workers) > 0 {
		plan.ServingMode = "aggregated"
	}
	if conflictingRole || anyAggregated && plan.ServingMode == "disaggregated" {
		plan.ServingMode = ""
	}
	if plan.Engine == "" && len(plan.Workers) == 0 {
		return nil
	}
	return plan
}

func summaryMainContainer(component map[string]any) map[string]any {
	if _, beta := component["podTemplate"]; beta {
		containers, _, _ := unstructured.NestedSlice(component, "podTemplate", "spec", "containers")
		var main map[string]any
		for _, raw := range containers {
			container, _ := raw.(map[string]any)
			if container["name"] == "main" {
				if main != nil {
					return nil
				}
				main = container
			}
		}
		return main
	}
	main, _, _ := unstructured.NestedMap(component, "extraPodSpec", "mainContainer")
	return main
}

func summaryWorkerGPUs(component, container map[string]any) *int32 {
	// Multinode replicas can span pods; a main-container GPU limit is then not
	// a per-replica count. Do not label it as one or invent a node multiplier.
	if raw, exists := component["multinode"]; exists && raw != nil {
		return nil
	}
	_, beta := component["podTemplate"]
	for _, kind := range []string{"limits", "requests"} {
		resources, _, _ := unstructured.NestedMap(container, "resources", kind)
		values := make(map[string]any)
		if !beta {
			legacy, _, _ := unstructured.NestedMap(component, "resources", kind)
			if value, exists := legacy["gpu"]; exists {
				gpuType, _ := legacy["gpuType"].(string)
				if gpuType == "" {
					gpuType = "nvidia.com/gpu"
				}
				if gpuType != "nvidia.com/gpu" && gpuType != "amd.com/gpu" {
					return nil
				}
				values[gpuType] = value
			}
		}
		// An alpha mainContainer resource entry overrides the same base entry;
		// requests do not override limits. Native beta only uses the container.
		for _, key := range []string{"nvidia.com/gpu", "amd.com/gpu"} {
			if value, exists := resources[key]; exists {
				values[key] = value
			}
		}
		if len(values) > 1 {
			return nil
		}
		for _, value := range values {
			return summaryGPUCount(value)
		}
	}
	return nil
}

func summaryGPUCount(value any) *int32 {
	if text, ok := value.(string); ok {
		return summaryDecimal(text, 0)
	}
	return summaryInt32(value, 0)
}

func summaryInt32(value any, minimum int32) *int32 {
	var number float64
	switch n := value.(type) {
	case int64:
		if n < int64(minimum) || n > math.MaxInt32 {
			return nil
		}
		number = float64(n)
	case float64:
		number = n
	default:
		return nil
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < float64(minimum) || number > math.MaxInt32 || math.Trunc(number) != number {
		return nil
	}
	result := int32(number)
	return &result
}

func summaryText(value string) string {
	if len(value) > 256 || strings.TrimSpace(value) == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\n\r\t") {
		return ""
	}
	return value
}

func intentHardware(md *api.ModelDeployment, request *unstructured.Unstructured) *api.ProviderIntentHardwareStatus {
	observed, _, _ := unstructured.NestedMap(request.Object, "spec", "hardware")
	sku, _ := observed["gpuSku"].(string)
	hardware := &api.ProviderIntentHardwareStatus{
		GPUSKU: summaryText(sku), NumGPUsPerNode: summaryInt32(observed["numGpusPerNode"], 1),
	}
	hardware.VRAMMB = summaryVRAMMiB(observed["vramMb"])
	if hardware.GPUSKU == "" && hardware.VRAMMB == nil && hardware.NumGPUsPerNode == nil {
		return nil
	}
	// Only compare provided values, not totalGpus (a budget), node selectors,
	// inferred SKU aliases, or engine settings. Legacy intent uses spec.hardware.
	var input struct {
		Intent *struct {
			Hardware dynamointent.Hardware `json:"hardware"`
		} `json:"intent"`
		Spec struct {
			Hardware dynamointent.Hardware `json:"hardware"`
		} `json:"spec"`
	}
	if md.Spec.Provider != nil && md.Spec.Provider.Overrides != nil {
		if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &input); err != nil {
			return hardware // Unknown provenance is not evidence of discovery.
		}
	}
	provided := input.Spec.Hardware
	if input.Intent != nil {
		provided = input.Intent.Hardware
	}
	matched, unmatched := false, false
	mark := func(supplied bool) {
		matched = matched || supplied
		unmatched = unmatched || !supplied
	}
	if hardware.GPUSKU != "" {
		mark(hardware.GPUSKU == provided.GPUSKU)
	}
	if hardware.VRAMMB != nil {
		mark(provided.VRAMMB != nil && summarySameVRAM(*hardware.VRAMMB, *provided.VRAMMB))
	}
	if hardware.NumGPUsPerNode != nil {
		mark(provided.NumGPUsPerNode != nil && *hardware.NumGPUsPerNode == *provided.NumGPUsPerNode)
	}
	switch {
	case matched && unmatched:
		hardware.Source = "mixed"
	case matched:
		hardware.Source = "provided"
	default:
		hardware.Source = "discovered"
	}
	return hardware
}

// DGDR vramMb uses MiB despite its name. Only exactly representable positive
// integer values fit Runway's status contract; never round or estimate memory.
func summaryVRAMMiB(raw any) *int64 {
	switch value := raw.(type) {
	case int64:
		if value > 0 {
			return &value
		}
	case float64:
		if value > 0 && value < float64(math.MaxInt64) && math.Trunc(value) == value {
			result := int64(value)
			return &result
		}
	}
	return nil
}

func summarySameVRAM(observed int64, provided float64) bool {
	value := summaryVRAMMiB(provided)
	return value != nil && observed == *value
}
