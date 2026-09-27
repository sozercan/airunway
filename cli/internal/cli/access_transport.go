package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/httpstream"
	streamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	transportspdy "k8s.io/client-go/transport/spdy"
)

func accessSelectedPod(ctx context.Context, client ClusterClient, e *accessEndpoint) (Object, int, error) {
	if e.Service == nil {
		return nil, 0, accessUnsupported("No gateway or runtime Service was discovered for port forwarding. Inspect its Service labels or use an explicitly trusted --server for chat.")
	}
	selector := object(get(e.Service, "spec", "selector"))
	if len(selector) == 0 || stringAt(e.Service, "spec", "type") == "ExternalName" {
		return nil, 0, accessUnsupported("This Service has no pod selector. CLI port forwarding requires a selector-backed Service.")
	}
	port, err := accessServicePort(e.Service, e.ServicePort)
	if err != nil {
		return nil, 0, err
	}
	labels := []string{}
	for k, v := range selector {
		labels = append(labels, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(labels)
	pods, err := accessList(ctx, client, resourceTypes["pod"], accessNamespace(e.Service, ""), url.Values{"labelSelector": {strings.Join(labels, ",")}})
	if err != nil {
		return nil, 0, err
	}
	ready := []Object{}
	for _, pod := range pods {
		if get(pod, "metadata", "deletionTimestamp") != nil || stringAt(pod, "status", "phase") != "Running" {
			continue
		}
		matches := true
		for k, v := range selector {
			if get(pod, "metadata", "labels", k) != v {
				matches = false
			}
		}
		healthy := false
		for _, c := range objects(get(pod, "status", "conditions")) {
			healthy = healthy || (stringAt(c, "type") == "Ready" && stringAt(c, "status") == "True")
		}
		if matches && healthy {
			ready = append(ready, pod)
		}
	}
	slices.SortFunc(ready, func(a, b Object) int {
		return strings.Compare(stringAt(a, "metadata", "name"), stringAt(b, "metadata", "name"))
	})
	if len(ready) == 0 {
		return nil, 0, accessUnsupported("The endpoint Service has no ready matching pods. Wait for readiness or inspect events.")
	}
	pod := ready[0]
	target := intAt(port, "port")
	if named, ok := port["targetPort"].(string); ok {
		matches := []Object{}
		for _, container := range objects(get(pod, "spec", "containers")) {
			for _, p := range objects(container["ports"]) {
				if stringAt(p, "name") == named && (stringAt(p, "protocol") == "" || stringAt(p, "protocol") == "TCP") {
					matches = append(matches, p)
				}
			}
		}
		if len(matches) != 1 {
			return nil, 0, accessUnsupported("The Service targetPort does not identify a unique TCP container port.")
		}
		target = intAt(matches[0], "containerPort")
	} else if port["targetPort"] != nil {
		target = intAt(port, "targetPort")
	}
	if target < 1 || target > 65535 {
		return nil, 0, accessUnsupported("The Service has an invalid targetPort.")
	}
	return pod, int(target), nil
}

// client-go's SPDY RoundTrip can block reading upgrade headers after cancellation.
// Keep the wire available to close throughout that read and the resulting tunnel.
// This still uses client-go's authenticated transport and pod port-forwarder.
type accessUpgrade struct {
	base *streamspdy.SpdyRoundTripper
	conn net.Conn
}

func (u *accessUpgrade) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(httpstream.HeaderConnection, httpstream.HeaderUpgrade)
	req.Header.Set(httpstream.HeaderUpgrade, streamspdy.HeaderSpdy31)
	conn, err := u.base.Dial(req)
	if err != nil {
		return nil, err
	}
	u.conn = conn
	stop := context.AfterFunc(req.Context(), func() { conn.Close() })
	// The wrapper's Close releases the cancellation callback when the tunnel ends.
	u.conn = &accessCancelConn{Conn: conn, stop: stop}
	response, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		u.conn.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		u.conn.Close()
		response.Body = io.NopCloser(strings.NewReader(""))
	}
	return response, nil
}
func (u *accessUpgrade) NewConnection(response *http.Response) (httpstream.Connection, error) {
	if response.StatusCode != http.StatusSwitchingProtocols || !strings.Contains(strings.ToLower(response.Header.Get(httpstream.HeaderConnection)), "upgrade") || !strings.Contains(strings.ToLower(response.Header.Get(httpstream.HeaderUpgrade)), strings.ToLower(streamspdy.HeaderSpdy31)) {
		if u.conn != nil {
			u.conn.Close()
		}
		return nil, fmt.Errorf("port-forward upgrade refused")
	}
	conn, err := streamspdy.NewClientConnectionWithPings(u.conn, 5*time.Second)
	if err != nil {
		u.conn.Close()
	}
	return conn, err
}

type accessCancelConn struct {
	net.Conn
	stop func() bool
}

func (c *accessCancelConn) Close() error { c.stop(); return c.Conn.Close() }

type accessPortDialer struct {
	ctx     context.Context
	client  *http.Client
	upgrade *accessUpgrade
	url     *url.URL
}

func (d *accessPortDialer) Dial(protocols ...string) (httpstream.Connection, string, error) {
	req, err := http.NewRequestWithContext(d.ctx, "POST", d.url.String(), nil)
	if err != nil {
		return nil, "", err
	}
	return transportspdy.Negotiate(d.upgrade, d.client, req, protocols...)
}

type accessTunnel struct {
	Port  int
	Pod   Object
	Done  <-chan struct{}
	close func()
}

func (t *accessTunnel) Close() { t.close() }
func accessOpenTunnel(parent context.Context, client ClusterClient, e *accessEndpoint, localPort int) (*accessTunnel, error) {
	config := client.RESTConfig()
	if config == nil {
		return nil, accessUnsupported("Port forwarding requires the selected cluster's kubeconfig.")
	}
	pod, target, err := accessSelectedPod(parent, client, e)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	fail := func() (*accessTunnel, error) {
		cancel()
		if parent.Err() != nil {
			return nil, accessContextError(parent)
		}
		return nil, cliError(1, "CONNECTION", "Cannot open the pod port-forward connection. Check cluster access or choose another --port.")
	}
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		return fail()
	}
	base, err := streamspdy.NewRoundTripperWithConfig(streamspdy.RoundTripperConfig{TLS: tlsConfig, Proxier: config.Proxy})
	if err != nil {
		return fail()
	}
	upgrade := &accessUpgrade{base: base}
	transport, err := rest.HTTPWrappersForConfig(config, upgrade)
	if err != nil {
		return fail()
	}
	server, err := url.Parse(config.Host)
	if err != nil || server.Host == "" || (server.Scheme != "http" && server.Scheme != "https") || server.RawQuery != "" || server.Fragment != "" || server.User != nil {
		return fail()
	}
	server.Path = strings.TrimSuffix(server.Path, "/") + resourcePath(resourceTypes["pod"], accessNamespace(pod, accessNamespace(e.Service, "")), stringAt(pod, "metadata", "name")) + "/portforward"
	dialer := &accessPortDialer{ctx: ctx, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, upgrade: upgrade, url: server}
	ready := make(chan struct{})
	done := make(chan struct{})
	forward, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("%d:%d", localPort, target)}, ctx.Done(), ready, io.Discard, io.Discard)
	if err != nil {
		return fail()
	}
	go func() { defer close(done); defer cancel(); _ = forward.ForwardPorts() }()
	// Do not call forward.Close concurrently with its initialization. Its own
	// deferred cleanup closes listeners and all streams when ctx.Done fires.
	closeTunnel := func() { cancel(); <-done }
	select {
	case <-parent.Done():
		cancel()
		return nil, accessContextError(parent)
	case <-done:
		return fail()
	case <-ready:
		if parent.Err() != nil {
			closeTunnel()
			return nil, accessContextError(parent)
		}
		ports, err := forward.GetPorts()
		if err != nil || len(ports) != 1 {
			closeTunnel()
			return fail()
		}
		return &accessTunnel{Port: int(ports[0].Local), Pod: pod, Done: done, close: closeTunnel}, nil
	}
}

type accessConnection struct {
	Endpoint *accessEndpoint
	Tunnel   *accessTunnel
	HTTP     *http.Client
}

func (c *accessConnection) Close() {
	if c.HTTP != nil {
		c.HTTP.CloseIdleConnections()
	}
	if c.Tunnel != nil {
		c.Tunnel.Close()
	}
}
func accessTransport(ctx context.Context, client ClusterClient, e *accessEndpoint, flags Flags) (*accessConnection, error) {
	connection := &accessConnection{Endpoint: e}
	if explicit := flags.Text("server"); explicit != "" {
		u, err := accessSafeURL(explicit)
		if err != nil {
			return nil, err
		}
		if u.String() != e.URL.String() {
			return nil, accessUnsupported("--server must exactly match the published endpoint URL.")
		}
	} else {
		if e.Service == nil {
			return nil, accessUnsupported("External status addresses are not contacted automatically. Verify the endpoint, then pass its exact URL with --server.")
		}
		tunnel, err := accessOpenTunnel(ctx, client, e, 0)
		if err != nil {
			return nil, err
		}
		connection.Tunnel = tunnel
	}
	connection.HTTP = accessHTTPClient(connection)
	return connection, nil
}
func accessHTTPClient(c *accessConnection) *http.Client {
	// Inference HTTP never inherits kubeconfig auth, CA roots, proxies, or insecure
	// TLS settings. Dial loopback while retaining the original URL identity.
	host := c.Endpoint.URL.Hostname()
	if h := c.Endpoint.Headers["host"]; h != "" {
		u, err := accessSafeURL(c.Endpoint.URL.Scheme + "://" + h)
		if err == nil {
			host = u.Hostname()
		}
	}
	d := &net.Dialer{}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}, TLSHandshakeTimeout: 10 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if c.Tunnel != nil {
			address = net.JoinHostPort("127.0.0.1", strconv.Itoa(c.Tunnel.Port))
		}
		return d.DialContext(ctx, network, address)
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func accessAPIURL(e *accessEndpoint, path string) *url.URL {
	u := *e.URL
	base := strings.TrimSuffix(u.Path, "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		base = strings.TrimSuffix(base, "/v1")
	}
	u.Path = base + path
	u.RawPath = ""
	return &u
}
func accessHTTPJSON(ctx context.Context, c *accessConnection, path string, body any, token string, expectJSON bool) (Object, error) {
	if ctx.Err() != nil {
		return nil, accessContextError(ctx)
	}
	method := "GET"
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return nil, cliError(1, "REQUEST", "Cannot encode chat request.")
		}
		method = "POST"
	}
	req, err := http.NewRequestWithContext(ctx, method, accessAPIURL(c.Endpoint, path).String(), bytes.NewReader(data))
	if err != nil {
		return nil, cliError(1, "CONNECTION", "Cannot connect to the endpoint. Check connectivity and TLS certificates.")
	}
	for k, v := range c.Endpoint.Headers {
		if strings.EqualFold(k, "host") {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, accessContextError(ctx)
		}
		return nil, cliError(1, "CONNECTION", "Cannot connect to the endpoint. Check connectivity and TLS certificates.")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, cliError(1, "HTTP", fmt.Sprintf("Endpoint request failed with HTTP %d. Redirects are not followed.", response.StatusCode))
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, maxInput+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, accessContextError(ctx)
		}
		return nil, cliError(1, "CONNECTION", "Endpoint response was interrupted.")
	}
	if len(data) > maxInput {
		return nil, cliError(1, "RESPONSE", "Endpoint response exceeds 4 MiB.")
	}
	if !expectJSON || len(data) == 0 {
		return Object{}, nil
	}
	var result Object
	if err := json.Unmarshal(data, &result); err != nil || result == nil {
		return nil, cliError(1, "RESPONSE", "Endpoint returned an invalid JSON response.")
	}
	return result, nil
}
func accessIngressToken(ctx context.Context, client ClusterClient, noun string, resource Object, e *accessEndpoint, c *accessConnection, flags Flags, fallback string) (string, error) {
	var ref Object
	if noun == "agent" {
		ref = e.AuthSecretRef
	} else if name := flags.Text("credential"); name != "" {
		ref = Object{"name": name, "key": "API_KEY"}
	}
	if ref == nil {
		return "", nil
	}
	if c.Tunnel == nil && e.URL.Scheme != "https" {
		return "", accessUnsupported("Credentials require verified HTTPS for direct external access. Use a cluster tunnel instead.")
	}
	ns := accessNamespace(resource, fallback)
	if noun == "agent" && c.Tunnel != nil {
		root, err := accessWorkload(ctx, client, noun, resource, ns)
		if err != nil {
			return "", err
		}
		if !accessOwnedBy(root, resource) || e.Service == nil || accessNamespace(e.Service, ns) != ns {
			return "", accessUnsupported("The agent endpoint is not owned by this AgentDeployment. Refusing to send its ingress token.")
		}
		owned, err := accessDescendsFrom(ctx, client, e.Service, resource, map[string]Object{}, map[string]bool{}, ns)
		if err != nil {
			return "", err
		}
		if !owned {
			return "", accessUnsupported("The agent endpoint is not owned by this AgentDeployment. Refusing to send its ingress token.")
		}
		owned, err = accessDescendsFrom(ctx, client, c.Tunnel.Pod, root, map[string]Object{}, map[string]bool{}, ns)
		if err != nil {
			return "", err
		}
		if !owned {
			return "", accessUnsupported("The selected pod is not owned by the agent workload. Refusing to send its ingress token.")
		}
	}
	if stringAt(ref, "name") == "" || stringAt(ref, "key") == "" {
		return "", accessUnsupported("The published ingress Secret reference is incomplete.")
	}
	secret, err := accessGet(ctx, client, resourceTypes["credential"], ns, stringAt(ref, "name"))
	if err != nil {
		if ctx.Err() != nil {
			return "", accessContextError(ctx)
		}
		return "", cliError(3, "AUTH", "Cannot read the endpoint ingress Secret. Request permission to read that Secret in the resource namespace.")
	}
	if noun == "agent" && !accessOwnedBy(secret, resource) {
		return "", accessUnsupported("The ingress Secret is not owned by this AgentDeployment. Refusing to use a model or unrelated credential.")
	}
	encoded := stringAt(secret, "data", stringAt(ref, "key"))
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || strings.ContainsAny(encoded, "\r\n") {
		return "", cliError(3, "AUTH", "The ingress Secret is missing its required key or contains invalid data.")
	}
	for _, b := range data {
		if b < 0x21 || b > 0x7e {
			return "", cliError(3, "AUTH", "The ingress Secret contains an invalid bearer token.")
		}
	}
	return string(data), nil
}
func accessRedact(value any, token string) any {
	if token == "" {
		return value
	}
	switch v := value.(type) {
	case string:
		return strings.ReplaceAll(v, token, "[redacted]")
	case map[string]any:
		out := Object{}
		for k, entry := range v {
			out[strings.ReplaceAll(k, token, "[redacted]")] = accessRedact(entry, token)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, entry := range v {
			out[i] = accessRedact(entry, token)
		}
		return out
	}
	return value
}
func accessChat(client ClusterClient, noun string, resource Object, e *accessEndpoint, c *CommandContext) error {
	ctx := c.Context
	if formats := array(get(resource, "status", "gateway", "apiFormats")); noun == "model" && len(formats) > 0 {
		supported := false
		for _, f := range formats {
			supported = supported || f == "openai-chat"
		}
		if !supported {
			return accessUnsupported("This model provider does not advertise the OpenAI chat API. Use a provider with openai-chat support.")
		}
	}
	type messageInput struct {
		text    string
		present bool
	}
	input, err := accessCall(ctx, func() (messageInput, error) {
		m, p, e := readFlagInput(c.Flags, "message", "message-file", c.IO)
		return messageInput{m, p}, e
	})
	if err != nil {
		return err
	}
	if !input.present && !c.IO.Interactive {
		return usage("Provide --message or --message-file when stdin is not a terminal.")
	}
	if input.present && strings.TrimSpace(input.text) == "" {
		return usage("The chat message must not be empty.")
	}
	options := Object{}
	if c.Flags.Has("temperature") {
		n, err := strconv.ParseFloat(strings.TrimSpace(c.Flags.Text("temperature")), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 2 {
			return usage("--temperature must be between 0 and 2.")
		}
		options["temperature"] = n
	}
	if c.Flags.Has("max-tokens") {
		n, err := integer(c.Flags, "max-tokens", 0, 1, 1<<53-1)
		if err != nil {
			return err
		}
		options["max_tokens"] = n
	}
	connection, err := accessTransport(ctx, client, e, c.Flags)
	if err != nil {
		return err
	}
	defer connection.Close()
	token, err := accessIngressToken(ctx, client, noun, resource, e, connection, c.Flags, c.Namespace)
	if err != nil {
		return err
	}
	model := ""
	if noun == "model" {
		model = e.ServedModelName
	}
	if model == "" {
		discovery, err := accessHTTPJSON(ctx, connection, "/v1/models", nil, token, true)
		if err != nil {
			return err
		}
		ids := []string{}
		for _, item := range objects(discovery["data"]) {
			if id := stringAt(item, "id"); id != "" {
				ids = append(ids, id)
			}
		}
		preferred := []string{stringAt(resource, "metadata", "name")}
		if noun == "model" {
			preferred = []string{stringAt(resource, "spec", "model", "servedName"), stringAt(resource, "spec", "model", "id")}
		}
		for _, id := range preferred {
			if id != "" && slices.Contains(ids, id) {
				model = id
				break
			}
		}
		if model == "" && len(ids) == 1 {
			model = ids[0]
		}
		if model == "" {
			return accessUnsupported("The endpoint did not identify a unique served model. Publish a resolved model name or configure a single served ID.")
		}
	}
	messages := []any{}
	turn := func(content string) error {
		messages = append(messages, Object{"role": "user", "content": content})
		body := cloneObject(options)
		body["model"], body["messages"], body["stream"] = model, messages, false
		result, err := accessHTTPJSON(ctx, connection, "/v1/chat/completions", body, token, true)
		if err != nil {
			return err
		}
		safe := object(accessRedact(result, token))
		choices := objects(safe["choices"])
		if len(choices) == 0 {
			return cliError(1, "RESPONSE", "The endpoint did not return a text chat response.")
		}
		answer, ok := get(choices[0], "message", "content").(string)
		if !ok {
			return cliError(1, "RESPONSE", "The endpoint did not return a text chat response.")
		}
		messages = append(messages, Object{"role": "assistant", "content": answer})
		if output := c.Flags.Text("output"); output != "" && output != "text" {
			return writeOutput(c.IO, c.Flags, safe)
		}
		return writeOutput(c.IO, c.Flags, answer)
	}
	if input.present {
		return turn(input.text)
	}
	reader := bufio.NewReader(c.IO.In)
	for {
		if c.Flags.Text("output") == "json" {
			progress(c, "Message (/exit to quit):")
		} else if _, err := fmt.Fprint(c.IO.Err, "Message (/exit to quit): "); err != nil {
			return err
		}
		line, err := accessCall(ctx, func() (string, error) { return reader.ReadString('\n') })
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if strings.TrimSpace(line) == "/exit" || strings.TrimSpace(line) == "/quit" {
			return nil
		}
		if strings.TrimSpace(line) != "" {
			if err := turn(line); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// Compile-time guard for the context-aware upgrade contract.
var _ transportspdy.Upgrader = (*accessUpgrade)(nil)
var _ httpstream.Dialer = (*accessPortDialer)(nil)
