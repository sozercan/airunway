package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These contracts exercise only the executable and its HTTP/filesystem effects.
// Help/version/source-only entrypoint checks live in main_test.go. An optional
// AIRUNWAY_TEST_BINARY runs these same contracts against a packaged executable.
func TestBinaryContracts(t *testing.T) {
	binary := os.Getenv("AIRUNWAY_TEST_BINARY")
	if binary != "" {
		var err error
		binary, err = filepath.Abs(binary)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		binary = filepath.Join(t.TempDir(), "airunway")
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
			t.Fatalf("build integration executable: %v\n%s", err, output)
		}
	}
	for _, tc := range []struct {
		name string
		run  func(*testing.T, string)
	}{
		{"lifecycle", integrationLifecycle},
		{"credentials", integrationCredentials},
		{"configuration", integrationConfiguration},
		{"context_selection", integrationContextSelection},
		{"dry_run", integrationDryRun},
		{"validation", integrationValidation},
		{"waiting", integrationWaiting},
		{"artifact_previews", integrationArtifactPreviews},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, binary) })
	}
}

const integrationToken = "integration-fixture-not-valid-credentials"
const integrationProcessLimit = 10 * time.Second

type integrationObject = map[string]any

type integrationRequest struct {
	method, path string
	query        url.Values
	body         integrationObject
}

type integrationResourceType struct {
	kind, apiVersion string
	namespaced       bool
}

var integrationTypes = map[string]integrationResourceType{
	"modeldeployments":         {"ModelDeployment", "airunway.ai/v1alpha1", true},
	"agentdeployments":         {"AgentDeployment", "airunway.ai/v1alpha1", true},
	"inferenceproviderconfigs": {"InferenceProviderConfig", "airunway.ai/v1alpha1", false},
	"agentproviderconfigs":     {"AgentProviderConfig", "airunway.ai/v1alpha1", false},
	"secrets":                  {"Secret", "v1", true},
}

var integrationResourcePath = regexp.MustCompile(`^/(?:apis/airunway\.ai/v1alpha1|api/v1)/(?:(?:namespaces/([^/]+)/))?([^/]+)(?:/([^/]+))?$`)
var integrationSecretFields = regexp.MustCompile(`(?m)(?:"(?:data|stringData)"\s*:|^\s*(?:data|stringData):)`)

// Patches deliberately retain the old status: accepting it as Ready after a
// generation change must fail the freshness contracts, not pass by fixture fiat.
// All inspection returns copies under the lock, including during SIGINT tests.
type integrationAPI struct {
	server       *httptest.Server
	mu           sync.Mutex
	documents    map[string]integrationObject
	requests     []integrationRequest
	revision     int
	createdReady bool
	activity     chan struct{}
}

func newIntegrationAPI(t *testing.T) *integrationAPI {
	t.Helper()
	a := &integrationAPI{documents: map[string]integrationObject{}, activity: make(chan struct{}, 1)}
	a.seed("inferenceproviderconfigs", "", "vllm", integrationObject{"capabilities": integrationObject{"engines": []any{
		integrationObject{"name": "vllm", "servingModes": []string{"aggregated", "disaggregated"}, "gpuSupport": true, "cpuSupport": true},
	}}})
	for _, name := range []string{"langgraph", "openclaw"} {
		a.seed("agentproviderconfigs", "", name, integrationObject{"capabilities": integrationObject{
			"backend": "container", "requiresOperator": false, "modelBindingModes": []string{"deploymentRef", "externalAPI", "gatewayEndpoint"},
			"protocols": []string{"openai"}, "cpuSupport": true,
		}})
	}
	for _, value := range a.documents {
		status := integrationStatus(true, 1)
		status["ready"] = true
		value["status"] = status
	}
	a.server = httptest.NewServer(http.HandlerFunc(a.serveHTTP))
	t.Cleanup(a.server.Close)
	return a
}

func integrationKey(plural, namespace, name string) string {
	return plural + "/" + namespace + "/" + name
}

func integrationClone(value integrationObject) integrationObject {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var copy integrationObject
	if err := json.Unmarshal(data, &copy); err != nil {
		panic(err)
	}
	return copy
}

func integrationStatus(ready bool, generation int) integrationObject {
	phase, condition := "Pending", "False"
	if ready {
		phase, condition = "Running", "True"
	}
	return integrationObject{"phase": phase, "observedGeneration": generation, "conditions": []any{
		integrationObject{"type": "Ready", "status": condition, "observedGeneration": generation},
	}}
}

func (a *integrationAPI) seed(plural, namespace, name string, spec integrationObject) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revision++
	typ := integrationTypes[plural]
	meta := integrationObject{"name": name, "uid": fmt.Sprintf("fixture-%d", a.revision), "resourceVersion": strconv.Itoa(a.revision), "generation": 1}
	if typ.namespaced {
		meta["namespace"] = namespace
	}
	a.documents[integrationKey(plural, namespace, name)] = integrationClone(integrationObject{
		"apiVersion": typ.apiVersion, "kind": typ.kind, "metadata": meta, "spec": spec, "status": integrationStatus(false, 1),
	})
}

func (a *integrationAPI) bindingModel() {
	a.seed("modeldeployments", "team-a", "binding-model", integrationObject{"model": integrationObject{"id": "Qwen/Qwen3-0.6B", "source": "huggingface"}})
}

func (a *integrationAPI) get(plural, namespace, name string) integrationObject {
	a.mu.Lock()
	defer a.mu.Unlock()
	return integrationClone(a.documents[integrationKey(plural, namespace, name)])
}

func (a *integrationAPI) setCreatedReady(ready bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.createdReady = ready
}

func (a *integrationAPI) calls() []integrationRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]integrationRequest(nil), a.requests...)
}

func (a *integrationAPI) writes() []integrationRequest {
	var writes []integrationRequest
	for _, call := range a.calls() {
		if call.method != http.MethodGet {
			writes = append(writes, call)
		}
	}
	return writes
}

func integrationMerge(target, patch integrationObject) integrationObject {
	result := integrationClone(target)
	if result == nil {
		result = integrationObject{}
	}
	for key, value := range patch {
		if value == nil {
			delete(result, key)
		} else if nested, ok := value.(map[string]any); ok {
			previous, _ := result[key].(map[string]any)
			result[key] = integrationMerge(previous, nested)
		} else {
			result[key] = value
		}
	}
	return result
}

func (a *integrationAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	respond := func(code int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
	failure := func(code int, reason string) {
		respond(code, integrationObject{"apiVersion": "v1", "kind": "Status", "status": "Failure", "code": code, "reason": reason, "message": reason})
	}
	var body integrationObject
	err := json.NewDecoder(r.Body).Decode(&body)
	// Record even rejected traffic, so offline and no-write assertions cannot
	// accidentally ignore an unexpected endpoint or malformed request.
	a.requests = append(a.requests, integrationRequest{r.Method, r.URL.Path, r.URL.Query(), integrationClone(body)})
	select {
	case a.activity <- struct{}{}:
	default:
	}
	if err != nil && !errors.Is(err, io.EOF) {
		failure(400, "Invalid JSON")
		return
	}
	if r.Method == http.MethodGet {
		group := integrationObject{"name": "airunway.ai", "versions": []any{integrationObject{"groupVersion": "airunway.ai/v1alpha1", "version": "v1alpha1"}}, "preferredVersion": integrationObject{"groupVersion": "airunway.ai/v1alpha1", "version": "v1alpha1"}}
		switch r.URL.Path {
		case "/version":
			respond(200, integrationObject{"major": "1", "minor": "32", "gitVersion": "v1.32.0"})
			return
		case "/api":
			respond(200, integrationObject{"kind": "APIVersions", "versions": []string{"v1"}})
			return
		case "/apis":
			respond(200, integrationObject{"kind": "APIGroupList", "groups": []any{group}})
			return
		case "/apis/airunway.ai":
			group["kind"] = "APIGroup"
			respond(200, group)
			return
		case "/apis/airunway.ai/v1alpha1", "/api/v1":
			version := "airunway.ai/v1alpha1"
			if r.URL.Path == "/api/v1" {
				version = "v1"
			}
			resources := []any{}
			for name, typ := range integrationTypes {
				if typ.apiVersion == version {
					resources = append(resources, integrationObject{"name": name, "kind": typ.kind, "namespaced": typ.namespaced, "verbs": []string{"get", "list", "create", "patch", "delete"}})
				}
			}
			respond(200, integrationObject{"kind": "APIResourceList", "groupVersion": version, "resources": resources})
			return
		}
	}
	match := integrationResourcePath.FindStringSubmatch(r.URL.Path)
	if match == nil {
		failure(404, "NotFound")
		return
	}
	namespace, plural, name := match[1], match[2], match[3]
	typ, ok := integrationTypes[plural]
	if !ok {
		failure(404, "NotFound")
		return
	}
	key := integrationKey(plural, namespace, name)
	current := a.documents[key]
	if r.Method == http.MethodGet {
		if name != "" {
			if current == nil {
				failure(404, "NotFound")
			} else {
				respond(200, current)
			}
			return
		}
		items := []integrationObject{}
		for _, value := range a.documents {
			if value["kind"] == typ.kind && (namespace == "" || integrationField(value, "metadata", "namespace") == namespace) {
				items = append(items, value)
			}
		}
		sort.Slice(items, func(i, j int) bool {
			return fmt.Sprint(integrationField(items[i], "metadata", "name")) < fmt.Sprint(integrationField(items[j], "metadata", "name"))
		})
		respond(200, integrationObject{"apiVersion": typ.apiVersion, "kind": typ.kind + "List", "metadata": integrationObject{"resourceVersion": strconv.Itoa(a.revision)}, "items": items})
		return
	}
	// No namespace creation or provider mutation is supported by this fixture.
	if !typ.namespaced || namespace == "" {
		failure(405, "MethodNotAllowed")
		return
	}
	if r.Method == http.MethodDelete {
		if current == nil {
			failure(404, "NotFound")
			return
		}
		if uid := integrationField(body, "preconditions", "uid"); uid != nil && uid != integrationField(current, "metadata", "uid") {
			failure(409, "Conflict")
			return
		}
		if r.URL.Query().Get("dryRun") != "All" {
			delete(a.documents, key)
		}
		respond(200, integrationObject{"apiVersion": "v1", "kind": "Status", "status": "Success"})
		return
	}
	if (r.Method != http.MethodPost && r.Method != http.MethodPatch) || body == nil {
		failure(405, "MethodNotAllowed")
		return
	}
	if name == "" {
		name, _ = integrationField(body, "metadata", "name").(string)
	}
	if name == "" {
		failure(422, "Invalid")
		return
	}
	key = integrationKey(plural, namespace, name)
	existing := a.documents[key]
	if r.Method == http.MethodPost && existing != nil {
		failure(409, "AlreadyExists")
		return
	}
	if r.Method == http.MethodPatch && existing == nil {
		failure(404, "NotFound")
		return
	}
	if version := integrationField(body, "metadata", "resourceVersion"); version != nil && version != integrationField(existing, "metadata", "resourceVersion") {
		failure(409, "Conflict")
		return
	}
	value := integrationMerge(existing, body)
	if value["kind"] != typ.kind || value["apiVersion"] != typ.apiVersion {
		failure(422, "Invalid")
		return
	}
	meta, ok := value["metadata"].(map[string]any)
	if !ok || (meta["namespace"] != nil && meta["namespace"] != namespace) {
		failure(422, "Invalid namespace")
		return
	}
	generation := 1
	a.revision++
	uid := fmt.Sprintf("fixture-%d", a.revision)
	if existing != nil {
		generation += int(integrationField(existing, "metadata", "generation").(float64))
		uid = integrationField(existing, "metadata", "uid").(string)
	}
	meta["name"], meta["namespace"], meta["uid"] = name, namespace, uid
	meta["resourceVersion"], meta["generation"] = strconv.Itoa(a.revision), generation
	if plural != "secrets" && existing == nil {
		value["status"] = integrationStatus(a.createdReady, generation)
	}
	if plural == "secrets" {
		data, _ := value["data"].(map[string]any)
		if data == nil {
			data = integrationObject{}
		}
		if input, ok := value["stringData"].(map[string]any); ok {
			for key, text := range input {
				data[key] = base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(text)))
			}
		}
		delete(value, "stringData")
		value["data"] = data
		meta["annotations"] = integrationObject{"fixture.example/unsafe-note": integrationToken}
	}
	if r.URL.Query().Get("dryRun") != "All" {
		a.documents[key] = integrationClone(value)
	}
	code := http.StatusOK
	if r.Method == http.MethodPost {
		code = http.StatusCreated
	}
	respond(code, value)
}

type integrationFixture struct {
	t      *testing.T
	binary string
	dir    string
	env    map[string]string
	api    *integrationAPI
}

func newIntegrationFixture(t *testing.T, binary string) *integrationFixture {
	t.Helper()
	dir := t.TempDir()
	f := &integrationFixture{t: t, binary: binary, dir: dir, api: newIntegrationAPI(t)}
	f.env = map[string]string{
		"PATH": "", "HOME": dir, "TMPDIR": dir, "XDG_CONFIG_HOME": filepath.Join(dir, ".config"),
		"KUBECONFIG": filepath.Join(dir, "kubeconfig.json"), "AIRUNWAY_CONFIG": filepath.Join(dir, "cli.json"),
		"NO_COLOR": "1", "TERM": "dumb",
	}
	// Do not inherit a real cluster, credentials, proxy, or user configuration.
	if runtime.GOOS == "windows" {
		f.env["SYSTEMROOT"] = os.Getenv("SYSTEMROOT")
	}
	f.writeKubeconfig(integrationObject{
		"apiVersion": "v1", "kind": "Config", "current-context": "alpha",
		"clusters": []any{integrationObject{"name": "loopback", "cluster": integrationObject{"server": f.api.server.URL}}},
		"users":    []any{integrationObject{"name": "fixture", "user": integrationObject{}}},
		"contexts": []any{
			integrationObject{"name": "alpha", "context": integrationObject{"cluster": "loopback", "user": "fixture", "namespace": "alpha-default"}},
			integrationObject{"name": "beta", "context": integrationObject{"cluster": "loopback", "user": "fixture", "namespace": "beta-default"}},
		},
	})
	return f
}

func (f *integrationFixture) write(path, contents string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *integrationFixture) read(path string) string {
	f.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *integrationFixture) kubeconfig() integrationObject {
	f.t.Helper()
	var config integrationObject
	if err := json.Unmarshal([]byte(f.read(f.env["KUBECONFIG"])), &config); err != nil {
		f.t.Fatal(err)
	}
	return config
}

func (f *integrationFixture) writeKubeconfig(config integrationObject) {
	f.t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(f.env["KUBECONFIG"], string(data))
}

type integrationResult struct {
	code           int
	stdout, stderr string
}

type integrationProcess struct {
	cmd    *exec.Cmd
	ctx    context.Context
	done   chan struct{}
	result integrationResult
	err    error
}

// Bound output as well as runtime: a broken wait loop must not hang CI or fill
// memory. The two buffers are read only after Wait finishes its copy goroutines.
type integrationOutput struct {
	buffer bytes.Buffer
	cancel context.CancelFunc
}

func (b *integrationOutput) Write(data []byte) (int, error) {
	const limit = 1024 * 1024
	if len(data) > limit-b.buffer.Len() {
		_, _ = b.buffer.Write(data[:limit-b.buffer.Len()])
		b.cancel()
		return len(data), nil
	}
	return b.buffer.Write(data)
}

func (f *integrationFixture) start(args []string, input string) *integrationProcess {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationProcessLimit)
	p := &integrationProcess{ctx: ctx, done: make(chan struct{})}
	p.cmd = exec.CommandContext(ctx, f.binary, args...)
	p.cmd.Dir = f.dir
	for key, value := range f.env {
		p.cmd.Env = append(p.cmd.Env, key+"="+value)
	}
	out, errOut := &integrationOutput{cancel: cancel}, &integrationOutput{cancel: cancel}
	p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr = strings.NewReader(input), out, errOut
	p.cmd.WaitDelay = time.Second
	if err := p.cmd.Start(); err != nil {
		cancel()
		f.t.Fatalf("start %v: %v", args, err)
	}
	go func() {
		p.err = p.cmd.Wait()
		p.result = integrationResult{p.cmd.ProcessState.ExitCode(), out.buffer.String(), errOut.buffer.String()}
		close(p.done)
	}()
	f.t.Cleanup(func() { cancel(); <-p.done })
	return p
}

func (p *integrationProcess) wait(t *testing.T) integrationResult {
	t.Helper()
	<-p.done
	if p.ctx.Err() != nil {
		t.Fatalf("CLI exceeded process/output limit: %v\nstdout: %s\nstderr: %s", p.cmd.Args[1:], p.result.stdout, p.result.stderr)
	}
	var exit *exec.ExitError
	if p.err != nil && !errors.As(p.err, &exit) {
		t.Fatalf("wait for CLI: %v", p.err)
	}
	if p.result.code < 0 {
		t.Fatalf("CLI was terminated by a signal instead of exiting: %v\n%s", p.err, p.result.stderr)
	}
	return p.result
}

func (f *integrationFixture) run(args []string, input ...string) integrationResult {
	f.t.Helper()
	stdin := ""
	if len(input) != 0 {
		stdin = input[0]
	}
	return f.start(args, stdin).wait(f.t)
}

func (a *integrationAPI) waitForRead(t *testing.T, p *integrationProcess, plural, name string) {
	t.Helper()
	for {
		submitted := false
		for _, call := range a.calls() {
			if call.method == "POST" && strings.HasSuffix(call.path, "/"+plural) && integrationField(call.body, "metadata", "name") == name {
				submitted = true
			}
			if submitted && call.method == "GET" && strings.HasSuffix(call.path, "/"+plural+"/"+name) {
				return
			}
		}
		select {
		case <-a.activity:
		case <-p.done:
			t.Fatalf("CLI exited before its post-submit wait read: %+v", p.wait(t))
		case <-p.ctx.Done():
			t.Fatal("CLI never entered its post-submit wait loop")
		}
	}
}

func integrationScope(args []string, target ...string) []string {
	contextName, namespace := "alpha", "team-a"
	if len(target) > 0 {
		contextName, namespace = target[0], target[1]
	}
	return append(append([]string(nil), args...), "--context", contextName, "--namespace", namespace, "--output", "json")
}

func integrationCreate(noun, name string) []string {
	if noun == "model" {
		return []string{"model", "create", name, "--id", "hf://Qwen/Qwen3-0.6B", "--provider", "vllm"}
	}
	return []string{"agent", "create", name, "--framework", "langgraph", "--model-ref", "binding-model", "--prompt", "Give short answers."}
}

func integrationField(value any, keys ...string) any {
	for _, key := range keys {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}

func integrationEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func integrationWantField(t *testing.T, object any, want any, keys ...string) {
	t.Helper()
	got := integrationField(object, keys...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v", strings.Join(keys, "."), got, want)
	}
}

func (r integrationResult) success(t *testing.T) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("CLI exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
}

func (r integrationResult) json(t *testing.T) any {
	t.Helper()
	r.success(t)
	var result any
	if err := json.Unmarshal([]byte(r.stdout), &result); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, r.stdout)
	}
	return result
}

func (r integrationResult) failure(t *testing.T, code int) {
	t.Helper()
	if r.code != code || strings.TrimSpace(r.stderr) == "" {
		t.Fatalf("expected exit %d and stderr, got exit %d\nstdout: %s\nstderr: %s", code, r.code, r.stdout, r.stderr)
	}
}

func (r integrationResult) structuredStderr(t *testing.T) {
	t.Helper()
	if json.Valid([]byte(r.stderr)) {
		return
	}
	if strings.TrimSpace(r.stderr) == "" {
		t.Fatal("expected JSON progress/error on stderr")
	}
	for _, line := range strings.Split(strings.TrimSpace(r.stderr), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("non-JSON stderr record: %s", line)
		}
	}
}

func (r integrationResult) redacted(t *testing.T, values ...string) {
	t.Helper()
	r.success(t)
	for _, value := range append([]string{integrationToken, "unsafe-note"}, values...) {
		if strings.Contains(r.stdout+r.stderr, value) || strings.Contains(r.stdout+r.stderr, base64.StdEncoding.EncodeToString([]byte(value))) {
			t.Fatal("credential output exposed secret contents or unsafe annotations")
		}
	}
	if integrationSecretFields.MatchString(r.stdout) {
		t.Fatal("credential output exposed data/stringData fields")
	}
}

func integrationNames(t *testing.T, value any) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("expected a JSON list, got %#v", value)
	}
	names := []string{}
	for _, item := range items {
		name, ok := integrationField(item, "metadata", "name").(string)
		if !ok {
			t.Fatalf("list item has no name: %#v", item)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func integrationNoRequests(t *testing.T, a *integrationAPI) {
	t.Helper()
	if calls := a.calls(); len(calls) != 0 {
		t.Fatalf("expected no API requests, got %+v", calls)
	}
}

func integrationWriteMethods(t *testing.T, a *integrationAPI, want ...string) {
	t.Helper()
	methods := []string{}
	for _, call := range a.writes() {
		methods = append(methods, call.method)
	}
	integrationEqual(t, methods, want)
}

func integrationNoPoll(t *testing.T, a *integrationAPI, plural, name string) {
	t.Helper()
	calls := a.calls()
	if len(calls) == 0 || (calls[len(calls)-1].method != "POST" && calls[len(calls)-1].method != "PATCH") {
		t.Fatalf("submission should be the last request, got %+v", calls)
	}
	for _, call := range calls {
		if call.method == "GET" && strings.HasSuffix(call.path, "/"+plural+"/"+name) {
			t.Fatal("creation unexpectedly polled its submitted resource")
		}
	}
}

func integrationLifecycle(t *testing.T, binary string) {
	for _, noun := range []string{"model", "agent"} {
		t.Run(noun, func(t *testing.T) {
			f := newIntegrationFixture(t, binary)
			plural, name := noun+"deployments", "demo"
			if noun == "agent" {
				f.api.bindingModel()
			}
			f.api.seed(plural, "team-b", name, integrationObject{"untouched": true})
			other := f.api.get(plural, "team-b", name)
			created := f.run(integrationScope(append(integrationCreate(noun, name), "--wait=false"))).json(t)
			integrationWantField(t, created, name, "metadata", "name")
			integrationWantField(t, created, "team-a", "metadata", "namespace")
			integrationWantField(t, created, "Pending", "status", "phase")
			integrationNoPoll(t, f.api, plural, name)
			listed := f.run(integrationScope([]string{noun, "list"})).json(t)
			integrationEqual(t, integrationNames(t, listed), []string{name})
			fetched := f.run(integrationScope([]string{noun, "get", name})).json(t)
			integrationWantField(t, fetched, integrationField(created, "metadata", "uid"), "metadata", "uid")
			updateArgs := []string{"--replicas", "2"}
			if noun == "agent" {
				prompt := filepath.Join(f.dir, "prompt.txt")
				f.write(prompt, "Use the revised instructions.")
				updateArgs = []string{"--prompt-file", prompt}
			}
			args := append([]string{noun, "update", name}, updateArgs...)
			updated := f.run(integrationScope(append(args, "--wait=false"))).json(t)
			integrationWantField(t, updated, integrationField(created, "metadata", "uid"), "metadata", "uid")
			integrationWantField(t, updated, "Pending", "status", "phase")
			if noun == "model" {
				integrationWantField(t, fetched, "Qwen/Qwen3-0.6B", "spec", "model", "id")
				integrationWantField(t, updated, "Qwen/Qwen3-0.6B", "spec", "model", "id")
				integrationWantField(t, updated, float64(2), "spec", "scaling", "replicas")
			} else {
				for _, value := range []any{created, fetched, updated} {
					integrationWantField(t, value, "langgraph", "spec", "framework", "name")
					integrationWantField(t, value, "binding-model", "spec", "model", "deploymentRef", "name")
				}
				integrationWantField(t, updated, "Use the revised instructions.", "spec", "config", "systemPrompt")
			}
			calls := f.api.calls()
			integrationEqual(t, calls[len(calls)-1].method, "PATCH") // --wait=false must not poll after the update.
			integrationWriteMethods(t, f.api, "POST", "PATCH")
			integrationWantField(t, f.api.writes()[1].body, integrationField(fetched, "metadata", "resourceVersion"), "metadata", "resourceVersion")
			f.run(integrationScope([]string{noun, "delete", name})).success(t)
			if f.api.get(plural, "team-a", name) != nil {
				t.Fatal("deleted resource still exists")
			}
			integrationWriteMethods(t, f.api, "POST", "PATCH", "DELETE")
			integrationWantField(t, f.api.writes()[2].body, integrationField(created, "metadata", "uid"), "preconditions", "uid")
			for _, call := range f.api.writes() {
				if !strings.HasPrefix(call.path, "/apis/airunway.ai/v1alpha1/namespaces/team-a/"+plural) {
					t.Fatalf("write escaped the selected resource/namespace: %+v", call)
				}
			}
			integrationEqual(t, f.api.get(plural, "team-b", name), other)
			if noun == "agent" && f.api.get("modeldeployments", "team-a", "binding-model") == nil {
				t.Fatal("deleting the agent deleted its shared model")
			}
		})
	}
}

func integrationCredentials(t *testing.T, binary string) {
	t.Run("huggingface_lifecycle", func(t *testing.T) {
		f := newIntegrationFixture(t, binary)
		created := f.run(integrationScope([]string{"credential", "create", "hf-access", "--type", "huggingface", "--from-file", "-"}), integrationToken+"\n")
		integrationWantField(t, created.json(t), "hf-access", "metadata", "name")
		integrationWantField(t, f.api.get("secrets", "team-a", "hf-access"), base64.StdEncoding.EncodeToString([]byte(integrationToken)), "data", "HF_TOKEN")
		created.redacted(t)
		for _, action := range []string{"get", "list"} {
			for _, format := range []string{"json", "yaml", "text"} {
				t.Run(action+"_"+format, func(t *testing.T) {
					args := []string{"credential", action}
					if action == "get" {
						args = append(args, "hf-access")
					}
					result := f.run(append(args, "--context", "alpha", "--namespace", "team-a", "--output", format))
					result.redacted(t)
					if !strings.Contains(result.stdout, "hf-access") {
						t.Fatal("credential metadata was missing from output")
					}
				})
			}
		}
		replacement := integrationToken + "-replacement"
		path := filepath.Join(f.dir, "replacement.txt")
		f.write(path, replacement+"\n")
		f.run(integrationScope([]string{"credential", "update", "hf-access", "--from-file", path})).redacted(t, replacement)
		integrationWantField(t, f.api.get("secrets", "team-a", "hf-access"), base64.StdEncoding.EncodeToString([]byte(replacement)), "data", "HF_TOKEN")
		f.run(integrationScope([]string{"credential", "get", "hf-access"})).redacted(t, replacement)
		f.run(integrationScope([]string{"credential", "delete", "hf-access"})).redacted(t, replacement)
		if f.api.get("secrets", "team-a", "hf-access") != nil {
			t.Fatal("credential was not deleted")
		}
		integrationWriteMethods(t, f.api, "POST", "PATCH", "DELETE")
		for _, call := range f.api.writes() {
			if !strings.HasPrefix(call.path, "/api/v1/namespaces/team-a/secrets") {
				t.Fatalf("credential write escaped its namespace/resource: %+v", call)
			}
		}
	})
	t.Run("api_and_artifact_inputs", func(t *testing.T) {
		f := newIntegrationFixture(t, binary)
		artifact := `{"AWS_ACCESS_KEY_ID":"fixture-access-id","AWS_SECRET_ACCESS_KEY":"` + integrationToken + `"}`
		artifactFile := filepath.Join(f.dir, "artifact.json")
		f.write(artifactFile, artifact)
		for _, tc := range []struct{ name, typ, file, input, key, value string }{
			{"api-access", "api-key", "-", integrationToken, "API_KEY", integrationToken},
			{"artifact-access", "artifact", artifactFile, "", "credentials", artifact},
		} {
			result := f.run(integrationScope([]string{"credential", "create", tc.name, "--type", tc.typ, "--from-file", tc.file}), tc.input)
			result.redacted(t, artifact, "fixture-access-id")
			integrationWantField(t, f.api.get("secrets", "team-a", tc.name), base64.StdEncoding.EncodeToString([]byte(tc.value)), "data", tc.key)
		}
		for _, format := range []string{"json", "yaml", "text"} {
			result := f.run([]string{"credential", "list", "--context", "alpha", "--namespace", "team-a", "--output", format})
			result.redacted(t, artifact, "fixture-access-id")
			for _, name := range []string{"api-access", "artifact-access"} {
				if !strings.Contains(result.stdout, name) {
					t.Fatalf("%s output omitted %s", format, name)
				}
			}
		}
		other := f.run(integrationScope([]string{"credential", "list"}, "alpha", "team-b")).json(t)
		integrationEqual(t, integrationNames(t, other), []string{})
	})
}

func integrationConfiguration(t *testing.T, binary string) {
	t.Run("agent_defaults_are_target_specific", func(t *testing.T) {
		f := newIntegrationFixture(t, binary)
		targets := []struct{ context, namespace, framework, model string }{
			{"alpha", "team-a", "langgraph", "one"},
			{"alpha", "team-b", "openclaw", "two"},
			{"beta", "team-a", "openclaw", "three"},
		}
		for _, target := range targets {
			for _, setting := range [][2]string{{"agent.framework", target.framework}, {"agent.model-ref", target.model}} {
				f.run(integrationScope([]string{"config", "set", setting[0], setting[1]}, target.context, target.namespace)).success(t)
			}
		}
		for _, target := range targets {
			preview := f.run(integrationScope([]string{"agent", "create", "assistant", "--prompt", "Be helpful.", "--dry-run", "client"}, target.context, target.namespace)).json(t)
			integrationWantField(t, preview, target.framework, "spec", "framework", "name")
			integrationWantField(t, preview, target.model, "spec", "model", "deploymentRef", "name")
			integrationWantField(t, preview, target.namespace, "metadata", "namespace")
		}
		integrationNoRequests(t, f.api)
		if strings.Contains(f.read(f.env["AIRUNWAY_CONFIG"]), integrationToken) {
			t.Fatal("configuration contains credentials")
		}
		info, err := os.Stat(f.env["AIRUNWAY_CONFIG"])
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" {
			integrationEqual(t, info.Mode().Perm(), os.FileMode(0600))
		}
	})
	t.Run("namespace_precedence", func(t *testing.T) {
		f := newIntegrationFixture(t, binary)
		f.run([]string{"config", "set", "namespace", "team-b", "--context", "alpha"}).success(t)
		for _, tc := range []struct {
			name, context, namespace string
			extra                    []string
		}{
			{"saved", "alpha", "team-b", nil},
			{"other-context", "beta", "beta-default", nil},
			{"explicit", "alpha", "team-a", []string{"--namespace", "team-a"}},
		} {
			args := append(integrationCreate("model", tc.name), "--context", tc.context, "--dry-run", "client", "--output", "json")
			preview := f.run(append(args, tc.extra...)).json(t)
			integrationWantField(t, preview, tc.namespace, "metadata", "namespace")
		}
		integrationNoRequests(t, f.api)
	})
	t.Run("explicit_binding_and_targeted_unset", func(t *testing.T) {
		f := newIntegrationFixture(t, binary)
		for _, namespace := range []string{"team-a", "team-b"} {
			for _, setting := range [][2]string{{"agent.framework", "langgraph"}, {"agent.model-ref", "saved-model"}} {
				f.run(integrationScope([]string{"config", "set", setting[0], setting[1]}, "alpha", namespace)).success(t)
			}
		}
		modelURL := f.api.server.URL + "/v1"
		preview := f.run(integrationScope([]string{"agent", "create", "explicit", "--model-url", modelURL, "--model-api", "openai", "--model-id", "served-model", "--prompt", "Be helpful.", "--dry-run", "client"})).json(t)
		integrationWantField(t, preview, nil, "spec", "model", "deploymentRef")
		binding, err := json.Marshal(integrationField(preview, "spec", "model"))
		if err != nil || !strings.Contains(string(binding), modelURL) {
			t.Fatalf("explicit model URL is absent from binding: %s, %v", binding, err)
		}
		f.run(integrationScope([]string{"config", "unset", "agent.model-ref"})).success(t)
		f.run(integrationScope([]string{"agent", "create", "missing-binding", "--prompt", "Be helpful.", "--dry-run", "client"})).failure(t, 2)
		other := f.run(integrationScope([]string{"agent", "create", "other-target", "--prompt", "Be helpful.", "--dry-run", "client"}, "alpha", "team-b")).json(t)
		integrationWantField(t, other, "saved-model", "spec", "model", "deploymentRef", "name")
		integrationNoRequests(t, f.api)
	})
	t.Run("defaults_do_not_rewrite_existing_agents", func(t *testing.T) {
		f := newIntegrationFixture(t, binary)
		f.api.bindingModel()
		f.run(integrationScope(append(integrationCreate("agent", "existing"), "--wait=false"))).json(t)
		for _, setting := range [][2]string{{"agent.framework", "openclaw"}, {"agent.model-ref", "different-model"}} {
			f.run(integrationScope([]string{"config", "set", setting[0], setting[1]})).success(t)
		}
		updated := f.run(integrationScope([]string{"agent", "update", "existing", "--prompt", "Revised prompt.", "--wait=false"})).json(t)
		integrationWantField(t, updated, "langgraph", "spec", "framework", "name")
		integrationWantField(t, updated, "binding-model", "spec", "model", "deploymentRef", "name")
		integrationWantField(t, updated, "Revised prompt.", "spec", "config", "systemPrompt")
	})
}

func integrationContextSelection(t *testing.T, binary string) {
	f := newIntegrationFixture(t, binary)
	beta := newIntegrationAPI(t)
	config := f.kubeconfig()
	config["clusters"] = append(config["clusters"].([]any), integrationObject{"name": "beta-loopback", "cluster": integrationObject{"server": beta.server.URL}})
	config["contexts"].([]any)[1].(map[string]any)["context"].(map[string]any)["cluster"] = "beta-loopback"
	f.writeKubeconfig(config)
	original := f.read(f.env["KUBECONFIG"])
	f.run([]string{"context", "use", "beta"}).success(t)
	integrationEqual(t, f.read(f.env["KUBECONFIG"]), original)
	beta.seed("modeldeployments", "beta-default", "beta-only", integrationObject{})
	f.api.seed("modeldeployments", "alpha-default", "alpha-only", integrationObject{})
	selected := f.run([]string{"model", "list", "--output", "json"}).json(t)
	integrationEqual(t, integrationNames(t, selected), []string{"beta-only"})
	integrationNoRequests(t, f.api)
	for _, tc := range []struct {
		api       *integrationAPI
		namespace string
	}{
		{beta, "beta-default"}, {f.api, "alpha-default"},
	} {
		if tc.api == f.api {
			explicit := f.run([]string{"model", "list", "--context", "alpha", "--output", "json"}).json(t)
			integrationEqual(t, integrationNames(t, explicit), []string{"alpha-only"})
		}
		found := false
		for _, call := range tc.api.calls() {
			found = found || call.path == "/apis/airunway.ai/v1alpha1/namespaces/"+tc.namespace+"/modeldeployments"
		}
		if !found || len(tc.api.writes()) != 0 {
			t.Fatalf("expected read-only list in %s, got %+v", tc.namespace, tc.api.calls())
		}
	}
	integrationEqual(t, f.read(f.env["KUBECONFIG"]), original)
}

// A kubeconfig exec plugin can run this test executable without depending on
// Bun, Node, or a shell. If a preview invokes it, the marker survives the failure.
func TestIntegrationExecCredentialHelper(t *testing.T) {
	marker := os.Getenv("AIRUNWAY_EXEC_AUTH_MARKER")
	if marker == "" {
		t.Skip("only run as a kubeconfig exec authentication trap")
	}
	if err := os.WriteFile(marker, []byte("invoked"), 0600); err != nil {
		t.Fatal(err)
	}
	os.Exit(9)
}

func integrationDryRun(t *testing.T, binary string) {
	t.Run("client_does_not_execute_auth", func(t *testing.T) {
		f := newIntegrationFixture(t, binary)
		marker := filepath.Join(f.dir, "auth-was-executed")
		helper, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		config := f.kubeconfig()
		config["users"] = []any{integrationObject{"name": "fixture", "user": integrationObject{"exec": integrationObject{
			"apiVersion": "client.authentication.k8s.io/v1beta1", "command": helper,
			"args": []string{"-test.run=^TestIntegrationExecCredentialHelper$"},
			"env":  []any{integrationObject{"name": "AIRUNWAY_EXEC_AUTH_MARKER", "value": marker}},
		}}}}
		f.writeKubeconfig(config)
		for _, noun := range []string{"model", "agent"} {
			preview := f.run(append(integrationCreate(noun, "offline-"+noun), "--context", "beta", "--dry-run", "client", "--output", "json")).json(t)
			integrationWantField(t, preview, "beta-default", "metadata", "namespace")
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("client preview invoked exec authentication or marker cannot be checked: %v", err)
		}
		integrationNoRequests(t, f.api)
	})
	t.Run("client_without_kubeconfig", func(t *testing.T) {
		f := newIntegrationFixture(t, binary)
		f.env["KUBECONFIG"] = filepath.Join(f.dir, "missing-kubeconfig")
		for _, noun := range []string{"model", "agent"} {
			preview := f.run(append(integrationCreate(noun, "offline-"+noun), "--namespace", "offline", "--dry-run", "client", "--output", "json")).json(t)
			integrationWantField(t, preview, "offline", "metadata", "namespace")
		}
		integrationNoRequests(t, f.api)
	})
	t.Run("server_does_not_persist_or_wait", func(t *testing.T) {
		f := newIntegrationFixture(t, binary)
		f.api.bindingModel()
		for _, noun := range []string{"model", "agent"} {
			name := "preview-" + noun
			f.run(integrationScope(append(integrationCreate(noun, name), "--dry-run", "server"))).json(t)
			integrationNoPoll(t, f.api, noun+"deployments", name)
			if f.api.get(noun+"deployments", "team-a", name) != nil {
				t.Fatal("server dry-run persisted a resource")
			}
		}
		integrationWriteMethods(t, f.api, "POST", "POST")
		for _, call := range f.api.writes() {
			integrationEqual(t, call.query.Get("dryRun"), "All")
		}
		if f.api.get("modeldeployments", "team-a", "binding-model") == nil {
			t.Fatal("server dry-run removed the shared model")
		}
	})
}

func integrationValidation(t *testing.T, binary string) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown_command", []string{"not-a-command"}},
		{"unsupported_completion", []string{"completion", "unsupported-shell"}},
		{"create_option", append(integrationCreate("model", "invalid"), "--imaginary-option")},
		{"agent_option", []string{"agent", "list", "--imaginary-option"}},
		{"credential_option", []string{"credential", "get", "missing", "--imaginary-option"}},
		{"wrong_command_option", []string{"provider", "list", "--replicas", "2"}},
		{"leading_option", []string{"--imaginary-option", "model", "list"}},
		{"equals_option", append(integrationCreate("model", "invalid-value"), "--imaginary-option=value")},
		{"leading_json_output", []string{"--output", "json", "model", "list", "--imaginary-option"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntegrationFixture(t, binary)
			args := tc.args
			if tc.name != "leading_json_output" {
				args = integrationScope(args)
			}
			result := f.run(args)
			result.failure(t, 2)
			integrationEqual(t, result.stdout, "")
			result.structuredStderr(t)
			integrationNoRequests(t, f.api)
		})
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"parent_path", []string{"--id", "hf://Qwen/Qwen3-0.6B", "--file", "../outside.gguf"}},
		{"absolute_path", []string{"--id", "hf://Qwen/Qwen3-0.6B", "--file", "/absolute.gguf"}},
		{"oci_revision", []string{"--id", "oci://registry.example.com/model:v1", "--revision", "main"}},
		{"s3_revision", []string{"--id", "s3://model-bucket/model", "--revision", "main"}},
		{"file_url", []string{"--id", "file:///tmp/model.gguf"}},
		{"ftp_url", []string{"--id", "ftp://example.com/model"}},
		{"signed_url", []string{"--id", "https://example.com/model?sig=fixture"}},
		{"userinfo_url", []string{"--id", "https://user:password@example.com/model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntegrationFixture(t, binary)
			args := append([]string{"model", "create", "invalid-source", "--dry-run", "client"}, tc.args...)
			result := f.run(integrationScope(args))
			result.failure(t, 2)
			integrationEqual(t, result.stdout, "")
			result.structuredStderr(t)
			integrationNoRequests(t, f.api)
		})
	}
}

func integrationWaiting(t *testing.T, binary string) {
	for _, noun := range []string{"model", "agent"} {
		t.Run(noun, func(t *testing.T) {
			for _, scenario := range []string{"duplicate", "stale_ready", "pending_timeout", "sigint", "current_ready"} {
				t.Run(scenario, func(t *testing.T) {
					if scenario == "sigint" && runtime.GOOS == "windows" {
						t.Skip("os.Interrupt cannot be sent to a child process on Windows")
					}
					f := newIntegrationFixture(t, binary)
					f.api.bindingModel()
					name, plural := "subject", noun+"deployments"
					create := integrationCreate(noun, name)
					switch scenario {
					case "duplicate":
						f.run(integrationScope(append(create, "--wait=false"))).json(t)
						before := f.api.get(plural, "team-a", name)
						f.run(integrationScope(append(create, "--wait=false"))).failure(t, 5)
						integrationEqual(t, f.api.get(plural, "team-a", name), before)
						integrationWriteMethods(t, f.api, "POST", "POST")
					case "stale_ready":
						f.api.setCreatedReady(true)
						created := f.run(integrationScope(append(create, "--wait=false"))).json(t)
						update := []string{"--replicas", "2"}
						if noun == "agent" {
							update = []string{"--prompt", "Updated prompt."}
						}
						args := append([]string{noun, "update", name, "--timeout", "250ms"}, update...)
						result := f.run(integrationScope(args))
						result.failure(t, 4)
						result.structuredStderr(t)
						stored := f.api.get(plural, "team-a", name)
						generation := integrationField(created, "metadata", "generation").(float64)
						integrationWantField(t, stored, generation+1, "metadata", "generation")
						integrationWantField(t, stored, generation, "status", "observedGeneration")
						integrationWantField(t, stored, "Running", "status", "phase")
						integrationEqual(t, integrationField(stored, "status", "conditions"), integrationField(created, "status", "conditions"))
						integrationWriteMethods(t, f.api, "POST", "PATCH")
					case "pending_timeout":
						result := f.run(integrationScope(append(create, "--timeout", "250ms")))
						result.failure(t, 4)
						result.structuredStderr(t)
						stored := f.api.get(plural, "team-a", name)
						integrationAssertReady(t, stored, false)
						integrationWriteMethods(t, f.api, "POST")
					case "sigint":
						child := f.start(integrationScope(append(create, "--timeout", "30s")), "")
						f.api.waitForRead(t, child, plural, name)
						if err := child.cmd.Process.Signal(os.Interrupt); err != nil {
							t.Fatalf("send SIGINT: %v", err)
						}
						result := child.wait(t)
						result.failure(t, 130)
						result.structuredStderr(t)
						integrationAssertReady(t, f.api.get(plural, "team-a", name), false)
						integrationWriteMethods(t, f.api, "POST")
					case "current_ready":
						f.api.setCreatedReady(true)
						result := f.run(integrationScope(append(create, "--timeout", "2s"))).json(t)
						integrationAssertReady(t, result, true)
						integrationWriteMethods(t, f.api, "POST")
					}
				})
			}
		})
	}
}

func integrationAssertReady(t *testing.T, value any, ready bool) {
	t.Helper()
	generation, ok := integrationField(value, "metadata", "generation").(float64)
	if !ok || generation < 1 {
		t.Fatal("submitted resource is missing or has no generation")
	}
	integrationWantField(t, value, generation, "status", "observedGeneration")
	conditions, ok := integrationField(value, "status", "conditions").([]any)
	if !ok || len(conditions) == 0 {
		t.Fatal("resource has no readiness conditions")
	}
	want, phase := "False", "Pending"
	if ready {
		want, phase = "True", "Running"
	}
	integrationWantField(t, value, phase, "status", "phase")
	found := false
	for _, condition := range conditions {
		if integrationField(condition, "type") == "Ready" {
			integrationWantField(t, condition, want, "status")
			integrationWantField(t, condition, generation, "observedGeneration")
			found = true
		}
	}
	if !found {
		t.Fatal("resource has no current-generation Ready condition")
	}
}

func integrationArtifactPreviews(t *testing.T, binary string) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name, id       string
		extra          []string
		artifact       bool
		revision, file string
	}{
		{"bare", "Qwen/Qwen3-0.6B", nil, false, "", ""},
		{"huggingface", "hf://Qwen/Qwen3-0.6B", nil, false, "", ""},
		{"pinned-hf", "hf://Qwen/Qwen3-0.6B", []string{"--revision", revision, "--file", "model.gguf"}, true, revision, "model.gguf"},
		{"s3", "s3://model-bucket/qwen/", []string{"--credential", "storage-access", "--service-account", "model-reader"}, true, "", ""},
		{"gcs", "gs://model-bucket/qwen/", nil, true, "", ""},
		{"https", "https://account.blob.core.windows.net/models/model.gguf", []string{"--file", "model.gguf"}, true, "", "model.gguf"},
		{"oci-tag", "oci://registry.example.com/models/qwen:v1", nil, true, "", ""},
		{"oci-digest", "oci://registry.example.com/models/qwen@sha256:" + strings.Repeat("a", 64), nil, true, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntegrationFixture(t, binary)
			args := append([]string{"model", "create", tc.name, "--id", tc.id, "--provider", "vllm", "--dry-run", "client"}, tc.extra...)
			preview := f.run(integrationScope(args)).json(t)
			artifact := integrationField(preview, "spec", "model", "artifact")
			if tc.artifact {
				integrationWantField(t, artifact, tc.id, "uri")
				if tc.revision != "" {
					integrationWantField(t, artifact, tc.revision, "revision")
				}
				if tc.file != "" {
					integrationWantField(t, artifact, tc.file, "file")
				}
				if tc.name == "s3" {
					integrationWantField(t, artifact, "storage-access", "credentialsRef", "name")
					integrationWantField(t, artifact, "model-reader", "serviceAccountName")
				}
			} else {
				integrationEqual(t, artifact, nil)
				integrationWantField(t, preview, "Qwen/Qwen3-0.6B", "spec", "model", "id")
				integrationWantField(t, preview, "huggingface", "spec", "model", "source")
			}
			integrationNoRequests(t, f.api)
		})
	}
	t.Run("existing_volume", func(t *testing.T) {
		f := newIntegrationFixture(t, binary)
		preview := f.run(integrationScope([]string{"model", "create", "volume", "--id", "pvc://model-store/qwen", "--provider", "vllm", "--dry-run", "client"})).json(t)
		volumes, ok := integrationField(preview, "spec", "model", "storage", "volumes").([]any)
		if !ok || len(volumes) == 0 {
			t.Fatal("existing volume preview has no volumes")
		}
		found := false
		for _, volume := range volumes {
			if integrationField(volume, "claimName") == "model-store" {
				integrationWantField(t, volume, nil, "size")
				found = true
			}
		}
		if !found {
			t.Fatal("preview lost the existing volume claim")
		}
		integrationWantField(t, preview, nil, "spec", "model", "artifact")
		integrationNoRequests(t, f.api)
	})
	t.Run("bundled_image", func(t *testing.T) {
		f := newIntegrationFixture(t, binary)
		preview := f.run(integrationScope([]string{"model", "create", "bundled", "--provider", "vllm", "--image", "registry.example.com/model:v1", "--model-path", "/models/qwen", "--dry-run", "client"})).json(t)
		integrationWantField(t, preview, "registry.example.com/model:v1", "spec", "engine", "image")
		integrationWantField(t, preview, nil, "spec", "model", "artifact")
		integrationNoRequests(t, f.api)
	})
}
