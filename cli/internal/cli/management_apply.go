package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	yaml "sigs.k8s.io/yaml/goyaml.v3"
)

var (
	managementCamelCase   = regexp.MustCompile(`([a-z])([A-Z])`)
	managementSecretKey   = regexp.MustCompile(`(?i)(?:^|[_-])(?:api[_-]?key|access[_-]?key|account[_-]?key|token|password|passwd|private[_-]?key|client[_-]?secret|authorization|secret|credentials)(?:$|[_-])`)
	managementSecretValue = regexp.MustCompile(`(?i)(?:\bhf_[A-Za-z0-9]{16,}|\bsk-[A-Za-z0-9_-]{16,}|-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----|\bBearer\s+\S+|[a-z][a-z0-9+.-]*://[^\s/?#]*@|[?&](?:token|api[_-]?key|sig|signature|x-amz-credential|x-amz-signature)=)`)
)

func managementSensitive(key string) bool {
	return managementSecretKey.MatchString(managementCamelCase.ReplaceAllString(key, "${1}_${2}"))
}

func managementUnsafeKey(key string) bool {
	return key == "__proto__" || key == "constructor" || key == "prototype" || key == "<<"
}

// Keep unknown desired fields, but bound all walks and reject common inline
// credentials. This guardrail does not replace server-side schema validation.
func managementValidateJSON(value any, rejectSecrets bool) error {
	nodes := 0
	var visit func(any, int, string) error
	visit = func(value any, depth int, path string) error {
		nodes++
		if nodes > 50000 || depth > 64 {
			return usage("Document is too complex.")
		}
		switch item := value.(type) {
		case nil, bool:
			return nil
		case string:
			if rejectSecrets && managementSecretValue.MatchString(item) {
				return usage("Inline credentials are not allowed. Use a credential reference.")
			}
			return nil
		case float64:
			if math.IsInf(item, 0) || math.IsNaN(item) {
				return usage("Documents must contain only JSON-compatible values.")
			}
			return nil
		case int, int64, uint64, json.Number:
			return nil
		case []any:
			for _, child := range item {
				if err := visit(child, depth+1, path); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, child := range item {
				if managementUnsafeKey(key) {
					return usage("Unsafe document key.")
				}
				reference := strings.HasSuffix(key, "Ref") || strings.HasSuffix(key, "Refs") || (key == "huggingFaceToken" && path == "spec.secrets")
				_, boolean := child.(bool)
				empty, stringValue := child.(string)
				if rejectSecrets && managementSensitive(key) && child != nil && !(stringValue && empty == "") && !boolean && !reference {
					return usage("Inline credential fields are not allowed. Use a credential reference.")
				}
				childPath := key
				if path != "" {
					childPath = path + "." + key
				}
				if err := visit(child, depth+1, childPath); err != nil {
					return err
				}
			}
			if name, ok := item["name"].(string); rejectSecrets && ok && managementSensitive(name) {
				if value, exists := item["value"]; exists && value != "" {
					return usage("Inline credential environment values are not allowed. Use secretKeyRef.")
				}
			}
		default:
			return usage("Documents must contain only JSON-compatible values.")
		}
		return nil
	}
	return visit(value, 0, "")
}

// Decode YAML nodes ourselves so aliases, merge keys, duplicate keys and
// non-JSON scalar types cannot be silently coerced or expanded without bounds.
func managementYAMLDocument(root *yaml.Node) (any, error) {
	nodes := 0
	active := map[*yaml.Node]bool{}
	var visit func(*yaml.Node, int) (any, error)
	visit = func(node *yaml.Node, depth int) (any, error) {
		nodes++
		if nodes > 50000 || depth > 64 {
			return nil, usage("Document is too complex.")
		}
		if active[node] {
			return nil, usage("Cyclic YAML aliases are not supported.")
		}
		active[node] = true
		defer delete(active, node)
		switch node.Kind {
		case yaml.DocumentNode:
			if len(node.Content) == 0 {
				return nil, nil
			}
			return visit(node.Content[0], depth)
		case yaml.AliasNode:
			return visit(node.Alias, depth)
		case yaml.MappingNode:
			if node.Tag != "!!map" {
				return nil, usage("Documents must contain only JSON-compatible values.")
			}
			result := Object{}
			for i := 0; i < len(node.Content); i += 2 {
				key := node.Content[i]
				if managementUnsafeKey(key.Value) {
					return nil, usage("Unsafe document key.")
				}
				if key.Kind != yaml.ScalarNode {
					return nil, usage("Document keys must be scalar values.")
				}
				keyValue, err := managementYAMLScalar(key)
				if err != nil {
					return nil, err
				}
				keyText := fmt.Sprint(keyValue)
				if keyValue == nil {
					keyText = "null"
				}
				if _, exists := result[keyText]; exists {
					return nil, usage("Duplicate document key.")
				}
				value, err := visit(node.Content[i+1], depth+1)
				if err != nil {
					return nil, err
				}
				result[keyText] = value
			}
			return result, nil
		case yaml.SequenceNode:
			if node.Tag != "!!seq" {
				return nil, usage("Documents must contain only JSON-compatible values.")
			}
			result := []any{}
			for _, child := range node.Content {
				value, err := visit(child, depth+1)
				if err != nil {
					return nil, err
				}
				result = append(result, value)
			}
			return result, nil
		case yaml.ScalarNode:
			return managementYAMLScalar(node)
		}
		return nil, usage("Documents must contain only JSON-compatible values.")
	}
	return visit(root, 0)
}

var (
	managementCoreInteger         = regexp.MustCompile(`^(?:0o[0-7]+|0x[0-9a-fA-F]+|[-+]?[0-9]+)$`)
	managementCoreExplicitInteger = regexp.MustCompile(`^(?:[-+]?0b[0-1]+|[-+]?0o[0-7]+|[-+]?0x[0-9a-fA-F]+|[-+]?[0-9]+)$`)
	managementCoreFloat           = regexp.MustCompile(`^(?:[-+]?[0-9]+(?:\.[0-9]*)?(?:[eE][-+]?[0-9]+)?|[-+]?\.[0-9]+(?:[eE][-+]?[0-9]+)?|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
)

// Re-resolve scalars using js-yaml's YAML 1.2 core rules. Go's default
// resolver otherwise treats 012 as octal and 0b101 / 1_000 as numbers.
func managementYAMLScalar(node *yaml.Node) (any, error) {
	value := node.Value
	explicit := node.Style&yaml.TaggedStyle != 0
	if (explicit && node.Tag == "!!str") || (!explicit && node.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0) {
		return value, nil
	}
	if !explicit || node.Tag == "!!null" {
		switch value {
		case "", "~", "null", "Null", "NULL":
			return nil, nil
		}
	}
	if !explicit || node.Tag == "!!bool" {
		switch value {
		case "true", "True", "TRUE":
			return true, nil
		case "false", "False", "FALSE":
			return false, nil
		}
	}
	integerPattern := managementCoreInteger
	if explicit {
		integerPattern = managementCoreExplicitInteger
	}
	if (!explicit || node.Tag == "!!int") && integerPattern.MatchString(value) {
		digits := value
		negative := false
		if strings.HasPrefix(digits, "-") {
			negative = true
			digits = digits[1:]
		} else {
			digits = strings.TrimPrefix(digits, "+")
		}
		base := 10
		if strings.HasPrefix(digits, "0b") {
			base = 2
			digits = digits[2:]
		} else if strings.HasPrefix(digits, "0o") {
			base = 8
			digits = digits[2:]
		} else if strings.HasPrefix(digits, "0x") {
			base = 16
			digits = digits[2:]
		}
		if number, ok := new(big.Int).SetString(digits, base); ok {
			if negative {
				number.Neg(number)
			}
			return json.Number(number.String()), nil
		}
	}
	if (!explicit || node.Tag == "!!float") && managementCoreFloat.MatchString(value) {
		lower := strings.ToLower(value)
		if strings.Contains(lower, ".inf") || lower == ".nan" {
			return nil, usage("Documents must contain only JSON-compatible values.")
		}
		// Keep the core schema's float range classification, but never retain
		// the approximate float value. Fractions and exponents can encode IDs.
		if _, err := strconv.ParseFloat(value, 64); err == nil {
			return managementYAMLJSONNumber(value), nil
		}
	}
	if explicit {
		return nil, usage("Documents must contain only JSON-compatible values.")
	}
	return value, nil
}

// YAML permits a leading plus, leading zeros, and omitted digits around a
// decimal point. Normalize only that syntax, without rounding the number.
func managementYAMLJSONNumber(value string) json.Number {
	negative := strings.HasPrefix(value, "-")
	value = strings.TrimLeft(value, "+-")
	exponent := ""
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		value, exponent = value[:index], value[index:]
	}
	integer, fraction, decimal := strings.Cut(value, ".")
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	if negative {
		integer = "-" + integer
	}
	if decimal {
		if fraction == "" {
			fraction = "0"
		}
		integer += "." + fraction
	}
	return json.Number(integer + exponent)
}

func managementManifest(value any, ns string) (Object, error) {
	if err := managementValidateJSON(value, true); err != nil {
		return nil, err
	}
	resource, ok := value.(map[string]any)
	if !ok || (stringAt(resource, "kind") != "ModelDeployment" && stringAt(resource, "kind") != "AgentDeployment") || stringAt(resource, "apiVersion") != "airunway.ai/v1alpha1" {
		return nil, usage("Apply accepts only airunway.ai/v1alpha1 ModelDeployment and AgentDeployment documents.")
	}
	for key := range resource {
		if key != "apiVersion" && key != "kind" && key != "metadata" && key != "spec" {
			return nil, usage("Apply accepts desired state only, without status or other top-level fields.")
		}
	}
	metadata, metaOK := resource["metadata"].(map[string]any)
	_, specOK := resource["spec"].(map[string]any)
	if !metaOK || !specOK {
		return nil, usage("Each document requires metadata and spec objects.")
	}
	if err := validateName(stringAt(metadata, "name"), "resource name"); err != nil {
		return nil, err
	}
	for key := range metadata {
		if key != "name" && key != "namespace" && key != "labels" && key != "annotations" {
			return nil, usage("Apply metadata accepts only name, namespace, labels, and annotations. Remove server-owned metadata.")
		}
	}
	if value, exists := metadata["namespace"]; exists && value != ns {
		return nil, usage("Document namespace differs from the selected namespace.")
	}
	for _, key := range []string{"labels", "annotations"} {
		if value, exists := metadata[key]; exists {
			mapping, ok := value.(map[string]any)
			if !ok {
				return nil, usage("Labels and annotations must be string maps.")
			}
			for _, v := range mapping {
				if _, ok := v.(string); !ok {
					return nil, usage("Labels and annotations must be string maps.")
				}
			}
		}
	}
	if _, exists := object(metadata["annotations"])["kubectl.kubernetes.io/last-applied-configuration"]; exists {
		return nil, usage("Remove last-applied configuration before applying desired state.")
	}
	metadata["namespace"] = ns
	return resource, nil
}

func managementManifestExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml", ".json":
		return true
	default:
		return false
	}
}

func managementManifestFiles(path string) ([]string, error) {
	stat, err := os.Lstat(path)
	if err != nil {
		return nil, usage("Cannot read the apply path.")
	}
	if stat.Mode().IsRegular() {
		if !managementManifestExtension(path) {
			return nil, usage("Apply accepts only .yaml, .yml, or .json files.")
		}
		return []string{path}, nil
	}
	if !stat.IsDir() {
		return nil, usage("Apply requires a regular file or directory, not a symlink.")
	}
	entries, err := os.ReadDir(path) // Sorted by filename; never recurse.
	if err != nil {
		return nil, usage("Cannot read the apply path.")
	}
	files := []string{}
	for _, entry := range entries {
		if !managementManifestExtension(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, usage("Cannot read the apply path.")
		}
		if !info.Mode().IsRegular() {
			return nil, usage("Manifest entries must be regular files.")
		}
		files = append(files, filepath.Join(path, entry.Name()))
	}
	if len(files) == 0 || len(files) > 100 {
		return nil, usage("Apply requires between 1 and 100 manifest files.")
	}
	return files, nil
}

func managementReadManifestFile(path string, remaining int64) ([]byte, error) {
	stat, err := os.Lstat(path)
	if err != nil {
		return nil, usage("Cannot parse apply input as YAML or JSON.")
	}
	if !stat.Mode().IsRegular() || stat.Size() > remaining {
		return nil, usage("Apply input is limited to 4 MiB of regular files.")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, usage("Cannot parse apply input as YAML or JSON.")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, remaining+1))
	if err != nil {
		return nil, usage("Cannot parse apply input as YAML or JSON.")
	}
	if int64(len(data)) > remaining {
		return nil, usage("Apply input is limited to 4 MiB.")
	}
	return data, nil
}

func managementApply(words []string, ctx *CommandContext) error {
	if err := managementArity(words, 1); err != nil {
		return err
	}
	if err := managementOptions(ctx, "file", "dry-run"); err != nil {
		return err
	}
	ns, err := managementNamespace(ctx)
	if err != nil {
		return err
	}
	dry, err := managementDryRun(ctx.Flags)
	if err != nil {
		return err
	}
	path, err := required(ctx.Flags, "file")
	if err != nil {
		return err
	}
	files, err := managementManifestFiles(path)
	if err != nil {
		return err
	}
	resources := []Object{}
	names := map[string]bool{}
	totalBytes := int64(0)
	appendDocument := func(doc any) error {
		if doc == nil {
			return nil
		}
		desired, err := managementManifest(doc, ns)
		if err != nil {
			return err
		}
		key := stringAt(desired, "kind") + "/" + stringAt(desired, "metadata", "name")
		if names[key] {
			return usage("Apply input contains duplicate resources.")
		}
		names[key] = true
		resources = append(resources, desired)
		if len(resources) > 200 {
			return usage("Apply is limited to 200 documents.")
		}
		return nil
	}
	// Validate the entire local batch before constructing a cluster client.
	for _, file := range files {
		if err := managementCanceled(ctx.Context); err != nil {
			return err
		}
		data, err := managementReadManifestFile(file, int64(maxInput)-totalBytes)
		if err != nil {
			return err
		}
		totalBytes += int64(len(data))
		if strings.EqualFold(filepath.Ext(file), ".json") {
			var doc any
			if err := decodeJSON(data, &doc); err != nil {
				return usage("Cannot parse apply input as YAML or JSON.")
			}
			if err := appendDocument(doc); err != nil {
				return err
			}
		} else {
			decoder := yaml.NewDecoder(bytes.NewReader(data))
			for {
				var node yaml.Node
				err := decoder.Decode(&node)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return usage("Cannot parse apply input as YAML or JSON.")
				}
				doc, err := managementYAMLDocument(&node)
				if err != nil {
					return err
				}
				if err := appendDocument(doc); err != nil {
					return err
				}
			}
		}
	}
	if len(resources) == 0 {
		return usage("No deployment documents found.")
	}
	if dry == "client" {
		return writeOutput(ctx.IO, ctx.Flags, resources)
	}
	if err := managementCanceled(ctx.Context); err != nil {
		return err
	}
	client, err := ctx.Client()
	if err != nil {
		return err
	}
	versions := map[string]string{}
	// Preflight every agent before any write, including server dry runs. Never
	// modify or replay an existing one-shot workload.
	for _, desired := range resources {
		if stringAt(desired, "kind") != "AgentDeployment" {
			continue
		}
		if err := managementCanceled(ctx.Context); err != nil {
			return err
		}
		name := stringAt(desired, "metadata", "name")
		existing, err := client.Get(ctx.Context, resourceTypes["agent"], ns, name)
		if err != nil {
			var e *CLIError
			if errors.As(err, &e) && e.Code == "HTTP_404" {
				continue
			}
			return err
		}
		if stringAt(existing, "spec", "lifecycle") == "job" || stringAt(desired, "spec", "lifecycle") == "job" {
			return usage("Apply cannot modify an existing one-shot agent. Create an agent with a new name.")
		}
		version := stringAt(existing, "metadata", "resourceVersion")
		if version == "" {
			return usage("Cannot apply an existing agent without its resourceVersion.")
		}
		versions[name] = version
	}
	results := []Object{}
	partial := func(err error) error {
		if len(results) > 0 {
			if outputErr := writeApplyOutput(ctx.IO, ctx.Flags, results); outputErr != nil {
				return outputErr
			}
			if dry == "server" {
				progress(ctx, fmt.Sprintf("Validation stopped after %d successful documents. No resources were persisted.", len(results)))
			} else {
				progress(ctx, fmt.Sprintf("Apply stopped after %d successful documents. The resources listed on stdout were not rolled back.", len(results)))
			}
		}
		return err
	}
	for _, desired := range resources {
		if err := managementCanceled(ctx.Context); err != nil {
			return partial(err)
		}
		t := resourceTypes["model"]
		name := stringAt(desired, "metadata", "name")
		if stringAt(desired, "kind") == "AgentDeployment" {
			t = resourceTypes["agent"]
			if version := versions[name]; version != "" {
				object(desired["metadata"])["resourceVersion"] = version
			}
		}
		query := url.Values{"fieldManager": {managementManager}, "force": {"false"}, "fieldValidation": {"Strict"}}
		if dry == "server" {
			query.Set("dryRun", "All")
		}
		result, err := client.Request(ctx.Context, http.MethodPatch, resourcePath(t, ns, name), desired, RequestOptions{ContentType: "application/apply-patch+yaml", Query: query})
		if err != nil {
			return partial(err)
		}
		// Server defaults and status can contain provider-generated credentials.
		receipt := managementPick(result, "apiVersion", "kind")
		receipt["metadata"] = managementPick(result["metadata"], "name", "namespace")
		results = append(results, receipt)
	}
	return writeApplyOutput(ctx.IO, ctx.Flags, results)
}
