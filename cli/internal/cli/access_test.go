package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/httpstream"
	streamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
)

type accessFakeCall struct {
	Method                string
	Type                  ResourceType
	Namespace, Name, Path string
	Options               RequestOptions
}
type accessFakeClient struct {
	mu        sync.Mutex
	resources []Object
	calls     []accessFakeCall
	config    *rest.Config
	onGet     func(context.Context, ResourceType, string, string) (Object, error)
	onList    func(context.Context, ResourceType, string, url.Values) error
	onRaw     func(context.Context, string, string, any, RequestOptions) (*http.Response, error)
	onRequest func(context.Context, string, string, any, RequestOptions) (Object, error)
}

func (c *accessFakeClient) record(call accessFakeCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}
func (c *accessFakeClient) snapshot() []accessFakeCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]accessFakeCall(nil), c.calls...)
}
func (c *accessFakeClient) Get(ctx context.Context, typ ResourceType, ns, name string) (Object, error) {
	c.record(accessFakeCall{Method: "get", Type: typ, Namespace: ns, Name: name})
	if c.onGet != nil {
		return c.onGet(ctx, typ, ns, name)
	}
	for _, r := range c.resources {
		if stringAt(r, "kind") == typ.Kind && stringAt(r, "metadata", "name") == name && (!typ.Namespaced || accessNamespace(r, "test") == ns) {
			return cloneObject(r), nil
		}
	}
	return nil, cliError(1, "HTTP_404", "not found")
}
func (c *accessFakeClient) List(ctx context.Context, typ ResourceType, ns string, q url.Values) ([]Object, error) {
	c.record(accessFakeCall{Method: "list", Type: typ, Namespace: ns, Options: RequestOptions{Query: q}})
	if c.onList != nil {
		if err := c.onList(ctx, typ, ns, q); err != nil {
			return nil, err
		}
	}
	out := []Object{}
	for _, r := range c.resources {
		if stringAt(r, "kind") == typ.Kind && (!typ.Namespaced || accessNamespace(r, "test") == ns) {
			out = append(out, cloneObject(r))
		}
	}
	return out, nil
}
func (c *accessFakeClient) Raw(ctx context.Context, method, path string, body any, opts RequestOptions) (*http.Response, error) {
	c.record(accessFakeCall{Method: method, Path: path, Options: opts})
	if c.onRaw != nil {
		return c.onRaw(ctx, method, path, body, opts)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("line one\nline two\n"))}, nil
}
func (c *accessFakeClient) Request(ctx context.Context, method, path string, body any, opts RequestOptions) (Object, error) {
	c.record(accessFakeCall{Method: method, Path: path, Options: opts})
	if c.onRequest != nil {
		return c.onRequest(ctx, method, path, body, opts)
	}
	return nil, fmt.Errorf("unexpected request %s", path)
}
func (c *accessFakeClient) RESTConfig() *rest.Config { return c.config }
func (c *accessFakeClient) Create(context.Context, Object, bool) (Object, error) {
	panic("unexpected write")
}
func (c *accessFakeClient) Patch(context.Context, ResourceType, string, string, any, bool) (Object, error) {
	panic("unexpected write")
}
func (c *accessFakeClient) Delete(context.Context, ResourceType, string, string, string) error {
	panic("unexpected write")
}
func accessTestObjects(v ...Object) []any {
	out := make([]any, len(v))
	for i, item := range v {
		out[i] = item
	}
	return out
}
func accessTestResource(kind, name string) Object {
	return Object{"apiVersion": "v1", "kind": kind, "metadata": Object{"name": name, "namespace": "test", "uid": name + "-uid", "generation": 2}, "spec": Object{}, "status": Object{}}
}
func accessTestModel() Object {
	r := accessTestResource("ModelDeployment", "llama")
	r["apiVersion"] = "airunway.ai/v1alpha1"
	r["spec"] = Object{"model": Object{"id": "repository/not-served", "servedName": "configured-name"}}
	r["status"] = Object{"phase": "Running", "observedGeneration": 2, "conditions": accessTestObjects(Object{"type": "Ready", "status": "True", "observedGeneration": 2}), "endpoint": Object{"service": "actual-api", "port": 8000}}
	return r
}
func accessTestOwner(r Object) Object {
	return Object{"apiVersion": r["apiVersion"], "kind": r["kind"], "name": get(r, "metadata", "name"), "uid": get(r, "metadata", "uid"), "controller": true}
}
func accessTestOwn(child, owner Object) {
	object(child["metadata"])["ownerReferences"] = accessTestObjects(accessTestOwner(owner))
}
func accessTestService(name string, port int) Object {
	r := accessTestResource("Service", name)
	r["spec"] = Object{"selector": Object{"workload": name}, "ports": accessTestObjects(Object{"name": "http", "port": port, "targetPort": "api"})}
	return r
}
func accessTestPod(service, owner Object) Object {
	r := accessTestResource("Pod", "selected-pod")
	object(r["metadata"])["labels"] = cloneObject(object(get(service, "spec", "selector")))
	r["spec"] = Object{"containers": accessTestObjects(Object{"name": "main", "ports": accessTestObjects(Object{"name": "api", "containerPort": 8080})})}
	r["status"] = Object{"phase": "Running", "conditions": accessTestObjects(Object{"type": "Ready", "status": "True"})}
	if owner != nil {
		accessTestOwn(r, owner)
	}
	return r
}
func accessTestAgent() (Object, Object, Object, Object, Object) {
	r := accessTestResource("AgentDeployment", "helper")
	r["apiVersion"] = "airunway.ai/v1alpha1"
	r["spec"] = Object{"model": Object{"credential": Object{"name": "never-read-model-key"}}}
	r["status"] = Object{"phase": "Running", "observedGeneration": 2, "conditions": accessTestObjects(Object{"type": "Ready", "status": "True", "observedGeneration": 2}), "runtime": Object{"address": "http://actual-agent-api.test.svc", "workloadRef": Object{"apiVersion": "apps/v1", "kind": "Deployment", "name": "agent-workload", "namespace": "test"}, "authSecretRef": Object{"name": "ingress-key", "key": "token"}}, "modelBinding": Object{"auth": Object{"secretRef": Object{"name": "never-read-model-key", "key": "token"}}}}
	root := accessTestResource("Deployment", "agent-workload")
	root["apiVersion"] = "apps/v1"
	accessTestOwn(root, r)
	svc := accessTestService("actual-agent-api", 80)
	accessTestOwn(svc, r)
	pod := accessTestPod(svc, root)
	secret := accessTestResource("Secret", "ingress-key")
	secret["data"] = Object{"token": base64.StdEncoding.EncodeToString([]byte("private-ingress-token"))}
	accessTestOwn(secret, r)
	return r, svc, pod, root, secret
}
func accessTestContext(client *accessFakeClient, flags Flags) (*CommandContext, *bytes.Buffer, *bytes.Buffer) {
	out, errout := &bytes.Buffer{}, &bytes.Buffer{}
	base := Flags{"output": {"json"}, "timeout": {"2s"}}
	for k, v := range flags {
		base[k] = v
	}
	return &CommandContext{Context: context.Background(), Flags: base, IO: &IO{In: strings.NewReader(""), Out: out, Err: errout}, Namespace: "test", Client: func() (ClusterClient, error) { return client, nil }}, out, errout
}
func accessTestCode(t *testing.T, err error, code string) {
	t.Helper()
	if code == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if !accessHasCode(err, code) {
		t.Fatalf("got %v, want error code %s", err, code)
	}
}
func accessTestJSON(t *testing.T, out *bytes.Buffer) Object {
	t.Helper()
	var result Object
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("invalid output %q: %v", out.String(), err)
	}
	return result
}

func TestAccessWaitFreshness(t *testing.T) {
	cases := []struct {
		name               string
		change             func(Object)
		noun, target, code string
	}{
		{"ready", func(Object) {}, "model", "ready", ""},
		{"stale status", func(r Object) { object(r["status"])["observedGeneration"] = 1 }, "model", "ready", "TIMEOUT"},
		{"stale ready", func(r Object) { objects(get(r, "status", "conditions"))[0]["observedGeneration"] = 1 }, "model", "ready", "TIMEOUT"},
		{"missing generation", func(r Object) { delete(object(r["metadata"]), "generation") }, "model", "ready", "TIMEOUT"},
		{"phase is not readiness", func(r Object) { object(r["status"])["conditions"] = []any{} }, "model", "ready", "TIMEOUT"},
		{"stale failure", func(r Object) {
			object(r["status"])["phase"] = "Failed"
			object(r["status"])["conditions"] = accessTestObjects(Object{"type": "Ready", "status": "False", "observedGeneration": 1}, Object{"type": "Validated", "status": "True", "observedGeneration": 2})
		}, "model", "ready", "TIMEOUT"},
		{"current validation failure", func(r Object) {
			object(r["status"])["phase"] = "Failed"
			object(r["status"])["conditions"] = accessTestObjects(Object{"type": "Validated", "status": "False", "observedGeneration": 2})
		}, "model", "ready", "FAILED"},
		{"old agent provider failure", func(r Object) {
			object(r["status"])["phase"] = "Failed"
			object(r["status"])["conditions"] = accessTestObjects(Object{"type": "Ready", "status": "False", "observedGeneration": 2}, Object{"type": "ProviderReady", "status": "False", "observedGeneration": 1})
		}, "agent", "ready", "TIMEOUT"},
		{"current agent failure", func(r Object) {
			object(r["status"])["phase"] = "Failed"
			object(r["status"])["conditions"] = accessTestObjects(Object{"type": "ProviderReady", "status": "False", "observedGeneration": 2})
		}, "agent", "ready", "FAILED"},
		{"job completed", func(r Object) {
			object(r["status"])["phase"] = "Completed"
			object(r["status"])["conditions"] = accessTestObjects(Object{"type": "ProviderReady", "status": "True", "reason": "JobCompleted", "observedGeneration": 2})
		}, "agent", "completed", ""},
		{"job completed counts as ready", func(r Object) {
			object(r["status"])["phase"] = "Completed"
			object(r["status"])["conditions"] = accessTestObjects(Object{"type": "Completed", "status": "True", "observedGeneration": 2})
		}, "agent", "ready", ""},
		{"ready is not completed", func(Object) {}, "agent", "completed", "TIMEOUT"},
		{"job failed", func(r Object) {
			object(r["status"])["conditions"] = accessTestObjects(Object{"type": "ProviderReady", "status": "False", "reason": "JobFailed", "observedGeneration": 2})
		}, "agent", "completed", "FAILED"},
		{"model cannot complete", func(Object) {}, "model", "completed", "USAGE"},
		{"unknown condition", func(Object) {}, "model", "healthy", "USAGE"},
		{"deleting", func(r Object) { object(r["metadata"])["deletionTimestamp"] = "2026-01-01T00:00:00Z" }, "model", "ready", "DELETED"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := accessTestModel()
			tt.change(r)
			client := &accessFakeClient{resources: []Object{r}}
			_, err := waitForResource(context.Background(), client, tt.noun, r, Flags{"for": {tt.target}, "timeout": {"5ms"}}, nil)
			accessTestCode(t, err, tt.code)
		})
	}
	_, err := waitForResource(context.Background(), &accessFakeClient{}, "model", accessTestModel(), Flags{"timeout": {"forever"}}, nil)
	accessTestCode(t, err, "USAGE")
}
func TestAccessWaitPollingAndCancellation(t *testing.T) {
	t.Run("namespace and newest generation", func(t *testing.T) {
		r := accessTestModel()
		object(r["metadata"])["namespace"] = "other"
		object(r["status"])["conditions"] = []any{}
		current := accessTestModel()
		object(current["metadata"])["namespace"] = "other"
		object(current["metadata"])["generation"] = 3
		object(current["status"])["observedGeneration"] = 3
		objects(get(current, "status", "conditions"))[0]["observedGeneration"] = 3
		c := &accessFakeClient{resources: []Object{current}}
		got, err := waitForResource(context.Background(), c, "model", r, Flags{"timeout": {"1s"}}, nil)
		accessTestCode(t, err, "")
		if intAt(got, "metadata", "generation") != 3 || c.snapshot()[0].Namespace != "other" {
			t.Fatal(got, c.snapshot())
		}
	})
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint("deleted or replaced ", replace), func(t *testing.T) {
			r := accessTestModel()
			object(r["status"])["conditions"] = []any{}
			c := &accessFakeClient{}
			if replace {
				next := accessTestModel()
				object(next["metadata"])["uid"] = "replaced"
				c.resources = []Object{next}
			}
			_, err := waitForResource(context.Background(), c, "model", r, Flags{"timeout": {"1s"}}, nil)
			accessTestCode(t, err, "DELETED")
		})
	}
	t.Run("uncooperative get", func(t *testing.T) {
		r := accessTestModel()
		object(r["status"])["conditions"] = []any{}
		release := make(chan struct{})
		defer close(release)
		c := &accessFakeClient{onGet: func(context.Context, ResourceType, string, string) (Object, error) { <-release; return nil, nil }}
		start := time.Now()
		_, err := waitForResource(context.Background(), c, "model", r, Flags{"timeout": {"275ms"}}, nil)
		accessTestCode(t, err, "TIMEOUT")
		if time.Since(start) > time.Second {
			t.Fatal("timeout not bounded")
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := waitForResource(ctx, &accessFakeClient{}, "model", accessTestModel(), Flags{}, nil)
	accessTestCode(t, err, "CANCELED")
}
func accessTestGateway() (Object, Object, Object, Object) {
	model := accessTestModel()
	object(model["status"])["gateway"] = Object{"gatewayName": "shared", "gatewayNamespace": "edge", "modelName": "served-alias"}
	gateway := accessTestResource("Gateway", "shared")
	gateway["apiVersion"] = "gateway.networking.k8s.io/v1"
	object(gateway["metadata"])["namespace"] = "edge"
	gateway["spec"] = Object{"listeners": accessTestObjects(Object{"name": "secure", "protocol": "HTTPS", "port": 8443})}
	gateway["status"] = Object{"addresses": accessTestObjects(Object{"type": "IPAddress", "value": "203.0.113.3"})}
	route := accessTestResource("HTTPRoute", "route")
	route["apiVersion"] = "gateway.networking.k8s.io/v1"
	accessTestOwn(route, model)
	route["spec"] = Object{"parentRefs": accessTestObjects(Object{"name": "shared", "namespace": "edge", "sectionName": "secure"}), "hostnames": []any{"inference.example.test"}, "rules": accessTestObjects(Object{"matches": accessTestObjects(Object{"path": Object{"type": "PathPrefix", "value": "/models"}, "headers": accessTestObjects(Object{"name": "x-gateway-model-name", "value": "served-alias"})})})}
	service := accessTestService("implementation-generated-42", 8443)
	object(service["metadata"])["namespace"] = "edge"
	object(service["metadata"])["labels"] = Object{"gateway.envoyproxy.io/owning-gateway-name": "shared", "gateway.envoyproxy.io/owning-gateway-namespace": "edge"}
	return model, gateway, route, service
}
func TestAccessEndpointDiscovery(t *testing.T) {
	t.Run("actual service port", func(t *testing.T) {
		model := accessTestModel()
		service := accessTestService("actual-api", 80)
		objects(get(service, "spec", "ports"))[0]["targetPort"] = 8000
		c, out, _ := accessTestContext(&accessFakeClient{resources: []Object{model, service}}, nil)
		accessTestCode(t, runAccess("model", "endpoint", "llama", c), "")
		r := accessTestJSON(t, out)
		if r["url"] != "http://actual-api.test.svc/" || intAt(r, "service", "port") != 80 {
			t.Fatal(r)
		}
	})
	t.Run("no guessed service", func(t *testing.T) {
		model := accessTestModel()
		delete(object(model["status"]), "endpoint")
		client := &accessFakeClient{resources: []Object{model}}
		c, _, _ := accessTestContext(client, nil)
		accessTestCode(t, runAccess("model", "endpoint", "llama", c), "UNSUPPORTED")
		if len(client.snapshot()) != 1 {
			t.Fatal(client.snapshot())
		}
	})
	t.Run("gateway route details", func(t *testing.T) {
		m, g, r, s := accessTestGateway()
		c, out, _ := accessTestContext(&accessFakeClient{resources: []Object{m, g, r, s}}, nil)
		accessTestCode(t, runAccess("model", "endpoint", "llama", c), "")
		result := accessTestJSON(t, out)
		if result["url"] != "https://203.0.113.3:8443/models" || stringAt(result, "headers", "host") != "inference.example.test" || stringAt(result, "service", "name") != "implementation-generated-42" || result["servedModelName"] != "served-alias" {
			t.Fatal(result)
		}
	})
	t.Run("gateway service fallback", func(t *testing.T) {
		m, g, r, s := accessTestGateway()
		g["status"] = Object{}
		client := &accessFakeClient{resources: []Object{m, g, r, s}}
		c, out, _ := accessTestContext(client, nil)
		accessTestCode(t, runAccess("model", "endpoint", "llama", c), "")
		if accessTestJSON(t, out)["url"] != "https://implementation-generated-42.edge.svc:8443/models" {
			t.Fatal(out.String())
		}
		delete(object(s["metadata"]), "labels")
		accessTestCode(t, runAccess("model", "endpoint", "llama", c), "UNSUPPORTED")
	})
	for _, scenario := range []string{"missing route", "foreign route", "regex", "credential header", "wildcard", "multiple listeners", "wrong parent", "wrong owner namespace"} {
		t.Run(scenario, func(t *testing.T) {
			m, g, r, s := accessTestGateway()
			rule := objects(get(r, "spec", "rules"))[0]
			match := objects(rule["matches"])[0]
			switch scenario {
			case "missing route":
				r["kind"] = "Unused"
			case "foreign route":
				delete(object(r["metadata"]), "ownerReferences")
			case "regex":
				objects(match["headers"])[0]["type"] = "RegularExpression"
			case "credential header":
				objects(match["headers"])[0]["name"] = "Authorization"
			case "wildcard":
				object(r["spec"])["hostnames"] = []any{"*.example.test"}
			case "multiple listeners":
				delete(objects(get(r, "spec", "parentRefs"))[0], "sectionName")
				object(g["spec"])["listeners"] = append(array(get(g, "spec", "listeners")), Object{"name": "other", "protocol": "HTTP", "port": 80})
			case "wrong parent":
				objects(get(r, "spec", "parentRefs"))[0]["namespace"] = "elsewhere"
			case "wrong owner namespace":
				g["status"] = Object{}
				object(get(s, "metadata", "labels"))["gateway.envoyproxy.io/owning-gateway-namespace"] = "elsewhere"
			}
			c, _, _ := accessTestContext(&accessFakeClient{resources: []Object{m, g, r, s}}, nil)
			accessTestCode(t, runAccess("model", "endpoint", "llama", c), "UNSUPPORTED")
		})
	}
	t.Run("agent reference only", func(t *testing.T) {
		a, s, p, w, secret := accessTestAgent()
		client := &accessFakeClient{resources: []Object{a, s, p, w, secret}}
		c, out, _ := accessTestContext(client, nil)
		accessTestCode(t, runAccess("agent", "endpoint", "helper", c), "")
		result := accessTestJSON(t, out)
		if result["authRequired"] != true || stringAt(result, "authSecretRef", "name") != "ingress-key" {
			t.Fatal(result)
		}
		for _, call := range client.snapshot() {
			if call.Type.Kind == "Secret" {
				t.Fatal("read secret for endpoint")
			}
		}
		if strings.Contains(out.String(), "private-ingress-token") {
			t.Fatal("token leaked")
		}
	})
	t.Run("jobs and unsupported runtime", func(t *testing.T) {
		a, _, _, _, _ := accessTestAgent()
		c, _, _ := accessTestContext(&accessFakeClient{resources: []Object{a}}, nil)
		object(a["spec"])["lifecycle"] = "job"
		accessTestCode(t, runAccess("agent", "endpoint", "helper", c), "UNSUPPORTED")
		delete(object(a["spec"]), "lifecycle")
		delete(object(get(a, "status", "runtime")), "address")
		err := runAccess("agent", "endpoint", "helper", c)
		if err == nil || !strings.Contains(err.Error(), "upstream operator") {
			t.Fatal(err)
		}
	})
}
func TestAccessSafeURLsAndPortSelection(t *testing.T) {
	// Construct fake userinfo without embedding a credential-shaped URL in source.
	credentialURL := &url.URL{Scheme: "http", Host: "example.test", User: url.UserPassword("fixture-user", "fixture-password")}
	for _, input := range []string{"file:///etc/passwd", "//example.test", credentialURL.String(), "http://example.test/?token=x", "http://example.test/?", "http://example.test/#", "http://example.test:99999", "http://example.test\\evil", "http://[fe80::1%25zone]/"} {
		t.Run(input, func(t *testing.T) { _, err := accessSafeURL(input); accessTestCode(t, err, "UNSUPPORTED") })
	}
	svc := accessTestService("actual-api", 8000)
	object(svc["spec"])["ports"] = append(array(get(svc, "spec", "ports")), Object{"port": 9000})
	_, err := accessServicePort(svc, 0)
	accessTestCode(t, err, "UNSUPPORTED")
	for _, input := range []string{"https://127.0.0.1", "https://[::1]"} {
		a, _, _, _, _ := accessTestAgent()
		object(get(a, "status", "runtime"))["address"] = input
		_, err := accessResolveEndpoint(context.Background(), &accessFakeClient{}, "agent", a, nil, "test", false)
		accessTestCode(t, err, "UNSUPPORTED")
	}
	u, err := accessAddressURL("2001:db8::1", "http", 8000)
	accessTestCode(t, err, "")
	if u.String() != "http://[2001:db8::1]:8000/" {
		t.Fatal(u)
	}
}

// Serve the real SPDY pod port-forward protocol, relaying data streams to an
// HTTP fixture. No kubectl, cluster, or replacement global tunnel hook is used.
func accessTestForwardServer(t *testing.T, upstream string) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	closed := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/test/pods/selected-pod/portforward" || r.Method != "POST" {
			http.Error(w, "wrong pod", 400)
			return
		}
		if _, err := httpstream.Handshake(r, w, []string{portforward.PortForwardProtocolV1Name}); err != nil {
			return
		}
		conn := streamspdy.NewResponseUpgrader().UpgradeResponse(w, r, func(stream httpstream.Stream, replySent <-chan struct{}) error {
			go func() {
				<-replySent
				if stream.Headers().Get("streamType") == "error" {
					return
				}
				remote, err := net.Dial("tcp", upstream)
				if err != nil {
					stream.Close()
					return
				}
				defer remote.Close()
				defer stream.Close()
				done := make(chan struct{})
				go func() { _, _ = io.Copy(remote, stream); remote.Close(); close(done) }()
				_, _ = io.Copy(stream, remote)
				stream.Close()
				<-done
			}()
			return nil
		})
		if conn == nil {
			return
		}
		defer conn.Close()
		<-conn.CloseChan()
		once.Do(func() { close(closed) })
	}))
	t.Cleanup(server.Close)
	return server, closed
}
func accessTestUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}
func accessTestReply(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func accessTestSetupForward(t *testing.T, client *accessFakeClient, upstream *httptest.Server) <-chan struct{} {
	t.Helper()
	server, closed := accessTestForwardServer(t, strings.TrimPrefix(upstream.URL, "http://"))
	client.config = &rest.Config{Host: server.URL, BearerToken: "cluster-token"}
	return closed
}
func TestAccessChatOverRealPortForward(t *testing.T) {
	a, s, p, w, secret := accessTestAgent()
	client := &accessFakeClient{resources: []Object{a, s, p, w, secret}}
	var mu sync.Mutex
	var paths []string
	upstream := accessTestUpstream(t, func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		paths = append(paths, request.URL.Path)
		mu.Unlock()
		if request.Header.Get("Authorization") != "Bearer private-ingress-token" {
			t.Error("missing ingress auth", request.Header.Get("Authorization"))
		}
		if request.URL.Path == "/v1/models" {
			accessTestReply(response, Object{"data": accessTestObjects(Object{"id": "agent-served-id"})})
			return
		}
		var body Object
		_ = json.NewDecoder(request.Body).Decode(&body)
		if body["model"] != "agent-served-id" || body["stream"] != false {
			t.Error(body)
		}
		accessTestReply(response, Object{"private-ingress-token": Object{"nested": []any{"private-ingress-token"}}, "choices": accessTestObjects(Object{"message": Object{"content": "answer private-ingress-token"}})})
	})
	closed := accessTestSetupForward(t, client, upstream)
	c, out, errout := accessTestContext(client, Flags{"message": {"hello"}})
	accessTestCode(t, runAccess("agent", "chat", "helper", c), "")
	if strings.Contains(out.String()+errout.String(), "private-ingress-token") || !strings.Contains(out.String(), "[redacted]") {
		t.Fatal(out.String(), errout.String())
	}
	for _, call := range client.snapshot() {
		if call.Type.Kind == "Secret" && call.Name != "ingress-key" {
			t.Fatal(call)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(paths, []string{"/v1/models", "/v1/chat/completions"}) {
		t.Fatal(paths)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("tunnel was not closed")
	}
}
func TestAccessModelChatNamesAndHistory(t *testing.T) {
	for _, scenario := range []string{"declared", "discover", "provider opts out", "raw invented fields", "gateway alias"} {
		t.Run(scenario, func(t *testing.T) {
			m := accessTestModel()
			svc := accessTestService("actual-api", 8000)
			pod := accessTestPod(svc, nil)
			client := &accessFakeClient{resources: []Object{m, svc, pod}}
			expected := "configured-name"
			discover := false
			switch scenario {
			case "discover":
				delete(object(get(m, "spec", "model")), "servedName")
				expected = "discovered-id"
				discover = true
			case "provider opts out":
				object(m["spec"])["provider"] = Object{"name": "provider"}
				object(m["spec"])["engine"] = Object{"type": "vllm"}
				provider := accessTestResource("InferenceProviderConfig", "provider")
				provider["spec"] = Object{"capabilities": Object{"engines": accessTestObjects(Object{"name": "vllm", "gateway": Object{"ignoresServedName": true}})}}
				client.resources = append(client.resources, provider)
				expected = "discovered-id"
				discover = true
			case "raw invented fields":
				delete(object(get(m, "spec", "model")), "servedName")
				object(get(m, "spec", "model"))["name"] = "not-real"
				object(m["status"])["servedModelName"] = "also-not-real"
				expected = "discovered-id"
				discover = true
			case "gateway alias":
				object(m["spec"])["gateway"] = Object{"modelName": "route-alias"}
				object(m["status"])["gateway"] = Object{"modelName": "route-alias"}
			}
			var mu sync.Mutex
			gets, posts := 0, 0
			upstream := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") != "" {
					t.Error("cluster credentials leaked")
				}
				if r.URL.Path == "/v1/models" {
					gets++
					accessTestReply(w, Object{"data": accessTestObjects(Object{"id": "discovered-id"})})
					return
				}
				posts++
				var body Object
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["model"] != expected {
					t.Error(body)
				}
				if len(array(body["messages"])) != posts*2-1 {
					t.Error("history", body)
				}
				accessTestReply(w, Object{"choices": accessTestObjects(Object{"message": Object{"content": "reply"}})})
			})
			accessTestSetupForward(t, client, upstream)
			c, out, stderr := accessTestContext(client, nil)
			c.IO.Interactive = true
			c.IO.In = strings.NewReader("first\nsecond\n/exit\nignored\n")
			accessTestCode(t, runAccess("model", "chat", "llama", c), "")
			for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
				var record Object
				if json.Unmarshal([]byte(line), &record) != nil || stringAt(record, "progress", "message") == "" {
					t.Fatalf("invalid JSON progress: %s", line)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if posts != 2 || (discover && gets != 1) || (!discover && gets != 0) || strings.Count(out.String(), "reply") != 2 {
				t.Fatal(gets, posts, out.String())
			}
		})
	}
}

func TestAccessIngressTrustBoundaries(t *testing.T) {
	for _, scenario := range []string{"valid", "model flags ignored", "foreign service", "foreign pod", "foreign workload", "foreign secret", "cross namespace", "invalid base64", "invalid bearer", "missing key", "forbidden"} {
		t.Run(scenario, func(t *testing.T) {
			a, s, p, w, secret := accessTestAgent()
			client := &accessFakeClient{resources: []Object{a, s, p, w, secret}}
			e := &accessEndpoint{URL: &url.URL{Scheme: "http", Host: "actual-agent-api.test.svc", Path: "/"}, Service: s, AuthSecretRef: object(get(a, "status", "runtime", "authSecretRef"))}
			connection := &accessConnection{Endpoint: e, Tunnel: &accessTunnel{Pod: p}}
			flags := Flags{}
			code := "UNSUPPORTED"
			switch scenario {
			case "valid":
				code = ""
			case "model flags ignored":
				code = ""
				flags = Flags{"credential": {"never-read-model-key"}, "model-credential": {"never-read-model-key"}}
			case "foreign service":
				delete(object(s["metadata"]), "ownerReferences")
			case "foreign pod":
				delete(object(p["metadata"]), "ownerReferences")
			case "foreign workload":
				delete(object(w["metadata"]), "ownerReferences")
			case "foreign secret":
				delete(object(secret["metadata"]), "ownerReferences")
			case "cross namespace":
				object(s["metadata"])["namespace"] = "other"
			case "invalid base64":
				object(secret["data"])["token"] = "!bad!"
				code = "AUTH"
			case "invalid bearer":
				object(secret["data"])["token"] = base64.StdEncoding.EncodeToString([]byte("has newline\n"))
				code = "AUTH"
			case "missing key":
				secret["data"] = Object{}
				code = "AUTH"
			case "forbidden":
				client.onGet = func(ctx context.Context, typ ResourceType, ns, name string) (Object, error) {
					if typ.Kind == "Secret" {
						return nil, errors.New("permission denied private-ingress-token")
					}
					return cloneObject(w), nil
				}
				code = "AUTH"
			}
			token, err := accessIngressToken(context.Background(), client, "agent", a, e, connection, flags, "test")
			accessTestCode(t, err, code)
			if code == "" && token != "private-ingress-token" {
				t.Fatal(token)
			}
			if err != nil && strings.Contains(err.Error(), "private-ingress-token") {
				t.Fatal("secret in error", err)
			}
			if scenario == "foreign service" || scenario == "foreign pod" || scenario == "foreign workload" || scenario == "cross namespace" {
				for _, call := range client.snapshot() {
					if call.Type.Kind == "Secret" {
						t.Fatal("secret read before verifying ownership")
					}
				}
			}
		})
	}
	t.Run("model API_KEY only", func(t *testing.T) {
		m := accessTestModel()
		secret := accessTestResource("Secret", "model-ingress")
		secret["data"] = Object{"API_KEY": base64.StdEncoding.EncodeToString([]byte("api-key")), "HF_TOKEN": base64.StdEncoding.EncodeToString([]byte("hf-token"))}
		e := &accessEndpoint{URL: &url.URL{Scheme: "https", Host: "model.test"}}
		token, err := accessIngressToken(context.Background(), &accessFakeClient{resources: []Object{secret}}, "model", m, e, &accessConnection{Endpoint: e}, Flags{"credential": {"model-ingress"}}, "test")
		accessTestCode(t, err, "")
		if token != "api-key" {
			t.Fatal(token)
		}
	})
}
func TestAccessExternalAndReadiness(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		if r.Method != "GET" || r.URL.Path != "/api/readyz" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected readiness request", r.Method, r.URL.Path, r.Header)
		}
		fmt.Fprint(w, "ready")
	})
	a, _, _, _, _ := accessTestAgent()
	object(get(a, "status", "runtime"))["address"] = server.URL + "/api"
	client := &accessFakeClient{resources: []Object{a}}
	for _, flags := range []Flags{{"check": {"true"}}, {"message": {"hello"}}, {"message": {"hello"}, "server": {server.URL + "/other"}}, {"message": {"hello"}, "server": {server.URL + "/api"}}} {
		c, _, _ := accessTestContext(client, flags)
		action := "chat"
		if flags.Bool("check") {
			action = "endpoint"
		}
		accessTestCode(t, runAccess("agent", action, "helper", c), "UNSUPPORTED")
	}
	mu.Lock()
	if requests != 0 {
		t.Fatal("untrusted address contacted")
	}
	mu.Unlock()
	c, out, _ := accessTestContext(client, Flags{"check": {"true"}, "server": {server.URL + "/api"}})
	accessTestCode(t, runAccess("agent", "endpoint", "helper", c), "")
	if accessTestJSON(t, out)["reachable"] != true {
		t.Fatal(out.String())
	}
	for _, call := range client.snapshot() {
		if call.Type.Kind == "Secret" {
			t.Fatal("readiness read auth")
		}
	}
}
func TestAccessChatInputAndResponses(t *testing.T) {
	m := accessTestModel()
	s := accessTestService("actual-api", 8000)
	p := accessTestPod(s, nil)
	for _, flags := range []Flags{{}, {"message": {" "}}, {"message": {"hello"}, "message-file": {"-"}}, {"message": {"hello"}, "temperature": {"NaN"}}, {"message": {"hello"}, "temperature": {"2.1"}}, {"message": {"hello"}, "max-tokens": {"0"}}} {
		c, _, _ := accessTestContext(&accessFakeClient{resources: []Object{m, s, p}}, flags)
		accessTestCode(t, runAccess("model", "chat", "llama", c), "USAGE")
	}
	client := &accessFakeClient{resources: []Object{m, s, p}}
	server := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var body Object
		_ = json.NewDecoder(r.Body).Decode(&body)
		messages := objects(body["messages"])
		if len(messages) != 1 || messages[0]["content"] != "from stdin" || body["temperature"] != 0.5 || intAt(body, "max_tokens") != 25 {
			t.Error(body)
		}
		accessTestReply(w, Object{"choices": accessTestObjects(Object{"message": Object{"content": "from stdin"}})})
	})
	accessTestSetupForward(t, client, server)
	c, out, _ := accessTestContext(client, Flags{"message-file": {"-"}, "temperature": {"0.5"}, "max-tokens": {"25"}, "output": {"text"}})
	c.IO.In = strings.NewReader("from stdin")
	accessTestCode(t, runAccess("model", "chat", "llama", c), "")
	if out.String() != "from stdin\n" {
		t.Fatal(out.String())
	}
}
func TestAccessHTTPTrustAndErrors(t *testing.T) {
	for _, scenario := range []string{"redirect", "invalid JSON", "too large", "interrupt", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			targetHit := make(chan struct{}, 1)
			target := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) { targetHit <- struct{}{}; accessTestReply(w, Object{}) })
			source := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "redirect":
					http.Redirect(w, r, target.URL, 307)
				case "invalid JSON":
					fmt.Fprint(w, "not json private-ingress-token")
				case "too large":
					fmt.Fprint(w, strings.Repeat("x", maxInput+1))
				case "interrupt":
					w.Header().Set("Content-Length", "99")
					fmt.Fprint(w, "{")
				case "timeout":
					<-r.Context().Done()
				}
			})
			u, _ := accessSafeURL(source.URL)
			connection := &accessConnection{Endpoint: &accessEndpoint{URL: u, Headers: map[string]string{}}}
			connection.HTTP = accessHTTPClient(connection)
			defer connection.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := accessHTTPJSON(ctx, connection, "/v1/models", nil, "private-ingress-token", true)
			code := "RESPONSE"
			if scenario == "redirect" {
				code = "HTTP"
			}
			if scenario == "interrupt" {
				code = "CONNECTION"
			}
			if scenario == "timeout" {
				code = "TIMEOUT"
			}
			accessTestCode(t, err, code)
			if strings.Contains(err.Error(), "private-ingress-token") {
				t.Fatal("secret in error")
			}
			select {
			case <-targetHit:
				t.Fatal("followed redirect")
			default:
			}
		})
	}
	t.Run("TLS identity through tunnel", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { accessTestReply(w, Object{}) }))
		defer server.Close()
		u, _ := accessSafeURL("https://original.example.test/api")
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
		localPort := 0
		fmt.Sscan(port, &localPort)
		connection := &accessConnection{Endpoint: &accessEndpoint{URL: u, Headers: map[string]string{"host": "route.example.test"}}, Tunnel: &accessTunnel{Port: localPort, close: func() {}}}
		connection.HTTP = accessHTTPClient(connection)
		defer connection.Close()
		transport := connection.HTTP.Transport.(*http.Transport)
		if transport.TLSClientConfig.ServerName != "route.example.test" || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.RootCAs != nil {
			t.Fatal("TLS identity weakened")
		}
		_, err := accessHTTPJSON(context.Background(), connection, "/v1/models", nil, "", true)
		accessTestCode(t, err, "CONNECTION")
	})
	t.Run("base v1 prefix", func(t *testing.T) {
		u, _ := accessSafeURL("http://service.test/prefix/v1/")
		got := accessAPIURL(&accessEndpoint{URL: u}, "/v1/models")
		if got.Path != "/prefix/v1/models" {
			t.Fatal(got)
		}
	})
}
func TestAccessPodSelection(t *testing.T) {
	s := accessTestService("actual-api", 8000)
	p := accessTestPod(s, nil)
	other := cloneObject(p)
	object(other["metadata"])["name"] = "aaa-unmatched"
	object(get(other, "metadata", "labels"))["workload"] = "different"
	unready := cloneObject(p)
	object(unready["metadata"])["name"] = "aaa-unready"
	unready["status"] = Object{"phase": "Pending"}
	c := &accessFakeClient{resources: []Object{other, unready, p}}
	got, port, err := accessSelectedPod(context.Background(), c, &accessEndpoint{Service: s, ServicePort: 8000})
	accessTestCode(t, err, "")
	if stringAt(got, "metadata", "name") != "selected-pod" || port != 8080 {
		t.Fatal(got, port)
	}
	ports := array(get(objects(get(p, "spec", "containers"))[0], "ports"))
	objects(get(p, "spec", "containers"))[0]["ports"] = append(ports, Object{"name": "api", "containerPort": 8081})
	_, _, err = accessSelectedPod(context.Background(), c, &accessEndpoint{Service: s, ServicePort: 8000})
	accessTestCode(t, err, "UNSUPPORTED")
	object(s["spec"])["type"] = "ExternalName"
	_, _, err = accessSelectedPod(context.Background(), c, &accessEndpoint{Service: s})
	accessTestCode(t, err, "UNSUPPORTED")
}
func TestAccessPortForwardCancelAndRedirect(t *testing.T) {
	for _, scenario := range []string{"pending upgrade", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			started, closed := make(chan struct{}), make(chan struct{})
			targetHit := make(chan struct{}, 1)
			target := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				targetHit <- struct{}{}
				http.Error(w, "unexpected", 500)
			})
			server := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer cluster-token" {
					t.Error("missing cluster auth")
				}
				close(started)
				if scenario == "redirect" {
					http.Redirect(w, r, target.URL, 307)
					return
				}
				<-r.Context().Done()
				close(closed)
			})
			s := accessTestService("actual-api", 8000)
			p := accessTestPod(s, nil)
			client := &accessFakeClient{resources: []Object{s, p}, config: &rest.Config{Host: server.URL, BearerToken: "cluster-token"}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				tunnel, err := accessOpenTunnel(ctx, client, &accessEndpoint{Service: s, ServicePort: 8000}, 0)
				if tunnel != nil {
					tunnel.Close()
				}
				done <- err
			}()
			<-started
			if scenario == "pending upgrade" {
				cancel()
			}
			select {
			case err := <-done:
				code := "CONNECTION"
				if scenario == "pending upgrade" {
					code = "CANCELED"
				}
				accessTestCode(t, err, code)
			case <-time.After(time.Second):
				t.Fatal("tunnel did not stop")
			}
			if scenario == "pending upgrade" {
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("upgrade wire was not closed")
				}
			}
			select {
			case <-targetHit:
				t.Fatal("replayed cluster credentials to redirect")
			default:
			}
		})
	}
}
func TestAccessConnectRawLoopback(t *testing.T) {
	a, s, p, w, secret := accessTestAgent()
	client := &accessFakeClient{resources: []Object{a, s, p, w, secret}}
	server := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer caller-token" {
			t.Error("tunnel rewrote auth")
		}
		fmt.Fprint(w, "raw")
	})
	closed := accessTestSetupForward(t, client, server)
	// Test the same direct SDK tunnel used by connect without concurrently reading
	// the command's writer while it emits its endpoint metadata.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, err := accessResolveEndpoint(ctx, client, "agent", a, nil, "test", false)
	accessTestCode(t, err, "")
	tunnel, err := accessOpenTunnel(ctx, client, e, 0)
	accessTestCode(t, err, "")
	defer tunnel.Close()
	request, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", tunnel.Port), nil)
	request.Header.Set("Authorization", "Bearer caller-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "raw" {
		t.Fatal(string(body))
	}
	cancel()
	tunnel.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("tunnel remains open")
	}
	for _, call := range client.snapshot() {
		if call.Type.Kind == "Secret" {
			t.Fatal("raw tunnel reads token")
		}
	}
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", tunnel.Port), 100*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("loopback listener left open")
	}
}

func TestAccessLogsOwnerDiscoveryAndOutput(t *testing.T) {
	t.Run("ReplicaSet ownership", func(t *testing.T) {
		a, s, p, w, _ := accessTestAgent()
		rs := accessTestResource("ReplicaSet", "replica")
		rs["apiVersion"] = "apps/v1"
		accessTestOwn(rs, w)
		accessTestOwn(p, rs)
		client := &accessFakeClient{resources: []Object{a, s, p, w, rs}}
		c, out, _ := accessTestContext(client, Flags{"pod": {"selected-pod"}, "timestamps": {"true"}, "tail": {"23"}})
		accessTestCode(t, runAccess("agent", "logs", "helper", c), "")
		var text string
		_ = json.Unmarshal(out.Bytes(), &text)
		if text != "line one\nline two\n" {
			t.Fatal(text)
		}
		calls := client.snapshot()
		last := calls[len(calls)-1]
		if last.Path != "/api/v1/namespaces/test/pods/selected-pod/log" || last.Options.Query.Get("container") != "main" || last.Options.Query.Get("tailLines") != "23" || last.Options.Query.Get("timestamps") != "true" {
			t.Fatal(last)
		}
		object(rs["metadata"])["uid"] = "new-owner"
		accessTestCode(t, runAccess("agent", "logs", "helper", c), "UNSUPPORTED")
	})
	t.Run("container and pod ambiguity", func(t *testing.T) {
		a, s, p, w, _ := accessTestAgent()
		object(p["spec"])["initContainers"] = accessTestObjects(Object{"name": "init"})
		client := &accessFakeClient{resources: []Object{a, s, p, w}}
		c, _, _ := accessTestContext(client, nil)
		accessTestCode(t, runAccess("agent", "logs", "helper", c), "USAGE")
		c.Flags["container"] = []string{"init"}
		accessTestCode(t, runAccess("agent", "logs", "helper", c), "")
		other := cloneObject(p)
		object(other["metadata"])["name"] = "other-pod"
		client.resources = append(client.resources, other)
		accessTestCode(t, runAccess("agent", "logs", "helper", c), "UNSUPPORTED")
	})
	t.Run("custom provider reference", func(t *testing.T) {
		m := accessTestModel()
		object(m["status"])["provider"] = Object{"resourceName": "upstream", "resourceKind": "CustomWorkload"}
		root := accessTestResource("CustomWorkload", "upstream")
		root["apiVersion"] = "example.test/v7"
		p := accessTestPod(accessTestService("actual-api", 8000), root)
		client := &accessFakeClient{resources: []Object{m, root, p}, onRequest: func(_ context.Context, _, path string, _ any, _ RequestOptions) (Object, error) {
			if path == "/apis" {
				return Object{"groups": accessTestObjects(Object{"preferredVersion": Object{"groupVersion": "example.test/v7"}})}, nil
			}
			return Object{"resources": accessTestObjects(Object{"name": "customworkloads", "kind": "CustomWorkload", "namespaced": true})}, nil
		}}
		c, _, _ := accessTestContext(client, nil)
		accessTestCode(t, runAccess("model", "logs", "llama", c), "")
		found := false
		for _, call := range client.snapshot() {
			if call.Name == "upstream" && call.Type.Plural == "customworkloads" {
				found = true
			}
		}
		if !found {
			t.Fatal(client.snapshot())
		}
	})
	t.Run("UTF8 follow", func(t *testing.T) {
		a, _, p, w, _ := accessTestAgent()
		client := &accessFakeClient{resources: []Object{a, p, w}, onRaw: func(context.Context, string, string, any, RequestOptions) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(&accessTestByteReader{data: []byte("café\nlast")})}, nil
		}}
		c, out, _ := accessTestContext(client, Flags{"follow": {"true"}})
		accessTestCode(t, runAccess("agent", "logs", "helper", c), "")
		decoder := json.NewDecoder(out)
		for _, expected := range []string{"café", "last"} {
			var line Object
			if err := decoder.Decode(&line); err != nil || line["line"] != expected {
				t.Fatal(line, err)
			}
		}
	})
}

type accessTestByteReader struct{ data []byte }

func (r *accessTestByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}
func TestAccessLogCancellationClosesBody(t *testing.T) {
	a, _, p, w, _ := accessTestAgent()
	reader, writer := io.Pipe()
	defer writer.Close()
	started := make(chan struct{})
	client := &accessFakeClient{resources: []Object{a, p, w}, onRaw: func(context.Context, string, string, any, RequestOptions) (*http.Response, error) {
		close(started)
		return &http.Response{StatusCode: 200, Body: reader}, nil
	}}
	c, _, _ := accessTestContext(client, Flags{"follow": {"true"}})
	ctx, cancel := context.WithCancel(context.Background())
	c.Context = ctx
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runAccess("agent", "logs", "helper", c) }()
	<-started
	cancel()
	select {
	case err := <-done:
		accessTestCode(t, err, "CANCELED")
	case <-time.After(time.Second):
		t.Fatal("logs did not cancel")
	}
	writeDone := make(chan error, 1)
	go func() { _, err := writer.Write([]byte("blocked")); writeDone <- err }()
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("body not closed")
		}
	case <-time.After(time.Second):
		t.Fatal("body not closed")
	}
}
func TestAccessEventsUseUID(t *testing.T) {
	m := accessTestModel()
	event := accessTestResource("Event", "event")
	event["involvedObject"] = Object{"uid": get(m, "metadata", "uid")}
	client := &accessFakeClient{resources: []Object{m, event}}
	c, out, _ := accessTestContext(client, nil)
	accessTestCode(t, runAccess("model", "events", "llama", c), "")
	calls := client.snapshot()
	if calls[1].Options.Query.Get("fieldSelector") != "involvedObject.uid=llama-uid" {
		t.Fatal(calls)
	}
	var events []Object
	if err := json.Unmarshal(out.Bytes(), &events); err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
	delete(object(m["metadata"]), "uid")
	accessTestCode(t, runAccess("model", "events", "llama", c), "UNSUPPORTED")
}

func TestAccessConfiguredHTTPRoute(t *testing.T) {
	t.Run("fetch exact user-owned route without listing", func(t *testing.T) {
		model, gateway, route, service := accessTestGateway()
		object(model["spec"])["gateway"] = Object{"httpRouteRef": "user-route"}
		unrelated := cloneObject(route)
		object(route["metadata"])["name"] = "user-route"
		delete(object(route["metadata"]), "ownerReferences")
		// The matching old controller-owned route must not make the explicit choice
		// ambiguous, and get permission alone must suffice for the configured route.
		client := &accessFakeClient{resources: []Object{model, gateway, route, unrelated, service}, onList: func(_ context.Context, typ ResourceType, _ string, _ url.Values) error {
			if typ.Kind == "HTTPRoute" {
				return cliError(1, "HTTP_403", "route listing forbidden")
			}
			return nil
		}}
		c, out, _ := accessTestContext(client, nil)
		accessTestCode(t, runAccess("model", "endpoint", "llama", c), "")
		result := accessTestJSON(t, out)
		if result["url"] != "https://203.0.113.3:8443/models" || stringAt(result, "headers", "host") != "inference.example.test" {
			t.Fatal(result)
		}
		found := false
		for _, call := range client.snapshot() {
			if call.Type.Kind != "HTTPRoute" {
				continue
			}
			if call.Method != "get" || call.Name != "user-route" || call.Namespace != "test" {
				t.Fatal("not the configured namespaced route", call)
			}
			found = true
		}
		if !found {
			t.Fatal("configured route was not fetched")
		}
	})
	t.Run("missing reference does not fall back to owned route or gateway namespace", func(t *testing.T) {
		model, gateway, owned, service := accessTestGateway()
		object(model["spec"])["gateway"] = Object{"httpRouteRef": "user-route"}
		wrongNamespace := cloneObject(owned)
		object(wrongNamespace["metadata"])["name"] = "user-route"
		object(wrongNamespace["metadata"])["namespace"] = "edge"
		c, _, _ := accessTestContext(&accessFakeClient{resources: []Object{model, gateway, owned, wrongNamespace, service}}, nil)
		accessTestCode(t, runAccess("model", "endpoint", "llama", c), "HTTP_404")
	})
	for _, mismatch := range []string{"name", "namespace", "kind", "group", "sectionName", "port", "selected listener"} {
		t.Run("still validates "+mismatch, func(t *testing.T) {
			model, gateway, route, service := accessTestGateway()
			object(model["spec"])["gateway"] = Object{"httpRouteRef": "route"}
			delete(object(route["metadata"]), "ownerReferences")
			parent := objects(get(route, "spec", "parentRefs"))[0]
			flags := Flags{}
			switch mismatch {
			case "port":
				parent["port"] = 443
			case "selected listener":
				flags["gateway-listener"] = []string{"other"}
			default:
				parent[mismatch] = "other"
			}
			c, _, _ := accessTestContext(&accessFakeClient{resources: []Object{model, gateway, route, service}}, flags)
			accessTestCode(t, runAccess("model", "endpoint", "llama", c), "UNSUPPORTED")
		})
	}
}

func TestAccessPublishedGatewayWithoutServicePermission(t *testing.T) {
	for _, action := range []string{"endpoint", "check", "chat"} {
		t.Run(action, func(t *testing.T) {
			requests := make(chan string, 2)
			server := accessTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				requests <- r.Method + " " + r.URL.Path
				if r.Host != "inference.example.test" || r.Header.Get("x-gateway-model-name") != "served-alias" {
					t.Error("route identity lost", r.Host, r.Header)
				}
				if r.Method == "GET" {
					accessTestReply(w, Object{"data": accessTestObjects(Object{"id": "served-alias"})})
					return
				}
				var body Object
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["model"] != "served-alias" {
					t.Error(body, err)
				}
				accessTestReply(w, Object{"choices": accessTestObjects(Object{"message": Object{"content": "direct reply"}})})
			})
			model, gateway, route, _ := accessTestGateway()
			published, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			port := 0
			fmt.Sscan(published.Port(), &port)
			listener := objects(get(gateway, "spec", "listeners"))[0]
			listener["protocol"], listener["port"] = "HTTP", port
			gateway["status"] = Object{"addresses": accessTestObjects(Object{"type": "IPAddress", "value": published.Hostname()})}
			client := &accessFakeClient{resources: []Object{model, gateway, route}, onList: func(_ context.Context, typ ResourceType, ns string, _ url.Values) error {
				if typ.Kind == "Service" {
					if ns != "edge" {
						t.Error("wrong service namespace", ns)
					}
					return cliError(1, "HTTP_403", "service listing forbidden")
				}
				return nil
			}}
			flags := Flags{}
			command := action
			if action != "endpoint" {
				flags["server"] = []string{server.URL + "/models"}
			}
			if action == "check" {
				command = "endpoint"
				flags["check"] = []string{"true"}
			}
			if action == "chat" {
				flags["message"] = []string{"hello"}
			}
			c, out, _ := accessTestContext(client, flags)
			accessTestCode(t, runAccess("model", command, "llama", c), "")
			result := accessTestJSON(t, out)
			if action == "chat" {
				choices := objects(result["choices"])
				if len(choices) != 1 || stringAt(choices[0], "message", "content") != "direct reply" {
					t.Fatal(result)
				}
			} else if result["url"] != server.URL+"/models" || result["access"] != "gateway" || result["service"] != nil {
				t.Fatal(result)
			}
			if action == "endpoint" {
				select {
				case got := <-requests:
					t.Fatal("endpoint unexpectedly contacted", got)
				default:
				}
				return
			}
			expected := "GET /models/v1/models"
			if action == "chat" {
				expected = "POST /models/v1/chat/completions"
			}
			select {
			case got := <-requests:
				if got != expected {
					t.Fatal(got)
				}
			default:
				t.Fatal("published gateway not contacted")
			}
		})
	}
	for _, action := range []string{"connect", "chat", "check", "internal fallback"} {
		t.Run("discovery remains required for "+action, func(t *testing.T) {
			model, gateway, route, _ := accessTestGateway()
			flags := Flags{}
			command := action
			if action == "chat" {
				flags["message"] = []string{"hello"}
			}
			if action == "check" {
				command = "endpoint"
				flags["check"] = []string{"true"}
			}
			if action == "internal fallback" {
				command = "endpoint"
				gateway["status"] = Object{}
			}
			client := &accessFakeClient{resources: []Object{model, gateway, route}, onList: func(_ context.Context, typ ResourceType, _ string, _ url.Values) error {
				if typ.Kind == "Service" {
					return cliError(1, "HTTP_403", "service listing forbidden")
				}
				return nil
			}}
			c, _, _ := accessTestContext(client, flags)
			accessTestCode(t, runAccess("model", command, "llama", c), "HTTP_403")
		})
	}
	t.Run("other discovery errors are not hidden", func(t *testing.T) {
		model, gateway, route, _ := accessTestGateway()
		client := &accessFakeClient{resources: []Object{model, gateway, route}, onList: func(_ context.Context, typ ResourceType, _ string, _ url.Values) error {
			if typ.Kind == "Service" {
				return cliError(1, "HTTP_500", "service listing failed")
			}
			return nil
		}}
		c, _, _ := accessTestContext(client, nil)
		accessTestCode(t, runAccess("model", "endpoint", "llama", c), "HTTP_500")
	})
}
