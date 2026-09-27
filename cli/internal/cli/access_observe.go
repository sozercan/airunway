package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Resolve custom owner kinds through discovery rather than guessing plurals.
func accessReferenceType(ctx context.Context, client ClusterClient, kind, apiVersion string) (ResourceType, error) {
	known := []ResourceType{{"apps", "v1", "deployments", "Deployment", true}, {"apps", "v1", "replicasets", "ReplicaSet", true}, {"apps", "v1", "statefulsets", "StatefulSet", true}, {"batch", "v1", "jobs", "Job", true}}
	for _, t := range resourceTypes {
		known = append(known, t)
	}
	for _, t := range known {
		version := t.Version
		if t.Group != "" {
			version = t.Group + "/" + version
		}
		if t.Kind == kind && (apiVersion == "" || apiVersion == version) {
			return t, nil
		}
	}
	versions := []string{apiVersion}
	if apiVersion == "" {
		discovery, err := accessCall(ctx, func() (Object, error) { return client.Request(ctx, "GET", "/apis", nil, RequestOptions{}) })
		if err != nil {
			return ResourceType{}, err
		}
		versions = nil
		for _, group := range objects(discovery["groups"]) {
			versions = append(versions, stringAt(group, "preferredVersion", "groupVersion"))
		}
	}
	for _, version := range versions {
		parts := strings.Split(version, "/")
		if len(parts) < 1 || len(parts) > 2 {
			return ResourceType{}, accessUnsupported("The workload API version is invalid.")
		}
		for _, part := range parts {
			if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "?#%\\") {
				return ResourceType{}, accessUnsupported("The workload API version is invalid.")
			}
		}
		path := "/api/" + url.PathEscape(version)
		if len(parts) == 2 {
			path = "/apis/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
		}
		discovery, err := accessCall(ctx, func() (Object, error) { return client.Request(ctx, "GET", path, nil, RequestOptions{}) })
		if err != nil {
			return ResourceType{}, err
		}
		for _, r := range objects(discovery["resources"]) {
			name := stringAt(r, "name")
			if stringAt(r, "kind") == kind && name != "" && !strings.Contains(name, "/") {
				group := ""
				if len(parts) == 2 {
					group = parts[0]
				}
				return ResourceType{group, parts[len(parts)-1], name, kind, boolAt(r, "namespaced")}, nil
			}
		}
	}
	return ResourceType{}, accessUnsupported(fmt.Sprintf("The API for workload kind %s is not installed or discoverable.", kind))
}
func accessWorkload(ctx context.Context, client ClusterClient, noun string, resource Object, fallback string) (Object, error) {
	ref := object(get(resource, "status", "runtime", "workloadRef"))
	if noun == "model" {
		ref = object(get(resource, "status", "workloadRef"))
		if len(ref) == 0 {
			provider := object(get(resource, "status", "provider"))
			ref = Object{"name": provider["resourceName"], "kind": provider["resourceKind"], "apiVersion": provider["apiVersion"]}
		}
	}
	if stringAt(ref, "name") == "" || stringAt(ref, "kind") == "" {
		return nil, accessUnsupported("The provider has not published a workload reference. Inspect the resource status and provider.")
	}
	t, err := accessReferenceType(ctx, client, stringAt(ref, "kind"), stringAt(ref, "apiVersion"))
	if err != nil {
		return nil, err
	}
	ns := stringAt(ref, "namespace")
	if ns == "" {
		ns = accessNamespace(resource, fallback)
	}
	if !t.Namespaced {
		ns = ""
	}
	return accessGet(ctx, client, t, ns, stringAt(ref, "name"))
}
func accessDescendsFrom(ctx context.Context, client ClusterClient, child, root Object, cache map[string]Object, seen map[string]bool, fallback string) (bool, error) {
	uid := stringAt(root, "metadata", "uid")
	if uid == "" {
		return false, nil
	}
	if stringAt(child, "metadata", "uid") == uid {
		return true, nil
	}
	for _, ref := range objects(get(child, "metadata", "ownerReferences")) {
		ownerUID := stringAt(ref, "uid")
		if ownerUID == uid {
			return true, nil
		}
		if ownerUID == "" || seen[ownerUID] || len(seen) >= 32 {
			continue
		}
		seen[ownerUID] = true
		parent, exists := cache[ownerUID]
		if !exists {
			t, err := accessReferenceType(ctx, client, stringAt(ref, "kind"), stringAt(ref, "apiVersion"))
			if err != nil {
				return false, err
			}
			ns := accessNamespace(child, fallback)
			if !t.Namespaced {
				ns = ""
			}
			parent, err = accessGet(ctx, client, t, ns, stringAt(ref, "name"))
			if err != nil && !accessHasCode(err, "HTTP_404") {
				return false, err
			}
			if err != nil || stringAt(parent, "metadata", "uid") != ownerUID {
				parent = nil
			}
			cache[ownerUID] = parent
		}
		if parent != nil {
			owned, err := accessDescendsFrom(ctx, client, parent, root, cache, seen, fallback)
			if err != nil || owned {
				return owned, err
			}
		}
	}
	return false, nil
}
func accessLogs(client ClusterClient, noun string, resource Object, c *CommandContext) error {
	ctx := c.Context
	root, err := accessWorkload(ctx, client, noun, resource, c.Namespace)
	if err != nil {
		return err
	}
	if stringAt(root, "metadata", "uid") == "" {
		return accessUnsupported("The backing workload has no UID; pod ownership cannot be verified.")
	}
	ns := accessNamespace(root, c.Namespace)
	var candidates []Object
	if name := c.Flags.Text("pod"); name != "" {
		p, e := accessGet(ctx, client, resourceTypes["pod"], ns, name)
		if e != nil {
			return e
		}
		candidates = []Object{p}
	} else {
		candidates, err = accessList(ctx, client, resourceTypes["pod"], ns, nil)
		if err != nil {
			return err
		}
	}
	cache := map[string]Object{}
	pods := []Object{}
	for _, p := range candidates {
		owned, e := accessDescendsFrom(ctx, client, p, root, cache, map[string]bool{}, ns)
		if e != nil {
			return e
		}
		if owned {
			pods = append(pods, p)
		}
	}
	if len(pods) > 1 {
		return accessUnsupported("More than one pod belongs to this workload. Select one with --pod.")
	}
	if len(pods) == 0 {
		return accessUnsupported("No owned pod was found. Choose a --pod belonging to the published workload.")
	}
	pod := pods[0]
	containers := map[string]bool{}
	for _, key := range []string{"containers", "initContainers", "ephemeralContainers"} {
		for _, container := range objects(get(pod, "spec", key)) {
			containers[stringAt(container, "name")] = true
		}
	}
	container := c.Flags.Text("container")
	if container == "" && len(containers) == 1 {
		for name := range containers {
			container = name
		}
	}
	if container == "" || !containers[container] {
		return usage("Select a --container that belongs to this pod.")
	}
	tail, err := integer(c.Flags, "tail", 100, 0, 1<<53-1)
	if err != nil {
		return err
	}
	query := url.Values{"container": {container}, "follow": {strconv.FormatBool(c.Flags.Bool("follow"))}, "tailLines": {strconv.FormatInt(tail, 10)}, "timestamps": {strconv.FormatBool(c.Flags.Bool("timestamps"))}}
	// Close a response that arrives after cancellation as well as an active body.
	response, err := accessCall(ctx, func() (*http.Response, error) {
		r, e := client.Raw(ctx, "GET", resourcePath(resourceTypes["pod"], ns, stringAt(pod, "metadata", "name"))+"/log", nil, RequestOptions{Query: query})
		if r != nil && r.Body != nil {
			context.AfterFunc(ctx, func() { r.Body.Close() })
		}
		return r, e
	})
	if err != nil {
		return err
	}
	if response == nil {
		return cliError(1, "LOGS", "Reading logs returned no response.")
	}
	if response.Body != nil {
		defer response.Body.Close()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return cliError(1, "LOGS", fmt.Sprintf("Reading logs failed with HTTP %d.", response.StatusCode))
	}
	if response.Body == nil {
		return nil
	}
	if output := c.Flags.Text("output"); output == "" || output == "text" {
		buffer := make([]byte, 32*1024)
		for {
			n, err := accessCall(ctx, func() (int, error) { return response.Body.Read(buffer) })
			if n > 0 {
				if _, e := c.IO.Out.Write(buffer[:n]); e != nil {
					return e
				}
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	if !c.Flags.Bool("follow") {
		// Structured output is a single string. Limit the raw bytes before
		// encoding it; ordinary text above streams without an aggregate limit.
		text, err := accessCall(ctx, func() ([]byte, error) {
			return io.ReadAll(io.LimitReader(response.Body, maxInput+1))
		})
		if err != nil {
			return err
		}
		if len(text) > maxInput {
			return cliError(1, "LOGS", "Log output exceeds 4 MiB for non-follow JSON/YAML output. Reduce --tail or use --output text to stream logs.")
		}
		return writeOutput(c.IO, c.Flags, string(text))
	}
	reader := bufio.NewReader(response.Body)
	for {
		line, err := accessCall(ctx, func() (string, error) { return accessReadLogLine(reader) })
		if line != "" {
			if e := writeOutput(c.IO, c.Flags, Object{"pod": stringAt(pod, "metadata", "name"), "container": container, "line": strings.TrimSuffix(line, "\n")}); e != nil {
				return e
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Structured follow output emits one record per line; cap a single line without
// limiting the length of the overall stream. Text output never buffers a line.
func accessReadLogLine(reader *bufio.Reader) (string, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > maxInput {
			return "", cliError(1, "LOGS", "Log line exceeds 4 MiB for JSON/YAML output. Use --output text to stream logs.")
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return string(line), err
	}
}

func accessEvents(client ClusterClient, resource Object, c *CommandContext) error {
	uid := stringAt(resource, "metadata", "uid")
	if uid == "" {
		return accessUnsupported("The resource has no UID; its events cannot be selected safely.")
	}
	events, err := accessList(c.Context, client, resourceTypes["event"], accessNamespace(resource, c.Namespace), url.Values{"fieldSelector": {"involvedObject.uid=" + uid}})
	if err != nil {
		return err
	}
	if output := c.Flags.Text("output"); output != "" && output != "text" {
		return writeOutput(c.IO, c.Flags, events)
	}
	rows := []Object{}
	for _, event := range events {
		timestamp := stringAt(event, "lastTimestamp")
		if timestamp == "" {
			timestamp = stringAt(event, "eventTime")
		}
		if timestamp == "" {
			timestamp = stringAt(event, "metadata", "creationTimestamp")
		}
		rows = append(rows, Object{"time": timestamp, "type": event["type"], "reason": event["reason"], "message": event["message"], "count": event["count"]})
	}
	return writeOutput(c.IO, c.Flags, rows)
}
