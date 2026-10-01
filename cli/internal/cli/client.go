package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func resourcePath(t ResourceType, namespace, name string) string {
	path := "/api/" + url.PathEscape(t.Version)
	if t.Group != "" {
		path = "/apis/" + url.PathEscape(t.Group) + "/" + url.PathEscape(t.Version)
	}
	if t.Namespaced && namespace != "" {
		path += "/namespaces/" + url.PathEscape(namespace)
	}
	path += "/" + url.PathEscape(t.Plural)
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	return path
}
func typeFor(resource Object) (ResourceType, error) {
	for _, t := range resourceTypes {
		api := t.Version
		if t.Group != "" {
			api = t.Group + "/" + api
		}
		if stringAt(resource, "kind") == t.Kind && stringAt(resource, "apiVersion") == api {
			return t, nil
		}
	}
	return ResourceType{}, usage("Unsupported resource kind or API version.")
}
func loadKubeConfig(path, contextName string) (clientcmd.ClientConfig, *clientcmdapi.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = path
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	raw, err := config.RawConfig()
	if err != nil {
		return nil, nil, cliError(3, "KUBECONFIG", "Cannot load kubeconfig. Check --kubeconfig and --context.")
	}
	if contextName != "" {
		raw.CurrentContext = contextName
	}
	return config, &raw, nil
}

type KubernetesClient struct {
	config    *rest.Config
	http      *http.Client
	namespace string
}

func NewKubernetesClient(config *rest.Config, namespace string) (*KubernetesClient, error) {
	cfg := rest.CopyConfig(config)
	cfg.Timeout = 0
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, cliError(3, "KUBECONFIG", "Cannot initialize cluster credentials.")
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
	return &KubernetesClient{config: cfg, http: httpClient, namespace: namespace}, nil
}
func (c *KubernetesClient) RESTConfig() *rest.Config { return rest.CopyConfig(c.config) }

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { b.cancel(); return b.ReadCloser.Close() }
func connectionError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return cliError(130, "INTERRUPTED", "Interrupted. Submitted resources were not deleted.")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return cliError(4, "TIMEOUT", "Cluster request canceled or timed out.")
	}
	return cliError(3, "CONNECTION", "Cannot connect to the selected cluster. Check connectivity and credentials.")
}
func (c *KubernetesClient) Raw(ctx context.Context, method, path string, body any, options RequestOptions) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#") {
		return nil, usage("Invalid cluster request path.")
	}
	u, err := url.Parse(strings.TrimRight(c.config.Host, "/") + path)
	if err != nil {
		return nil, usage("Invalid cluster request path.")
	}
	u.RawQuery = options.Query.Encode()
	var reader io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			reader = strings.NewReader(s)
		} else {
			data, e := json.Marshal(body)
			if e != nil {
				return nil, usage("Cannot encode resource.")
			}
			reader = bytes.NewReader(data)
		}
	}
	requestCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(requestCtx, method, u.String(), reader)
	if err != nil {
		cancel()
		return nil, usage("Invalid cluster request.")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		contentType := options.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	timer := time.AfterFunc(30*time.Second, cancel)
	resp, err := c.http.Do(req)
	timedOut := !timer.Stop()
	if err != nil {
		cancel()
		if timedOut && ctx.Err() == nil {
			return nil, cliError(4, "TIMEOUT", "Cluster request canceled or timed out.")
		}
		return nil, connectionError(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		cancel()
		code := resp.StatusCode
		exit := 1
		message := fmt.Sprintf("Cluster request failed with HTTP %d.", code)
		switch code {
		case 401:
			message = "Cluster credentials were rejected."
			exit = 3
		case 403:
			message = "The cluster denied this operation. Check your permissions and admission policies."
			exit = 3
		case 404:
			message = "The requested resource or API is not installed."
		case 409:
			message = "The resource already exists or changed concurrently. Read it again before updating."
			exit = 5
		case 422:
			message = "The cluster rejected the resource. Check its fields and provider compatibility."
		}
		return nil, cliError(exit, fmt.Sprintf("HTTP_%d", code), message)
	}
	resp.Body = &cancelBody{resp.Body, cancel}
	return resp, nil
}
func (c *KubernetesClient) Request(ctx context.Context, method, path string, body any, options RequestOptions) (Object, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.Raw(requestCtx, method, path, body, options)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024+1))
	if err != nil {
		return nil, connectionError(requestCtx, err)
	}
	if len(data) == 0 {
		return Object{}, nil
	}
	var value Object
	if len(data) > 32*1024*1024 || decodeJSON(data, &value) != nil || value == nil {
		return nil, cliError(1, "RESPONSE", "The cluster returned an invalid JSON response.")
	}
	return value, nil
}
func (c *KubernetesClient) List(ctx context.Context, t ResourceType, namespace string, query url.Values) ([]Object, error) {
	items := []Object{}
	q := url.Values{}
	for k, v := range query {
		q[k] = append([]string(nil), v...)
	}
	q.Set("limit", "500")
	seen := map[string]bool{}
	for {
		page, err := c.Request(ctx, http.MethodGet, resourcePath(t, namespace, ""), nil, RequestOptions{Query: q})
		if err != nil {
			return nil, err
		}
		raw, ok := page["items"].([]any)
		if !ok {
			return nil, cliError(1, "RESPONSE", "The cluster returned an invalid resource list.")
		}
		for _, v := range raw {
			obj, ok := v.(map[string]any)
			if !ok {
				return nil, cliError(1, "RESPONSE", "The cluster returned an invalid resource list.")
			}
			// Kubernetes typed lists omit TypeMeta on individual items.
			// Preserve explicit values so callers can still reject mismatches.
			if _, exists := obj["kind"]; !exists {
				obj["kind"] = t.Kind
			}
			if _, exists := obj["apiVersion"]; !exists {
				obj["apiVersion"] = t.Version
				if t.Group != "" {
					obj["apiVersion"] = t.Group + "/" + t.Version
				}
			}
			items = append(items, obj)
		}
		next := stringAt(page, "metadata", "continue")
		if next == "" {
			return items, nil
		}
		if seen[next] {
			return nil, cliError(1, "RESPONSE", "The cluster repeated a pagination token.")
		}
		seen[next] = true
		q.Set("continue", next)
	}
}
func (c *KubernetesClient) Get(ctx context.Context, t ResourceType, namespace, name string) (Object, error) {
	return c.Request(ctx, http.MethodGet, resourcePath(t, namespace, name), nil, RequestOptions{})
}
func writeQuery(dry bool) url.Values {
	q := url.Values{"fieldManager": {"airunway-cli"}, "fieldValidation": {"Strict"}}
	if dry {
		q.Set("dryRun", "All")
	}
	return q
}
func (c *KubernetesClient) Create(ctx context.Context, resource Object, dry bool) (Object, error) {
	t, err := typeFor(resource)
	if err != nil {
		return nil, err
	}
	ns := stringAt(resource, "metadata", "namespace")
	if ns == "" {
		ns = c.namespace
	}
	return c.Request(ctx, http.MethodPost, resourcePath(t, ns, ""), resource, RequestOptions{Query: writeQuery(dry)})
}
func (c *KubernetesClient) Patch(ctx context.Context, t ResourceType, namespace, name string, patch any, dry bool) (Object, error) {
	return c.Request(ctx, http.MethodPatch, resourcePath(t, namespace, name), patch, RequestOptions{Query: writeQuery(dry), ContentType: "application/merge-patch+json"})
}
func (c *KubernetesClient) Delete(ctx context.Context, t ResourceType, namespace, name, uid string) error {
	body := Object{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Background"}
	if uid != "" {
		body["preconditions"] = Object{"uid": uid}
	}
	_, err := c.Request(ctx, http.MethodDelete, resourcePath(t, namespace, name), body, RequestOptions{})
	return err
}
