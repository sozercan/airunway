package dynamo

import (
	"context"
	"reflect"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIntentSummaryReadRefreshesAndClears(t *testing.T) {
	ctx := context.Background()
	md := newMDForController("summary", "default")
	setIntentMode(md)
	request := requestFixture(t, md, "Failed")
	_ = unstructured.SetNestedMap(request.Object, map[string]any{"gpuSku": "h100_pcie", "vramMb": int64(80000)}, "spec", "hardware")
	_ = unstructured.SetNestedField(request.Object, "GPU discovery failed", "status", "message")
	selected := summaryFixture(true, "decode", "vllm", "--tensor-parallel-size", "4")
	_ = unstructured.SetNestedMap(request.Object, selected.Object, "status", "profilingResults", "selectedConfig")
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(request).Build()
	before := request.DeepCopy()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	if _, err := r.readServingStatus(ctx, md, request); err != nil {
		t.Fatal(err)
	}
	intent := md.Status.Provider.Intent
	if intent.Hardware == nil || intent.Plan == nil || intent.Plan.Source != "selectedConfig" || intent.Diagnostic != gpuDiscoveryDiagnostic || intent.Phase != "Failed" {
		t.Fatalf("missing intent summary: %+v", intent)
	}
	if !reflect.DeepEqual(before.Object, request.Object) {
		t.Fatal("status observation changed the request")
	}
	copy := md.DeepCopy()
	*copy.Status.Provider.Intent.Hardware.VRAMMB = 1
	*copy.Status.Provider.Intent.Plan.Workers[0].TensorParallelism = 1
	if *intent.Hardware.VRAMMB != 80000 || *intent.Plan.Workers[0].TensorParallelism != 4 {
		t.Fatal("API deepcopy aliases summary pointers")
	}
	// An observed request with no summary fields replaces all previous values.
	request.Object["status"] = map[string]any{"phase": "Pending"}
	unstructured.RemoveNestedField(request.Object, "spec", "hardware")
	annotations := request.GetAnnotations()
	annotations[dynamointent.AttemptAnnotation] = "second"
	request.SetAnnotations(annotations)
	if _, err := r.readServingStatus(ctx, md, request); err != nil {
		t.Fatal(err)
	}
	intent = md.Status.Provider.Intent
	if intent.Hardware != nil || intent.Plan != nil || intent.Diagnostic != "" || intent.Attempt != "second" || intent.Phase != "Pending" {
		t.Fatalf("stale intent summary survived: %+v", intent)
	}
}

func TestIntentSummaryVerifiedWorkloadFallback(t *testing.T) {
	for _, version := range []string{DynamoAPIVersion, dynamoBetaVersion} {
		t.Run(version, func(t *testing.T) {
			md := newMDForController("summary", "default")
			setIntentMode(md)
			request := requestFixture(t, md, "Deployed")
			workload := workloadFixture(request, version)
			workload.Object["spec"] = summaryFixture(version == dynamoBetaVersion, "worker", "vllm", "--tensor-parallel-size", "8").Object["spec"]
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(request, workload).Build()
			r := NewDynamoProviderReconciler(c, newScheme(), "")
			if _, err := r.readServingStatus(context.Background(), md, request); err != nil {
				t.Fatal(err)
			}
			p := md.Status.Provider
			if p.Intent.Plan == nil || p.Intent.Plan.Source != "workload" || *p.Intent.Plan.Workers[0].TensorParallelism != 8 || p.WorkloadRef.UID != string(workload.GetUID()) {
				t.Fatalf("incorrect workload fallback: %+v", p)
			}
		})
	}
}

func TestIntentSummaryDoesNotTrustForeignWorkload(t *testing.T) {
	md := newMDForController("summary", "default")
	setIntentMode(md)
	request := requestFixture(t, md, "Deployed")
	workload := workloadFixture(request, DynamoAPIVersion)
	workload.SetLabels(map[string]string{dynamoDGDRNameLabel: "another-request"})
	workload.Object["spec"] = summaryFixture(false, "worker", "vllm", "--tensor-parallel-size", "8").Object["spec"]
	md.Status.Provider.Intent = &api.ProviderIntentStatus{Plan: &api.ProviderIntentPlanStatus{Source: "workload", Engine: "old"}}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(request, workload).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	if _, err := r.readServingStatus(context.Background(), md, request); err == nil {
		t.Fatal("foreign workload was accepted")
	}
	if md.Status.Provider.Intent.Plan != nil {
		t.Fatal("foreign or stale workload summary was retained")
	}
}
