package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

const managementTestToken = "hf_testValueThatMustNeverAppear123456"

type managementCall struct {
	op, path, namespace, name, uid string
	typ                            ResourceType
	body                           Object
	options                        RequestOptions
	dry                            bool
}

type managementFakeClient struct {
	calls     []managementCall
	resources []Object
	failure   func(managementCall) error
	response  func(managementCall, Object) Object
}

var _ ClusterClient = (*managementFakeClient)(nil)

func (c *managementFakeClient) record(call managementCall) error {
	c.calls = append(c.calls, call)
	if c.failure != nil {
		return c.failure(call)
	}
	return nil
}
func (c *managementFakeClient) result(call managementCall, resource Object) Object {
	result := cloneObject(resource)
	if c.response != nil {
		return c.response(call, result)
	}
	return result
}
func (c *managementFakeClient) Request(_ context.Context, method, path string, body any, options RequestOptions) (Object, error) {
	call := managementCall{op: method, path: path, body: cloneObject(object(body)), options: options}
	if err := c.record(call); err != nil {
		return nil, err
	}
	return c.result(call, object(body)), nil
}
func (c *managementFakeClient) Raw(context.Context, string, string, any, RequestOptions) (*http.Response, error) {
	return nil, errors.New("unexpected raw request")
}
func (c *managementFakeClient) List(_ context.Context, typ ResourceType, ns string, query url.Values) ([]Object, error) {
	if err := c.record(managementCall{op: "list", typ: typ, namespace: ns, options: RequestOptions{Query: query}}); err != nil {
		return nil, err
	}
	result := []Object{}
	for _, item := range c.resources {
		if stringAt(item, "kind") == typ.Kind && (!typ.Namespaced || stringAt(item, "metadata", "namespace") == ns) {
			result = append(result, cloneObject(item))
		}
	}
	return result, nil
}
func (c *managementFakeClient) Get(_ context.Context, typ ResourceType, ns, name string) (Object, error) {
	if err := c.record(managementCall{op: "get", typ: typ, namespace: ns, name: name}); err != nil {
		return nil, err
	}
	for _, item := range c.resources {
		if stringAt(item, "kind") == typ.Kind && stringAt(item, "metadata", "name") == name && (!typ.Namespaced || stringAt(item, "metadata", "namespace") == ns) {
			return cloneObject(item), nil
		}
	}
	return nil, cliError(1, "HTTP_404", "Not found")
}
func (c *managementFakeClient) Create(_ context.Context, resource Object, dry bool) (Object, error) {
	call := managementCall{op: "create", body: cloneObject(resource), dry: dry}
	if err := c.record(call); err != nil {
		return nil, err
	}
	return c.result(call, resource), nil
}
func (c *managementFakeClient) Patch(_ context.Context, typ ResourceType, ns, name string, body any, dry bool) (Object, error) {
	call := managementCall{op: "patch", typ: typ, namespace: ns, name: name, body: cloneObject(object(body)), dry: dry}
	if err := c.record(call); err != nil {
		return nil, err
	}
	for _, item := range c.resources {
		if stringAt(item, "kind") == typ.Kind && stringAt(item, "metadata", "name") == name && stringAt(item, "metadata", "namespace") == ns {
			return c.result(call, item), nil
		}
	}
	return nil, errors.New("missing test patch target")
}
func (c *managementFakeClient) Delete(_ context.Context, typ ResourceType, ns, name, uid string) error {
	return c.record(managementCall{op: "delete", typ: typ, namespace: ns, name: name, uid: uid})
}
func (c *managementFakeClient) RESTConfig() *rest.Config { return nil }
func (c *managementFakeClient) writes() []managementCall {
	result := []managementCall{}
	for _, call := range c.calls {
		switch call.op {
		case "create", "patch", "delete", "PATCH", "POST", "DELETE":
			result = append(result, call)
		}
	}
	return result
}

type managementHarness struct {
	ctx         *CommandContext
	client      *managementFakeClient
	out, stderr bytes.Buffer
	connections int
}

func managementTestContext(flags ...string) *managementHarness {
	h := &managementHarness{client: &managementFakeClient{}}
	f := Flags{"output": {"json"}}
	for i := 0; i < len(flags); i += 2 {
		f[flags[i]] = []string{flags[i+1]}
	}
	h.ctx = &CommandContext{Context: context.Background(), Flags: f, Namespace: "team", ContextName: "test-context", IO: &IO{
		In: strings.NewReader(managementTestToken + "\n"), Out: &h.out, Err: &h.stderr,
	}, Client: func() (ClusterClient, error) { h.connections++; return h.client, nil }}
	return h
}
func managementTestRun(t *testing.T, h *managementHarness, words ...string) {
	t.Helper()
	handled, err := runManagement(words, h.ctx)
	if !handled || err != nil {
		t.Fatalf("run %v: handled=%v err=%v", words, handled, err)
	}
}
func managementTestError(t *testing.T, h *managementHarness, message string, words ...string) error {
	t.Helper()
	handled, err := runManagement(words, h.ctx)
	if !handled || err == nil {
		t.Fatalf("expected handled error for %v; handled=%v err=%v", words, handled, err)
	}
	if !strings.Contains(err.Error(), message) {
		t.Fatalf("error %q does not contain %q", err, message)
	}
	if strings.Contains(err.Error(), managementTestToken) {
		t.Fatal("error disclosed credential input")
	}
	return err
}
func managementTestCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *CLIError
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error=%v, want code %s", err, code)
	}
}
func managementTestJSON(t *testing.T, a, b any) {
	t.Helper()
	normalize := func(v any) any {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err := decodeJSON(data, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !reflect.DeepEqual(normalize(a), normalize(b)) {
		t.Fatalf("mismatch:\ngot %#v\nwant %#v", a, b)
	}
}
func managementTestOutput(t *testing.T, h *managementHarness) any {
	t.Helper()
	var result any
	if err := decodeJSON(h.out.Bytes(), &result); err != nil {
		t.Fatalf("invalid output %q: %v", h.out.String(), err)
	}
	return result
}
func managementTestNoDisclosure(t *testing.T, h *managementHarness) {
	t.Helper()
	for _, value := range []string{managementTestToken, base64.StdEncoding.EncodeToString([]byte(managementTestToken)), "last-applied", "private-label", "stringData"} {
		if strings.Contains(h.out.String()+h.stderr.String(), value) {
			t.Fatalf("output disclosed %q", value)
		}
	}
}
func managementTestSecret(kind string) Object {
	return Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": "token", "namespace": "team", "uid": "secret-uid", "resourceVersion": "7", "creationTimestamp": "2026-01-01T00:00:00Z", "labels": Object{managementManagedBy: managementManager, managementCredentialType: kind, "private-label": managementTestToken}, "annotations": Object{"kubectl.kubernetes.io/last-applied-configuration": managementTestToken}}, "data": Object{"HF_TOKEN": base64.StdEncoding.EncodeToString([]byte(managementTestToken))}, "stringData": Object{"HF_TOKEN": managementTestToken}}
}
func managementTestModel(name string) Object {
	return Object{"apiVersion": "airunway.ai/v1alpha1", "kind": "ModelDeployment", "metadata": Object{"name": name, "namespace": "team"}, "spec": Object{"model": Object{"id": "Qwen/Qwen3-0.6B", "source": "huggingface"}, "engine": Object{"type": "vllm"}}}
}
func managementTestAgent(name, lifecycle string) Object {
	return Object{"apiVersion": "airunway.ai/v1alpha1", "kind": "AgentDeployment", "metadata": Object{"name": name, "namespace": "team"}, "spec": Object{"framework": Object{"name": "openclaw"}, "lifecycle": lifecycle, "model": Object{"deploymentRef": Object{"name": "model"}}, "config": Object{"systemPrompt": "Answer questions."}}}
}
func managementTestFramework(name string, entries any) Object {
	if entries == nil {
		entries = []any{Object{"name": "helper", "title": "Helper"}}
	}
	data, _ := json.Marshal(entries)
	return Object{"apiVersion": "airunway.ai/v1alpha1", "kind": "AgentProviderConfig", "metadata": Object{"name": name, "annotations": Object{"airunway.ai/agent-catalog": string(data), "airunway.ai/install-instructions": "never execute"}}, "spec": Object{"capabilities": Object{"backend": "container"}}, "status": Object{"ready": true}}
}
func managementTestWrite(t *testing.T, directory, name string, value any) string {
	t.Helper()
	var data []byte
	var err error
	if text, ok := value.(string); ok {
		data = []byte(text)
	} else if strings.HasSuffix(name, ".json") {
		data, err = json.Marshal(value)
	} else {
		data, err = marshalYAML(value)
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func managementTestBatch(t *testing.T, values ...Object) string {
	t.Helper()
	var data []string
	for _, value := range values {
		b, err := marshalYAML(value)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, string(b))
	}
	return managementTestWrite(t, t.TempDir(), "batch.yaml", strings.Join(data, "\n---\n"))
}

func TestManagementDispatch(t *testing.T) {
	for _, words := range [][]string{nil, {"model", "get", "foo"}, {"doctor"}} {
		h := managementTestContext()
		if handled, err := runManagement(words, h.ctx); handled || err != nil || h.connections != 0 {
			t.Fatalf("unexpected dispatch %v: %v %v", words, handled, err)
		}
	}
	for _, words := range [][]string{{"credential"}, {"credential", "install"}, {"provider", "install"}, {"framework", "delete"}, {"catalog"}, {"catalog", "model", "list"}, {"catalog", "agent", "search"}, {"catalog", "unknown"}, {"provider", "get"}, {"credential", "list", "extra"}, {"apply", "extra"}} {
		t.Run(strings.Join(words, "/"), func(t *testing.T) {
			h := managementTestContext()
			managementTestError(t, h, "", words...)
			if h.connections != 0 {
				t.Fatal("invalid arguments acquired a client")
			}
		})
	}
	for _, flags := range [][]string{{"output", "bad"}, {"dry-run", "bad"}, {managementTestToken, "private"}} {
		h := managementTestContext(append([]string{"type", "api-key", "from-file", "-"}, flags...)...)
		managementTestError(t, h, "", "credential", "create", "token")
		if h.connections != 0 {
			t.Fatal("invalid flags acquired a client")
		}
	}
}

func TestManagementCredentialsCreateAndDryRun(t *testing.T) {
	for _, kind := range []string{"huggingface", "api-key"} {
		for _, dry := range []string{"", "client", "server"} {
			t.Run(kind+"/"+dry, func(t *testing.T) {
				h := managementTestContext("type", kind, "from-file", "-")
				if dry != "" {
					h.ctx.Flags["dry-run"] = []string{dry}
				}
				h.client.response = func(_ managementCall, result Object) Object {
					object(result["metadata"])["annotations"] = Object{"private": managementTestToken}
					result["status"] = Object{"private": managementTestToken}
					return result
				}
				managementTestRun(t, h, "credential", "create", "token")
				if dry == "client" {
					if h.connections != 0 {
						t.Fatal("client dry-run connected")
					}
				} else {
					writes := h.client.writes()
					if len(writes) != 1 || writes[0].op != "create" || writes[0].dry != (dry == "server") {
						t.Fatalf("writes %#v", writes)
					}
					managementTestJSON(t, writes[0].body, Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": "token", "namespace": "team", "labels": Object{managementManagedBy: managementManager, managementCredentialType: kind}}, "data": Object{managementCredentialKeys[kind]: base64.StdEncoding.EncodeToString([]byte(managementTestToken))}})
				}
				if len(object(managementTestOutput(t, h))) != 3 {
					t.Fatal("credential output is not metadata-only")
				}
				managementTestNoDisclosure(t, h)
			})
		}
	}
}

func TestManagementCredentialsInputValidation(t *testing.T) {
	for _, input := range []string{"", "first\nsecond", "token\n\n", "token ", "token\t", "token\x00", "token\ufeff", strings.Repeat("x", maxInput+1)} {
		h := managementTestContext("type", "api-key", "from-file", "-")
		h.ctx.IO.In = strings.NewReader(input)
		managementTestError(t, h, "", "credential", "create", "token")
		if h.connections != 0 {
			t.Fatal("invalid credential connected")
		}
	}
	for _, flags := range [][]string{{"type", "other", "from-file", "-"}, {"type", "api-key"}, {"from-file", "-"}} {
		h := managementTestContext(flags...)
		managementTestError(t, h, "", "credential", "create", "token")
		if h.connections != 0 {
			t.Fatal("missing option connected")
		}
	}
	path := managementTestWrite(t, t.TempDir(), "token", managementTestToken+"\r\n")
	h := managementTestContext("type", "api-key", "from-file", path)
	managementTestRun(t, h, "credential", "create", "token")
	if stringAt(h.client.writes()[0].body, "data", "API_KEY") != base64.StdEncoding.EncodeToString([]byte(managementTestToken)) {
		t.Fatal("token trailing newline not trimmed")
	}
	link := filepath.Join(t.TempDir(), "token-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		h := managementTestContext("type", "api-key", "from-file", path)
		managementTestError(t, h, "", "credential", "create", "token")
		if h.connections != 0 {
			t.Fatal("invalid file connected")
		}
	}
}

func TestManagementArtifactCredentials(t *testing.T) {
	value := Object{"aws_access_key_id": "example-id", "aws_secret_access_key": managementTestToken, "region_name": "us-west-2"}
	data, _ := json.MarshalIndent(value, "", "  ")
	h := managementTestContext("type", "artifact", "from-file", "-")
	h.ctx.IO.In = bytes.NewReader(data)
	managementTestRun(t, h, "credential", "create", "token")
	created := h.client.writes()[0].body
	encoded := stringAt(created, "data", "credentials")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := decodeJSON(decoded, &got); err != nil {
		t.Fatal(err)
	}
	managementTestJSON(t, got, value)
	if len(object(created["data"])) != 1 {
		t.Fatal("artifact uses more than one data key")
	}
	object(created["metadata"])["resourceVersion"] = "3"
	h.client.resources = []Object{created}
	h.out.Reset()
	h.ctx.IO.In = bytes.NewReader(data)
	managementTestRun(t, h, "credential", "update", "token")
	managementTestJSON(t, h.client.writes()[1].body, Object{"metadata": Object{"resourceVersion": "3"}, "data": Object{"credentials": encoded}})
	managementTestNoDisclosure(t, h)
}

func TestManagementArtifactLimitsAndSafeErrors(t *testing.T) {
	for _, size := range []int{64 * 1024, 64*1024 + 1} {
		data := `{"token":"` + strings.Repeat("x", size-len(`{"token":""}`)) + `"}`
		for _, source := range []string{"stdin", "file"} {
			h := managementTestContext("type", "artifact", "from-file", "-")
			h.ctx.IO.In = strings.NewReader(data)
			if source == "file" {
				h.ctx.Flags["from-file"] = []string{managementTestWrite(t, t.TempDir(), "credentials.json", data)}
			}
			if size > 64*1024 {
				managementTestError(t, h, "64 KiB", "credential", "create", "token")
				if h.connections != 0 {
					t.Fatal("oversized artifact connected")
				}
			} else {
				managementTestRun(t, h, "credential", "create", "token")
			}
		}
	}
	for _, data := range []string{"{" + managementTestToken, "{}", "[]", "null", `"string"`, `{"__proto__":{"secret":"value"}}`, `{"nested":{"constructor":0}}`} {
		h := managementTestContext("type", "artifact", "from-file", "-")
		h.ctx.IO.In = strings.NewReader(data)
		managementTestError(t, h, "nonempty JSON object", "credential", "create", "token")
		if h.connections != 0 {
			t.Fatal("invalid artifact connected")
		}
	}
}

func TestManagementCredentialsReadOwnership(t *testing.T) {
	for _, output := range []string{"json", "yaml", "text"} {
		h := managementTestContext("output", output)
		h.client.resources = []Object{managementTestSecret("huggingface")}
		managementTestRun(t, h, "credential", "get", "token")
		managementTestNoDisclosure(t, h)
	}
	h := managementTestContext()
	foreign := managementTestSecret("huggingface")
	object(get(foreign, "metadata", "labels"))[managementManagedBy] = "other"
	otherNS := managementTestSecret("huggingface")
	object(otherNS["metadata"])["namespace"] = "other"
	owned := managementTestSecret("api-key")
	owner := managementTestSecret("api-key")
	object(owner["metadata"])["ownerReferences"] = []any{Object{"uid": "owner"}}
	h.client.resources = []Object{owned, foreign, otherNS, owner}
	managementTestRun(t, h, "credential", "list")
	if len(array(managementTestOutput(t, h))) != 1 {
		t.Fatal("listed unmanaged credentials")
	}
	if h.client.calls[0].options.Query.Get("labelSelector") != managementManagedBy+"="+managementManager+","+managementCredentialType {
		t.Fatal("wrong label selector")
	}
	for _, namespace := range []string{"", "a.b", "Bad", strings.Repeat("a", 64)} {
		h := managementTestContext()
		h.ctx.Namespace = namespace
		managementTestError(t, h, "", "credential", "list")
		if h.connections != 0 {
			t.Fatal("invalid namespace connected")
		}
	}
	h = managementTestContext("all-namespaces", "true")
	managementTestError(t, h, "", "credential", "list")
	for _, mutation := range []func(Object){func(o Object) { object(get(o, "metadata", "labels"))[managementManagedBy] = "other" }, func(o Object) { o["type"] = "kubernetes.io/tls" }, func(o Object) { o["apiVersion"] = "other/v1" }, func(o Object) { object(o["metadata"])["ownerReferences"] = []any{Object{"uid": "owner"}} }} {
		for _, action := range []string{"get", "update", "delete"} {
			h := managementTestContext()
			item := managementTestSecret("api-key")
			mutation(item)
			h.client.resources = []Object{item}
			managementTestError(t, h, "not a CLI-managed", "credential", action, "token")
			if len(h.client.writes()) != 0 {
				t.Fatal("modified unmanaged secret")
			}
		}
	}
}

func TestManagementCredentialUpdates(t *testing.T) {
	for _, dry := range []string{"", "client", "server"} {
		h := managementTestContext("from-file", "-")
		if dry != "" {
			h.ctx.Flags["dry-run"] = []string{dry}
		}
		h.client.resources = []Object{managementTestSecret("api-key")}
		managementTestRun(t, h, "credential", "update", "token")
		if dry == "client" {
			if len(h.client.writes()) != 0 {
				t.Fatal("client preview wrote")
			}
		} else {
			writes := h.client.writes()
			if len(writes) != 1 || writes[0].dry != (dry == "server") {
				t.Fatalf("unexpected writes %#v", writes)
			}
			managementTestJSON(t, writes[0].body, Object{"metadata": Object{"resourceVersion": "7"}, "data": Object{"API_KEY": base64.StdEncoding.EncodeToString([]byte(managementTestToken))}})
			if writes[0].namespace != "team" || writes[0].name != "token" {
				t.Fatal("incorrect update target")
			}
		}
		managementTestNoDisclosure(t, h)
	}
	h := managementTestContext("type", "api-key", "from-file", "-")
	h.client.resources = []Object{managementTestSecret("huggingface")}
	managementTestError(t, h, "type cannot change", "credential", "update", "token")
	delete(h.ctx.Flags, "type")
	delete(object(h.client.resources[0]["metadata"]), "resourceVersion")
	managementTestError(t, h, "resourceVersion", "credential", "update", "token")
	if len(h.client.writes()) != 0 {
		t.Fatal("invalid update wrote")
	}
	h = managementTestContext("from-file", "-")
	h.client.resources = []Object{managementTestSecret("huggingface")}
	h.client.failure = func(c managementCall) error {
		if c.op == "patch" {
			return cliError(5, "HTTP_409", "Changed concurrently")
		}
		return nil
	}
	managementTestCode(t, managementTestError(t, h, "", "credential", "update", "token"), "HTTP_409")
	if len(h.client.writes()) != 1 {
		t.Fatal("concurrency failure retried")
	}
}

func TestManagementCredentialDeleteReferences(t *testing.T) {
	refs := []Object{
		{"secrets": Object{"huggingFaceToken": "token"}},
		{"model": Object{"artifact": Object{"credentialsRef": Object{"name": "token", "key": "credentials"}}}},
		{"env": []any{Object{"name": "HF_TOKEN", "valueFrom": Object{"secretKeyRef": Object{"name": "token", "key": "HF_TOKEN"}}}}},
		{"imagePullSecrets": []any{Object{"name": "token"}}},
		{"model": Object{"externalAPI": Object{"credentialsRef": Object{"name": "token"}}}},
		{"template": Object{"authSecretRef": Object{"name": "token"}}},
		{"nested": Object{"secretRef": Object{"name": "token"}}},
	}
	for i, ref := range refs {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			h := managementTestContext()
			resource := managementTestModel("model")
			resource["spec"] = ref
			h.client.resources = []Object{managementTestSecret("api-key"), resource}
			managementTestCode(t, managementTestError(t, h, "referenced", "credential", "delete", "token"), "IN_USE")
			if len(h.client.writes()) != 0 {
				t.Fatal("deleted referenced credential")
			}
		})
	}
	h := managementTestContext()
	agent := managementTestAgent("agent", "deployment")
	agent["status"] = Object{"modelBinding": Object{"credentialsRef": Object{"name": "token"}}}
	h.client.resources = []Object{managementTestSecret("api-key"), agent}
	managementTestCode(t, managementTestError(t, h, "", "credential", "delete", "token"), "IN_USE")
}

func TestManagementCredentialDeletePreconditions(t *testing.T) {
	h := managementTestContext()
	other := managementTestModel("model")
	object(other["metadata"])["namespace"] = "other"
	other["spec"] = Object{"secrets": Object{"huggingFaceToken": "token"}}
	h.client.resources = []Object{managementTestSecret("api-key"), other}
	managementTestRun(t, h, "credential", "delete", "token")
	if len(h.client.calls) != 4 || h.client.calls[1].typ.Kind != "ModelDeployment" || h.client.calls[2].typ.Kind != "AgentDeployment" {
		t.Fatalf("reference scans %#v", h.client.calls)
	}
	for _, call := range h.client.calls {
		if call.namespace != "team" {
			t.Fatal("cross namespace operation")
		}
	}
	if len(h.client.writes()) != 1 || h.client.writes()[0].uid != "secret-uid" {
		t.Fatal("missing UID precondition")
	}
	managementTestNoDisclosure(t, h)
	for _, code := range []string{"HTTP_403", "HTTP_404"} {
		h := managementTestContext()
		h.client.resources = []Object{managementTestSecret("api-key")}
		h.client.failure = func(c managementCall) error {
			if c.op == "list" {
				return cliError(3, code, "Cannot list")
			}
			return nil
		}
		managementTestCode(t, managementTestError(t, h, "", "credential", "delete", "token"), code)
		if len(h.client.writes()) != 0 {
			t.Fatal("reference scan failed open")
		}
	}
	h = managementTestContext()
	item := managementTestSecret("api-key")
	delete(object(item["metadata"]), "uid")
	h.client.resources = []Object{item}
	managementTestError(t, h, "UID", "credential", "delete", "token")
	if len(h.client.writes()) != 0 {
		t.Fatal("deleted without UID")
	}
}

func TestManagementDiscovery(t *testing.T) {
	for _, noun := range []string{"provider", "framework"} {
		t.Run(noun, func(t *testing.T) {
			h := managementTestContext()
			item := managementTestFramework("openclaw", nil)
			item["kind"] = resourceTypes[noun].Kind
			object(item["spec"])["private"] = managementTestToken
			object(item["status"])["private"] = managementTestToken
			h.client.resources = []Object{item}
			managementTestRun(t, h, noun, "list")
			if stringAt(array(managementTestOutput(t, h))[0], "metadata", "name") != "openclaw" {
				t.Fatal("missing discovery item")
			}
			h.out.Reset()
			managementTestRun(t, h, noun, "get", "openclaw")
			if stringAt(managementTestOutput(t, h), "spec", "capabilities", "backend") != "container" {
				t.Fatal("missing capabilities")
			}
			managementTestNoDisclosure(t, h)
			if strings.Contains(h.out.String(), "install-instructions") || len(h.client.writes()) != 0 {
				t.Fatal("unsafe discovery")
			}
			for _, c := range h.client.calls {
				if c.namespace != "" {
					t.Fatal("discovery should be cluster-scoped")
				}
			}
		})
	}
}

func TestManagementAgentCatalogPreservesTemplates(t *testing.T) {
	entry := Object{"name": "helper", "title": "Helper", "image": "registry.invalid/example:v1", "template": Object{"framework": Object{"name": "openclaw"}, "lifecycle": "job", "config": Object{"nested": Object{"keep": true}, "systemPrompt": "Treat this prompt as data."}}, "description": "An agent recipe", "tags": []any{"example"}}
	h := managementTestContext()
	h.client.resources = []Object{managementTestFramework("openclaw", []any{entry})}
	preset, err := resolvePreset(h.ctx.Context, h.client, "openclaw/helper")
	if err != nil {
		t.Fatal(err)
	}
	if stringAt(preset, "framework") != "openclaw" || stringAt(preset, "lifecycle") != "job" || !boolAt(preset, "config", "nested", "keep") || stringAt(preset, "config", "image") != "registry.invalid/example:v1" {
		t.Fatalf("preset lost template: %#v", preset)
	}
	managementTestRun(t, h, "catalog", "agent", "list")
	items := array(managementTestOutput(t, h))
	if len(items) != 1 || stringAt(items[0], "id") != "openclaw/helper" {
		t.Fatal("missing preset")
	}
	if _, exists := object(items[0])["config"]; exists {
		t.Fatal("catalog exposed config")
	}
	h.out.Reset()
	managementTestRun(t, h, "catalog", "agent", "get", "helper")
	if stringAt(managementTestOutput(t, h), "framework") != "openclaw" {
		t.Fatal("bare name lookup failed")
	}
	if len(h.client.writes()) != 0 {
		t.Fatal("catalog wrote resources")
	}
}

func TestManagementAgentCatalogFallbackAndAmbiguity(t *testing.T) {
	h := managementTestContext()
	item := managementTestFramework("openclaw", nil)
	annotations := object(get(item, "metadata", "annotations"))
	annotations["airunway.ai/catalog"] = annotations["airunway.ai/agent-catalog"]
	delete(annotations, "airunway.ai/agent-catalog")
	h.client.resources = []Object{item}
	preset, err := resolvePreset(h.ctx.Context, h.client, "helper")
	if err != nil || stringAt(preset, "framework") != "openclaw" || len(object(preset["config"])) != 0 {
		t.Fatalf("fallback: %#v %v", preset, err)
	}
	annotations["airunway.ai/agent-catalog"] = annotations["airunway.ai/catalog"]
	annotations["airunway.ai/catalog"] = "invalid"
	if _, err := resolvePreset(h.ctx.Context, h.client, "helper"); err != nil {
		t.Fatal(err)
	}
	h.client.resources = []Object{managementTestFramework("one", nil), managementTestFramework("two", nil)}
	if _, err := resolvePreset(h.ctx.Context, h.client, "helper"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity: %v", err)
	}
	preset, err = resolvePreset(h.ctx.Context, h.client, "one/helper")
	if err != nil || stringAt(preset, "framework") != "one" {
		t.Fatalf("qualified lookup: %#v %v", preset, err)
	}
	_, err = resolvePreset(h.ctx.Context, h.client, "missing")
	managementTestCode(t, err, "NOT_FOUND")
	for _, id := range []string{"", "one/helper/extra", "../helper", "Upper", strings.Repeat("x", 64)} {
		before := len(h.client.calls)
		if _, err := resolvePreset(h.ctx.Context, h.client, id); err == nil {
			t.Fatalf("accepted ID %q", id)
		}
		if len(h.client.calls) != before {
			t.Fatal("invalid preset ID connected")
		}
	}
}

func TestManagementAgentCatalogRejectsUnsafeData(t *testing.T) {
	valid := func() Object { return Object{"name": "helper", "title": "Helper"} }
	entries := []any{
		Object{"entries": "not an array"}, []any{Object{"name": "helper"}}, []any{Object{"name": "helper", "title": " "}},
		[]any{Object{"name": "helper", "title": "Helper", "template": Object{"config": []any{}}}},
		[]any{Object{"name": "helper", "title": "Helper", "template": Object{"framework": Object{"name": "other"}}}},
		[]any{Object{"name": "helper", "title": "Helper", "template": Object{"config": Object{"apiKey": managementTestToken}}}},
		[]any{valid(), valid()}, []any{Object{"name": "helper", "title": "Helper", "template": Object{"config": Object{"__proto__": Object{"polluted": true}}}}},
		[]any{Object{"name": "helper", "title": "Helper", "image": "a", "template": Object{"config": Object{"image": "b"}}}},
		[]any{Object{"name": "helper", "title": "Helper", "image": 123}}, []any{Object{"name": "helper", "title": "Helper", "template": nil}},
		[]any{Object{"name": "with.dot", "title": "Helper"}},
	}
	for i, entry := range entries {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			h := managementTestContext()
			h.client.resources = []Object{managementTestFramework("openclaw", entry)}
			_, err := resolvePreset(h.ctx.Context, h.client, "helper")
			managementTestCode(t, err, "CATALOG")
			if strings.Contains(err.Error(), managementTestToken) {
				t.Fatal("catalog error exposed input")
			}
		})
	}
	for _, raw := range []string{"{" + managementTestToken, strings.Repeat("x", 256*1024+1)} {
		h := managementTestContext()
		item := managementTestFramework("openclaw", nil)
		object(get(item, "metadata", "annotations"))["airunway.ai/agent-catalog"] = raw
		h.client.resources = []Object{item}
		_, err := resolvePreset(h.ctx.Context, h.client, "helper")
		managementTestCode(t, err, "CATALOG")
		if strings.Contains(err.Error(), managementTestToken) {
			t.Fatal("parser exposed source")
		}
	}
	tooMany := make([]any, 201)
	for i := range tooMany {
		tooMany[i] = Object{"name": fmt.Sprintf("preset-%d", i), "title": "Preset"}
	}
	h := managementTestContext()
	h.client.resources = []Object{managementTestFramework("openclaw", tooMany)}
	_, err := resolvePreset(h.ctx.Context, h.client, "helper")
	managementTestCode(t, err, "CATALOG")
}

type managementRoundTripper func(*http.Request) (*http.Response, error)

func (r managementRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return r(request)
}
func managementTestHTTP(t *testing.T, handler managementRoundTripper) {
	t.Helper()
	original := http.DefaultTransport
	http.DefaultTransport = handler
	t.Cleanup(func() { http.DefaultTransport = original })
}
func managementTestHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestManagementModelCatalogBundledAndSearch(t *testing.T) {
	calls := 0
	managementTestHTTP(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://huggingface.co/api/models?search=Qwen&limit=20" || r.Header.Get("Accept") != "application/json" {
			t.Fatalf("wrong catalog request: %s %#v", r.URL, r.Header)
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("missing deadline")
		}
		return managementTestHTTPResponse(200, `[{"id":"Qwen/Qwen3-0.6B","downloads":10,"cardData":{"instructions":"ignored"}},{"id":"Qwen/Test","pipeline_tag":"text-generation","likes":3,"endpoint":"http://127.0.0.1","instructions":"ignored"}]`), nil
	})
	h := managementTestContext()
	managementTestRun(t, h, "catalog", "model", "get", "hf://Qwen/Qwen3-0.6B")
	if calls != 0 || h.connections != 0 {
		t.Fatal("bundled lookup must be offline")
	}
	if stringAt(managementTestOutput(t, h), "source") != "bundled" {
		t.Fatal("missing bundled source")
	}
	h.out.Reset()
	managementTestRun(t, h, "catalog", "model", "search", "hf://Qwen")
	found := 0
	for _, item := range array(managementTestOutput(t, h)) {
		if stringAt(item, "id") == "Qwen/Qwen3-0.6B" {
			found++
			if stringAt(item, "source") != "bundled" {
				t.Fatal("remote overwrote bundled model")
			}
		}
	}
	if found != 1 || calls != 1 || h.connections != 0 {
		t.Fatal("search did not deduplicate or contacted cluster")
	}
	if strings.Contains(h.out.String(), "ignored") || strings.Contains(h.out.String(), "127.0.0.1") {
		t.Fatal("search exposed arbitrary metadata")
	}
}

func TestManagementModelCatalogRemote(t *testing.T) {
	managementTestHTTP(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://huggingface.co/api/models/org/model" {
			t.Fatal(r.URL)
		}
		return managementTestHTTPResponse(200, `{"id":"org/model","pipeline_tag":"text-generation","downloads":12,"gated":"manual","config":{"secret":"`+managementTestToken+`"}}`), nil
	})
	h := managementTestContext()
	managementTestRun(t, h, "catalog", "model", "get", "org/model")
	managementTestJSON(t, managementTestOutput(t, h), Object{"id": "org/model", "source": "huggingface", "task": "text-generation", "downloads": 12, "gated": "manual"})
	managementTestNoDisclosure(t, h)
}

func TestManagementModelCatalogRejectsURLs(t *testing.T) {
	calls := 0
	managementTestHTTP(t, func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") })
	for _, id := range []string{"https://huggingface.co/org/model", "http://localhost/x", "../secret", "org/../../x", "org/model?token=x", "org%2fmodel", "hf://", "org/model/extra", "//host/path", "org/model--a", "org/model.", strings.Repeat("x", 97)} {
		h := managementTestContext()
		managementTestError(t, h, "identifier", "catalog", "model", "get", id)
	}
	if calls != 0 {
		t.Fatal("invalid ID contacted remote")
	}
}

func TestManagementModelCatalogFailures(t *testing.T) {
	for _, test := range []struct {
		name, body, code, action string
		status                   int
	}{
		{"mismatch", `{"id":"other/model"}`, "CATALOG", "get", 200},
		{"syntax", "bad " + managementTestToken, "CATALOG", "get", 200},
		{"limit", strings.Repeat("x", maxInput+1), "CATALOG", "get", 200},
		{"not-found", managementTestToken, "NOT_FOUND", "get", 404},
		{"forbidden", managementTestToken, "CATALOG", "get", 403},
		{"bad-model", `{"id":"https://evil.invalid"}`, "CATALOG", "get", 200},
		{"not-object", `[]`, "CATALOG", "get", 200},
		{"bad-search", `{}`, "CATALOG", "search", 200},
		{"long-search", "[" + strings.Repeat(`{"id":"org/model"},`, 20) + `{"id":"org/model"}]`, "CATALOG", "search", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			managementTestHTTP(t, func(*http.Request) (*http.Response, error) {
				return managementTestHTTPResponse(test.status, test.body), nil
			})
			h := managementTestContext()
			managementTestCode(t, managementTestError(t, h, "", "catalog", "model", test.action, "org/model"), test.code)
			managementTestNoDisclosure(t, h)
		})
	}
}

func TestManagementModelCatalogRedirectAndCancellation(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		calls := 0
		managementTestHTTP(t, func(*http.Request) (*http.Response, error) {
			calls++
			response := managementTestHTTPResponse(302, managementTestToken)
			response.Header.Set("Location", "http://127.0.0.1/private")
			return response, nil
		})
		h := managementTestContext()
		managementTestCode(t, managementTestError(t, h, "", "catalog", "model", "get", "org/model"), "CATALOG")
		if calls != 1 {
			t.Fatal("followed redirect")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		managementTestHTTP(t, func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
		h := managementTestContext("timeout", "1ms")
		managementTestCode(t, managementTestError(t, h, "timed out", "catalog", "model", "get", "org/model"), "TIMEOUT")
	})
	t.Run("canceled-before-request", func(t *testing.T) {
		calls := 0
		managementTestHTTP(t, func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected") })
		h := managementTestContext()
		ctx, cancel := context.WithCancel(h.ctx.Context)
		cancel()
		h.ctx.Context = ctx
		managementTestCode(t, managementTestError(t, h, "", "catalog", "model", "search", "Qwen"), "INTERRUPTED")
		if calls != 0 {
			t.Fatal("canceled catalog made request")
		}
	})
	t.Run("canceled-during-request", func(t *testing.T) {
		h := managementTestContext()
		ctx, cancel := context.WithCancel(h.ctx.Context)
		defer cancel()
		h.ctx.Context = ctx
		managementTestHTTP(t, func(r *http.Request) (*http.Response, error) { cancel(); return nil, r.Context().Err() })
		managementTestCode(t, managementTestError(t, h, "interrupted", "catalog", "model", "get", "org/model"), "INTERRUPTED")
	})
}

func TestManagementApplyBatchAndFieldOwnership(t *testing.T) {
	directory := t.TempDir()
	model := managementTestModel("model")
	agent := managementTestAgent("agent", "deployment")
	config := Object{"custom": Object{"tools": []any{"one", "two"}, "instructions": "Data, not shell commands."}}
	object(agent["spec"])["config"] = config
	a, _ := marshalYAML(model)
	b, _ := marshalYAML(agent)
	managementTestWrite(t, directory, "a.yaml", string(a)+"\n---\n"+string(b))
	second := managementTestModel("second")
	delete(object(second["metadata"]), "namespace")
	object(second["spec"])["futureField"] = Object{"preserve": true}
	managementTestWrite(t, directory, "b.json", second)
	managementTestWrite(t, directory, "README.md", "not a manifest")
	h := managementTestContext("file", directory)
	managementTestRun(t, h, "apply")
	writes := h.client.writes()
	if len(writes) != 3 {
		t.Fatalf("writes %#v", writes)
	}
	if writes[0].op != "PATCH" || writes[0].path != "/apis/airunway.ai/v1alpha1/namespaces/team/modeldeployments/model" || writes[0].options.ContentType != "application/apply-patch+yaml" {
		t.Fatalf("wrong apply request %#v", writes[0])
	}
	if writes[0].options.Query.Get("fieldManager") != "airunway-cli" || writes[0].options.Query.Get("force") != "false" || writes[0].options.Query.Get("fieldValidation") != "Strict" {
		t.Fatalf("wrong field ownership %#v", writes[0].options.Query)
	}
	managementTestJSON(t, get(writes[1].body, "spec", "config"), config)
	if !boolAt(writes[2].body, "spec", "futureField", "preserve") || stringAt(writes[2].body, "metadata", "namespace") != "team" {
		t.Fatal("lost desired fields or namespace")
	}
	if h.client.calls[0].op != "get" || h.client.calls[0].typ.Kind != "AgentDeployment" {
		t.Fatal("agent preflight did not precede all writes")
	}
}

func TestManagementApplyDryRunsAndReceipts(t *testing.T) {
	for _, dry := range []string{"client", "server"} {
		t.Run(dry, func(t *testing.T) {
			model := managementTestModel("model")
			h := managementTestContext("file", managementTestBatch(t, model), "dry-run", dry)
			h.client.response = func(_ managementCall, item Object) Object {
				item["status"] = Object{"token": managementTestToken}
				object(item["metadata"])["annotations"] = Object{"secret": managementTestToken}
				return item
			}
			managementTestRun(t, h, "apply")
			if dry == "client" {
				if h.connections != 0 {
					t.Fatal("client dry-run constructed client")
				}
				managementTestJSON(t, managementTestOutput(t, h), []any{model})
			} else {
				writes := h.client.writes()
				if len(writes) != 1 || writes[0].options.Query.Get("dryRun") != "All" || writes[0].options.Query.Get("force") != "false" {
					t.Fatal("incorrect server dry-run")
				}
				receipt := object(array(managementTestOutput(t, h))[0])
				if len(receipt) != 3 || len(object(receipt["metadata"])) != 2 {
					t.Fatal("response contains more than a receipt")
				}
			}
			managementTestNoDisclosure(t, h)
		})
	}
}

func TestManagementApplyRejectsWholeInvalidBatch(t *testing.T) {
	for _, suffix := range []string{"secret", "syntax"} {
		t.Run(suffix, func(t *testing.T) {
			directory := t.TempDir()
			managementTestWrite(t, directory, "a.yaml", managementTestModel("model"))
			if suffix == "secret" {
				managementTestWrite(t, directory, "z.yaml", managementTestSecret("api-key"))
			} else {
				managementTestWrite(t, directory, "z.yaml", "invalid: ["+managementTestToken)
			}
			h := managementTestContext("file", directory)
			managementTestError(t, h, "", "apply")
			if h.connections != 0 {
				t.Fatal("invalid later document connected")
			}
		})
	}
	path := managementTestBatch(t, managementTestModel("model"))
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\n---\ninvalid: [" + managementTestToken)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	h := managementTestContext("file", path)
	managementTestError(t, h, "Cannot parse apply input", "apply")
	if h.connections != 0 {
		t.Fatal("syntax error after valid document connected")
	}
}

func TestManagementApplyManifestValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(Object)
		message string
	}{
		{"namespace", func(o Object) { object(o["metadata"])["namespace"] = "other" }, "namespace differs"},
		{"api", func(o Object) { o["apiVersion"] = "other/v1" }, "only airunway"},
		{"kind", func(o Object) { o["kind"] = "Job" }, "only airunway"},
		{"name", func(o Object) { object(o["metadata"])["name"] = "../bad" }, "resource name"},
		{"status", func(o Object) { o["status"] = Object{} }, "desired state only"},
		{"last-applied", func(o Object) {
			object(o["metadata"])["annotations"] = Object{"kubectl.kubernetes.io/last-applied-configuration": "{}"}
		}, "last-applied"},
		{"labels", func(o Object) { object(o["metadata"])["labels"] = Object{"value": true} }, "string maps"},
		{"annotations", func(o Object) { object(o["metadata"])["annotations"] = []any{} }, "string maps"},
		{"spec", func(o Object) { o["spec"] = []any{} }, "spec objects"},
		{"metadata", func(o Object) { o["metadata"] = nil }, "metadata and spec"},
	}
	for _, key := range []string{"resourceVersion", "ownerReferences", "uid", "managedFields", "generateName"} {
		cases = append(cases, struct {
			name    string
			mutate  func(Object)
			message string
		}{key, func(o Object) { object(o["metadata"])[key] = "value" }, "server-owned metadata"})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			model := managementTestModel("model")
			test.mutate(model)
			h := managementTestContext("file", managementTestBatch(t, model))
			managementTestError(t, h, test.message, "apply")
			if h.connections != 0 {
				t.Fatal("invalid manifest acquired client")
			}
		})
	}
	path := managementTestWrite(t, t.TempDir(), "array.json", []any{managementTestModel("model")})
	h := managementTestContext("file", path)
	managementTestError(t, h, "only airunway", "apply")
	if h.connections != 0 {
		t.Fatal("array acquired client")
	}
}

func TestManagementApplyInlineCredentialGuard(t *testing.T) {
	// Construct fake userinfo without embedding a credential-shaped URL in source.
	credentialURL := &url.URL{Scheme: "https", Host: "example.test", User: url.UserPassword("fixture-user", "fixture-password")}
	for _, config := range []Object{{"apiKey": managementTestToken}, {"token": managementTestToken}, {"secretAccessKey": managementTestToken}, {"url": credentialURL.String()}, {"url": "https://example.test/model?sig=example"}, {"password": "pass"}, {"nested": Object{"authorization": "Bearer opaque"}}, {"env": []any{Object{"name": "HF_TOKEN", "value": "arbitrary"}}}, {"message": "-----BEGIN PRIVATE KEY-----"}} {
		agent := managementTestAgent("agent", "deployment")
		object(agent["spec"])["config"] = config
		h := managementTestContext("file", managementTestBatch(t, agent))
		managementTestError(t, h, "Inline credential", "apply")
		if h.connections != 0 {
			t.Fatal("inline secret connected")
		}
	}
	model := managementTestModel("model")
	object(model["spec"])["secrets"] = Object{"huggingFaceToken": "token"}
	object(model["spec"])["env"] = []any{Object{"name": "HF_TOKEN", "valueFrom": Object{"secretKeyRef": Object{"name": "token", "key": "HF_TOKEN"}}}}
	object(get(model, "spec", "model"))["artifact"] = Object{"credentialsRef": Object{"name": "artifact", "key": "credentials"}}
	h := managementTestContext("file", managementTestBatch(t, model), "dry-run", "client")
	managementTestRun(t, h, "apply")
	managementTestJSON(t, get(array(managementTestOutput(t, h))[0], "spec"), model["spec"])
}

func TestManagementApplyYAMLSafetyAndCoreSchema(t *testing.T) {
	for _, data := range []string{"x: &x [*x]", `{"__proto__":{"polluted":true}}`, "x: 1\nx: 2", "x: &x {a: 1}\ny:\n  <<: *x", "x: .nan", "x: !!binary aGVsbG8=", "x: !!timestamp 2026-01-01", "x: !custom [1]", "x: !custom {a: 1}"} {
		h := managementTestContext("file", managementTestWrite(t, t.TempDir(), "unsafe.yaml", data))
		managementTestError(t, h, "", "apply")
		if h.connections != 0 {
			t.Fatal("unsafe YAML connected")
		}
	}
	model := managementTestModel("model")
	data, _ := marshalYAML(model)
	raw := string(data) + "  futureField:\n    date: 2026-01-01\n    answer: yes\n    enabled: true\n"
	h := managementTestContext("file", managementTestWrite(t, t.TempDir(), "core.yaml", raw), "dry-run", "client")
	managementTestRun(t, h, "apply")
	fields := get(array(managementTestOutput(t, h))[0], "spec", "futureField")
	managementTestJSON(t, fields, Object{"date": "2026-01-01", "answer": "yes", "enabled": true})
	for _, doc := range []string{strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66), "[" + strings.Repeat("0,", 50001) + "0]"} {
		h := managementTestContext("file", managementTestWrite(t, t.TempDir(), "complex.yaml", doc))
		managementTestError(t, h, "complex", "apply")
		if h.connections != 0 {
			t.Fatal("complex YAML connected")
		}
	}
}

func TestManagementApplyPathsAndBounds(t *testing.T) {
	input := managementTestBatch(t, managementTestModel("model"))
	link := filepath.Join(t.TempDir(), "link.yaml")
	if err := os.Symlink(input, link); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ path, message string }{{link, "symlink"}, {managementTestWrite(t, t.TempDir(), "input.txt", "x"), "only .yaml"}, {t.TempDir(), "between 1 and 100"}, {managementTestWrite(t, t.TempDir(), "empty.yaml", "---\n# empty\n"), "No deployment documents"}, {managementTestWrite(t, t.TempDir(), "big.json", strings.Repeat("x", maxInput+1)), "4 MiB"}} {
		h := managementTestContext("file", test.path)
		managementTestError(t, h, test.message, "apply")
		if h.connections != 0 {
			t.Fatal("invalid input path connected")
		}
	}
	h := managementTestContext("file", managementTestBatch(t, managementTestModel("model"), managementTestModel("model")))
	managementTestError(t, h, "duplicate resources", "apply")
	if h.connections != 0 {
		t.Fatal("duplicate connected")
	}
	directory := t.TempDir()
	for i := 0; i < 101; i++ {
		managementTestWrite(t, directory, fmt.Sprintf("%03d.yaml", i), "---")
	}
	h = managementTestContext("file", directory)
	managementTestError(t, h, "between 1 and 100", "apply")
	docs := []Object{}
	for i := 0; i < 201; i++ {
		docs = append(docs, managementTestModel(fmt.Sprintf("model-%d", i)))
	}
	h = managementTestContext("file", managementTestBatch(t, docs...))
	managementTestError(t, h, "200 documents", "apply")
	if h.connections != 0 {
		t.Fatal("document overflow connected")
	}
	directory = t.TempDir()
	managementTestWrite(t, directory, "a.yaml", "#"+strings.Repeat("x", maxInput/2))
	managementTestWrite(t, directory, "b.yaml", "#"+strings.Repeat("x", maxInput/2))
	h = managementTestContext("file", directory)
	managementTestError(t, h, "4 MiB", "apply")
	if h.connections != 0 {
		t.Fatal("batch byte overflow connected")
	}
}

func TestManagementApplyAgentPreflight(t *testing.T) {
	for _, test := range []struct{ existing, desired, dry string }{{"job", "deployment", ""}, {"deployment", "job", ""}, {"job", "job", "server"}} {
		h := managementTestContext("file", managementTestBatch(t, managementTestModel("first"), managementTestAgent("agent", test.desired)))
		if test.dry != "" {
			h.ctx.Flags["dry-run"] = []string{test.dry}
		}
		h.client.resources = []Object{managementTestAgent("agent", test.existing)}
		managementTestError(t, h, "one-shot", "apply")
		if len(h.client.writes()) != 0 {
			t.Fatal("wrote before job preflight completed")
		}
	}
	h := managementTestContext("file", managementTestBatch(t, managementTestAgent("agent", "deployment")))
	item := managementTestAgent("agent", "deployment")
	object(item["metadata"])["resourceVersion"] = "17"
	h.client.resources = []Object{item}
	managementTestRun(t, h, "apply")
	if stringAt(h.client.writes()[0].body, "metadata", "resourceVersion") != "17" {
		t.Fatal("missing agent version precondition")
	}
	h = managementTestContext("file", managementTestBatch(t, managementTestAgent("agent", "deployment")))
	h.client.resources = []Object{managementTestAgent("agent", "deployment")}
	managementTestError(t, h, "resourceVersion", "apply")
	if len(h.client.writes()) != 0 {
		t.Fatal("applied agent without version")
	}
	h = managementTestContext("file", managementTestBatch(t, managementTestAgent("new-job", "job")))
	managementTestRun(t, h, "apply")
	if len(h.client.writes()) != 1 || h.client.writes()[0].op != "PATCH" {
		t.Fatal("new one-shot did not use SSA")
	}
	h = managementTestContext("file", managementTestBatch(t, managementTestAgent("agent", "deployment")))
	h.client.failure = func(c managementCall) error {
		if c.op == "get" {
			return cliError(3, "HTTP_403", "Forbidden")
		}
		return nil
	}
	managementTestCode(t, managementTestError(t, h, "", "apply"), "HTTP_403")
	if len(h.client.writes()) != 0 {
		t.Fatal("treated forbidden as missing")
	}
}

func TestManagementApplyPartialAndConflictFailures(t *testing.T) {
	for _, dry := range []string{"", "server"} {
		h := managementTestContext("file", managementTestBatch(t, managementTestModel("first"), managementTestModel("second")))
		if dry != "" {
			h.ctx.Flags["dry-run"] = []string{dry}
		}
		h.client.failure = func(c managementCall) error {
			if c.op == "PATCH" && strings.HasSuffix(c.path, "/second") {
				return cliError(3, "HTTP_403", "Forbidden")
			}
			return nil
		}
		managementTestCode(t, managementTestError(t, h, "", "apply"), "HTTP_403")
		managementTestJSON(t, managementTestOutput(t, h), []any{Object{"apiVersion": "airunway.ai/v1alpha1", "kind": "ModelDeployment", "metadata": Object{"name": "first", "namespace": "team"}}})
		if len(h.client.writes()) != 2 {
			t.Fatal("wrong partial write count")
		}
		if dry == "server" {
			if !strings.Contains(h.stderr.String(), "No resources were persisted") || strings.Contains(h.stderr.String(), "not rolled back") {
				t.Fatal("misleading dry run failure")
			}
		} else if !strings.Contains(h.stderr.String(), "not rolled back") {
			t.Fatal("missing partial receipt warning")
		}
	}
	for _, code := range []string{"HTTP_409", "HTTP_422"} {
		h := managementTestContext("file", managementTestBatch(t, managementTestModel("model")))
		h.client.failure = func(c managementCall) error {
			if c.op == "PATCH" {
				return cliError(5, code, "Rejected")
			}
			return nil
		}
		managementTestCode(t, managementTestError(t, h, "", "apply"), code)
		if len(h.client.writes()) != 1 || h.client.writes()[0].options.Query.Get("force") != "false" {
			t.Fatal("retried or forced conflict")
		}
	}
}

func TestManagementCancellation(t *testing.T) {
	for _, words := range [][]string{{"credential", "list"}, {"credential", "create", "token"}, {"provider", "list"}, {"catalog", "agent", "list"}, {"apply"}} {
		h := managementTestContext()
		switch words[0] {
		case "apply":
			h.ctx.Flags["file"] = []string{managementTestBatch(t, managementTestModel("model"))}
		case "credential":
			if words[1] == "create" {
				h.ctx.Flags["type"] = []string{"api-key"}
				h.ctx.Flags["from-file"] = []string{"-"}
			}
		}
		ctx, cancel := context.WithCancel(h.ctx.Context)
		cancel()
		h.ctx.Context = ctx
		managementTestCode(t, managementTestError(t, h, "", words...), "INTERRUPTED")
		if len(h.client.calls) != 0 {
			t.Fatal("canceled command made cluster calls")
		}
	}
	h := managementTestContext("file", managementTestBatch(t, managementTestModel("first"), managementTestModel("second")))
	ctx, cancel := context.WithCancel(h.ctx.Context)
	defer cancel()
	h.ctx.Context = ctx
	h.client.response = func(_ managementCall, item Object) Object { cancel(); return item }
	managementTestCode(t, managementTestError(t, h, "", "apply"), "INTERRUPTED")
	if len(h.client.writes()) != 1 || !strings.Contains(h.stderr.String(), "not rolled back") || len(array(managementTestOutput(t, h))) != 1 {
		t.Fatal("canceled batch lost receipt or kept writing")
	}
}

func TestManagementApplyCoreScalarParity(t *testing.T) {
	// Expected values checked against the repository's pinned js-yaml 5.1 CORE_SCHEMA.
	cases := []struct {
		yaml string
		want any
	}{
		{"012", float64(12)}, {"0o12", float64(10)}, {"0b101", "0b101"}, {"1_000", "1_000"}, {"0xFF", float64(255)},
		{"+0o12", "+0o12"}, {"0XFF", "0XFF"}, {"1e3", json.Number("1e3")}, {"yes", "yes"}, {"2026-01-01", "2026-01-01"},
		{"!!int 0b101", float64(5)}, {"!!int +0o12", float64(10)}, {"!!str 0xFF", "0xFF"}, {"1e999", "1e999"},
	}
	for _, test := range cases {
		t.Run(test.yaml, func(t *testing.T) {
			raw := "apiVersion: airunway.ai/v1alpha1\nkind: ModelDeployment\nmetadata: {name: model}\nspec:\n  futureField: " + test.yaml + "\n"
			h := managementTestContext("file", managementTestWrite(t, t.TempDir(), "core.yaml", raw), "dry-run", "client")
			managementTestRun(t, h, "apply")
			managementTestJSON(t, get(array(managementTestOutput(t, h))[0], "spec", "futureField"), test.want)
		})
	}
	for _, value := range []string{"!!bool yes", "!!int 1_000", "!custom [1]", "!custom {a: 1}"} {
		raw := "apiVersion: airunway.ai/v1alpha1\nkind: ModelDeployment\nmetadata: {name: model}\nspec:\n  futureField: " + value + "\n"
		h := managementTestContext("file", managementTestWrite(t, t.TempDir(), "tag.yaml", raw))
		managementTestError(t, h, "JSON-compatible", "apply")
		if h.connections != 0 {
			t.Fatal("unsupported tag reached client")
		}
	}
}

func TestManagementApplyPartialFailureJSONThroughRun(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	for _, dry := range []string{"", "server"} {
		t.Run("dry="+dry, func(t *testing.T) {
			file := managementTestBatch(t, managementTestModel("first"), managementTestModel("second"))
			h := managementTestContext()
			h.client.failure = func(c managementCall) error {
				if c.op == "PATCH" && strings.HasSuffix(c.path, "/second") {
					return cliError(3, "HTTP_403", "Forbidden")
				}
				return nil
			}
			args := []string{"apply", "--file", file, "--namespace", "team", "--output=json"}
			if dry != "" {
				args = append(args, "--dry-run", dry)
			}
			code := Run(context.Background(), args, RunOptions{
				IO: h.ctx.IO, Client: h.client, Config: &CLIConfig{Version: 1, Contexts: map[string]*ContextDefaults{}},
			})
			if code != 3 {
				t.Fatalf("code=%d stderr=%s", code, h.stderr.String())
			}
			var receipts []Object
			if decodeJSON(h.out.Bytes(), &receipts) != nil || len(receipts) != 1 || stringAt(receipts[0], "metadata", "name") != "first" {
				t.Fatalf("invalid partial receipt: %s", h.out.String())
			}
			lines := strings.Split(strings.TrimSpace(h.stderr.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("expected progress and error records: %s", h.stderr.String())
			}
			for i, line := range lines {
				var record Object
				if decodeJSON([]byte(line), &record) != nil {
					t.Fatalf("stderr is not JSON-lines: %s", line)
				}
				if i == 0 {
					message := stringAt(record, "progress", "message")
					expected := "not rolled back"
					if dry == "server" {
						expected = "No resources were persisted"
					}
					if !strings.Contains(message, expected) {
						t.Fatalf("missing partial failure progress: %s", line)
					}
				} else if stringAt(record, "error", "code") != "HTTP_403" {
					t.Fatalf("missing error: %s", line)
				}
			}
		})
	}
}
