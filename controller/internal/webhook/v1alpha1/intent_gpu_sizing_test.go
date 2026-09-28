package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func intentResourceOverride(t *testing.T, shape, listName, resourceName string, quantity any) (*api.ModelDeploymentSpec, string) {
	t.Helper()
	resourceList := map[string]any{resourceName: quantity}
	resources := map[string]any{listName: resourceList}
	var dgdSpec map[string]any
	version := "v1alpha1"
	basePath := "spec.provider.overrides.spec.overrides.dgd.spec."
	switch shape {
	case "alpha custom":
		resources[listName] = map[string]any{"custom": resourceList}
		dgdSpec = map[string]any{"services": map[string]any{"worker": map[string]any{"resources": resources}}}
		basePath += "services.worker.resources." + listName + ".custom."
	case "alpha main":
		dgdSpec = map[string]any{"services": map[string]any{"worker": map[string]any{"extraPodSpec": map[string]any{"mainContainer": map[string]any{"resources": resources}}}}}
		basePath += "services.worker.extraPodSpec.mainContainer.resources." + listName + "."
	case "beta main", "beta init":
		version = "v1beta1"
		containers := "containers"
		if shape == "beta init" {
			containers = "initContainers"
		}
		dgdSpec = map[string]any{"components": []any{map[string]any{"name": "worker", "podTemplate": map[string]any{"spec": map[string]any{
			containers: []any{map[string]any{"name": "main", "resources": resources}},
		}}}}}
		basePath += "components[0].podTemplate.spec." + containers + "[0].resources." + listName + "."
	default:
		t.Fatalf("unknown shape %q", shape)
	}
	root := map[string]any{"deploymentMode": "intent", "spec": map[string]any{"overrides": map[string]any{"dgd": map[string]any{
		"apiVersion": "nvidia.com/" + version, "kind": "DynamoGraphDeployment", "spec": dgdSpec,
	}}}}
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return &api.ModelDeploymentSpec{Provider: &api.ProviderSpec{Name: "dynamo", Overrides: &runtime.RawExtension{Raw: raw}}}, basePath + resourceName
}

func TestLegacyIntentAcceleratorResourceCeilings(t *testing.T) {
	for _, shape := range []string{"alpha custom", "alpha main", "beta main", "beta init"} {
		for _, list := range []string{"requests", "limits"} {
			for _, name := range []string{
				"gpu", "nvidia.com/gpu", "amd.com/gpu", "example.com/gpu",
				"nvidia.com/mig-1g.5gb", "nvidia.com/mig-7g.80gb", "nvidia.com/mig-1g.5gb.shared",
				"nvidia.com/gpu.shared", "gpu.intel.com/i915", "gpu.intel.com/xe",
			} {
				for _, quantity := range []struct {
					name      string
					value     any
					wantError string
				}{
					{"below", "63", ""},
					{"maximum", "64", ""},
					{"numeric maximum", 64, ""},
					{"whole millicount", "64000m", ""},
					{"zero", "0", ""},
					{"above", "65", "exceeds maximum allowed (64)"},
					{"numeric above", 65, "exceeds maximum allowed (64)"},
					{"fractional", "1.5", "integer"},
					{"numeric fractional", 1.5, "integer"},
					{"fractional millicount", "500m", "integer"},
					{"negative", "-1", "integer"},
					{"invalid", "many", "invalid resource quantity"},
					{"empty", "", "invalid resource quantity"},
					{"null", nil, "invalid resource quantity"},
				} {
					t.Run(shape+"/"+list+"/"+name+"/"+quantity.name, func(t *testing.T) {
						spec, path := intentResourceOverride(t, shape, list, name, quantity.value)
						errs := (&ModelDeploymentCustomValidator{}).validateOverrides(spec, field.NewPath("spec"))
						if quantity.wantError == "" {
							if len(errs) != 0 {
								t.Fatalf("valid resource count rejected: %v", errs)
							}
							return
						}
						if len(errs) != 1 || errs[0].Field != path || !strings.Contains(errs[0].Detail, quantity.wantError) {
							t.Fatalf("got %v; want one %q error at %s", errs, quantity.wantError, path)
						}
					})
				}
			}
		}
	}
}

func TestLegacyIntentNonAcceleratorControls(t *testing.T) {
	for _, shape := range []string{"alpha custom", "alpha main", "beta main"} {
		for _, list := range []string{"requests", "limits"} {
			for _, resource := range []struct {
				name, quantity string
				wantError      bool
			}{
				{"cpu", "500m", false},
				{"cpu", MaxCPU, false},
				{"cpu", "100000", true},
				{"memory", MaxMemory, false},
				{"memory", "100000Ti", true},
				{"example.com/fpga", "1000", false},
				{"example.com/network", "1000", false},
				{"gpu.intel.com/monitoring", "1000", false},
			} {
				t.Run(shape+"/"+list+"/"+resource.name+"/"+resource.quantity, func(t *testing.T) {
					spec, path := intentResourceOverride(t, shape, list, resource.name, resource.quantity)
					errs := (&ModelDeploymentCustomValidator{}).validateOverrides(spec, field.NewPath("spec"))
					if resource.wantError {
						requireValidationErrorField(t, errs, path)
					} else if len(errs) != 0 {
						t.Fatalf("nonaccelerator control rejected: %v", errs)
					}
				})
			}
		}
	}
}

func TestAcceleratorSizingStillForbiddenOutsideLegacyIntent(t *testing.T) {
	for _, shape := range []string{"alpha custom", "alpha main", "beta main"} {
		for _, mode := range []string{"manual", "other provider", "typed intent"} {
			t.Run(shape+"/"+mode, func(t *testing.T) {
				spec, _ := intentResourceOverride(t, shape, "limits", "nvidia.com/mig-1g.5gb.shared", "1")
				var root map[string]any
				if err := json.Unmarshal(spec.Provider.Overrides.Raw, &root); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "manual":
					root["deploymentMode"] = "manual"
				case "other provider":
					spec.Provider.Name = "kuberay"
				case "typed intent":
					root["intent"] = map[string]any{"hardware": map[string]any{"totalGpus": 2}, "overrides": root["spec"].(map[string]any)["overrides"]}
					delete(root, "spec")
				}
				raw, err := json.Marshal(root)
				if err != nil {
					t.Fatal(err)
				}
				spec.Provider.Overrides.Raw = raw
				errs := (&ModelDeploymentCustomValidator{}).validateOverrides(spec, field.NewPath("spec"))
				if len(errs) == 0 {
					t.Fatal("sizing exemption escaped legacy Dynamo intent")
				}
			})
		}
	}
}
