package dynamointent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

var parserIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ToolParsers resolves the convenience setting to Dynamo-native parser names.
// Keep automatic selection narrow: model aliases and unknown families need an
// explicit parser rather than a best guess based on a substring.
func ToolParsers(md *api.ModelDeployment) (tool, reasoning string, err error) {
	e := md.Spec.Engine
	if !e.ToolCalling {
		if e.ToolCallParser != "" || e.ReasoningParser != "" {
			return "", "", fmt.Errorf("engine.toolCallParser and engine.reasoningParser require engine.toolCalling: true")
		}
		return "", "", nil
	}
	for _, entry := range []struct{ name, value string }{{"toolCallParser", e.ToolCallParser}, {"reasoningParser", e.ReasoningParser}} {
		if entry.value == "none" {
			return "", "", fmt.Errorf("engine.%s: none is not a supported Dynamo disable value; omit the field for model defaults", entry.name)
		}
		if entry.value == "auto" {
			return "", "", fmt.Errorf("omit engine.%s to use automatic parser selection instead of the literal auto", entry.name)
		}
		if entry.value != "" && (len(entry.value) > 64 || !parserIdentifier.MatchString(entry.value)) {
			return "", "", fmt.Errorf("engine.%s must be a lowercase parser identifier of at most 64 characters", entry.name)
		}
	}
	switch id := strings.ToLower(md.Spec.Model.ID); {
	case strings.HasPrefix(id, "qwen/qwen3-coder"):
		tool = "qwen3_coder"
	case strings.HasPrefix(id, "qwen/qwen3.5-"):
		tool, reasoning = "qwen3_coder", "qwen3"
	case strings.HasPrefix(id, "qwen/qwen3-"):
		tool, reasoning = "hermes", "qwen3"
	}
	if e.ToolCallParser != "" {
		tool = e.ToolCallParser
	}
	if tool == "" {
		return "", "", fmt.Errorf("no automatic tool parser is known for model %q; set engine.toolCallParser to a compatible Dynamo-native parser", md.Spec.Model.ID)
	}
	if e.ReasoningParser != "" {
		reasoning = e.ReasoningParser
	}
	return tool, reasoning, nil
}

// ValidateToolCalling also runs outside intent mode so unsupported providers and
// conflicting escape-hatch settings cannot silently ignore the new API fields.
func ValidateToolCalling(md *api.ModelDeployment) error {
	if _, _, err := ToolParsers(md); err != nil {
		return err
	}
	if !md.Spec.Engine.ToolCalling {
		return nil
	}
	provider := ""
	if md.Spec.Provider != nil {
		provider = md.Spec.Provider.Name
	}
	if provider == "" && md.Status.Provider != nil {
		provider = md.Status.Provider.Name
	}
	if provider != "dynamo" && !(provider == "" && Enabled(md)) {
		return fmt.Errorf("engine.toolCalling currently requires the dynamo provider")
	}
	if md.Status.Provider != nil && md.Status.Provider.Name != "" && md.Status.Provider.Name != "dynamo" {
		return fmt.Errorf("engine.toolCalling cannot use the already-selected provider %q", md.Status.Provider.Name)
	}
	if md.ResolvedEngineType() == api.EngineTypeLlamaCpp {
		return fmt.Errorf("engine.toolCalling requires a Dynamo vllm, sglang or trtllm engine")
	}
	if md.Annotations["airunway.ai/dynamo-test-backend"] == "mocker" {
		return fmt.Errorf("engine.toolCalling is not supported by the mocker backend")
	}
	// These structured fields own parser selection. Engine fallback flags and
	// per-component overrides otherwise take precedence over the generated defaults.
	values := []any{md.Spec.Engine.Args, md.Spec.Engine.ExtraArgs, md.Spec.Env}
	if md.Spec.Provider != nil && md.Spec.Provider.Overrides != nil {
		var overrides any
		if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &overrides); err != nil {
			return fmt.Errorf("invalid Dynamo overrides: %w", err)
		}
		values = append(values, overrides)
	}
	for _, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		var normalized any
		if err := json.Unmarshal(raw, &normalized); err != nil {
			return err
		}
		if setting := conflictingParserSetting(normalized); setting != "" {
			return fmt.Errorf("engine.toolCalling conflicts with raw parser or chat processor setting %s; use engine.toolCallParser and engine.reasoningParser, or disable engine.toolCalling", setting)
		}
	}
	return nil
}

func conflictingParserSetting(value any) string {
	switch v := value.(type) {
	case map[string]any:
		for _, key := range sortedOverrideKeys(v) {
			if s := parserSetting(key); s != "" {
				return s
			}
			if s := conflictingParserSetting(v[key]); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range v {
			if s := conflictingParserSetting(child); s != "" {
				return s
			}
		}
	case string:
		return parserSetting(v)
	}
	return ""
}

func parserSetting(value string) string {
	for _, name := range []string{"DYN_TOOL_CALL_PARSER", "DYN_REASONING_PARSER", "DYN_CHAT_PROCESSOR"} {
		if strings.Contains(value, name) {
			return name
		}
	}
	for _, name := range []string{"dyn-tool-call-parser", "dyn-reasoning-parser", "dyn-chat-processor", "tool-call-parser", "reasoning-parser", "enable-auto-tool-choice"} {
		if value == name || strings.Contains(value, "--"+name) {
			return name
		}
	}
	return ""
}
