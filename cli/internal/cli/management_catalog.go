package cli

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var managementModelSegment = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,95}$`)

func managementModelID(value any) (string, error) {
	id, ok := value.(string)
	if !ok {
		return "", usage("Provide a Hugging Face identifier or hf:// identifier.")
	}
	id = strings.TrimPrefix(id, "hf://")
	segments := strings.Split(id, "/")
	valid := len(segments) <= 2
	for _, segment := range segments {
		valid = valid && managementModelSegment.MatchString(segment) && !strings.HasSuffix(segment, ".") && !strings.HasSuffix(segment, "-") && !strings.Contains(segment, "..") && !strings.Contains(segment, "--")
	}
	if !valid {
		return "", usage("Provide a Hugging Face identifier or hf:// identifier, not a URL or file path.")
	}
	return id, nil
}

func managementFetchModels(endpoint string, ctx *CommandContext) (any, error) {
	timeout, err := parseDuration(ctx.Flags.Text("timeout"))
	if err != nil {
		return nil, err
	}
	requestContext, cancel := context.WithTimeout(ctx.Context, min(timeout, 15*time.Second))
	defer cancel()
	failure := func() error {
		if ctx.Context.Err() != nil {
			return cliError(130, "INTERRUPTED", "Catalog request interrupted.")
		}
		if requestContext.Err() != nil {
			return cliError(4, "TIMEOUT", "Hugging Face catalog request timed out.")
		}
		return cliError(1, "CATALOG", "Cannot read the Hugging Face catalog response.")
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, failure()
	}
	request.Header.Set("Accept", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, failure()
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, cliError(1, "NOT_FOUND", "Model not found in Hugging Face.")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, cliError(1, "CATALOG", "Hugging Face catalog request failed.")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxInput+1))
	if err != nil || len(data) > maxInput {
		return nil, failure()
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, failure()
	}
	return value, nil
}

func managementRemoteModel(value any) (Object, error) {
	model, ok := value.(map[string]any)
	if !ok {
		return nil, cliError(1, "CATALOG", "Invalid Hugging Face catalog entry.")
	}
	idValue := model["id"]
	if idValue == nil {
		idValue = model["modelId"]
	}
	id, err := managementModelID(idValue)
	if err != nil {
		return nil, cliError(1, "CATALOG", "Invalid Hugging Face model identifier in response.")
	}
	result := Object{"id": id, "source": "huggingface"}
	// Never forward cardData, instructions, endpoints, or arbitrary configuration.
	if task, ok := model["pipeline_tag"].(string); ok {
		runes := []rune(task)
		result["task"] = string(runes[:min(len(runes), 100)])
	}
	for _, key := range []string{"downloads", "likes"} {
		if n, ok := model[key].(float64); ok && !math.IsInf(n, 0) && !math.IsNaN(n) {
			result[key] = n
		}
	}
	if gated, ok := model["gated"].(bool); ok {
		result["gated"] = gated
	}
	if gated, ok := model["gated"].(string); ok && (gated == "auto" || gated == "manual") {
		result["gated"] = gated
	}
	return result, nil
}

func managementModelCatalog(words []string, ctx *CommandContext) error {
	if len(words) < 3 || (words[2] != "search" && words[2] != "get") {
		return usage("Use catalog model search QUERY or get ID.")
	}
	if err := managementArity(words, 4); err != nil {
		return err
	}
	id, err := managementModelID(words[3])
	if err != nil {
		return err
	}
	if err := managementCanceled(ctx.Context); err != nil {
		return err
	}
	if words[2] == "get" {
		for _, model := range managementBundledModels {
			if stringAt(model, "id") == id {
				result := cloneObject(model)
				result["source"] = "bundled"
				return writeOutput(ctx.IO, ctx.Flags, result)
			}
		}
		parts := strings.Split(id, "/")
		for i := range parts {
			parts[i] = url.PathEscape(parts[i])
		}
		response, err := managementFetchModels("https://huggingface.co/api/models/"+strings.Join(parts, "/"), ctx)
		if err != nil {
			return err
		}
		model, err := managementRemoteModel(response)
		if err != nil {
			return err
		}
		if stringAt(model, "id") != id {
			return cliError(1, "CATALOG", "Hugging Face returned a different model identifier.")
		}
		return writeOutput(ctx.IO, ctx.Flags, model)
	}
	// Keep the fixed origin separate from the user-controlled query.
	response, err := managementFetchModels("https://huggingface.co/api/models?search="+url.QueryEscape(id)+"&limit=20", ctx)
	if err != nil {
		return err
	}
	items, ok := response.([]any)
	if !ok || len(items) > 20 {
		return cliError(1, "CATALOG", "Invalid Hugging Face search response.")
	}
	results := []Object{}
	indexes := map[string]int{}
	put := func(model Object) {
		id := stringAt(model, "id")
		if index, exists := indexes[id]; exists {
			results[index] = model
		} else {
			indexes[id] = len(results)
			results = append(results, model)
		}
	}
	for _, item := range items {
		model, err := managementRemoteModel(item)
		if err != nil {
			return err
		}
		put(model)
	}
	for _, model := range managementBundledModels {
		if strings.Contains(strings.ToLower(stringAt(model, "id")+" "+stringAt(model, "name")), strings.ToLower(id)) {
			result := cloneObject(model)
			result["source"] = "bundled"
			put(result)
		}
	}
	return writeOutput(ctx.IO, ctx.Flags, results)
}

func managementPresetEntries(raw, framework string) ([]Object, error) {
	invalid := func() ([]Object, error) {
		return nil, cliError(1, "CATALOG", "Invalid agent catalog for framework \""+framework+"\".")
	}
	if len(raw) > 256*1024 {
		return invalid()
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return invalid()
	}
	if err := managementValidateJSON(value, true); err != nil {
		return invalid()
	}
	entries, ok := value.([]any)
	if !ok || len(entries) > 200 {
		return invalid()
	}
	seen := map[string]bool{}
	result := []Object{}
	for _, value := range entries {
		entry, ok := value.(map[string]any)
		if !ok {
			return invalid()
		}
		name, err := managementIdentifier(entry["name"], "preset name", 63)
		if err != nil || strings.Contains(name, ".") || seen[name] || strings.TrimSpace(stringAt(entry, "title")) == "" {
			return invalid()
		}
		seen[name] = true
		template := Object{}
		if value, exists := entry["template"]; exists {
			var ok bool
			template, ok = value.(map[string]any)
			if !ok {
				return invalid()
			}
		}
		config := Object{}
		if value, exists := template["config"]; exists {
			var ok bool
			config, ok = value.(map[string]any)
			if !ok {
				return invalid()
			}
			config = cloneObject(config)
		}
		if value, exists := template["framework"]; exists {
			if stringAt(value, "name") != framework {
				return invalid()
			}
		}
		if value, exists := entry["image"]; exists {
			image, ok := value.(string)
			if !ok {
				return invalid()
			}
			if image != "" {
				if existing, exists := config["image"]; exists && existing != image {
					return invalid()
				}
				config["image"] = image
			}
		}
		preset := cloneObject(template)
		preset["id"], preset["name"], preset["framework"], preset["title"], preset["config"] = framework+"/"+name, name, framework, entry["title"], config
		if description, ok := entry["description"].(string); ok {
			preset["description"] = description
		}
		if tags, ok := entry["tags"].([]any); ok {
			valid := true
			for _, tag := range tags {
				if _, ok := tag.(string); !ok {
					valid = false
					break
				}
			}
			if valid {
				preset["tags"] = tags
			}
		}
		result = append(result, preset)
	}
	return result, nil
}

func managementPresets(ctx context.Context, client ClusterClient) ([]Object, error) {
	if err := managementCanceled(ctx); err != nil {
		return nil, err
	}
	providers, err := client.List(ctx, resourceTypes["framework"], "", nil)
	if err != nil {
		return nil, err
	}
	result := []Object{}
	for _, provider := range providers {
		framework, err := managementIdentifier(get(provider, "metadata", "name"), "framework name", 63)
		if err != nil {
			return nil, err
		}
		annotations := object(get(provider, "metadata", "annotations"))
		value, exists := annotations["airunway.ai/agent-catalog"]
		if !exists || value == nil {
			value = annotations["airunway.ai/catalog"]
		}
		if value == nil || value == "" {
			continue
		}
		raw, ok := value.(string)
		if !ok {
			return nil, cliError(1, "CATALOG", "Invalid agent catalog for framework \""+framework+"\".")
		}
		entries, err := managementPresetEntries(raw, framework)
		if err != nil {
			return nil, err
		}
		result = append(result, entries...)
	}
	return result, nil
}

func resolvePreset(ctx context.Context, client ClusterClient, id string) (Object, error) {
	parts := strings.Split(id, "/")
	if len(parts) > 2 {
		return nil, usage("Use PRESET or FRAMEWORK/PRESET.")
	}
	for _, part := range parts {
		if _, err := managementIdentifier(part, "preset identifier", 63); err != nil {
			return nil, err
		}
	}
	presets, err := managementPresets(ctx, client)
	if err != nil {
		return nil, err
	}
	var match Object
	for _, preset := range presets {
		key := "name"
		if len(parts) == 2 {
			key = "id"
		}
		if stringAt(preset, key) != id {
			continue
		}
		if match != nil {
			return nil, usage("Preset name is ambiguous. Use FRAMEWORK/PRESET.")
		}
		match = preset
	}
	if match == nil {
		return nil, cliError(1, "NOT_FOUND", "Preset not found in the installed frameworks.")
	}
	return match, nil
}

func managementAgentCatalog(words []string, ctx *CommandContext) error {
	if len(words) < 3 || (words[2] != "list" && words[2] != "get") {
		return usage("Use catalog agent list or get FRAMEWORK/PRESET.")
	}
	count := 3
	if words[2] == "get" {
		count = 4
	}
	if err := managementArity(words, count); err != nil {
		return err
	}
	if err := managementCanceled(ctx.Context); err != nil {
		return err
	}
	client, err := ctx.Client()
	if err != nil {
		return err
	}
	metadata := func(preset Object) Object {
		return managementPick(preset, "id", "name", "framework", "title", "description", "tags")
	}
	if words[2] == "get" {
		preset, err := resolvePreset(ctx.Context, client, words[3])
		if err != nil {
			return err
		}
		return writeOutput(ctx.IO, ctx.Flags, metadata(preset))
	}
	presets, err := managementPresets(ctx.Context, client)
	if err != nil {
		return err
	}
	result := []Object{}
	for _, preset := range presets {
		result = append(result, metadata(preset))
	}
	return writeOutput(ctx.IO, ctx.Flags, result)
}
