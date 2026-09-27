package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
)

var Version = "dev"
var GitCommit = "unknown"
var BuildTime = "unknown"

type RunOptions struct {
	IO     *IO
	Client ClusterClient
	Config *CLIConfig
}

// Run executes a command, returning the documented shell exit code.
func Run(ctx context.Context, argv []string, opts RunOptions) int {
	streams := opts.IO
	if streams == nil {
		streams = &IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Interactive: term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))}
	}
	if streams.In == nil {
		streams.In = strings.NewReader("")
	}
	if streams.Out == nil {
		streams.Out = io.Discard
	}
	if streams.Err == nil {
		streams.Err = io.Discard
	}
	flags := Flags{"output": {requestedOutput(argv)}}
	words, parsed, err := parseArgs(argv)
	if err == nil {
		flags = parsed
		err = run(ctx, words, flags, streams, opts)
	}
	if err == nil {
		return 0
	}
	var failure *CLIError
	if !errors.As(err, &failure) {
		failure = &CLIError{Message: "Command failed.", ExitCode: 1, Code: "FAILED"}
	}
	if ctx.Err() != nil {
		failure = &CLIError{Message: "Interrupted. Submitted resources were not deleted.", ExitCode: 130, Code: "INTERRUPTED"}
	}
	if flags.Text("output") == "json" {
		_ = json.NewEncoder(streams.Err).Encode(Object{"error": Object{"code": failure.Code, "message": failure.Message}})
	} else {
		fmt.Fprintln(streams.Err, "Error: "+failure.Message)
	}
	return failure.ExitCode
}
func run(ctx context.Context, words []string, flags Flags, streams *IO, opts RunOptions) error {
	if flags.Bool("help") || len(words) > 0 && words[0] == "help" {
		_, err := io.WriteString(streams.Out, help)
		return err
	}
	format := flags.Text("output")
	if format != "" && format != "text" && format != "json" && format != "yaml" {
		return usage("--output must be text, json, or yaml.")
	}
	if flags.Bool("version") || len(words) > 0 && words[0] == "version" {
		if !flags.Bool("version") {
			if err := assertPositionals(words, 1); err != nil {
				return err
			}
			if err := assertFlags(flags, nil); err != nil {
				return err
			}
		}
		if format == "json" || format == "yaml" {
			return writeOutput(streams, flags, Object{"version": Version, "gitCommit": GitCommit, "buildTime": BuildTime})
		}
		return writeOutput(streams, flags, fmt.Sprintf("AI Runway %s (%s)", Version, GitCommit))
	}
	if _, err := parseDuration(flags.Text("timeout")); err != nil {
		return err
	}
	if len(words) == 0 && len(flags) > 0 {
		return usage("Unknown or missing command. Run airunway --help.")
	}
	if len(words) == 0 || words[0] == "serve" || words[0] == "login" || words[0] == "logout" {
		return runDashboard(ctx, words, flags, streams)
	}
	noun := words[0]
	if noun == "completion" {
		if err := assertPositionals(words, 2); err != nil {
			return err
		}
		if err := assertFlags(flags, nil); err != nil {
			return err
		}
		text, err := completion(words[1])
		if err != nil {
			return err
		}
		return writeOutput(streams, flags, text)
	}
	if !strings.Contains(" model agent context config doctor credential provider framework catalog apply ", " "+noun+" ") {
		return usage("Unknown command. Run airunway --help.")
	}
	config := opts.Config
	if config == nil {
		var err error
		config, err = readConfig(configPath())
		if err != nil {
			return err
		}
	}
	selected := flags.Text("context")
	if selected == "" {
		selected = config.Context
	}
	kube, raw, kubeErr := loadKubeConfig(flags.Text("kubeconfig"), selected)
	localPreview := flags.Text("dry-run") == "client" || noun == "catalog" && len(words) > 1 && words[1] == "model"
	if kubeErr != nil && !localPreview && opts.Client == nil {
		return kubeErr
	}
	if raw != nil && selected == "" {
		selected = raw.CurrentContext
	}
	namespace := flags.Text("namespace")
	if namespace == "" && config.Contexts[selected] != nil {
		namespace = config.Contexts[selected].Namespace
	}
	if namespace == "" && raw != nil && raw.Contexts[selected] != nil {
		namespace = raw.Contexts[selected].Namespace
	}
	if namespace == "" {
		namespace = "default"
	}
	if err := validateNamespace(namespace); err != nil {
		return err
	}
	client := opts.Client
	command := &CommandContext{Context: ctx, Flags: flags, IO: streams, Namespace: namespace, ContextName: selected}
	command.Client = func() (ClusterClient, error) {
		if client != nil {
			return client, nil
		}
		if kubeErr != nil {
			return nil, kubeErr
		}
		cfg, err := kube.ClientConfig()
		if err != nil {
			return nil, cliError(3, "KUBECONFIG", "Cannot load cluster credentials. Check --kubeconfig and --context.")
		}
		client, err = NewKubernetesClient(cfg, namespace)
		return client, err
	}
	if noun == "context" {
		if err := assertFlags(flags, nil); err != nil {
			return err
		}
		if len(words) < 2 {
			return usage("Use context list, current, or use NAME.")
		}
		switch words[1] {
		case "list":
			if err := assertPositionals(words, 2); err != nil {
				return err
			}
			if kubeErr != nil {
				return kubeErr
			}
			names := []string{}
			for name := range raw.Contexts {
				names = append(names, name)
			}
			sort.Strings(names)
			values := []Object{}
			for _, name := range names {
				c := raw.Contexts[name]
				ns := c.Namespace
				if ns == "" {
					ns = "default"
				}
				values = append(values, Object{"name": name, "cluster": c.Cluster, "namespace": ns, "selected": name == selected})
			}
			return writeOutput(streams, flags, values)
		case "current":
			if err := assertPositionals(words, 2); err != nil {
				return err
			}
			return writeOutput(streams, flags, Object{"context": selected, "namespace": namespace})
		case "use":
			if err := assertPositionals(words, 3); err != nil {
				return err
			}
			if raw == nil || raw.Contexts[words[2]] == nil {
				return usage("That context does not exist in kubeconfig.")
			}
			config.Context = words[2]
			if err := writeConfig(config, configPath()); err != nil {
				return err
			}
			return writeOutput(streams, flags, Object{"context": words[2]})
		default:
			return usage("Use context list, current, or use NAME.")
		}
	}
	if noun == "config" {
		if err := assertFlags(flags, nil); err != nil {
			return err
		}
		if len(words) < 2 {
			return usage("Use config get, set KEY VALUE, or unset KEY.")
		}
		switch words[1] {
		case "get":
			if err := assertPositionals(words, 2); err != nil {
				return err
			}
			result := Object{"context": selected, "namespace": namespace}
			for k, v := range effectiveDefaults(config, selected, namespace) {
				result[k] = v[len(v)-1]
			}
			return writeOutput(streams, flags, result)
		case "set", "unset":
			count := 3
			var value *string
			if words[1] == "set" {
				count = 4
			}
			if err := assertPositionals(words, count); err != nil {
				return err
			}
			if count == 4 {
				value = &words[3]
			}
			if err := changeConfig(config, selected, namespace, words[2], value); err != nil {
				return err
			}
			if err := writeConfig(config, configPath()); err != nil {
				return err
			}
			return writeOutput(streams, flags, Object{"setting": words[2], "value": value, "context": selected, "namespace": namespace})
		default:
			return usage("Use config get, set KEY VALUE, or unset KEY.")
		}
	}
	if noun == "doctor" {
		return runDoctor(command, words)
	}
	if noun != "model" && noun != "agent" {
		handled, err := runManagement(words, command)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
		return usage("Unknown command. Run airunway --help.")
	}
	return runResource(words, command, config, localPreview)
}
func runResource(words []string, c *CommandContext, config *CLIConfig, localPreview bool) error {
	if len(words) < 2 {
		return usage("Provide an action. Run airunway --help.")
	}
	noun, action := words[0], words[1]
	t := resourceTypes[noun]
	f := c.Flags
	if action == "list" {
		if err := assertPositionals(words, 2); err != nil {
			return err
		}
		if err := assertFlags(f, []string{"all-namespaces"}); err != nil {
			return err
		}
		client, err := c.Client()
		if err != nil {
			return err
		}
		ns := c.Namespace
		if f.Bool("all-namespaces") {
			ns = ""
		}
		value, err := client.List(c.Context, t, ns, nil)
		if err != nil {
			return err
		}
		return writeOutput(c.IO, f, value)
	}
	if err := assertPositionals(words, 3); err != nil {
		return err
	}
	name := words[2]
	if err := validateName(name, "name"); err != nil {
		return err
	}
	allowed := map[string][]string{
		"get": {}, "create": createOptions, "update": {}, "delete": {"wait"}, "wait": {"for"}, "endpoint": {"check", "gateway", "gateway-listener", "server", "credential"}, "connect": {"port", "gateway", "gateway-listener"}, "chat": {"message", "message-file", "temperature", "max-tokens", "gateway", "gateway-listener", "server", "credential"}, "logs": {"follow", "tail", "pod", "container", "timestamps"}, "events": {},
	}
	for _, key := range createOptions {
		if key != "preset" {
			allowed["update"] = append(allowed["update"], key)
		}
	}
	options, ok := allowed[action]
	if !ok {
		return usage("Unknown action. Run airunway --help.")
	}
	if err := assertFlags(f, options); err != nil {
		return err
	}
	if action == "create" {
		dry, err := dryRun(f)
		if err != nil {
			return err
		}
		creation := f
		if noun == "agent" {
			creation = mergeAgentDefaults(f, effectiveDefaults(config, c.ContextName, c.Namespace))
			if f.Has("preset") {
				if localPreview {
					return usage("Preset resolution needs a cluster. Use --dry-run server or provide framework configuration directly.")
				}
				client, err := c.Client()
				if err != nil {
					return err
				}
				preset, err := resolvePreset(c.Context, client, f.Text("preset"))
				if err != nil {
					return err
				}
				framework := stringAt(preset, "framework")
				if f.Has("framework") && f.Text("framework") != framework {
					return usage("The preset and --framework disagree.")
				}
				creation["framework"] = []string{framework}
				b, _ := json.Marshal(preset["config"])
				creation["__preset-config"] = []string{string(b)}
			}
		}
		builder := buildModel
		if noun == "agent" {
			builder = buildAgent
		}
		resource, err := builder(name, creation, c.Namespace, c.IO)
		if err != nil {
			return err
		}
		wait, err := writeWaitEnabled(noun, nil, resource, f, dry)
		if err != nil {
			return err
		}
		if dry == "client" {
			return writeOutput(c.IO, f, resource)
		}
		client, err := c.Client()
		if err != nil {
			return err
		}
		if err := preflight(resource, noun, c, client); err != nil {
			return err
		}
		created, err := client.Create(c.Context, resource, dry == "server")
		if err != nil {
			return err
		}
		if !wait {
			return writeOutput(c.IO, f, created)
		}
		progress(c, fmt.Sprintf("Created %s %q in %s. Waiting; timeout or interruption will not delete it.", noun, name, c.Namespace))
		waitFlags := f.Copy()
		target := "ready"
		if stringAt(resource, "spec", "lifecycle") == "job" {
			target = "completed"
		}
		waitFlags["for"] = []string{target}
		ready, err := waitForResource(c.Context, client, noun, created, waitFlags, c.IO)
		if err != nil {
			return err
		}
		return writeOutput(c.IO, f, ready)
	}
	if action == "update" {
		dry, err := dryRun(f)
		if err != nil {
			return err
		}
		if dry == "client" {
			return usage("Updates need the existing resource. Use --dry-run server.")
		}
		client, err := c.Client()
		if err != nil {
			return err
		}
		existing, err := client.Get(c.Context, t, c.Namespace, name)
		if err != nil {
			return err
		}
		patch, err := updateResource(noun, existing, f, c.IO)
		if err != nil {
			return err
		}
		wait, err := writeWaitEnabled(noun, existing, patch, f, dry)
		if err != nil {
			return err
		}
		updated, err := client.Patch(c.Context, t, c.Namespace, name, patch, dry == "server")
		if err != nil {
			return err
		}
		if !wait {
			return writeOutput(c.IO, f, updated)
		}
		ready, err := waitForResource(c.Context, client, noun, updated, f, c.IO)
		if err != nil {
			return err
		}
		return writeOutput(c.IO, f, ready)
	}
	if action == "get" || action == "delete" || action == "wait" {
		client, err := c.Client()
		if err != nil {
			return err
		}
		resource, err := client.Get(c.Context, t, c.Namespace, name)
		if err != nil {
			return err
		}
		switch action {
		case "get":
			return writeOutput(c.IO, f, resource)
		case "wait":
			ready, err := waitForResource(c.Context, client, noun, resource, f, c.IO)
			if err != nil {
				return err
			}
			return writeOutput(c.IO, f, ready)
		case "delete":
			uid := stringAt(resource, "metadata", "uid")
			if err := client.Delete(c.Context, t, c.Namespace, name, uid); err != nil {
				return err
			}
			if !f.Has("wait") || f.Bool("wait") {
				timeout, _ := parseDuration(f.Text("timeout"))
				waitContext, cancel := context.WithTimeout(c.Context, timeout)
				defer cancel()
				pending := func() error {
					return cliError(4, "TIMEOUT", "Deletion is still pending. Inspect its events; no other resources were deleted.")
				}
				for {
					current, err := accessGet(waitContext, client, t, c.Namespace, name)
					if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
						return pending()
					}
					if c.Context.Err() != nil {
						return cliError(130, "INTERRUPTED", "Interrupted. Submitted resources were not deleted.")
					}
					var ce *CLIError
					if errors.As(err, &ce) && ce.Code == "HTTP_404" {
						break
					}
					if err != nil {
						return err
					}
					if stringAt(current, "metadata", "uid") != uid {
						break
					}
					if err := pause(waitContext, time.Second); err != nil {
						if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
							return pending()
						}
						return err
					}
				}
			}
			return writeOutput(c.IO, f, Object{"name": name, "namespace": c.Namespace, "deletionRequested": true})
		}
	}
	return runAccess(noun, action, name, c)
}

// A zero-replica write returns the submitted object, not proof of pod
// termination. An explicit readiness wait is rejected before any mutation.
func writeWaitEnabled(noun string, existing, desired Object, flags Flags, dry string) (bool, error) {
	if dry != "" || flags.Has("wait") && !flags.Bool("wait") {
		return false, nil
	}
	if get(desired, "spec", "scaling", "replicas") == nil {
		desired = existing
	}
	zero := noun == "model" && get(desired, "spec", "scaling", "replicas") != nil &&
		intAt(desired, "spec", "scaling", "replicas") == 0
	if zero && flags.Has("wait") {
		return false, usage("--wait=true cannot wait for readiness with zero desired replicas. " +
			"Omit --wait or use --wait=false; submission does not confirm pod termination.")
	}
	return !zero, nil
}

func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return cliError(130, "INTERRUPTED", "Interrupted. Submitted resources were not deleted.")
	case <-timer.C:
		return nil
	}
}
func preflight(resource Object, noun string, c *CommandContext, client ClusterClient) error {
	// The write itself proves the deployment API exists. Collection-wide read
	// permission is not required just to submit a resource.
	if noun == "agent" {
		framework, err := client.Get(c.Context, resourceTypes["framework"], "", stringAt(resource, "spec", "framework", "name"))
		// Framework discovery is advisory for namespaced creators. Credential
		// and model-reference authorization below remains mandatory.
		if err != nil && !accessHasCode(err, "HTTP_403") {
			return err
		}
		binding := object(get(resource, "spec", "model"))
		if err == nil {
			if !boolAt(framework, "status", "ready") {
				return cliError(1, "NOT_READY", "The selected agent framework is not ready. Run airunway framework get NAME.")
			}
			backend := stringAt(framework, "spec", "capabilities", "backend")
			if stringAt(resource, "spec", "lifecycle") == "job" && backend != "container" {
				return cliError(2, "UNSUPPORTED", "One-shot mode requires a container-backed framework.")
			}
			if (get(resource, "spec", "resources") != nil || get(resource, "spec", "config", "image") != nil) &&
				backend != "container" {
				return cliError(2, "UNSUPPORTED", "Image and resource overrides require a container-backed framework.")
			}
			modes := array(get(framework, "spec", "capabilities", "modelBindingModes"))
			if modes != nil {
				for key := range binding {
					found := false
					for _, m := range modes {
						if m == key {
							found = true
						}
					}
					if !found {
						return cliError(2, "UNSUPPORTED", "The selected framework does not support this model binding.")
					}
				}
			}
		}
		for mode, key := range map[string]string{"deploymentRef": "model", "gatewayEndpoint": "gateway"} {
			ref := object(binding[mode])
			if mode == "gatewayEndpoint" {
				ref = object(ref["gatewayRef"])
			}
			if name := stringAt(ref, "name"); name != "" {
				ns := stringAt(ref, "namespace")
				if ns == "" {
					ns = c.Namespace
				}
				if _, err := client.Get(c.Context, resourceTypes[key], ns, name); err != nil {
					return err
				}
			}
		}
		if name := stringAt(binding, "externalAPI", "credentialsRef", "name"); name != "" {
			_, err := client.Get(c.Context, resourceTypes["credential"], c.Namespace, name)
			return err
		}
	} else {
		requested := stringAt(resource, "spec", "provider", "name")
		var providers []Object
		var err error
		if requested != "" {
			var provider Object
			provider, err = client.Get(c.Context, resourceTypes["provider"], "", requested)
			providers = []Object{provider}
		} else {
			providers, err = client.List(c.Context, resourceTypes["provider"], "", nil)
		}
		// Discovery is advisory. The shipped editor role can submit models
		// without reading cluster-scoped provider registrations; admission and
		// reconciliation remain authoritative for selection and compatibility.
		if err != nil && !accessHasCode(err, "HTTP_403") {
			return err
		}
		if err == nil {
			found := false
			for _, provider := range providers {
				if boolAt(provider, "status", "ready") {
					found = true
				}
			}
			if !found {
				return cliError(1, "NOT_READY", "No matching model provider is ready. Run airunway provider list.")
			}
		}
		for _, name := range []string{stringAt(resource, "spec", "secrets", "huggingFaceToken"), stringAt(resource, "spec", "model", "artifact", "credentialsRef", "name")} {
			if name != "" {
				if _, err := client.Get(c.Context, resourceTypes["credential"], c.Namespace, name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func runDoctor(c *CommandContext, words []string) error {
	if err := assertPositionals(words, 1); err != nil {
		return err
	}
	if err := assertFlags(c.Flags, nil); err != nil {
		return err
	}
	checks := []Object{}
	all := true
	for _, noun := range []string{"model", "agent", "provider", "framework"} {
		client, err := c.Client()
		var values []Object
		if err == nil {
			values, err = client.List(c.Context, resourceTypes[noun], c.Namespace, nil)
		}
		detail := fmt.Sprintf("%d visible", len(values))
		if err != nil {
			detail = err.Error()
			all = false
		}
		checks = append(checks, Object{"check": noun, "ok": err == nil, "detail": detail})
	}
	client, err := c.Client()
	if err != nil {
		return err
	}
	for _, noun := range []string{"model", "agent"} {
		result, err := client.Request(c.Context, "POST", "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", Object{"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview", "spec": Object{"resourceAttributes": Object{"namespace": c.Namespace, "group": "airunway.ai", "resource": resourceTypes[noun].Plural, "verb": "create"}}}, RequestOptions{})
		if err != nil {
			return err
		}
		ok := boolAt(result, "status", "allowed")
		detail := "Allowed"
		if !ok {
			all = false
			detail = "Not allowed"
		}
		checks = append(checks, Object{"check": "create " + noun, "ok": ok, "detail": detail})
	}
	if err := writeOutput(c.IO, c.Flags, Object{"context": c.ContextName, "namespace": c.Namespace, "checks": checks}); err != nil {
		return err
	}
	if !all {
		return cliError(1, "DOCTOR", "One or more cluster checks failed.")
	}
	return nil
}
func runDashboard(ctx context.Context, words []string, flags Flags, streams *IO) error {
	command := "serve"
	if len(words) > 0 {
		command = words[0]
	}
	allowed := []string{}
	if command == "login" {
		allowed = []string{"server"}
	}
	if err := assertFlags(flags, allowed); err != nil {
		return err
	}
	if len(words) > 1 {
		return usage("Unexpected dashboard arguments.")
	}
	exe, err := os.Executable()
	if err != nil {
		return cliError(1, "DASHBOARD", "Cannot locate the dashboard executable.")
	}
	path, err := findDashboard(exe)
	if err != nil {
		return err
	}
	args := []string{command}
	for _, key := range []string{"server", "context"} {
		if flags.Has(key) {
			args = append(args, "--"+key, flags.Text(key))
		}
	}
	cmd := exec.CommandContext(ctx, path, args...)
	if kubeconfig := flags.Text("kubeconfig"); kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	}
	cmd.Stdin = streams.In
	cmd.Stdout = streams.Out
	cmd.Stderr = streams.Err
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return cliError(exit.ExitCode(), "DASHBOARD", "Dashboard command failed.")
		}
		return cliError(1, "DASHBOARD", "Cannot start the dashboard executable.")
	}
	return nil
}

// Prefer the matching release companion without requiring either asset to be
// renamed. Canonical installations and PATH remain supported.
func dashboardNames(executable, version, platform, arch string) []string {
	extension := ""
	if platform == "windows" {
		extension = ".exe"
	}
	canonical := "airunway-web" + extension
	names := []string{}
	base := filepath.Base(executable)
	if strings.HasPrefix(base, "airunway-") && !strings.HasPrefix(base, "airunway-web") {
		names = append(names, "airunway-web-"+strings.TrimPrefix(base, "airunway-"))
	}
	if version != "" && version != "dev" && !strings.ContainsAny(version, "/\\") {
		names = append(names, "airunway-web-"+version+"-"+platform+"-"+arch+extension)
	}
	return append(names, canonical)
}

func findDashboard(executable string) (string, error) {
	current, _ := os.Stat(executable)
	usable := func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.Mode().IsRegular() && (current == nil || !os.SameFile(current, info))
	}
	names := dashboardNames(executable, Version, runtime.GOOS, runtime.GOARCH)
	for _, name := range names {
		path := filepath.Join(filepath.Dir(executable), name)
		if usable(path) {
			return path, nil
		}
	}
	if path, err := exec.LookPath(names[len(names)-1]); err == nil && usable(path) {
		return path, nil
	}
	return "", cliError(1, "DASHBOARD", "The dashboard is a separate executable. Keep the matching CLI and dashboard release assets together, run make compile, or use airunway --help for CLI commands.")
}
