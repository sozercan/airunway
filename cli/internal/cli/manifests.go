package cli

import (
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

const manifestInt32Max = 2147483647

var (
	manifestCommonFlags       = []string{"kubeconfig", "context", "namespace", "output", "timeout", "help", "version", "dry-run", "wait"}
	manifestModelMutableFlags = []string{"gpus", "cpu", "memory", "replicas", "served-name", "context-length", "credential", "image", "engine-arg", "trust-remote-code", "gateway"}
	manifestSourceFlags       = []string{"id", "model-path", "revision", "file", "storage-size", "storage-class", "storage-access-mode", "artifact-image", "service-account"}
	manifestBindingFlags      = []string{"model-ref", "model-url", "model-api", "model-id", "model-credential", "model-gateway", "gateway-listener"}
	manifestPromptFlags       = []string{"prompt", "prompt-file"}
	manifestImmutableFlags    = []string{"id", "provider", "engine", "model-source", "framework", "mode", "model-path", "revision", "file", "storage-size", "storage-class", "storage-access-mode", "artifact-image", "service-account"}
	manifestDNSPattern        = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]*[a-z0-9])?)*$`)
	manifestQuantityPattern   = regexp.MustCompile(`^(\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+|[numkMGTPE]|[KMGTPE]i)?$`)
	manifestSecretKeyPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	manifestDigestPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	manifestImageTagPattern   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	manifestRegistryPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::\d+)?$`)
	manifestImagePartPattern  = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	manifestURLPattern        = regexp.MustCompile(`^([a-z][a-z0-9+.-]*):\/\/([^/]+)(/.*)?$`)
	manifestSchemePattern     = regexp.MustCompile(`^([a-z][a-z0-9+.-]*):\/\/`)
	manifestHFPartPattern     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
)

func manifestAnyFlag(flags Flags, keys []string) bool {
	return slices.ContainsFunc(keys, flags.Has)
}

func manifestCheckFlags(flags Flags, allowed []string, operation string) error {
	// Sort diagnostics so a malformed set of flags always reports the same error.
	keys := make([]string, 0, len(flags))
	for key := range flags {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if slices.Contains(manifestCommonFlags, key) {
			continue
		}
		if !slices.Contains(allowed, key) {
			return usage(fmt.Sprintf("--%s is not supported for %s.", key, operation))
		}
		if key == "engine-arg" {
			for _, arg := range flags.Values(key) {
				if arg == "" || strings.ContainsRune(arg, '\x00') {
					return usage("--engine-arg must contain nonempty raw arguments.")
				}
			}
		} else if key == "gateway" || key == "trust-remote-code" {
			if value := flags.Text(key); value != "true" && value != "false" {
				return usage("--" + key + " must be true or false.")
			}
		} else if len(flags.Values(key)) == 0 {
			return usage("--" + key + " requires a value.")
		}
	}
	return nil
}

// JavaScript trim and URL whitespace checks also reject a byte-order mark.
func manifestWhitespace(r rune) bool {
	return unicode.IsSpace(r) || r == '\ufeff'
}

func manifestNonempty(flags Flags, key string) (string, error) {
	value, err := required(flags, key)
	if err != nil {
		return "", err
	}
	if strings.TrimFunc(value, manifestWhitespace) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", usage("--" + key + " must be a nonempty value without surrounding whitespace or control characters.")
	}
	return value, nil
}

func manifestDNSName(value, label string) (string, error) {
	if len(value) > 253 || !manifestDNSPattern.MatchString(value) {
		return "", usage(label + " must be a valid lowercase name.")
	}
	return value, nil
}

func manifestDNSLabel(value, label string) (string, error) {
	if strings.Contains(value, ".") || len(value) > 63 {
		return "", usage(label + " must be a valid lowercase name without dots.")
	}
	return manifestDNSName(value, label)
}

func manifestQuantity(flags Flags, key string) (string, error) {
	value, err := manifestNonempty(flags, key)
	if err != nil {
		return "", err
	}
	match := manifestQuantityPattern.FindStringSubmatch(value)
	if match != nil {
		number, parseErr := strconv.ParseFloat(match[1], 64)
		if parseErr == nil && !math.IsInf(number, 0) && number > 0 {
			return value, nil
		}
	}
	return "", usage("--" + key + " must be a positive resource quantity, such as 500m, 4, or 8Gi.")
}

func manifestRelativePath(value, label string) (string, error) {
	if value == "" || len(value) > 1024 || strings.ContainsAny(value, `\%`) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", usage(label + " must be a clean relative path without traversal or encoded characters.")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return "", usage(label + " must be a clean relative path without traversal or encoded characters.")
		}
	}
	return value, nil
}

func manifestSecretRef(value, label string, keyRequired bool) (Object, error) {
	parts := strings.Split(value, "/")
	if len(parts) > 2 || (keyRequired && len(parts) != 2) {
		form := "NAME or NAME/KEY"
		if keyRequired {
			form = "NAME/KEY"
		}
		return nil, usage(label + " must be " + form + ".")
	}
	name, err := manifestDNSName(parts[0], label)
	if err != nil {
		return nil, err
	}
	ref := Object{"name": name}
	if len(parts) == 2 {
		key := parts[1]
		if len(key) > 253 || !manifestSecretKeyPattern.MatchString(key) {
			return nil, usage(label + " has an invalid credential key.")
		}
		ref["key"] = key
	}
	return ref, nil
}

func manifestImageReference(value, label string, pinned bool) (string, error) {
	if value == "" || len(value) > 512 || strings.ContainsAny(value, `\%?#`) || strings.IndexFunc(value, func(r rune) bool { return manifestWhitespace(r) || unicode.IsControl(r) }) >= 0 {
		return "", usage(label + " must be a container image reference.")
	}
	parts := strings.Split(value, "@")
	if len(parts) > 2 || (len(parts) == 2 && !manifestDigestPattern.MatchString(parts[1])) {
		return "", usage(label + " has an invalid image digest.")
	}
	image := parts[0]
	colon := strings.LastIndexByte(image, ':')
	hasTag := colon > strings.LastIndexByte(image, '/')
	if hasTag {
		if !manifestImageTagPattern.MatchString(image[colon+1:]) {
			return "", usage(label + " has an invalid image tag.")
		}
		image = image[:colon]
	}
	if pinned && !hasTag && len(parts) == 1 {
		return "", usage(label + " requires an explicit tag or sha256 digest.")
	}
	parts = strings.Split(image, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		registry := parts[0]
		parts = parts[1:]
		if !manifestRegistryPattern.MatchString(registry) {
			return "", usage(label + " has an invalid registry.")
		}
		if _, err := manifestDNSName(strings.Split(registry, ":")[0], label+" registry"); err != nil {
			return "", err
		}
	}
	for _, part := range parts {
		if !manifestImagePartPattern.MatchString(part) {
			return "", usage(label + " has an invalid image repository.")
		}
	}
	return value, nil
}

func manifestImageFlag(flags Flags, key string, pinned bool) (string, error) {
	value, err := manifestNonempty(flags, key)
	if err != nil {
		return "", err
	}
	return manifestImageReference(value, "--"+key, pinned)
}

// Validate raw paths before parsing: URL normalization must not hide traversal.
// Never include rejected URLs or parser errors in diagnostics, as they can contain credentials.
func manifestCleanURL(value, label string, protocols []string) (host, pathname string, err error) {
	if len(value) > 4096 || strings.ContainsAny(value, `\%?#`) || strings.IndexFunc(value, func(r rune) bool { return manifestWhitespace(r) || unicode.IsControl(r) }) >= 0 {
		return "", "", usage(label + " must not contain credentials, query parameters, fragments, or encoded paths.")
	}
	match := manifestURLPattern.FindStringSubmatch(value)
	if match == nil || !slices.Contains(protocols, match[1]) || strings.Contains(match[2], "@") {
		return "", "", usage(label + " must use " + strings.Join(protocols, " or ") + " without inline credentials.")
	}
	pathname = match[3]
	if pathname != "" && pathname != "/" {
		if _, err := manifestRelativePath(strings.TrimSuffix(pathname[1:], "/"), label); err != nil {
			return "", "", err
		}
	}
	parsed, parseErr := url.Parse(value)
	if parseErr != nil || parsed.Hostname() == "" || parsed.User != nil || strings.ContainsAny(parsed.Hostname(), "<>^|") {
		return "", "", usage(label + " is not a valid URL.")
	}
	if strings.HasPrefix(parsed.Host, "[") {
		if address, parseErr := netip.ParseAddr(parsed.Hostname()); parseErr != nil || !address.Is6() {
			return "", "", usage(label + " is not a valid URL.")
		}
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		if port := parsed.Port(); port != "" {
			if _, parseErr := strconv.ParseUint(port, 10, 16); parseErr != nil {
				return "", "", usage(label + " is not a valid URL.")
			}
		}
	}
	// Keep the raw path so Unicode filenames are not percent-encoded.
	return parsed.Host, pathname, nil
}

func manifestResource(noun, name, namespace string, spec Object) (Object, error) {
	if err := validateName(name, "name"); err != nil {
		return nil, err
	}
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	resource := resourceTypes[noun]
	return Object{
		"apiVersion": resource.Group + "/" + resource.Version,
		"kind":       resource.Kind,
		"metadata":   Object{"name": name, "namespace": namespace, "annotations": Object{"airunway.ai/managed-by": "cli"}},
		"spec":       spec,
	}, nil
}

func manifestModelOptions(flags Flags) (Object, error) {
	spec, resources, engine := Object{}, Object{}, Object{}
	if flags.Has("gpus") {
		count, err := integer(flags, "gpus", 0, 0, manifestInt32Max)
		if err != nil {
			return nil, err
		}
		resources["gpu"] = Object{"count": count}
	}
	for _, key := range []string{"cpu", "memory"} {
		if flags.Has(key) {
			value, err := manifestQuantity(flags, key)
			if err != nil {
				return nil, err
			}
			resources[key] = value
		}
	}
	if len(resources) > 0 {
		spec["resources"] = resources
	}
	if flags.Has("replicas") {
		count, err := integer(flags, "replicas", 0, 0, manifestInt32Max)
		if err != nil {
			return nil, err
		}
		spec["scaling"] = Object{"replicas": count}
	}
	if flags.Has("served-name") {
		value, err := manifestNonempty(flags, "served-name")
		if err != nil {
			return nil, err
		}
		spec["model"] = Object{"servedName": value}
	}
	if flags.Has("context-length") {
		value, err := integer(flags, "context-length", 0, 1, manifestInt32Max)
		if err != nil {
			return nil, err
		}
		engine["contextLength"] = value
	}
	if flags.Has("image") {
		value, err := manifestImageFlag(flags, "image", false)
		if err != nil {
			return nil, err
		}
		engine["image"] = value
	}
	if flags.Has("engine-arg") {
		args := make([]any, len(flags.Values("engine-arg")))
		for i, arg := range flags.Values("engine-arg") {
			args[i] = arg
		}
		engine["extraArgs"] = args
	}
	if flags.Has("trust-remote-code") {
		engine["trustRemoteCode"] = flags.Bool("trust-remote-code")
	}
	if len(engine) > 0 {
		spec["engine"] = engine
	}
	if flags.Has("gateway") {
		spec["gateway"] = Object{"enabled": flags.Bool("gateway")}
	}
	return spec, nil
}

func manifestHFCredential(flags Flags) (Object, error) {
	if !flags.Has("credential") {
		return nil, nil
	}
	value, err := manifestNonempty(flags, "credential")
	if err != nil {
		return nil, err
	}
	ref, err := manifestSecretRef(value, "--credential", false)
	if err != nil {
		return nil, err
	}
	if key, ok := ref["key"]; ok && key != "HF_TOKEN" {
		return nil, usage("Unstaged Hugging Face models require the HF_TOKEN credential key.")
	}
	return Object{"huggingFaceToken": ref["name"]}, nil
}

func manifestRequireVLLM(spec Object, description string) error {
	provider, engine := stringAt(spec, "provider", "name"), stringAt(spec, "engine", "type")
	if (provider != "" && provider != "vllm") || (engine != "" && engine != "vllm") {
		return usage(description + " currently requires --provider vllm and --engine vllm.")
	}
	spec["provider"] = Object{"name": "vllm"}
	options := object(spec["engine"])
	options["type"] = "vllm"
	spec["engine"] = options
	return nil
}

func manifestRejectOptions(flags Flags, keys []string, description string) error {
	for _, key := range keys {
		if flags.Has(key) {
			return usage("--" + key + " is not supported for " + description + ".")
		}
	}
	return nil
}

func buildModel(name string, flags Flags, namespace string, streams *IO) (Object, error) {
	allowed := slices.Concat(manifestModelMutableFlags, manifestSourceFlags, []string{"provider", "engine"})
	if err := manifestCheckFlags(flags, allowed, "model creation"); err != nil {
		return nil, err
	}
	spec, err := manifestModelOptions(flags)
	if err != nil {
		return nil, err
	}
	gpus, err := integer(flags, "gpus", 1, 0, manifestInt32Max)
	if err != nil {
		return nil, err
	}
	replicas, err := integer(flags, "replicas", 1, 0, manifestInt32Max)
	if err != nil {
		return nil, err
	}
	resources := object(spec["resources"])
	resources["gpu"] = Object{"count": gpus}
	spec["resources"] = resources
	spec["scaling"] = Object{"replicas": replicas}
	if flags.Has("provider") {
		value, err := manifestNonempty(flags, "provider")
		if err != nil {
			return nil, err
		}
		value, err = manifestDNSName(value, "--provider")
		if err != nil {
			return nil, err
		}
		spec["provider"] = Object{"name": value}
	}
	if flags.Has("engine") {
		value, err := manifestNonempty(flags, "engine")
		if err != nil {
			return nil, err
		}
		if !slices.Contains([]string{"vllm", "sglang", "trtllm", "llamacpp"}, value) {
			return nil, usage("--engine must be vllm, sglang, trtllm, or llamacpp.")
		}
		engine := object(spec["engine"])
		engine["type"] = value
		spec["engine"] = engine
	}
	model := object(spec["model"])
	spec["model"] = model
	if flags.Has("model-path") {
		if err := manifestRejectOptions(flags, []string{"id", "revision", "file", "credential", "storage-size", "storage-class", "storage-access-mode", "artifact-image", "service-account"}, "bundled models"); err != nil {
			return nil, err
		}
		if _, err := manifestNonempty(flags, "image"); err != nil {
			return nil, err
		}
		path, err := manifestNonempty(flags, "model-path")
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(path, "/") {
			return nil, usage("--model-path must be an absolute path inside the image.")
		}
		if _, err := manifestRelativePath(path[1:], "--model-path"); err != nil {
			return nil, err
		}
		if err := manifestRejectOptions(flags, []string{"served-name"}, "bundled models"); err != nil {
			return nil, err
		}
		if err := manifestRequireVLLM(spec, "Bundled models"); err != nil {
			return nil, err
		}
		model["source"], model["id"] = "custom", path
		return manifestResource("model", name, namespace, spec)
	}
	source, err := manifestNonempty(flags, "id")
	if err != nil {
		return nil, err
	}
	scheme := "hf"
	if match := manifestSchemePattern.FindStringSubmatch(source); match != nil {
		scheme = match[1]
	}
	uri := source
	if scheme == "hf" {
		repository := strings.TrimPrefix(source, "hf://")
		parts := strings.Split(repository, "/")
		valid := len(repository) <= 4091 && len(parts) <= 2
		for _, part := range parts {
			valid = valid && manifestHFPartPattern.MatchString(part) && !strings.Contains(part, "..")
		}
		if !valid {
			return nil, usage("--id must identify a Hugging Face repository; select files with --file.")
		}
		uri = "hf://" + repository
		if !manifestAnyFlag(flags, []string{"revision", "file"}) {
			if err := manifestRejectOptions(flags, []string{"storage-size", "storage-class", "storage-access-mode", "artifact-image", "service-account"}, "unstaged Hugging Face models"); err != nil {
				return nil, err
			}
			model["source"], model["id"] = "huggingface", repository
			secrets, err := manifestHFCredential(flags)
			if err != nil {
				return nil, err
			}
			if secrets != nil {
				spec["secrets"] = secrets
			}
			return manifestResource("model", name, namespace, spec)
		}
	}
	if scheme == "pvc" {
		host, path, err := manifestCleanURL(uri, "--id", []string{"pvc"})
		if err != nil {
			return nil, err
		}
		if err := manifestRejectOptions(flags, []string{"revision", "file", "credential", "storage-size", "storage-class", "storage-access-mode", "artifact-image", "service-account"}, "existing storage references"); err != nil {
			return nil, err
		}
		claim, err := manifestDNSName(host, "PVC claim")
		if err != nil {
			return nil, err
		}
		if err := manifestRejectOptions(flags, []string{"served-name"}, "existing storage references"); err != nil {
			return nil, err
		}
		if err := manifestRequireVLLM(spec, "Existing storage references"); err != nil {
			return nil, err
		}
		path = strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/")
		model["source"], model["id"] = "custom", "/model-cache"
		if path != "" {
			model["id"] = "/model-cache/" + path
		}
		model["storage"] = Object{"volumes": []any{Object{"name": "model-cache", "purpose": "modelCache", "claimName": claim, "readOnly": true, "mountPath": "/model-cache"}}}
		return manifestResource("model", name, namespace, spec)
	}

	var host, pathname string
	if scheme == "oci" {
		reference := strings.TrimPrefix(uri, "oci://")
		if !strings.Contains(reference, "/") {
			return nil, usage("OCI sources require a registry and repository.")
		}
		if _, err := manifestImageReference(reference, "--id OCI source", true); err != nil {
			return nil, err
		}
	} else {
		host, pathname, err = manifestCleanURL(uri, "--id", []string{"hf", "s3", "gs", "https"})
		if err != nil {
			return nil, err
		}
	}
	if scheme == "s3" {
		if _, err := manifestDNSName(host, "Artifact bucket"); err != nil {
			return nil, err
		}
	}
	if scheme == "https" && (pathname == "" || strings.HasSuffix(pathname, "/")) {
		return nil, usage("HTTPS sources must identify a single file.")
	}
	if err := manifestRequireVLLM(spec, "Staged model artifacts"); err != nil {
		return nil, err
	}
	artifact := Object{"uri": uri}
	if flags.Has("revision") {
		if scheme != "hf" {
			return nil, usage("--revision is supported only for Hugging Face sources.")
		}
		value, err := manifestNonempty(flags, "revision")
		if err != nil {
			return nil, err
		}
		if len(value) > 256 {
			return nil, usage("--revision must be at most 256 characters.")
		}
		value, err = manifestRelativePath(value, "--revision")
		if err != nil {
			return nil, err
		}
		artifact["revision"] = value
	}
	if flags.Has("file") {
		value, err := manifestNonempty(flags, "file")
		if err != nil {
			return nil, err
		}
		value, err = manifestRelativePath(value, "--file")
		if err != nil {
			return nil, err
		}
		artifact["file"] = value
	}
	if flags.Has("credential") {
		value, err := manifestNonempty(flags, "credential")
		if err != nil {
			return nil, err
		}
		ref, err := manifestSecretRef(value, "--credential", false)
		if err != nil {
			return nil, err
		}
		if scheme == "hf" && (ref["key"] == nil || ref["key"] == "HF_TOKEN") {
			spec["secrets"] = Object{"huggingFaceToken": ref["name"]}
		} else {
			artifact["credentialsRef"] = ref
		}
	}
	if flags.Has("artifact-image") {
		value, err := manifestImageFlag(flags, "artifact-image", true)
		if err != nil {
			return nil, err
		}
		artifact["image"] = value
	}
	if flags.Has("service-account") {
		value, err := manifestNonempty(flags, "service-account")
		if err != nil {
			return nil, err
		}
		value, err = manifestDNSName(value, "--service-account")
		if err != nil {
			return nil, err
		}
		artifact["serviceAccountName"] = value
	}
	size := "100Gi"
	if flags.Has("storage-size") {
		size, err = manifestQuantity(flags, "storage-size")
		if err != nil {
			return nil, err
		}
	}
	accessMode := "ReadWriteOnce"
	if replicas > 1 {
		accessMode = "ReadWriteMany"
	}
	if flags.Has("storage-access-mode") {
		accessMode = flags.Text("storage-access-mode")
		if !slices.Contains([]string{"ReadWriteOnce", "ReadWriteMany"}, accessMode) {
			return nil, usage("--storage-access-mode must be ReadWriteOnce or ReadWriteMany.")
		}
	}
	volume := Object{"name": "model-cache", "purpose": "modelCache", "mountPath": "/model-cache", "readOnly": false, "size": size, "accessMode": accessMode}
	if flags.Has("storage-class") {
		class := flags.Text("storage-class")
		if class != "" {
			if _, err := manifestDNSName(class, "--storage-class"); err != nil {
				return nil, err
			}
		}
		volume["storageClassName"] = class
	}
	file := stringAt(artifact, "file")
	if file == "" && scheme == "https" {
		file = pathname[strings.LastIndexByte(pathname, '/')+1:]
	}
	model["source"], model["id"] = "custom", "/model-cache/"+airunwayv1alpha1.ArtifactDirectory
	if file != "" {
		model["id"] = model["id"].(string) + "/" + file
	}
	model["artifact"] = artifact
	model["storage"] = Object{"volumes": []any{volume}}
	// Admission and the downloader share this contract, including reserved paths.
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, usage("Cannot encode the model artifact specification.")
	}
	var typed airunwayv1alpha1.ModelDeploymentSpec
	if err := json.Unmarshal(encoded, &typed); err != nil {
		return nil, usage("Invalid model artifact specification.")
	}
	if err := typed.ValidateArtifact(); err != nil {
		return nil, usage(err.Error())
	}
	return manifestResource("model", name, namespace, spec)
}

func manifestObjectRef(value, label string) (Object, error) {
	parts := strings.Split(value, "/")
	if len(parts) > 2 {
		return nil, usage(label + " must be NAME or NAMESPACE/NAME.")
	}
	name, err := manifestDNSName(parts[len(parts)-1], label)
	if err != nil {
		return nil, err
	}
	ref := Object{"name": name}
	if len(parts) == 2 {
		namespace, err := manifestDNSLabel(parts[0], label)
		if err != nil {
			return nil, err
		}
		ref["namespace"] = namespace
	}
	return ref, nil
}

func manifestBinding(flags Flags, existing Object) (Object, error) {
	selectors := []string{"model-ref", "model-url", "model-gateway"}
	kinds := []string{"deploymentRef", "externalAPI", "gatewayEndpoint"}
	kind, selected := "", 0
	var oldKinds []string
	for i, selector := range selectors {
		if flags.Has(selector) {
			kind = kinds[i]
			selected++
		}
		if existing[kinds[i]] != nil {
			oldKinds = append(oldKinds, kinds[i])
		}
	}
	if selected > 1 {
		return nil, usage("Choose only one of --model-ref, --model-url, or --model-gateway.")
	}
	if selected == 0 && len(oldKinds) == 1 {
		kind = oldKinds[0]
	}
	if kind == "" {
		return nil, usage("Provide --model-ref, --model-url, or --model-gateway.")
	}
	old, hasOld := existing[kind]
	hasOld = hasOld && old != nil
	patch := Object{}
	switch kind {
	case "deploymentRef":
		if err := manifestRejectOptions(flags, []string{"model-url", "model-api", "model-id", "model-credential", "model-gateway", "gateway-listener"}, "a model deployment binding"); err != nil {
			return nil, err
		}
		value, err := manifestNonempty(flags, "model-ref")
		if err != nil {
			return nil, err
		}
		ref, err := manifestObjectRef(value, "--model-ref")
		if err != nil {
			return nil, err
		}
		if stringAt(old, "namespace") != "" && ref["namespace"] == nil {
			ref["namespace"] = nil
		}
		patch[kind] = ref
	case "externalAPI":
		if err := manifestRejectOptions(flags, []string{"model-ref", "model-gateway", "gateway-listener"}, "an external API binding"); err != nil {
			return nil, err
		}
		api := Object{}
		if flags.Has("model-url") || !hasOld {
			value, err := manifestNonempty(flags, "model-url")
			if err != nil {
				return nil, err
			}
			if _, _, err := manifestCleanURL(value, "--model-url", []string{"http", "https"}); err != nil {
				return nil, err
			}
			api["baseURL"] = value
		}
		if flags.Has("model-api") || !hasOld {
			value, err := manifestNonempty(flags, "model-api")
			if err != nil {
				return nil, err
			}
			if !slices.Contains([]string{"openai", "anthropic", "azure-openai", "azureOpenAI", "custom"}, value) {
				return nil, usage("--model-api must be openai, anthropic, azure-openai, or custom.")
			}
			if value == "azure-openai" {
				value = "azureOpenAI"
			}
			api["type"] = value
		}
		if flags.Has("model-id") || !hasOld {
			value, err := manifestNonempty(flags, "model-id")
			if err != nil {
				return nil, err
			}
			api["modelName"] = value
		}
		if flags.Has("model-credential") {
			value, err := manifestNonempty(flags, "model-credential")
			if err != nil {
				return nil, err
			}
			ref, err := manifestSecretRef(value, "--model-credential", true)
			if err != nil {
				return nil, err
			}
			api["credentialsRef"] = ref
		}
		patch[kind] = api
	case "gatewayEndpoint":
		if err := manifestRejectOptions(flags, []string{"model-ref", "model-url", "model-api", "model-credential"}, "a gateway binding"); err != nil {
			return nil, err
		}
		gateway := Object{}
		if flags.Has("model-gateway") || !hasOld {
			value, err := manifestNonempty(flags, "model-gateway")
			if err != nil {
				return nil, err
			}
			ref, err := manifestObjectRef(value, "--model-gateway")
			if err != nil {
				return nil, err
			}
			if stringAt(old, "gatewayRef", "namespace") != "" && ref["namespace"] == nil {
				ref["namespace"] = nil
			}
			if stringAt(old, "gatewayRef", "listenerName") != "" && !flags.Has("gateway-listener") {
				ref["listenerName"] = nil
			}
			gateway["gatewayRef"] = ref
		}
		if flags.Has("gateway-listener") {
			value, err := manifestNonempty(flags, "gateway-listener")
			if err != nil {
				return nil, err
			}
			value, err = manifestDNSName(value, "--gateway-listener")
			if err != nil {
				return nil, err
			}
			ref := object(gateway["gatewayRef"])
			ref["listenerName"] = value
			gateway["gatewayRef"] = ref
		}
		if flags.Has("model-id") || !hasOld {
			value, err := manifestNonempty(flags, "model-id")
			if err != nil {
				return nil, err
			}
			gateway["modelName"] = value
		}
		patch[kind] = gateway
	}
	// Merge patch retains omitted union members; explicit nulls remove old modes.
	for _, oldKind := range oldKinds {
		if oldKind != kind {
			patch[oldKind] = nil
		}
	}
	return patch, nil
}

func manifestJSONObject(raw, label string) (Object, error) {
	var parsed any
	if err := decodeJSON([]byte(raw), &parsed); err != nil {
		return nil, usage(label + " must contain valid JSON.")
	}
	result, ok := parsed.(map[string]any)
	if !ok {
		return nil, usage(label + " must contain a JSON object.")
	}
	if err := managementValidateJSON(result, true); err != nil {
		return nil, err
	}
	return result, nil
}

func manifestMergeConfig(target, source Object, label string) (Object, error) {
	result := make(Object, len(target)+len(source))
	for key, value := range target {
		result[key] = value
	}
	for key, value := range source {
		old, present := result[key]
		oldObject, oldIsObject := old.(map[string]any)
		newObject, newIsObject := value.(map[string]any)
		if present && oldIsObject && newIsObject {
			merged, err := manifestMergeConfig(oldObject, newObject, label)
			if err != nil {
				return nil, err
			}
			result[key] = merged
		} else {
			if present && !reflect.DeepEqual(old, value) {
				return nil, usage(label + " conflicts with configuration field " + key + ". Supply that field in only one place.")
			}
			result[key] = value
		}
	}
	return result, nil
}

func manifestCheckStdin(flags Flags) error {
	count := 0
	for _, key := range []string{"prompt-file", "task-file", "config-file"} {
		if flags.Text(key) == "-" {
			count++
		}
	}
	if count > 1 {
		return usage("Only one input file can read from stdin.")
	}
	return nil
}

func buildAgent(name string, flags Flags, namespace string, streams *IO) (Object, error) {
	allowed := slices.Concat([]string{"framework"}, manifestBindingFlags, manifestPromptFlags, []string{"task", "task-file", "config-file", "__preset-config", "preset", "mode", "image", "cpu", "memory"})
	if err := manifestCheckFlags(flags, allowed, "agent creation"); err != nil {
		return nil, err
	}
	framework, err := manifestNonempty(flags, "framework")
	if err != nil {
		return nil, err
	}
	framework, err = manifestDNSLabel(framework, "--framework")
	if err != nil {
		return nil, err
	}
	mode := "deployment"
	if flags.Has("mode") {
		mode = flags.Text("mode")
	}
	if !slices.Contains([]string{"deployment", "once", "job"}, mode) {
		return nil, usage("--mode must be deployment or once.")
	}
	if mode == "once" {
		mode = "job"
	}
	model, err := manifestBinding(flags, nil)
	if err != nil {
		return nil, err
	}
	if err := manifestCheckStdin(flags); err != nil {
		return nil, err
	}
	config := Object{}
	if flags.Has("__preset-config") {
		raw, err := manifestNonempty(flags, "__preset-config")
		if err != nil {
			return nil, err
		}
		config, err = manifestJSONObject(raw, "Preset configuration")
		if err != nil {
			return nil, err
		}
	}
	fromFile, hasFile, err := readFlagInput(flags, "__unused-config", "config-file", streams)
	if err != nil {
		return nil, err
	}
	if hasFile {
		value, err := manifestJSONObject(fromFile, "--config-file")
		if err != nil {
			return nil, err
		}
		config, err = manifestMergeConfig(config, value, "--config-file")
		if err != nil {
			return nil, err
		}
	}
	prompt, hasPrompt, err := readFlagInput(flags, "prompt", "prompt-file", streams)
	if err != nil {
		return nil, err
	}
	task, hasTask, err := readFlagInput(flags, "task", "task-file", streams)
	if err != nil {
		return nil, err
	}
	if hasPrompt {
		config, err = manifestMergeConfig(config, Object{"systemPrompt": prompt}, "--prompt")
		if err != nil {
			return nil, err
		}
	}
	if hasTask {
		config, err = manifestMergeConfig(config, Object{"task": task}, "--task")
		if err != nil {
			return nil, err
		}
	}
	if flags.Has("image") {
		image, err := manifestImageFlag(flags, "image", false)
		if err != nil {
			return nil, err
		}
		config, err = manifestMergeConfig(config, Object{"image": image}, "--image")
		if err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"systemPrompt", "task", "image"} {
		if value, present := config[key]; present {
			if _, ok := value.(string); !ok {
				return nil, usage("Configuration field " + key + " must be a string.")
			}
		}
	}
	if image, ok := config["image"].(string); ok {
		if _, err := manifestImageReference(image, "Configuration image", false); err != nil {
			return nil, err
		}
	}
	jobTask := config["task"]
	if jobTask == nil {
		jobTask = config["prompt"]
	}
	if text, ok := jobTask.(string); mode == "job" && (!ok || strings.TrimFunc(text, manifestWhitespace) == "") {
		return nil, usage("--mode once requires a task, using --task, --task-file, or config.task.")
	}
	if mode != "job" && hasTask {
		return nil, usage("--task and --task-file require --mode once.")
	}
	spec := Object{"framework": Object{"name": framework}, "lifecycle": mode, "model": model}
	if len(config) > 0 {
		spec["config"] = config
	}
	requests := Object{}
	for _, key := range []string{"cpu", "memory"} {
		if flags.Has(key) {
			value, err := manifestQuantity(flags, key)
			if err != nil {
				return nil, err
			}
			requests[key] = value
		}
	}
	if len(requests) > 0 {
		spec["resources"] = Object{"requests": requests}
	}
	return manifestResource("agent", name, namespace, spec)
}

func updateResource(noun string, existing Object, flags Flags, streams *IO) (Object, error) {
	if noun != "model" && noun != "agent" {
		return nil, usage("Only model and agent resources support updates.")
	}
	if stringAt(existing, "kind") != resourceTypes[noun].Kind {
		return nil, usage("The existing resource is not a " + noun + ".")
	}
	for _, key := range manifestImmutableFlags {
		if flags.Has(key) {
			return nil, usage("--" + key + " is immutable. Create a new " + noun + " instead.")
		}
	}
	if noun == "agent" && stringAt(existing, "spec", "lifecycle") == "job" {
		return nil, usage("One-shot agents cannot be updated. Create a new agent to run another task.")
	}
	allowed := manifestModelMutableFlags
	if noun == "agent" {
		allowed = slices.Concat(manifestPromptFlags, manifestBindingFlags)
	}
	if err := manifestCheckFlags(flags, allowed, noun+" updates"); err != nil {
		return nil, err
	}
	spec := Object{}
	if noun == "model" {
		var err error
		spec, err = manifestModelOptions(flags)
		if err != nil {
			return nil, err
		}
		custom := stringAt(existing, "spec", "model", "source") == "custom"
		artifact := get(existing, "spec", "model", "artifact") != nil
		if flags.Has("served-name") && custom && !artifact {
			return nil, usage("--served-name is not supported for unstaged custom models.")
		}
		if flags.Has("credential") {
			if artifact {
				return nil, usage("Staged artifact credentials are immutable. Rotate the existing credential or create a new model.")
			}
			if custom {
				return nil, usage("--credential is only supported for Hugging Face model updates.")
			}
			secrets, err := manifestHFCredential(flags)
			if err != nil {
				return nil, err
			}
			spec["secrets"] = secrets
		}
	} else {
		if err := manifestCheckStdin(flags); err != nil {
			return nil, err
		}
		prompt, present, err := readFlagInput(flags, "prompt", "prompt-file", streams)
		if err != nil {
			return nil, err
		}
		if present {
			spec["config"] = Object{"systemPrompt": prompt}
		}
		if manifestAnyFlag(flags, manifestBindingFlags) {
			model, err := manifestBinding(flags, object(get(existing, "spec", "model")))
			if err != nil {
				return nil, err
			}
			spec["model"] = model
		}
	}
	if len(spec) == 0 {
		return nil, usage("Supply at least one mutable field for the " + noun + " update.")
	}
	metadata := Object{"name": get(existing, "metadata", "name")}
	for _, key := range []string{"namespace", "resourceVersion"} {
		if value, present := object(existing["metadata"])[key]; present {
			metadata[key] = value
		}
	}
	return Object{"apiVersion": existing["apiVersion"], "kind": existing["kind"], "metadata": metadata, "spec": spec}, nil
}
