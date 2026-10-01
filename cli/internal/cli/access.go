package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var accessRouteType = ResourceType{"gateway.networking.k8s.io", "v1", "httproutes", "HTTPRoute", true}

func accessUnsupported(message string) error { return cliError(2, "UNSUPPORTED", message) }
func accessHasCode(err error, code string) bool {
	var e *CLIError
	return errors.As(err, &e) && e.Code == code
}
func accessContextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return cliError(4, "TIMEOUT", "Operation timed out. Increase --timeout or inspect logs and events.")
	}
	return cliError(130, "CANCELED", "Operation canceled.")
}
func accessDeadline(parent context.Context, flags Flags) (context.Context, context.CancelFunc, error) {
	d, err := parseDuration(flags.Text("timeout"))
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(parent, d)
	return ctx, cancel, nil
}

// Bound injected clients and credential plugins that do not honor context. The
// worker only reads; it must never write output or start follow-up operations.
func accessCall[T any](ctx context.Context, work func() (T, error)) (T, error) {
	var zero T
	if ctx.Err() != nil {
		return zero, accessContextError(ctx)
	}
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() { v, err := work(); done <- result{v, err} }()
	select {
	case <-ctx.Done():
		return zero, accessContextError(ctx)
	case r := <-done:
		if ctx.Err() != nil {
			return zero, accessContextError(ctx)
		}
		return r.value, r.err
	}
}
func accessGet(ctx context.Context, client ClusterClient, t ResourceType, ns, name string) (Object, error) {
	return accessCall(ctx, func() (Object, error) { return client.Get(ctx, t, ns, name) })
}
func accessList(ctx context.Context, client ClusterClient, t ResourceType, ns string, query url.Values) ([]Object, error) {
	return accessCall(ctx, func() ([]Object, error) { return client.List(ctx, t, ns, query) })
}
func accessNamespace(resource Object, fallback string) string {
	if ns := stringAt(resource, "metadata", "namespace"); ns != "" {
		return ns
	}
	if fallback != "" {
		return fallback
	}
	return "default"
}

const (
	conditionFalse  = "False"
	conditionTrue   = "True"
	conditionFailed = "Failed"
)

func waitFailure(resource Object) error {
	message := "The resource failed."
	generation := intAt(resource, "metadata", "generation")
	for _, condition := range objects(get(resource, "status", "conditions")) {
		if intAt(condition, "observedGeneration") < generation {
			continue
		}
		failed := stringAt(condition, "status") == conditionFalse ||
			(stringAt(condition, "type") == conditionFailed && stringAt(condition, "status") == conditionTrue)
		if reason := stringAt(condition, "reason"); failed && reason != "" {
			message = "The resource failed: " + textCell(reason) + "."
			break
		}
	}
	return cliError(1, "FAILED", message+" Inspect its logs and events for details.")
}

func waitForResource(parent context.Context, client ClusterClient, noun string, resource Object, flags Flags, _ *IO) (Object, error) {
	target := strings.ToLower(flags.Text("for"))
	if target == "" {
		target = "ready"
	}
	if (noun != "model" && noun != "agent") || (target != "ready" && !(target == "completed" && noun == "agent")) {
		return nil, usage("Use --for ready, or --for completed for an agent job.")
	}
	ctx, cancel, err := accessDeadline(parent, flags)
	if err != nil {
		return nil, err
	}
	defer cancel()
	uid := stringAt(resource, "metadata", "uid")
	current := resource
	for {
		if ctx.Err() != nil {
			return nil, accessContextError(ctx)
		}
		if get(current, "metadata", "deletionTimestamp") != nil || (uid != "" && stringAt(current, "metadata", "uid") != uid) {
			return nil, cliError(1, "DELETED", "The resource was deleted or replaced while waiting.")
		}
		generation := intAt(current, "metadata", "generation")
		fresh := generation > 0 && intAt(current, "status", "observedGeneration") >= generation
		phase := stringAt(current, "status", "phase")
		if fresh && (phase == "Failed" || phase == "Error") {
			return nil, waitFailure(current)
		}
		ready, completed := false, false
		for _, condition := range objects(get(current, "status", "conditions")) {
			if generation <= 0 || intAt(condition, "observedGeneration") < generation {
				continue
			}
			kind, status, reason := stringAt(condition, "type"), stringAt(condition, "status"), stringAt(condition, "reason")
			phaseOwner := kind == "ProviderReady"
			if noun == "model" {
				phaseOwner = slices.Contains([]string{"Ready", "Validated", "ProviderCompatible", "ResourceCreated"}, kind)
			}
			if (kind == "Failed" && status == "True") || (status == "False" && (reason == "JobFailed" || ((phase == "Failed" || phase == "Error") && phaseOwner))) {
				return nil, waitFailure(current)
			}
			ready = ready || (kind == "Ready" && status == "True")
			completed = completed || (noun == "agent" && phase == "Completed" && status == "True" && (kind == "Ready" || kind == "Completed" || (kind == "ProviderReady" && reason == "JobCompleted")))
		}
		if fresh && (completed || (target == "ready" && ready)) {
			return current, nil
		}
		if err := pause(ctx, 250*time.Millisecond); err != nil {
			return nil, accessContextError(ctx)
		}
		current, err = accessGet(ctx, client, resourceTypes[noun], accessNamespace(resource, flags.Text("namespace")), stringAt(resource, "metadata", "name"))
		if accessHasCode(err, "HTTP_404") {
			return nil, cliError(1, "DELETED", "The resource was deleted while waiting.")
		}
		if err != nil {
			return nil, err
		}
	}
}

type accessEndpoint struct {
	URL                *url.URL
	Access             string
	Headers            map[string]string
	ServedModelName    string
	Service            Object
	ServicePort        int
	AuthSecretRef      Object
	RequiresKnownModel bool
}

func accessSafeURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u == nil || u.Hostname() == "" || u.Opaque != "" {
		return nil, accessUnsupported("The published address is not an absolute HTTP or HTTPS URL.")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(value, "#") {
		return nil, accessUnsupported("The endpoint must be an HTTP or HTTPS URL without credentials, query parameters, or fragments.")
	}
	if strings.ContainsAny(u.Host, "\\\r\n\t ") || strings.Contains(u.Hostname(), "%") {
		return nil, accessUnsupported("The endpoint hostname is invalid.")
	}
	if u.Port() != "" {
		p, err := strconv.Atoi(u.Port())
		if err != nil || p < 1 || p > 65535 {
			return nil, accessUnsupported("The endpoint port is invalid.")
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u, nil
}
func accessAddressURL(address, scheme string, port int) (*url.URL, error) {
	if strings.ContainsAny(address, "/@?#\\\r\n\t ") || port < 1 || port > 65535 {
		return nil, accessUnsupported("The published endpoint address or port is invalid.")
	}
	return accessSafeURL(scheme + "://" + net.JoinHostPort(strings.Trim(address, "[]"), strconv.Itoa(port)))
}
func accessServicePort(service Object, requested int) (Object, error) {
	ports := []Object{}
	for _, p := range objects(get(service, "spec", "ports")) {
		if protocol := stringAt(p, "protocol"); protocol == "" || protocol == "TCP" {
			ports = append(ports, p)
		}
	}
	matches := []Object{}
	for _, p := range ports {
		if requested == 0 || intAt(p, "port") == int64(requested) {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 && requested != 0 {
		for _, p := range ports {
			if _, named := p["targetPort"].(string); !named && intAt(p, "targetPort") == int64(requested) {
				matches = append(matches, p)
			}
		}
	}
	if len(matches) != 1 || intAt(matches[0], "port") < 1 || intAt(matches[0], "port") > 65535 {
		return nil, accessUnsupported(fmt.Sprintf("Cannot choose a unique port on Service %s. Check its ports and the published endpoint.", stringAt(service, "metadata", "name")))
	}
	return matches[0], nil
}
func accessPortScheme(port Object) string {
	switch stringAt(port, "appProtocol") {
	case "https":
		return "https"
	case "http", "kubernetes.io/h2c":
		return "http"
	}
	name := stringAt(port, "name")
	if name == "https" || strings.HasPrefix(name, "https-") || intAt(port, "port") == 443 {
		return "https"
	}
	return "http"
}

var accessServiceDNS = regexp.MustCompile(`^([a-z0-9-]+)\.([a-z0-9-]+)\.svc(?:\.cluster\.local)?\.?$`)
var accessShortServiceDNS = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func accessAddressService(ctx context.Context, client ClusterClient, u *url.URL, ns string) (Object, error) {
	if match := accessServiceDNS.FindStringSubmatch(u.Hostname()); match != nil {
		return accessGet(ctx, client, resourceTypes["service"], match[2], match[1])
	}
	if accessShortServiceDNS.MatchString(u.Hostname()) {
		s, err := accessGet(ctx, client, resourceTypes["service"], ns, u.Hostname())
		if !accessHasCode(err, "HTTP_404") {
			return s, err
		}
	}
	return nil, nil
}
func accessOwnedBy(resource, owner Object) bool {
	uid := stringAt(owner, "metadata", "uid")
	if uid == "" {
		return false
	}
	for _, ref := range objects(get(resource, "metadata", "ownerReferences")) {
		if stringAt(ref, "uid") == uid {
			return true
		}
	}
	return false
}
func accessServedModelName(ctx context.Context, client ClusterClient, resource Object) (string, error) {
	if stringAt(resource, "spec", "gateway", "modelName") == "" {
		if resolved := stringAt(resource, "status", "gateway", "modelName"); resolved != "" {
			return resolved, nil
		}
	}
	declared := stringAt(resource, "spec", "model", "servedName")
	if declared == "" {
		return "", nil
	}
	provider := stringAt(resource, "status", "provider", "name")
	if provider == "" {
		provider = stringAt(resource, "spec", "provider", "name")
	}
	engine := stringAt(resource, "status", "engine", "type")
	if engine == "" {
		engine = stringAt(resource, "spec", "engine", "type")
	}
	if provider != "" && engine != "" {
		p, err := accessGet(ctx, client, resourceTypes["provider"], "", provider)
		if accessHasCode(err, "HTTP_404") || accessHasCode(err, "HTTP_403") {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		for _, capability := range objects(get(p, "spec", "capabilities", "engines")) {
			if stringAt(capability, "name") == engine {
				if boolAt(capability, "gateway", "ignoresServedName") {
					return "", nil
				}
				return declared, nil
			}
		}
		return "", nil
	}
	return declared, nil
}

// Parent status corresponds to the spec reference, including its listener/port
// constraints. Defaults apply only to omitted fields, not an explicit core group.
func accessRouteParentKey(ref Object, namespace string) [6]string {
	group, kind := accessRouteType.Group, "Gateway"
	if ref["group"] != nil {
		group = stringAt(ref, "group")
	}
	if ref["kind"] != nil {
		kind = stringAt(ref, "kind")
	}
	if ref["namespace"] != nil {
		namespace = stringAt(ref, "namespace")
	}
	return [6]string{group, kind, namespace, stringAt(ref, "name"), stringAt(ref, "sectionName"), strconv.FormatInt(intAt(ref, "port"), 10)}
}

func accessRouteParentCurrent(route, parent Object, namespace string) bool {
	generation := intAt(route, "metadata", "generation")
	if generation <= 0 {
		return false
	}
	key := accessRouteParentKey(parent, namespace)
	if key[0] != accessRouteType.Group || key[1] != "Gateway" {
		return false
	}
	matched := false
	for _, status := range objects(get(route, "status", "parents")) {
		if accessRouteParentKey(object(status["parentRef"]), namespace) != key {
			continue
		}
		matched = true
		accepted, resolved := false, false
		for _, condition := range objects(status["conditions"]) {
			kind := stringAt(condition, "type")
			if kind != "Accepted" && kind != "ResolvedRefs" {
				continue
			}
			if stringAt(condition, "status") != "True" || intAt(condition, "observedGeneration") < generation {
				return false
			}
			accepted = accepted || kind == "Accepted"
			resolved = resolved || kind == "ResolvedRefs"
		}
		if !accepted || !resolved {
			return false
		}
	}
	return matched
}

// Discovery must use the same path and routing headers as chat. A separate
// GET match with otherwise identical constraints also supports that request.
func accessRouteSupportsGET(route, selected Object) bool {
	if stringAt(selected, "method") == "" || stringAt(selected, "method") == "GET" {
		return true
	}
	selected = cloneObject(selected)
	delete(selected, "method")
	for _, rule := range objects(get(route, "spec", "rules")) {
		for _, match := range objects(rule["matches"]) {
			if stringAt(match, "method") == "GET" {
				match = cloneObject(match)
				delete(match, "method")
				if reflect.DeepEqual(match, selected) {
					return true
				}
			}
		}
	}
	return false
}

func accessGatewayEndpoint(ctx context.Context, client ClusterClient, resource Object, flags Flags, fallback string, requireService bool) (*accessEndpoint, error) {
	status := object(get(resource, "status", "gateway"))
	ns := stringAt(status, "gatewayNamespace")
	if ns == "" {
		ns = accessNamespace(resource, fallback)
	}
	gateway, err := accessGet(ctx, client, resourceTypes["gateway"], ns, stringAt(status, "gatewayName"))
	if err != nil {
		return nil, err
	}
	// Both route variants have a known name in the model namespace. Only the
	// managed route requires ownership; neither needs namespace-wide list access.
	routeName := stringAt(resource, "spec", "gateway", "httpRouteRef")
	managed := routeName == ""
	if managed {
		routeName = stringAt(resource, "metadata", "name")
	}
	route, err := accessGet(ctx, client, accessRouteType, accessNamespace(resource, fallback), routeName)
	if err != nil {
		return nil, err
	}
	method := "POST"
	if flags.Bool("check") {
		method = "GET"
	}
	type candidate struct{ route, listener, match Object }
	candidates := []candidate{}
	if managed && !accessOwnedBy(route, resource) {
		return nil, accessUnsupported("The managed HTTPRoute is not owned by this ModelDeployment.")
	}
	for _, parent := range objects(get(route, "spec", "parentRefs")) {
		if !accessRouteParentCurrent(route, parent, accessNamespace(route, fallback)) {
			continue
		}
		parentNS := stringAt(parent, "namespace")
		if parentNS == "" {
			parentNS = accessNamespace(route, fallback)
		}
		if stringAt(parent, "name") != stringAt(gateway, "metadata", "name") || parentNS != ns || (stringAt(parent, "kind") != "" && stringAt(parent, "kind") != "Gateway") || (stringAt(parent, "group") != "" && stringAt(parent, "group") != accessRouteType.Group) {
			continue
		}
		for _, listener := range objects(get(gateway, "spec", "listeners")) {
			protocol, name := stringAt(listener, "protocol"), stringAt(listener, "name")
			if (protocol != "HTTP" && protocol != "HTTPS") || (stringAt(parent, "sectionName") != "" && stringAt(parent, "sectionName") != name) || (intAt(parent, "port") != 0 && intAt(parent, "port") != intAt(listener, "port")) || (flags.Text("gateway-listener") != "" && flags.Text("gateway-listener") != name) {
				continue
			}
			for _, rule := range objects(get(route, "spec", "rules")) {
				matches := objects(rule["matches"])
				if len(matches) == 0 {
					matches = []Object{{}}
				}
				for _, match := range matches {
					if (stringAt(match, "method") != "" && stringAt(match, "method") != method) ||
						len(array(match["queryParams"])) > 0 ||
						(stringAt(match, "path", "type") != "" && stringAt(match, "path", "type") != "PathPrefix") {
						continue
					}
					usable := true
					for _, h := range objects(match["headers"]) {
						if stringAt(h, "type") != "" && stringAt(h, "type") != "Exact" {
							usable = false
						}
					}
					if usable {
						candidates = append(candidates, candidate{route, listener, match})
					}
				}
			}
		}
	}
	if len(candidates) == 0 {
		return nil, accessUnsupported("No current, accepted HTTPRoute with resolved references and a matching HTTP(S) listener was found for this model. Inspect its gateway route.")
	}
	if len(candidates) != 1 {
		return nil, accessUnsupported("The gateway has multiple matching routes or listeners. Select one with --gateway-listener or simplify the route.")
	}
	c := candidates[0]
	headers := map[string]string{}
	for _, h := range objects(c.match["headers"]) {
		key, value := strings.ToLower(stringAt(h, "name")), stringAt(h, "value")
		if !accessHeaderName.MatchString(key) || strings.ContainsAny(value, "\r\n") || slices.Contains([]string{"authorization", "proxy-authorization", "cookie", "connection", "content-length", "transfer-encoding", "host"}, key) {
			return nil, accessUnsupported("This route requires an unsupported credential or transport header.")
		}
		headers[key] = value
	}
	hosts := array(get(c.route, "spec", "hostnames"))
	hostname := ""
	if len(hosts) > 1 {
		return nil, accessUnsupported("The route needs an explicit hostname. Use a single concrete HTTPRoute hostname.")
	}
	if len(hosts) == 1 {
		hostname, _ = hosts[0].(string)
		if hostname == "" || strings.Contains(hostname, "*") {
			return nil, accessUnsupported("The route needs an explicit hostname. Use a single concrete HTTPRoute hostname.")
		}
	} else if h := stringAt(c.listener, "hostname"); !strings.Contains(h, "*") {
		hostname = h
	}
	if hostname != "" {
		u, err := accessSafeURL("https://" + hostname)
		if err != nil || u.Hostname() != hostname || u.Port() != "" || u.Path != "/" {
			return nil, accessUnsupported("The route hostname is invalid.")
		}
		headers["host"] = hostname
	}
	scheme := strings.ToLower(stringAt(c.listener, "protocol"))
	port := int(intAt(c.listener, "port"))
	e := &accessEndpoint{
		Access: "gateway", Headers: headers, ServicePort: port,
		RequiresKnownModel: !accessRouteSupportsGET(c.route, c.match),
	}
	e.ServedModelName, err = accessServedModelName(ctx, client, resource)
	if err != nil {
		return nil, err
	}
	for _, address := range objects(get(gateway, "status", "addresses")) {
		if slices.Contains([]string{"", "IPAddress", "Hostname"}, stringAt(address, "type")) && stringAt(address, "value") != "" {
			e.URL, err = accessAddressURL(stringAt(address, "value"), scheme, port)
			if err != nil {
				return nil, err
			}
			break
		}
	}
	services, err := accessList(ctx, client, resourceTypes["service"], ns, nil)
	if err != nil {
		// A published address remains usable without permission to discover
		// the Gateway's Service. Tunnels and internal fallback still require it.
		if requireService || e.URL == nil || !accessHasCode(err, "HTTP_403") {
			return nil, err
		}
		services = nil
	}
	usable := []Object{}
	gatewayName := stringAt(gateway, "metadata", "name")
	for _, s := range services {
		labels := object(get(s, "metadata", "labels"))
		matching := accessOwnedBy(s, gateway) || stringAt(labels, "gateway.networking.k8s.io/gateway-name") == gatewayName || stringAt(labels, "istio.io/gateway-name") == gatewayName || (stringAt(labels, "gateway.envoyproxy.io/owning-gateway-name") == gatewayName && stringAt(labels, "gateway.envoyproxy.io/owning-gateway-namespace") == ns)
		if !matching {
			continue
		}
		for _, p := range objects(get(s, "spec", "ports")) {
			if intAt(p, "port") == int64(port) && (stringAt(p, "protocol") == "" || stringAt(p, "protocol") == "TCP") {
				usable = append(usable, s)
				break
			}
		}
	}
	if len(usable) == 1 {
		e.Service = usable[0]
		if e.URL == nil {
			e.URL, err = accessAddressURL(stringAt(e.Service, "metadata", "name")+"."+ns+".svc", scheme, port)
			if err != nil {
				return nil, err
			}
			e.Access = "internal"
		}
	}
	if e.URL == nil {
		return nil, accessUnsupported("The gateway has no published address or uniquely identified Service. Inspect the Gateway and its Service labels.")
	}
	path := stringAt(c.match, "path", "value")
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n?#") {
		return nil, accessUnsupported("The route path is invalid.")
	}
	e.URL.Path = path
	return e, nil
}

var accessHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9a-z-]+$")

func accessResolveEndpoint(ctx context.Context, client ClusterClient, noun string, resource Object, flags Flags, fallback string, requireService bool) (*accessEndpoint, error) {
	ns := accessNamespace(resource, fallback)
	if noun == "agent" {
		if stringAt(resource, "spec", "lifecycle") == "job" || stringAt(resource, "status", "runtime", "workloadRef", "kind") == "Job" {
			return nil, accessUnsupported("Agent jobs do not expose endpoints. Use agent wait --for completed, logs, or events.")
		}
		runtime := object(get(resource, "status", "runtime"))
		address := stringAt(runtime, "address")
		if address == "" {
			return nil, accessUnsupported("This agent provider has not published status.runtime.address. For kagent or Orka, use the upstream operator's access tools; use a container-backed provider for CLI chat and connect.")
		}
		u, err := accessSafeURL(address)
		if err != nil {
			return nil, err
		}
		if u.Scheme == "https" && net.ParseIP(u.Hostname()) != nil {
			return nil, accessUnsupported("Agent HTTPS endpoints must publish a DNS hostname matching their certificate. TLS hostname fallback for IP addresses is not supported.")
		}
		service, err := accessAddressService(ctx, client, u, ns)
		if err != nil {
			return nil, err
		}
		e := &accessEndpoint{URL: u, Access: "external", Headers: map[string]string{}, Service: service}
		if runtime["authSecretRef"] != nil {
			ref := object(runtime["authSecretRef"])
			e.AuthSecretRef = Object{"name": stringAt(ref, "name"), "key": stringAt(ref, "key")}
		}
		if service != nil {
			port := 80
			if u.Scheme == "https" {
				port = 443
			}
			if u.Port() != "" {
				port, _ = strconv.Atoi(u.Port())
			}
			p, err := accessServicePort(service, port)
			if err != nil {
				return nil, err
			}
			e.ServicePort = int(intAt(p, "port"))
			e.Access = "internal"
		}
		return e, nil
	}
	if flags.Text("gateway") != "false" && stringAt(resource, "status", "gateway", "gatewayName") != "" {
		return accessGatewayEndpoint(ctx, client, resource, flags, fallback, requireService)
	}
	if flags.Bool("gateway") {
		return nil, accessUnsupported("The model has not published a gateway reference. Inspect gateway status or use --gateway=false for its internal Service.")
	}
	published := object(get(resource, "status", "endpoint"))
	if stringAt(published, "service") == "" {
		return nil, accessUnsupported("The model has not published an endpoint Service. Wait for readiness or inspect its provider; no Service name can be inferred safely.")
	}
	service, err := accessGet(ctx, client, resourceTypes["service"], ns, stringAt(published, "service"))
	if err != nil {
		return nil, err
	}
	port, err := accessServicePort(service, int(intAt(published, "port")))
	if err != nil {
		return nil, err
	}
	u, err := accessAddressURL(stringAt(service, "metadata", "name")+"."+accessNamespace(service, ns)+".svc", accessPortScheme(port), int(intAt(port, "port")))
	if err != nil {
		return nil, err
	}
	model, err := accessServedModelName(ctx, client, resource)
	if err != nil {
		return nil, err
	}
	return &accessEndpoint{URL: u, Access: "internal", Headers: map[string]string{}, Service: service, ServicePort: int(intAt(port, "port")), ServedModelName: model}, nil
}
func accessEndpointView(e *accessEndpoint, resource Object) Object {
	v := Object{"name": stringAt(resource, "metadata", "name"), "namespace": stringAt(resource, "metadata", "namespace"), "url": e.URL.String(), "access": e.Access, "headers": e.Headers, "authRequired": e.AuthSecretRef != nil}
	if e.ServedModelName != "" {
		v["servedModelName"] = e.ServedModelName
	}
	if e.AuthSecretRef != nil {
		v["authSecretRef"] = e.AuthSecretRef
	}
	if e.Service != nil {
		v["service"] = Object{"name": stringAt(e.Service, "metadata", "name"), "namespace": stringAt(e.Service, "metadata", "namespace"), "port": e.ServicePort}
	}
	return v
}

func runAccess(noun, action, name string, c *CommandContext) error {
	if noun != "model" && noun != "agent" {
		return usage("Unknown resource type.")
	}
	allowed := map[string][]string{
		"endpoint": strings.Fields("gateway gateway-listener check server credential"), "connect": strings.Fields("gateway gateway-listener port"),
		"chat": strings.Fields("gateway gateway-listener server message message-file temperature max-tokens credential"),
		"logs": strings.Fields("pod container follow tail timestamps"), "events": {},
	}
	options, ok := allowed[action]
	if !ok {
		return usage("Unknown access action.")
	}
	if noun == "model" && action == actionChat {
		options = append(options, "system", "system-file")
	}
	if err := assertFlags(c.Flags, options); err != nil {
		return err
	}
	if action == "endpoint" && c.Flags.Has("credential") && (noun != "model" || !c.Flags.Bool("check")) {
		return usage("--credential is only supported for model endpoint --check.")
	}
	if err := validateName(name, "name"); err != nil {
		return err
	}
	session := action == "connect" || (action == "logs" && c.Flags.Bool("follow")) ||
		(action == "chat" && c.IO.Interactive && !c.Flags.Has("message") && !c.Flags.Has("message-file"))
	var ctx context.Context
	var cancel context.CancelFunc
	var err error
	if session && !c.Flags.Has("timeout") {
		// Keep cancellation and any caller deadline, without imposing a finite
		// operation's default timeout on an open-ended session.
		ctx, cancel = context.WithCancel(c.Context)
	} else {
		ctx, cancel, err = accessDeadline(c.Context, c.Flags)
		if err != nil {
			return err
		}
	}
	defer cancel()
	client, err := accessCall(ctx, c.Client)
	if err != nil {
		return err
	}
	resource, err := accessGet(ctx, client, resourceTypes[noun], c.Namespace, name)
	if err != nil {
		return err
	}
	scoped := *c
	scoped.Context = ctx
	if action == "logs" {
		return accessLogs(client, noun, resource, &scoped)
	}
	if action == "events" {
		return accessEvents(client, resource, &scoped)
	}
	requireService := action == "connect" || ((action == "chat" || c.Flags.Bool("check")) && c.Flags.Text("server") == "")
	e, err := accessResolveEndpoint(ctx, client, noun, resource, c.Flags, c.Namespace, requireService)
	if err != nil {
		return err
	}
	if action == "chat" && e.RequiresKnownModel && e.ServedModelName == "" {
		return accessUnsupported("This gateway route does not support GET model discovery. " +
			"Publish a resolved served model name for POST-only chat, or use a route that also supports GET.")
	}
	switch action {
	case "endpoint":
		if c.Flags.Bool("check") {
			connection, err := accessTransport(ctx, client, e, c.Flags)
			if err != nil {
				return err
			}
			defer connection.Close()
			path, token := "/v1/models", ""
			if noun == "agent" {
				// Agent readiness is deliberately unauthenticated.
				path = "/readyz"
			} else {
				token, err = accessIngressToken(ctx, client, noun, resource, e, connection, c.Flags, c.Namespace)
				if err != nil {
					return err
				}
			}
			if _, err := accessHTTPJSON(ctx, connection, path, nil, token, noun == "model"); err != nil {
				return err
			}
		}
		v := accessEndpointView(e, resource)
		if c.Flags.Bool("check") {
			v["reachable"] = true
		}
		return writeOutput(c.IO, c.Flags, v)
	case "connect":
		port, err := integer(c.Flags, "port", 0, 0, 65535)
		if err != nil {
			return err
		}
		tunnel, err := accessOpenTunnel(ctx, client, e, int(port))
		if err != nil {
			return err
		}
		defer tunnel.Close()
		local := *e.URL
		loopback := net.JoinHostPort("127.0.0.1", strconv.Itoa(tunnel.Port))
		v := accessEndpointView(e, resource)
		if local.Scheme == "https" {
			// A loopback URL would change certificate verification and SNI. Keep
			// the TLS identity and advertise the separate network mapping instead.
			host, port := accessTLSHost(e), local.Port()
			if port == "" {
				port = "443"
				local.Host = host
				if strings.Contains(host, ":") {
					local.Host = "[" + host + "]"
				}
			} else {
				local.Host = net.JoinHostPort(host, port)
			}
			v["connectTo"] = net.JoinHostPort(host, port) + ":" + loopback
		} else {
			local.Host = loopback
		}
		v["url"], v["upstream"], v["access"] = local.String(), e.URL.String(), "loopback"
		if err := writeOutput(c.IO, c.Flags, v); err != nil {
			return err
		}
		if e.AuthSecretRef != nil {
			progress(c, "This raw tunnel preserves upstream authentication. Read the ingress Secret separately if authorized; no token is printed or injected.")
		}
		select {
		case <-ctx.Done():
			return accessContextError(ctx)
		case <-tunnel.Done:
			return cliError(1, "CONNECTION", "The port-forward connection closed.")
		}
	case "chat":
		return accessChat(client, noun, resource, e, &scoped)
	}
	return nil
}
