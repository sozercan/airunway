package dynamointent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Keep native JSON intact, including versioned DGD merge directives. Runway
// validates the envelope and its own policy; Dynamo validates native fields.
func validateOverrides(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	const path = "intent.overrides"
	fields, err := overrideObject(raw, path)
	if err != nil {
		return err
	}
	for _, key := range sortedOverrideKeys(fields) {
		value := fields[key]
		childPath := path + "." + key
		switch key {
		case "profilingJob":
			if _, err := overrideObject(value, childPath); err != nil {
				return err
			}
		case "dgd":
			dgd, err := overrideObject(value, childPath)
			if err != nil {
				return err
			}
			for _, name := range sortedOverrideKeys(dgd) {
				switch name {
				case "apiVersion", "kind", "metadata", "spec":
				default:
					return fmt.Errorf("%s has unsupported field %q", childPath, name)
				}
			}
			var apiVersion, kind string
			if err := json.Unmarshal(dgd["apiVersion"], &apiVersion); err != nil || (apiVersion != "nvidia.com/v1alpha1" && apiVersion != "nvidia.com/v1beta1") {
				return fmt.Errorf("%s.apiVersion must be nvidia.com/v1alpha1 or nvidia.com/v1beta1", childPath)
			}
			if err := json.Unmarshal(dgd["kind"], &kind); err != nil || kind != "DynamoGraphDeployment" {
				return fmt.Errorf("%s.kind must be DynamoGraphDeployment", childPath)
			}
			if _, err := overrideObject(dgd["spec"], childPath+".spec"); err != nil {
				return err
			}
			if metadata, present := dgd["metadata"]; present {
				if _, err := overrideObject(metadata, childPath+".metadata"); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("%s supports only profilingJob and dgd", path)
		}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%s must contain valid JSON: %w", path, err)
	}
	return validateOverridePolicy(value, path)
}

func overrideObject(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil || result == nil {
		return nil, fmt.Errorf("%s must be an object", path)
	}
	return result, nil
}

func sortedOverrideKeys[V any](value map[string]V) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateOverridePolicy(value any, path string) error {
	switch value := value.(type) {
	case map[string]any:
		for _, key := range sortedOverrideKeys(value) {
			childPath := path + "." + key
			// Retain the webhook's recursive restrictions, including casing variants,
			// during reconciliation when admission is unavailable.
			switch strings.ToLower(key) {
			case "securitycontext", "serviceaccountname", "serviceaccount", "hostnetwork", "hostpid", "hostipc", "automountserviceaccounttoken", "nodename", "priorityclassname", "runtimeclassname":
				return fmt.Errorf("%s is not allowed for security reasons", childPath)
			case "resources", "replicas":
				return fmt.Errorf("%s is not allowed in automatic configuration; Dynamo chooses sizing within intent.hardware.totalGpus", childPath)
			}
			if err := validateOverridePolicy(value[key], childPath); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range value {
			if err := validateOverridePolicy(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}
