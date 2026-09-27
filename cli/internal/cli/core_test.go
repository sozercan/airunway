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
	"sync/atomic"
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
func TestStorageAccessModeCLI(t *testing.T) {
	t.Setenv("AIRUNWAY_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent"))
	for _, tc := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"default", nil, "ReadWriteOnce"},
		{"one replica", []string{"--replicas", "1"}, "ReadWriteOnce"},
		{"multiple replicas", []string{"--replicas", "2"}, "ReadWriteMany"},
		{"shared override", []string{"--storage-access-mode", "ReadWriteMany"}, "ReadWriteMany"},
		{"single writer override", []string{"--replicas", "2", "--storage-access-mode=ReadWriteOnce"}, "ReadWriteOnce"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"model", "create", "demo", "--id", "hf://Qwen/Qwen3-0.6B", "--revision", "main", "--storage-class", "managed-csi", "--storage-size", "10Gi", "--dry-run=client", "-ojson"}, tc.extra...)
			code, stdout, stderr := coreRun(t, args...)
			var result Object
			if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &result) != nil {
				t.Fatalf("preview failed: %d %s %s", code, stdout, stderr)
			}
			volumes := array(get(result, "spec", "model", "storage", "volumes"))
			if len(volumes) != 1 || stringAt(object(volumes[0]), "accessMode") != tc.want {
				t.Fatalf("got volumes %v, want accessMode %s", volumes, tc.want)
			}
		})
	}
	for _, args := range [][]string{
		{"model", "create", "demo", "--id", "hf://org/model", "--revision", "main", "--storage-access-mode", "invalid"},
		{"model", "create", "demo", "--id", "hf://org/model", "--storage-access-mode", "ReadWriteOnce"},
		{"model", "create", "demo", "--id", "pvc://weights/model", "--storage-access-mode", "ReadWriteOnce"},
		{"model", "create", "demo", "--image", "demo:v1", "--model-path", "/models/demo", "--storage-access-mode", "ReadWriteOnce"},
		{"agent", "create", "helper", "--framework", "langgraph", "--model-ref", "demo", "--storage-access-mode", "ReadWriteOnce"},
	} {
		code, stdout, stderr := coreRun(t, append(args, "--dry-run=client", "-ojson")...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "--storage-access-mode") {
			t.Errorf("%v: %d %s %s", args, code, stdout, stderr)
		}
	}
	for _, args := range [][]string{{"--help"}, {"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}} {
		code, stdout, stderr := coreRun(t, args...)
		if code != 0 || stderr != "" || !strings.Contains(stdout, "--storage-access-mode") {
			t.Errorf("%v: %d %s %s", args, code, stdout, stderr)
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

func TestDeleteWaitDeadlineBoundsInflightGET(t *testing.T) {
	for _, noun := range []string{"model", "agent"} {
		t.Run(noun, func(t *testing.T) {
			var deleted atomic.Bool
			pollCanceled := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deleted.Store(true)
					fmt.Fprint(w, `{}`)
					return
				}
				if deleted.Load() {
					<-r.Context().Done()
					close(pollCanceled)
					return
				}
				_ = json.NewEncoder(w).Encode(Object{"apiVersion": "airunway.ai/v1alpha1", "kind": resourceTypes[noun].Kind, "metadata": Object{"name": "demo", "namespace": "default", "uid": "original"}})
			}))
			defer server.Close()
			client, err := NewKubernetesClient(&rest.Config{Host: server.URL}, "default")
			if err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			started := time.Now()
			code := Run(context.Background(), []string{noun, "delete", "demo", "--timeout=50ms", "--output=json"}, RunOptions{IO: &IO{Out: &out, Err: &stderr}, Client: client, Config: &CLIConfig{Version: 1, Contexts: map[string]*ContextDefaults{}}})
			if code != 4 || time.Since(started) > time.Second {
				t.Fatalf("unbounded delete wait: code=%d elapsed=%s stderr=%s", code, time.Since(started), stderr.String())
			}
			var failure Object
			if json.Unmarshal(stderr.Bytes(), &failure) != nil || stringAt(failure, "error", "code") != "TIMEOUT" {
				t.Fatal(stderr.String())
			}
			select {
			case <-pollCanceled:
			case <-time.After(time.Second):
				t.Fatal("poll request not canceled")
			}
			if !deleted.Load() {
				t.Fatal("delete was not submitted")
			}
		})
	}
}

func TestAgentConfigFileRejectsInlineCredentials(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	secretValue := "fixture-inline-config-secret"
	for _, config := range []Object{
		{"apiKey": secretValue}, {"nested": Object{"token": secretValue}},
		{"authorization": "Bearer " + secretValue}, {"env": []any{Object{"name": "API_KEY", "value": secretValue}}},
	} {
		raw, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"create", "update"} {
			client := &managementFakeClient{resources: []Object{managementTestAgent("demo", "deployment")}}
			args := []string{"agent", action, "demo", "--namespace", "team", "--config-file", file, "--output=json", "--wait=false"}
			if action == "create" {
				args = append(args, "--framework", "langgraph", "--model-ref", "model", "--dry-run=client")
			}
			var out, stderr bytes.Buffer
			code := Run(context.Background(), args, RunOptions{IO: &IO{Out: &out, Err: &stderr}, Client: client, Config: &CLIConfig{Version: 1, Contexts: map[string]*ContextDefaults{}}})
			if code != 2 || len(client.writes()) != 0 {
				t.Fatalf("accepted config for %s: code=%d stderr=%s", action, code, stderr.String())
			}
			if strings.Contains(out.String()+stderr.String(), secretValue) {
				t.Fatal("inline credentials leaked")
			}
		}
	}
	raw := `{"credentialsRef":{"name":"api-credential","key":"API_KEY"},"timeout":30}`
	config, err := manifestJSONObject(raw, "--config-file")
	if err != nil || stringAt(config, "credentialsRef", "name") != "api-credential" {
		t.Fatal("credential reference rejected", err)
	}
}

func TestGCSUnderscoreBucketPreview(t *testing.T) {
	t.Setenv("AIRUNWAY_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	code, stdout, stderr := coreRun(t, "model", "create", "demo", "--id", "gs://model_cache/weights", "--dry-run=client", "--output=json")
	var manifest Object
	if code != 0 || json.Unmarshal([]byte(stdout), &manifest) != nil || stringAt(manifest, "spec", "model", "artifact", "uri") != "gs://model_cache/weights" {
		t.Fatalf("GCS preview failed: %d %s %s", code, stdout, stderr)
	}
	if code, _, _ := coreRun(t, "model", "create", "demo", "--id", "s3://model_cache/weights", "--dry-run=client"); code != 2 {
		t.Fatal("GCS-specific naming leaked into S3 validation", code)
	}
}

func TestAgentConfigFilePreservesLargeInteger(t *testing.T) {
	t.Setenv("AIRUNWAY_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, []byte(`{"seed":9007199254740993}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := coreRun(t, "agent", "create", "demo", "--framework", "langgraph", "--model-ref", "model", "--config-file", file, "--dry-run=client", "--output=json")
	var manifest Object
	if code != 0 || decodeJSON([]byte(stdout), &manifest) != nil || get(manifest, "spec", "config", "seed") != json.Number("9007199254740993") {
		t.Fatalf("config integer changed: %d %s %s", code, stdout, stderr)
	}
}

func TestPresetConfigPreservesLargeInteger(t *testing.T) {
	presets, err := managementPresetEntries(`[{"name":"exact","title":"Exact seed","template":{"config":{"seed":9007199254740993}}}]`, "langgraph")
	if err != nil || len(presets) != 1 || get(presets[0], "config", "seed") != json.Number("9007199254740993") {
		t.Fatalf("preset precision: %v %v", presets, err)
	}
}

func TestModelCreateWithoutProviderDiscoveryAccess(t *testing.T) {
	for _, provider := range []string{"", "vllm"} {
		for _, dry := range []string{"", "server"} {
			t.Run(provider+"/"+dry, func(t *testing.T) {
				var mu sync.Mutex
				paths := []string{}
				writes := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					paths = append(paths, r.Method+" "+r.URL.Path)
					if r.Method == http.MethodGet {
						w.WriteHeader(http.StatusForbidden)
						fmt.Fprint(w, `{}`)
						return
					}
					if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/modeldeployments") {
						t.Error("unexpected write", r.Method, r.URL.Path)
						w.WriteHeader(400)
						return
					}
					writes++
					if (r.URL.Query().Get("dryRun") == "All") != (dry == "server") {
						t.Error("dry-run lost")
					}
					var resource Object
					_ = json.NewDecoder(r.Body).Decode(&resource)
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(resource)
				}))
				defer server.Close()
				client, err := NewKubernetesClient(&rest.Config{Host: server.URL}, "team")
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"model", "create", "demo", "--id", "hf://Qwen/Qwen3-8B", "--namespace", "team", "--wait=false", "--output=json"}
				if provider != "" {
					args = append(args, "--provider", provider)
				}
				if dry != "" {
					args = append(args, "--dry-run", dry)
				}
				var out, stderr bytes.Buffer
				code := Run(context.Background(), args, RunOptions{IO: &IO{Out: &out, Err: &stderr}, Client: client, Config: &CLIConfig{Version: 1, Contexts: map[string]*ContextDefaults{}}})
				if code != 0 {
					t.Fatalf("editor create failed: %d %s", code, stderr.String())
				}
				mu.Lock()
				defer mu.Unlock()
				if writes != 1 || len(paths) != 2 {
					t.Fatalf("unexpected requests: %v", paths)
				}
				expected := "GET /apis/airunway.ai/v1alpha1/inferenceproviderconfigs"
				if provider != "" {
					expected += "/vllm"
				}
				if paths[0] != expected {
					t.Fatalf("discovery required excess access: %v", paths)
				}
			})
		}
	}
}

func TestModelDiscoveryFallbackPreservesCredentialChecks(t *testing.T) {
	resource := Object{"spec": Object{"model": Object{"id": "org/model", "source": "huggingface"}, "secrets": Object{"huggingFaceToken": "private"}}}
	client := &managementFakeClient{failure: func(c managementCall) error { return cliError(3, "HTTP_403", "Forbidden") }}
	ctx := &CommandContext{Context: context.Background(), Namespace: "team"}
	err := preflight(resource, "model", ctx, client)
	if !accessHasCode(err, "HTTP_403") {
		t.Fatalf("credential authorization bypassed: %v", err)
	}
	if len(client.calls) != 2 || client.calls[1].typ.Kind != "Secret" {
		t.Fatal(client.calls)
	}
}

func TestDashboardReleaseNames(t *testing.T) {
	cases := []struct{ binary, version, platform, arch, want string }{
		{"airunway-v1.2.3-linux-amd64", "v1.2.3", "linux", "amd64", "airunway-web-v1.2.3-linux-amd64"},
		{"airunway-v1.2.3-windows-amd64.exe", "v1.2.3", "windows", "amd64", "airunway-web-v1.2.3-windows-amd64.exe"},
		{"airunway", "v1.2.3", "darwin", "arm64", "airunway-web-v1.2.3-darwin-arm64"},
		{"airunway", "dev", "linux", "amd64", "airunway-web"},
		{"airunway", "../../other", "linux", "amd64", "airunway-web"},
	}
	for _, tc := range cases {
		names := dashboardNames(tc.binary, tc.version, tc.platform, tc.arch)
		if names[0] != tc.want {
			t.Fatalf("%+v: %v", tc, names)
		}
		for _, name := range names {
			if filepath.Base(name) != name || strings.ContainsAny(name, "/\\") {
				t.Fatal("unsafe companion name", name)
			}
		}
	}
}
