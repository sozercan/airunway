package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestArgs(t *testing.T) {
	words, f, err := parseArgs([]string{"-n", "team", "model", "create", "demo", "--wait=false", "--engine-arg=--dtype=float16", "--engine-arg=--enforce-eager", "-ojson"})
	if err != nil || strings.Join(words, " ") != "model create demo" || f.Bool("wait") || !f.Has("wait") || f.Text("namespace") != "team" || len(f.Values("engine-arg")) != 2 {
		t.Fatalf("bad parse: %v %v %v", words, f, err)
	}
	for _, args := range [][]string{{"--wat"}, {"--namespace"}, {"--wait=invalid"}} {
		if _, _, err := parseArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if requestedOutput([]string{"--output=json", "--", "--output=yaml"}) != "json" {
		t.Fatal("parsed positional flag")
	}
}
func TestNamesAndDurations(t *testing.T) {
	for _, value := range []string{"", "Bad", "x.y", "../secret", strings.Repeat("a", 64)} {
		if validateName(value, "name") == nil {
			t.Errorf("accepted %q", value)
		}
	}
	for _, value := range []string{"", "1ms", "30s", "2m", "24h"} {
		if _, err := parseDuration(value); err != nil {
			t.Errorf("rejected %q: %v", value, err)
		}
	}
	for _, value := range []string{"0s", "-1s", "1d", "25h", "999999999999999999999999999h", "1.5s"} {
		if _, err := parseDuration(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}
func TestConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "cli.json")
	config, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	value := "demo"
	if err := changeConfig(config, "dev", "team", "agent.model-ref", &value); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(config, path); err != nil {
		t.Fatal(err)
	}
	loaded, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if effectiveDefaults(loaded, "dev", "team").Text("model-ref") != "demo" || len(effectiveDefaults(loaded, "dev", "other")) != 0 {
		t.Fatal("defaults not scoped")
	}
	stat, _ := os.Stat(path)
	if stat.Mode().Perm() != 0600 {
		t.Fatal("config permissions", stat.Mode())
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"contexts":{"dev":null}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(path); err == nil {
		t.Fatal("accepted malformed nested config")
	}
}
func TestAgentDefaultsReplaceBinding(t *testing.T) {
	flags := mergeAgentDefaults(Flags{"model-url": {"https://example.test/v1"}, "model-id": {"remote"}}, Flags{"model-ref": {"old"}, "framework": {"langgraph"}})
	if flags.Has("model-ref") || flags.Text("framework") != "langgraph" {
		t.Fatal(flags)
	}
}
func TestClusterClient(t *testing.T) {
	var requests []Object
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/denied") {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"private-value"}`)
			return
		}
		if r.URL.Path == "/invalid" {
			fmt.Fprint(w, "private-invalid-json")
			return
		}
		if r.Header.Get("Authorization") != "Bearer integration-fixture-not-valid-credentials" {
			t.Error("missing auth")
		}
		var body Object
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		requests = append(requests, Object{"method": r.Method, "path": r.URL.Path, "query": r.URL.Query(), "body": body})
		if r.URL.Path == resourcePath(resourceTypes["model"], "team", "") && r.Method == "GET" {
			token := r.URL.Query().Get("continue")
			next := "next"
			name := "one"
			if token != "" {
				next = ""
				name = "two"
			}
			_ = json.NewEncoder(w).Encode(Object{"items": []Object{{"metadata": Object{"name": name}}}, "metadata": Object{"continue": next}})
			return
		}
		_ = json.NewEncoder(w).Encode(Object{"apiVersion": "airunway.ai/v1alpha1", "kind": "ModelDeployment", "metadata": Object{"name": "demo"}})
	}))
	defer server.Close()
	client, err := NewKubernetesClient(&rest.Config{Host: server.URL, BearerToken: "integration-fixture-not-valid-credentials"}, "team")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resource := Object{"apiVersion": "airunway.ai/v1alpha1", "kind": "ModelDeployment", "metadata": Object{"name": "demo", "namespace": "team"}}
	if _, err = client.Create(ctx, resource, true); err != nil {
		t.Fatal(err)
	}
	if q := requests[0]["query"].(url.Values); q.Get("dryRun") != "All" || q.Get("fieldValidation") != "Strict" {
		t.Fatal(q)
	}
	if _, err = client.Patch(ctx, resourceTypes["model"], "team", "demo", Object{"metadata": Object{"resourceVersion": "2"}}, false); err != nil {
		t.Fatal(err)
	}
	if stringAt(requests[1], "body", "metadata", "resourceVersion") != "2" {
		t.Fatal(requests)
	}
	if err = client.Delete(ctx, resourceTypes["model"], "team", "demo", "original-uid"); err != nil {
		t.Fatal(err)
	}
	if stringAt(requests[2], "body", "preconditions", "uid") != "original-uid" {
		t.Fatal(requests)
	}
	list, err := client.List(ctx, resourceTypes["model"], "team", nil)
	if err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	for _, path := range []string{"/denied", "/invalid", "/redirect"} {
		_, err := client.Request(ctx, "GET", path, nil, RequestOptions{})
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Errorf("unsafe error: %v", err)
		}
	}
}
func TestClientDeadlineAndPaths(t *testing.T) {
	if resourcePath(resourceTypes["framework"], "ignored", "langgraph") != "/apis/airunway.ai/v1alpha1/agentproviderconfigs/langgraph" {
		t.Fatal("cluster resource namespaced")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := NewKubernetesClient(&rest.Config{Host: server.URL}, "default")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := client.Request(ctx, "GET", "/stream", nil, RequestOptions{})
	var failure *CLIError
	if !errors.As(err, &failure) || failure.ExitCode != 4 {
		t.Fatalf("expected timeout: %v", err)
	}
	for _, path := range []string{"https://other.test", "//other.test", "/api?token=value"} {
		if _, err := client.Request(context.Background(), "GET", path, nil, RequestOptions{}); err == nil {
			t.Fatal("accepted", path)
		}
	}
}
func coreRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Run(context.Background(), args, RunOptions{IO: &IO{In: strings.NewReader(""), Out: &out, Err: &stderr}})
	return code, out.String(), stderr.String()
}
func TestOfflineCommands(t *testing.T) {
	t.Setenv("AIRUNWAY_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent"))
	for _, args := range [][]string{{"--help"}, {"version", "-ojson"}, {"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}, {"model", "create", "demo", "--id", "hf://Qwen/Qwen3-8B", "--dry-run=client", "-ojson"}, {"agent", "create", "demo", "--framework", "langgraph", "--model-ref", "model", "--dry-run=client", "-ojson"}} {
		code, out, err := coreRun(t, args...)
		if code != 0 || out == "" || err != "" {
			t.Errorf("%v: %d %s %s", args, code, out, err)
		}
	}
	for _, args := range [][]string{{"--output=json"}, {"model", "create", "demo", "--id", "hf://Qwen/Qwen3-8B", "--dry-run=wrong", "-ojson"}, {"--wat", "-ojson"}, {"model", "get", "demo", "--gpus", "1", "-ojson"}} {
		code, _, stderr := coreRun(t, args...)
		var failure Object
		if code != 2 || json.Unmarshal([]byte(stderr), &failure) != nil || get(failure, "error") == nil {
			t.Errorf("%v: %d %s", args, code, stderr)
		}
	}
}
func TestCLILifecycleThroughHTTP(t *testing.T) {
	var mu sync.Mutex
	var resource Object
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "inferenceproviderconfigs") {
			_ = json.NewEncoder(w).Encode(Object{"items": []Object{{"metadata": Object{"name": "vllm"}, "status": Object{"ready": true}}}})
			return
		}
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "modeldeployments") {
			_ = json.NewEncoder(w).Encode(Object{"items": []Object{}})
			return
		}
		switch r.Method {
		case "POST":
			writes++
			_ = json.NewDecoder(r.Body).Decode(&resource)
			object(resource["metadata"])["uid"] = "original"
			object(resource["metadata"])["resourceVersion"] = "1"
			object(resource["metadata"])["generation"] = 1
			resource["status"] = Object{"phase": "Running", "observedGeneration": 1, "conditions": []Object{{"type": "Ready", "status": "True", "observedGeneration": 1}}}
		case "DELETE":
			var body Object
			_ = json.NewDecoder(r.Body).Decode(&body)
			if stringAt(body, "preconditions", "uid") != "original" {
				t.Error("missing delete UID")
			}
			resource = nil
			fmt.Fprint(w, `{}`)
			return
		}
		if resource == nil {
			w.WriteHeader(404)
			fmt.Fprint(w, `{}`)
			return
		}
		_ = json.NewEncoder(w).Encode(resource)
	}))
	defer server.Close()
	dir := t.TempDir()
	kube := filepath.Join(dir, "kubeconfig")
	content := fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: test\nclusters:\n- name: local\n  cluster:\n    server: %s\ncontexts:\n- name: test\n  context:\n    cluster: local\n    namespace: team\n    user: local\nusers:\n- name: local\n  user: {}\n", server.URL)
	if err := os.WriteFile(kube, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kube)
	t.Setenv("AIRUNWAY_CONFIG", filepath.Join(dir, "config.json"))
	for _, args := range [][]string{{"model", "create", "demo", "--id", "hf://Qwen/Qwen3-8B"}, {"model", "get", "demo"}, {"model", "delete", "demo"}} {
		args = append(args, "-ojson", "--timeout=1s")
		code, out, stderr := coreRun(t, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out, stderr)
		}
	}
	if writes != 1 {
		t.Fatal("expected one create", writes)
	}
}

func TestDigitPrefixedNamespace(t *testing.T) {
	t.Setenv("AIRUNWAY_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	for _, namespace := range []string{"123-team", "1", "team-1"} {
		if err := validateNamespace(namespace); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"model", "create", "demo", "--id", "hf://Qwen/Qwen3-8B"},
			{"agent", "create", "demo", "--framework", "langgraph", "--model-ref", "model"},
		} {
			args = append(args, "--namespace", namespace, "--dry-run=client", "-ojson")
			code, stdout, stderr := coreRun(t, args...)
			var manifest Object
			if code != 0 || json.Unmarshal([]byte(stdout), &manifest) != nil || stringAt(manifest, "metadata", "namespace") != namespace {
				t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, stdout, stderr)
			}
		}
		config := &CLIConfig{Version: 1, Contexts: map[string]*ContextDefaults{}}
		if err := changeConfig(config, "dev", "default", "namespace", &namespace); err != nil {
			t.Fatal(err)
		}
		if config.Contexts["dev"].Namespace != namespace {
			t.Fatal(config)
		}
	}
	for _, namespace := range []string{"", "bad.namespace", "Team", "-team", "team-", strings.Repeat("a", 64)} {
		if err := validateNamespace(namespace); err == nil {
			t.Fatalf("accepted invalid namespace %q", namespace)
		}
	}
}

func TestClientCachesExecCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX shell")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	helper := filepath.Join(dir, "credential-helper")
	script := `#!/bin/sh
printf x >> "$1"
printf '%s' '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"exec-fixture-not-valid-credentials"}}'
`
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer exec-fixture-not-valid-credentials" {
			t.Error("missing exec credential")
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	client, err := NewKubernetesClient(&rest.Config{Host: server.URL, ExecProvider: &clientcmdapi.ExecConfig{
		Command: helper, Args: []string{calls}, APIVersion: "client.authentication.k8s.io/v1", InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
	}}, "default")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := client.Request(context.Background(), "GET", "/resource", nil, RequestOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(calls)
	if err != nil || string(data) != "x" {
		t.Fatalf("expected one credential-helper invocation, got %q (%v)", data, err)
	}
}
