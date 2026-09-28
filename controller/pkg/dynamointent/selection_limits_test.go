package dynamointent

import (
	"fmt"
	"strings"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestIntentBeforeProviderSelection(t *testing.T) {
	for _, typed := range []bool{false, true} {
		md := fixture(valid)
		if !typed {
			md.Spec.Provider.Overrides.Raw = []byte(`{"deploymentMode":"intent","spec":{"searchStrategy":"rapid"}}`)
			md.Spec.Resources = &api.ResourceSpec{GPU: &api.GPUSpec{Count: 2}}
		}
		before, err := Fingerprint(md)
		if err != nil {
			t.Fatal(err)
		}
		md.Spec.Provider.Name = ""
		if !Enabled(md) || GPUCount(md) != 2 {
			t.Fatal("intent lost before provider selection")
		}
		if err := Validate(md); err != nil {
			t.Fatal(err)
		}
		parsed, err := Parse(md)
		if err != nil || (parsed != nil) != typed {
			t.Fatalf("incorrect intent parsing: %v", err)
		}
		after, err := Fingerprint(md)
		if err != nil || before != after {
			t.Fatal("provider selection changed the intent fingerprint")
		}
		md.Spec.Provider.Name = "kaito"
		if err := Validate(md); err == nil {
			t.Fatal("explicit other provider accepted Dynamo intent")
		}
		md.Spec.Provider.Name = ""
		md.Status.Provider = &api.ProviderStatus{Name: "kaito"}
		if err := Validate(md); err == nil {
			t.Fatal("intent switched an already-selected provider")
		}
	}
	other := fixture(`{"deploymentMode":"manual","spec":{"custom":true}}`)
	other.Spec.Provider.Name = "kaito"
	if Enabled(other) {
		t.Fatal("other provider's manual override treated as Dynamo intent")
	}
	if err := Validate(other); err != nil {
		t.Fatal(err)
	}
}

func TestIntentLimitsMatchREST(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		valid        bool
	}{
		{"vram max", `"hardware":{"totalGpus":1,"vramMb":10000000}`, true},
		{"vram over", `"hardware":{"totalGpus":1,"vramMb":10000000.1}`, false},
		{"rate max", `"workload":{"requestRate":1000000}`, true},
		{"rate over", `"workload":{"requestRate":1000000.1}`, false},
		{"concurrency max", `"workload":{"concurrency":1000000}`, true},
		{"concurrency over", `"workload":{"concurrency":1000000.1}`, false},
		{"ttft max", `"sla":{"ttft":86400000}`, true},
		{"ttft over", `"sla":{"ttft":86400000.1}`, false},
		{"itl max", `"sla":{"itl":86400000}`, true},
		{"itl over", `"sla":{"itl":86400000.1}`, false},
		{"e2e max", `"sla":{"e2eLatency":86400000}`, true},
		{"e2e over", `"sla":{"e2eLatency":86400000.1}`, false},
		{"sku max", fmt.Sprintf(`"hardware":{"totalGpus":1,"gpuSku":%q}`, strings.Repeat("a", 128)), true},
		{"sku over", fmt.Sprintf(`"hardware":{"totalGpus":1,"gpuSku":%q}`, strings.Repeat("a", 129)), false},
		{"sku utf16 max", fmt.Sprintf(`"hardware":{"totalGpus":1,"gpuSku":%q}`, strings.Repeat("😀", 64)), true},
		{"sku utf16 over", fmt.Sprintf(`"hardware":{"totalGpus":1,"gpuSku":%q}`, strings.Repeat("😀", 65)), false},
		{"sku blank", `"hardware":{"totalGpus":1,"gpuSku":"   "}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := tc.fields
			if !strings.HasPrefix(fields, `"hardware"`) {
				fields = `"hardware":{"totalGpus":1},` + fields
			}
			md := fixture(`{"deploymentMode":"intent","intent":{` + fields + `}}`)
			if err := Validate(md); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestUnnamedIntentFingerprintTracksOverrides(t *testing.T) {
	md := fixture(`{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":1},"overrides":{"profilingJob":{"activeDeadlineSeconds":1800}}}}`)
	md.Spec.Provider.Name = ""
	before, err := Fingerprint(md)
	if err != nil {
		t.Fatal(err)
	}
	md.Spec.Provider.Overrides = &runtime.RawExtension{Raw: []byte(strings.ReplaceAll(string(md.Spec.Provider.Overrides.Raw), "1800", "900"))}
	after, err := Fingerprint(md)
	if err != nil || before == after {
		t.Fatalf("unnamed intent override absent from fingerprint: %v", err)
	}
}
