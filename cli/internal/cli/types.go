// Package cli implements the standalone AI Runway command-line interface.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"k8s.io/client-go/rest"
)

type Object = map[string]any
type Flags map[string][]string

func (f Flags) Has(key string) bool { _, ok := f[key]; return ok }
func (f Flags) Text(key string) string {
	v := f[key]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}
func (f Flags) Bool(key string) bool       { return f.Text(key) == "true" }
func (f Flags) Values(key string) []string { return f[key] }
func (f Flags) Copy() Flags {
	out := Flags{}
	for k, v := range f {
		out[k] = append([]string(nil), v...)
	}
	return out
}

type IO struct {
	In          io.Reader
	Out         io.Writer
	Err         io.Writer
	Interactive bool
}
type CLIError struct {
	Message  string
	ExitCode int
	Code     string
}

func (e *CLIError) Error() string { return e.Message }
func cliError(exit int, code, message string) error {
	return &CLIError{Message: message, ExitCode: exit, Code: code}
}
func usage(message string) error { return cliError(2, "USAGE", message) }

type ResourceType struct {
	Group, Version, Plural, Kind string
	Namespaced                   bool
}

var resourceTypes = map[string]ResourceType{
	"model":      {"airunway.ai", "v1alpha1", "modeldeployments", "ModelDeployment", true},
	"agent":      {"airunway.ai", "v1alpha1", "agentdeployments", "AgentDeployment", true},
	"provider":   {"airunway.ai", "v1alpha1", "inferenceproviderconfigs", "InferenceProviderConfig", false},
	"framework":  {"airunway.ai", "v1alpha1", "agentproviderconfigs", "AgentProviderConfig", false},
	"credential": {"", "v1", "secrets", "Secret", true},
	"service":    {"", "v1", "services", "Service", true},
	"pod":        {"", "v1", "pods", "Pod", true},
	"event":      {"", "v1", "events", "Event", true},
	"gateway":    {"gateway.networking.k8s.io", "v1", "gateways", "Gateway", true},
}

type RequestOptions struct {
	Query       url.Values
	ContentType string
}
type ClusterClient interface {
	Request(context.Context, string, string, any, RequestOptions) (Object, error)
	Raw(context.Context, string, string, any, RequestOptions) (*http.Response, error)
	List(context.Context, ResourceType, string, url.Values) ([]Object, error)
	Get(context.Context, ResourceType, string, string) (Object, error)
	Create(context.Context, Object, bool) (Object, error)
	Patch(context.Context, ResourceType, string, string, any, bool) (Object, error)
	Delete(context.Context, ResourceType, string, string, string) error
	RESTConfig() *rest.Config
}
type CommandContext struct {
	Context                context.Context
	Flags                  Flags
	IO                     *IO
	Namespace, ContextName string
	Client                 func() (ClusterClient, error)
}

func object(v any) Object {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return Object{}
}
func get(v any, path ...string) any {
	for _, k := range path {
		v = object(v)[k]
	}
	return v
}
func stringAt(v any, path ...string) string { s, _ := get(v, path...).(string); return s }
func boolAt(v any, path ...string) bool     { b, _ := get(v, path...).(bool); return b }
func intAt(v any, path ...string) int64 {
	x := get(v, path...)
	switch n := x.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		v, _ := n.Int64()
		return v
	}
	return 0
}
func array(v any) []any { a, _ := v.([]any); return a }
func objects(v any) []Object {
	out := []Object{}
	for _, x := range array(v) {
		out = append(out, object(x))
	}
	return out
}
func cloneObject(v Object) Object {
	b, _ := json.Marshal(v)
	var out Object
	_ = json.Unmarshal(b, &out)
	return out
}
func required(f Flags, key string) (string, error) {
	if f.Text(key) == "" {
		return "", usage("Provide --" + key + ".")
	}
	return f.Text(key), nil
}
func integer(f Flags, key string, fallback, min, max int64) (int64, error) {
	if !f.Has(key) {
		return fallback, nil
	}
	s := f.Text(key)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < min || n > max || s == "" || s[0] == '+' || s[0] == '-' {
		return 0, usage(fmt.Sprintf("--%s must be an integer between %d and %d.", key, min, max))
	}
	return n, nil
}
