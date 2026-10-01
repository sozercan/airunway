package dynamo

import (
	"fmt"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// applyToolCalling uses graph-level environment defaults, which Dynamo applies
// to each main container. This does not depend on profiler-generated component
// names or replace generated launch args. Native parser env vars work in both
// supported runtime contracts (1.1.1 alpha and 1.5 beta).
func (t *Transformer) applyToolCalling(md *api.ModelDeployment, obj *unstructured.Unstructured) error {
	if !md.Spec.Engine.ToolCalling {
		return nil
	}
	tool, reasoning, err := dynamointent.ToolParsers(md)
	if err != nil {
		return err
	}
	spec, ok := obj.Object["spec"].(map[string]any)
	if !ok {
		return fmt.Errorf("Dynamo spec must be an object")
	}
	version := obj.GetAPIVersion()
	if obj.GetKind() == DynamoGraphDeploymentRequestKind {
		overrides, err := toolObject(spec, "overrides")
		if err != nil {
			return err
		}
		dgd, err := toolObject(overrides, "dgd")
		if err != nil {
			return err
		}
		if len(dgd) == 0 {
			version = DynamoAPIGroup + "/v1alpha1"
			if t.modernRuntime {
				version = DynamoAPIGroup + "/v1beta1"
			}
			dgd["apiVersion"], dgd["kind"] = version, DynamoGraphDeploymentKind
		} else {
			version, _ = dgd["apiVersion"].(string)
		}
		spec, err = toolObject(dgd, "spec")
		if err != nil {
			return err
		}
	}
	// Legacy intent bypasses applyTypedIntent, so enforce the same version
	// contract here. Dynamo 1.1.1 otherwise merges beta env into an alpha
	// graph and silently loses these parser defaults.
	if version == DynamoAPIGroup+"/v1beta1" && t.runtimeVersion != "" && !t.modernRuntime {
		return fmt.Errorf("tool calling with a nvidia.com/v1beta1 DGD override requires Dynamo 1.5.0 or newer; use nvidia.com/v1alpha1 for older runtimes")
	}
	key := "envs"
	switch version {
	case DynamoAPIGroup + "/v1alpha1":
	case DynamoAPIGroup + "/v1beta1":
		key = "env"
	default:
		return fmt.Errorf("tool calling requires a versioned DynamoGraphDeployment override")
	}
	var env []any
	if existing, found := spec[key]; found {
		var ok bool
		env, ok = existing.([]any)
		if !ok {
			return fmt.Errorf("Dynamo %s must be an environment list", key)
		}
	}
	// Reapplying after alpha-to-beta conversion is intentional. Native spec.env
	// overrides may replace the converted list, but not the structured selection.
	filtered := make([]any, 0, len(env)+2)
	for _, raw := range env {
		entry, _ := raw.(map[string]any)
		if entry["name"] != "DYN_TOOL_CALL_PARSER" && entry["name"] != "DYN_REASONING_PARSER" {
			filtered = append(filtered, raw)
		}
	}
	filtered = append(filtered, map[string]any{"name": "DYN_TOOL_CALL_PARSER", "value": tool})
	if reasoning != "" {
		filtered = append(filtered, map[string]any{"name": "DYN_REASONING_PARSER", "value": reasoning})
	}
	spec[key] = filtered
	return nil
}

func toolObject(parent map[string]any, key string) (map[string]any, error) {
	if value, exists := parent[key]; exists {
		if object, ok := value.(map[string]any); ok {
			return object, nil
		}
		return nil, fmt.Errorf("Dynamo %s must be an object", key)
	}
	object := map[string]any{}
	parent[key] = object
	return object, nil
}
