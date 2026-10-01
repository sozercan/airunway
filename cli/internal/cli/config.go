package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type NamespaceDefaults struct {
	Framework string `json:"agent.framework,omitempty"`
	ModelRef  string `json:"agent.model-ref,omitempty"`
}
type ContextDefaults struct {
	Namespace  string                        `json:"namespace,omitempty"`
	Namespaces map[string]*NamespaceDefaults `json:"namespaces"`
}
type CLIConfig struct {
	Version  int                         `json:"version"`
	Context  string                      `json:"context,omitempty"`
	Contexts map[string]*ContextDefaults `json:"contexts"`
}

func configPath() string {
	if path := os.Getenv("AIRUNWAY_CONFIG"); path != "" {
		return path
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "airunway", "cli.json")
}
func readConfig(path string) (*CLIConfig, error) {
	handle, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return &CLIConfig{Version: 1, Contexts: map[string]*ContextDefaults{}}, nil
	}
	fail := func() (*CLIConfig, error) {
		return nil, cliError(2, "CONFIG", "Cannot read AI Runway configuration. Check AIRUNWAY_CONFIG.")
	}
	if err != nil {
		return fail()
	}
	defer handle.Close()
	b, err := readInput(handle, 1024*1024)
	if err != nil {
		return fail()
	}
	var config CLIConfig
	if json.Unmarshal(b, &config) != nil || config.Version != 1 || config.Contexts == nil {
		return fail()
	}
	for _, entry := range config.Contexts {
		if entry == nil {
			return fail()
		}
	}
	return &config, nil
}
func writeConfig(config *CLIConfig, path string) error {
	fail := func() error {
		return cliError(2, "CONFIG", "Cannot write AI Runway configuration. Check AIRUNWAY_CONFIG.")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fail()
	}
	handle, err := os.CreateTemp(filepath.Dir(path), ".cli-*.tmp")
	if err != nil {
		return fail()
	}
	temp := handle.Name()
	defer os.Remove(temp)
	if err = json.NewEncoder(handle).Encode(config); err != nil {
		handle.Close()
		return fail()
	}
	if err = handle.Sync(); err != nil {
		handle.Close()
		return fail()
	}
	if err = handle.Close(); err != nil {
		return fail()
	}
	if err = os.Rename(temp, path); err != nil {
		return fail()
	}
	return nil
}
func effectiveDefaults(config *CLIConfig, contextName, namespace string) Flags {
	out := Flags{}
	if c := config.Contexts[contextName]; c != nil {
		if n := c.Namespaces[namespace]; n != nil {
			if n.Framework != "" {
				out["framework"] = []string{n.Framework}
			}
			if n.ModelRef != "" {
				out["model-ref"] = []string{n.ModelRef}
			}
		}
	}
	return out
}
func changeConfig(config *CLIConfig, contextName, namespace, key string, value *string) error {
	if key != "namespace" && key != "agent.framework" && key != "agent.model-ref" {
		return usage("Supported settings: namespace, agent.framework, agent.model-ref.")
	}
	if value != nil {
		if key == "namespace" {
			if err := validateNamespace(*value); err != nil {
				return err
			}
		} else if err := validateName(*value, key); err != nil {
			return err
		}
	}
	target := config.Contexts[contextName]
	if target == nil {
		target = &ContextDefaults{Namespaces: map[string]*NamespaceDefaults{}}
		config.Contexts[contextName] = target
	}
	val := ""
	if value != nil {
		val = *value
	}
	if key == "namespace" {
		target.Namespace = val
		return nil
	}
	if target.Namespaces == nil {
		target.Namespaces = map[string]*NamespaceDefaults{}
	}
	if target.Namespaces[namespace] == nil {
		target.Namespaces[namespace] = &NamespaceDefaults{}
	}
	if key == "agent.framework" {
		target.Namespaces[namespace].Framework = val
	} else {
		target.Namespaces[namespace].ModelRef = val
	}
	return nil
}
