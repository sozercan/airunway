package dynamo

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func summaryFixture(beta bool, role, engine string, args ...string) *unstructured.Unstructured {
	main := map[string]any{
		"name": "main", "command": []any{"python3", "-m", "dynamo." + engine},
		"args": toInterfaceSlice(args),
	}
	component := map[string]any{"replicas": int64(2)}
	spec := map[string]any{}
	version := "v1alpha1"
	if beta {
		version = "v1beta1"
		main["resources"] = map[string]any{"limits": map[string]any{"nvidia.com/gpu": "4"}}
		component["name"], component["type"] = "custom-worker", role
		component["podTemplate"] = map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "sidecar", "command": []any{"python3", "-m", "dynamo.trtllm"}}, main,
		}}}
		spec["components"] = []any{component}
	} else {
		component["componentType"] = "worker"
		if role != "worker" {
			component["subComponentType"] = role
		}
		component["extraPodSpec"] = map[string]any{"mainContainer": main}
		component["resources"] = map[string]any{"limits": map[string]any{"gpu": "4"}}
		spec["services"] = map[string]any{"custom-worker": component}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "nvidia.com/" + version, "kind": DynamoGraphDeploymentKind, "spec": spec,
	}}
}

func summaryFixtureComponent(dgd *unstructured.Unstructured) map[string]any {
	spec := dgd.Object["spec"].(map[string]any)
	if components, ok := spec["components"].([]any); ok {
		return components[0].(map[string]any)
	}
	return spec["services"].(map[string]any)["custom-worker"].(map[string]any)
}

func TestIntentPlanReleasedShapes(t *testing.T) {
	for _, beta := range []bool{false, true} {
		for _, engine := range []string{"vllm", "sglang"} {
			t.Run(fmt.Sprintf("beta=%v/%s", beta, engine), func(t *testing.T) {
				tp, pp := "--tensor-parallel-size=4", "--pipeline-parallel-size"
				if engine == "sglang" {
					tp, pp = "--tp=4", "--pp-size"
				}
				dgd := summaryFixture(beta, "prefill", engine, tp, pp, "2", "--disaggregation-mode", "prefill")
				before := dgd.DeepCopy()
				plan := intentPlan(nil, dgd)
				if plan == nil || plan.Source != "workload" || plan.Engine != engine || plan.ServingMode != "disaggregated" || len(plan.Workers) != 1 {
					t.Fatalf("unexpected plan: %#v", plan)
				}
				worker := plan.Workers[0]
				if worker.Name != "custom-worker" || worker.Role != "prefill" || *worker.Replicas != 2 || *worker.GPUsPerReplica != 4 || *worker.TensorParallelism != 4 || *worker.PipelineParallelism != 2 {
					t.Fatalf("unexpected worker: %+v", worker)
				}
				if !reflect.DeepEqual(before.Object, dgd.Object) {
					t.Fatal("summary mutated the upstream resource")
				}
			})
		}
	}
}

func TestIntentPlanSourceAndFallback(t *testing.T) {
	workload := summaryFixture(true, "worker", "vllm", "--tensor-parallel-size", "8")
	selected := summaryFixture(false, "worker", "sglang", "--tp=2")
	request := &unstructured.Unstructured{Object: map[string]any{}}
	_ = unstructured.SetNestedMap(request.Object, selected.Object, "status", "profilingResults", "selectedConfig")
	plan := intentPlan(request, workload)
	if plan.Source != "selectedConfig" || plan.Engine != "sglang" || *plan.Workers[0].TensorParallelism != 2 {
		t.Fatalf("selected config did not win: %+v", plan)
	}
	// A partial selected plan is not padded with values from the workload.
	delete(summaryFixtureComponent(selected), "replicas")
	_ = unstructured.SetNestedMap(request.Object, selected.Object, "status", "profilingResults", "selectedConfig")
	if got := intentPlan(request, workload); got.Source != "selectedConfig" || got.Workers[0].Replicas != nil {
		t.Fatalf("mixed selected and workload sources: %+v", got)
	}
	for _, invalid := range []any{
		nil, "not a manifest", map[string]any{}, map[string]any{"tensor_parallel_size": int64(4)},
		map[string]any{"kind": "Job", "spec": selected.Object["spec"]},
		map[string]any{"apiVersion": "nvidia.com/v9", "spec": selected.Object["spec"]},
	} {
		_ = unstructured.SetNestedField(request.Object, invalid, "status", "profilingResults", "selectedConfig")
		got := intentPlan(request, workload)
		if got == nil || got.Source != "workload" || got.Engine != "vllm" || *got.Workers[0].TensorParallelism != 8 {
			t.Fatalf("bad fallback for %#v: %+v", invalid, got)
		}
		if got := intentPlan(request, nil); got != nil {
			t.Fatalf("unsupported selected config was summarized: %+v", got)
		}
	}
	_ = unstructured.SetNestedMap(request.Object, selected.Object, "status", "profilingResults", "selectedConfig")
	request.SetGeneration(2)
	_ = unstructured.SetNestedField(request.Object, int64(1), "status", "observedGeneration")
	if got := intentPlan(request, workload); got.Source != "workload" {
		t.Fatalf("stale selected config was used: %+v", got)
	}
}

func TestIntentPlanUnknownsAndNoNameInference(t *testing.T) {
	dgd := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"services": map[string]any{
		"VllmPrefillWorker": map[string]any{"replicas": int64(3)},
		"Frontend":          map[string]any{"componentType": "frontend", "replicas": int64(1)},
		"Epp":               map[string]any{"componentType": "epp", "replicas": int64(1)},
		"SglangDecodeWorker": map[string]any{"componentType": "worker", "tensorParallelism": int64(9),
			"gpuCount": int64(8), "extraPodSpec": map[string]any{"mainContainer": map[string]any{
				"image": "sglang-runtime:1.5.0", "args": []any{"--tp", "2"},
			}}},
	}}}}
	plan := intentPlan(nil, dgd)
	if plan == nil || len(plan.Workers) != 1 || plan.Engine != "" || plan.ServingMode != "" {
		t.Fatalf("inferred fields from names: %+v", plan)
	}
	worker := plan.Workers[0]
	if worker.Role != "worker" || worker.Replicas != nil || worker.GPUsPerReplica != nil || worker.TensorParallelism != nil || worker.PipelineParallelism != nil {
		t.Fatalf("fabricated values: %+v", worker)
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"source":"workload","workers":[{"name":"SglangDecodeWorker","role":"worker"}]}` {
		t.Fatalf("unknown fields must be absent on the wire: %s", data)
	}
	// Beta's aggregated template explicitly uses type=decode. Do not call it
	// disaggregated, and do not invent default TP/PP=1 from an absent flag.
	plan = intentPlan(nil, summaryFixture(true, "decode", "vllm"))
	if plan.ServingMode != "" || plan.Workers[0].TensorParallelism != nil || plan.Workers[0].PipelineParallelism != nil {
		t.Fatalf("invented defaults: %+v", plan)
	}
}

func TestIntentPlanBoundedDeterministicWorkers(t *testing.T) {
	services := make(map[string]any)
	for i := 39; i >= 0; i-- {
		dgd := summaryFixture(false, "worker", "vllm")
		services[fmt.Sprintf("worker-%02d", i)] = summaryFixtureComponent(dgd)
	}
	// This is beyond the retained prefix. Global fields must still account
	// for it instead of presenting the first 32 as a complete topology.
	services["worker-39"] = summaryFixtureComponent(summaryFixture(false, "prefill", "sglang"))
	dgd := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"services": services}}}
	var previous *api.ProviderIntentPlanStatus
	for i := 0; i < 10; i++ {
		plan := intentPlan(nil, dgd)
		if len(plan.Workers) != maxIntentWorkers || plan.Engine != "" || plan.ServingMode != "disaggregated" {
			t.Fatalf("incorrect bounded summary: %+v", plan)
		}
		for i, worker := range plan.Workers {
			if worker.Name != fmt.Sprintf("worker-%02d", i) {
				t.Fatalf("unordered summary: %+v", plan.Workers)
			}
		}
		if previous != nil && !reflect.DeepEqual(previous, plan) {
			t.Fatal("summary changed across identical observations")
		}
		previous = plan
	}
}

func TestIntentPlanNumericSafety(t *testing.T) {
	for _, value := range []any{nil, "2", float64(1.5), int64(-1), int64(math.MaxInt32) + 1, math.NaN(), math.Inf(1)} {
		dgd := summaryFixture(false, "worker", "vllm")
		component := summaryFixtureComponent(dgd)
		component["replicas"] = value
		component["resources"] = map[string]any{"limits": map[string]any{"gpu": value}}
		worker := intentPlan(nil, dgd).Workers[0]
		if worker.Replicas != nil {
			t.Fatalf("invalid replicas %v: %+v", value, worker)
		}
		if value != "2" && worker.GPUsPerReplica != nil {
			t.Fatalf("invalid GPU count %v: %+v", value, worker)
		}
	}
	for _, value := range []any{int64(0), float64(0), int64(math.MaxInt32)} {
		if got := summaryInt32(value, 0); got == nil {
			t.Fatalf("explicit integral replicas should survive: %v", value)
		}
	}
}

func TestIntentPlanGPUResourcePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   *int32
	}{
		{"request does not replace limit", func(c map[string]any) {
			c["extraPodSpec"].(map[string]any)["mainContainer"].(map[string]any)["resources"] = map[string]any{"requests": map[string]any{"nvidia.com/gpu": "2"}}
		}, summaryDecimal("4", 0)},
		{"main limit override", func(c map[string]any) {
			c["extraPodSpec"].(map[string]any)["mainContainer"].(map[string]any)["resources"] = map[string]any{"limits": map[string]any{"nvidia.com/gpu": "8"}}
		}, summaryDecimal("8", 0)},
		{"multinode", func(c map[string]any) { c["multinode"] = map[string]any{"nodeCount": int64(2)} }, nil},
		{"MIG is not a full GPU", func(c map[string]any) {
			c["resources"] = map[string]any{"limits": map[string]any{"gpu": "4", "gpuType": "nvidia.com/mig-1g.10gb"}}
		}, nil},
		{"malformed limit does not fall back", func(c map[string]any) {
			c["resources"] = map[string]any{"limits": map[string]any{"gpu": "0.5"}, "requests": map[string]any{"gpu": "4"}}
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dgd := summaryFixture(false, "worker", "vllm")
			tc.mutate(summaryFixtureComponent(dgd))
			if got := intentPlan(nil, dgd).Workers[0].GPUsPerReplica; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIntentHardwareProvenanceAndMiB(t *testing.T) {
	observed := map[string]any{"gpuSku": "h100_pcie", "vramMb": float64(80000), "numGpusPerNode": int64(4), "totalGpus": int64(8)}
	for _, tc := range []struct{ name, input, source string }{
		{"discovered", `{"intent":{"hardware":{"totalGpus":8}}}`, "discovered"},
		{"provided", `{"intent":{"hardware":{"totalGpus":8,"gpuSku":"h100_pcie","vramMb":80000,"numGpusPerNode":4}}}`, "provided"},
		{"mixed", `{"intent":{"hardware":{"totalGpus":8,"gpuSku":"h100_pcie"}}}`, "mixed"},
		{"legacy", `{"spec":{"hardware":{"gpuSku":"h100_pcie","vramMb":80000,"numGpusPerNode":4}}}`, "provided"},
		{"different supplied SKU", `{"intent":{"hardware":{"totalGpus":8,"gpuSku":"h100_sxm"}}}`, "discovered"},
		{"invalid intent", `{"intent":"bad"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newMDForController("test", "default")
			md.Spec.Provider = &api.ProviderSpec{Overrides: &runtime.RawExtension{Raw: []byte(tc.input)}}
			request := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"hardware": observed}}}
			before := request.DeepCopy()
			got := intentHardware(md, request)
			if got == nil || got.GPUSKU != "h100_pcie" || *got.VRAMMB != 80000 || *got.NumGPUsPerNode != 4 || got.Source != tc.source {
				t.Fatalf("incorrect hardware/provenance: %+v", got)
			}
			if !reflect.DeepEqual(request.Object, before.Object) {
				t.Fatal("summary mutated observed hardware")
			}
			data, err := json.Marshal(got)
			if err != nil || strings.Contains(string(data), "totalGpus") || strings.Contains(string(data), "sxm") {
				t.Fatalf("invented or unbounded hardware fields: %s, %v", data, err)
			}
		})
	}
}

func TestIntentHardwareUnknownValues(t *testing.T) {
	md := newMDForController("test", "default")
	for _, hardware := range []any{nil, "invalid", map[string]any{}, map[string]any{"totalGpus": int64(8)},
		map[string]any{"gpuSku": strings.Repeat("x", 257), "vramMb": 80000.5, "numGpusPerNode": int64(0)},
		map[string]any{"gpuSku": " ", "vramMb": math.NaN(), "numGpusPerNode": 3.5},
	} {
		request := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"hardware": hardware}}}
		if got := intentHardware(md, request); got != nil {
			t.Fatalf("unknown hardware was fabricated: %+v", got)
		}
	}
	for _, value := range []any{float64(1.5), float64(math.MaxInt64), math.NaN(), math.Inf(1), int64(-1), int64(0), "80000"} {
		if got := summaryVRAMMiB(value); got != nil {
			t.Fatalf("invalid MiB value %v became %d", value, *got)
		}
	}
	for _, value := range []any{float64(80000), int64(80000), int64(math.MaxInt64)} {
		if got := summaryVRAMMiB(value); got == nil {
			t.Fatalf("exact positive MiB value omitted: %v", value)
		}
	}
}

func TestIntentPlanNormalAggregatedRuntimeDefaults(t *testing.T) {
	// Mirrors the released profiler agg templates' executable, role, image and
	// model arguments, with optional opaque credential imports omitted. Decode
	// is intentional for vLLM, even though the engine serves both phases.
	for _, version := range []string{"1.1.1", "1.5.0"} {
		for _, engine := range []string{"vllm", "sglang", "trtllm"} {
			t.Run(version+"/"+engine, func(t *testing.T) {
				repository, role := engine+"-runtime", "worker"
				if engine == "vllm" {
					role = "decode"
				}
				if engine == "trtllm" {
					repository = "tensorrtllm-runtime"
				}
				modelFlag := "--model-path"
				if engine == "vllm" {
					modelFlag = "--model"
				}
				dgd := summaryFixture(version == "1.5.0", role, engine, modelFlag, "Qwen/Qwen3-0.6B")
				component := summaryFixtureComponent(dgd)
				var main map[string]any
				if version == "1.5.0" {
					main = component["podTemplate"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[1].(map[string]any)
				} else {
					main = component["extraPodSpec"].(map[string]any)["mainContainer"].(map[string]any)
				}
				main["image"] = "nvcr.io/nvidia/ai-dynamo/" + repository + ":" + version
				main["env"] = []any{map[string]any{"name": "HF_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "hf", "key": "token"}}}}
				request := &unstructured.Unstructured{Object: map[string]any{}}
				_ = unstructured.SetNestedMap(request.Object, dgd.Object, "status", "profilingResults", "selectedConfig")
				if plan := intentPlan(request, nil); plan == nil || plan.Source != "selectedConfig" || plan.ServingMode != "aggregated" {
					t.Fatalf("normal aggregate launch was not recognized: %+v", plan)
				}
				main["env"] = []any{map[string]any{"name": "DYN_VLLM_DISAGGREGATION_MODE", "value": "prefill"}}
				if plan := intentPlan(nil, dgd); plan.ServingMode != "" {
					t.Fatalf("mode override was ignored: %+v", plan)
				}
				delete(main, "env")
				main["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "custom-env"}}}
				if plan := intentPlan(nil, dgd); plan.ServingMode != "" {
					t.Fatalf("opaque environment was treated as known: %+v", plan)
				}
				delete(main, "envFrom")
				main["image"] = "custom/runtime:latest"
				if plan := intentPlan(nil, dgd); plan.ServingMode != "" {
					t.Fatalf("custom image default was assumed: %+v", plan)
				}
			})
		}
	}
}
