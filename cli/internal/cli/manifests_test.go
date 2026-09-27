package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type manifestTestReader struct {
	io.Reader
	reads int
}

func (r *manifestTestReader) Read(p []byte) (int, error) { r.reads++; return r.Reader.Read(p) }
func manifestTestIO(input string) (*IO, *manifestTestReader, *bytes.Buffer) {
	r := &manifestTestReader{Reader: strings.NewReader(input)}
	out := &bytes.Buffer{}
	return &IO{In: r, Out: out, Err: out}, r, out
}
func manifestTestFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A nil value in test overrides removes a default, matching TS undefined fixtures.
func manifestTestDefaults(defaults, flags Flags) Flags {
	result := defaults.Copy()
	for k, v := range flags {
		if v == nil {
			delete(result, k)
		} else {
			result[k] = v
		}
	}
	return result
}
func manifestTestModelResult(flags Flags, streams *IO) (Object, error) {
	return buildModel("demo", manifestTestDefaults(Flags{"id": {"org/model"}}, flags), "team", streams)
}
func manifestTestAgentResult(flags Flags, streams *IO) (Object, error) {
	return buildAgent("helper", manifestTestDefaults(Flags{"framework": {"langgraph"}, "model-ref": {"demo"}}, flags), "team", streams)
}
func manifestTestModel(t *testing.T, flags Flags) Object {
	t.Helper()
	streams, _, _ := manifestTestIO("")
	result, err := manifestTestModelResult(flags, streams)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func manifestTestAgent(t *testing.T, flags Flags) Object {
	t.Helper()
	streams, _, _ := manifestTestIO("")
	result, err := manifestTestAgentResult(flags, streams)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func manifestTestExternal(flags Flags) Flags {
	result := Flags{"model-ref": nil, "model-url": {"https://api.example.test/v1"}, "model-api": {"openai"}, "model-id": {"remote-model"}}
	for k, v := range flags {
		result[k] = v
	}
	return result
}
func manifestTestGateway(flags Flags) Flags {
	result := Flags{"model-ref": nil, "model-gateway": {"edge"}, "model-id": {"served-model"}}
	for k, v := range flags {
		result[k] = v
	}
	return result
}
func manifestTestUsage(t *testing.T, err error, contains string) {
	t.Helper()
	var failure *CLIError
	if !errors.As(err, &failure) || failure.ExitCode != 2 || failure.Code != "USAGE" {
		t.Fatalf("expected usage error, got %v", err)
	}
	if !strings.Contains(failure.Message, contains) {
		t.Fatalf("error %q does not contain %q", failure.Message, contains)
	}
	if strings.Contains(failure.Message, "private-value") {
		t.Fatal("diagnostic disclosed a credential")
	}
}
func manifestTestEqual(t *testing.T, actual, expected any) {
	t.Helper()
	a, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("got  %s\nwant %s", a, b)
	}
}
func manifestTestServerFields(resource Object) Object {
	m := object(resource["metadata"])
	m["resourceVersion"] = "42"
	m["uid"] = "existing-uid"
	m["labels"] = Object{"owner": "team"}
	m["annotations"] = Object{"untouched": "annotation"}
	m["managedFields"] = []any{Object{"manager": "controller"}}
	resource["status"] = Object{"phase": "Running", "private": Object{"details": "server state"}}
	return resource
}

// Apply RFC 7396 to verify omitted fields survive and nulls remove old unions.
func manifestTestMergePatch(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	result := cloneObject(object(target))
	for k, v := range p {
		if v == nil {
			delete(result, k)
		} else {
			result[k] = manifestTestMergePatch(result[k], v)
		}
	}
	return result
}
func manifestTestUpdate(t *testing.T, noun string, existing Object, flags Flags, input string) Object {
	t.Helper()
	streams, _, _ := manifestTestIO(input)
	patch, err := updateResource(noun, existing, flags, streams)
	if err != nil {
		t.Fatal(err)
	}
	return patch
}
func manifestTestCredentialURL(raw string) string {
	u, _ := url.Parse(raw)
	u.User = url.UserPassword("user", "private-value")
	return u.String()
}

func TestManifestModelConstruction(t *testing.T) {
	for _, id := range []string{"org/model", "hf://org/model", "hf://Qwen/Qwen3-8B", "gpt2"} {
		t.Run("plain HF "+id, func(t *testing.T) {
			manifestTestEqual(t, manifestTestModel(t, Flags{"id": {id}}), Object{
				"apiVersion": "airunway.ai/v1alpha1", "kind": "ModelDeployment",
				"metadata": Object{"name": "demo", "namespace": "team", "annotations": Object{"airunway.ai/managed-by": "cli"}},
				"spec":     Object{"resources": Object{"gpu": Object{"count": 1}}, "scaling": Object{"replicas": 1}, "model": Object{"source": "huggingface", "id": strings.TrimPrefix(id, "hf://")}},
			})
		})
	}
	t.Run("explicit zero", func(t *testing.T) {
		result := manifestTestModel(t, Flags{"gpus": {"0"}, "replicas": {"0"}, "cpu": {"500m"}, "memory": {"4Gi"}})
		manifestTestEqual(t, get(result, "spec", "resources"), Object{"gpu": Object{"count": 0}, "cpu": "500m", "memory": "4Gi"})
		manifestTestEqual(t, get(result, "spec", "scaling"), Object{"replicas": 0})
		manifestTestEqual(t, get(result, "spec", "provider"), nil)
		manifestTestEqual(t, get(result, "spec", "engine"), nil)
	})
	t.Run("engine options and raw arguments", func(t *testing.T) {
		args := []string{"--quantization", "awq", `--json={"name":"value with spaces"}`, "--tensor-parallel-size=2"}
		flags := Flags{"provider": {"dynamo"}, "engine": {"sglang"}, "image": {"registry.example.test/runtime:v2"}, "served-name": {"chat"}, "context-length": {"8192"}, "gpus": {"2"}, "credential": {"hf-token"}, "engine-arg": args, "trust-remote-code": {"false"}, "gateway": {"false"}}
		result := manifestTestModel(t, flags)
		manifestTestEqual(t, get(result, "spec", "provider"), Object{"name": "dynamo"})
		manifestTestEqual(t, get(result, "spec", "model"), Object{"source": "huggingface", "id": "org/model", "servedName": "chat"})
		manifestTestEqual(t, get(result, "spec", "engine"), Object{"type": "sglang", "image": "registry.example.test/runtime:v2", "contextLength": 8192, "trustRemoteCode": false, "extraArgs": args})
		manifestTestEqual(t, get(result, "spec", "image"), nil)
		manifestTestEqual(t, get(result, "spec", "secrets"), Object{"huggingFaceToken": "hf-token"})
		manifestTestEqual(t, get(result, "spec", "gateway"), Object{"enabled": false})
		args[0] = "changed"
		if array(get(result, "spec", "engine", "extraArgs"))[0] != "--quantization" {
			t.Fatal("arguments alias caller flags")
		}
	})
	t.Run("explicit true", func(t *testing.T) {
		result := manifestTestModel(t, Flags{"trust-remote-code": {"true"}, "gateway": {"true"}, "engine-arg": {"--dtype=half", "--enforce-eager"}})
		manifestTestEqual(t, get(result, "spec", "engine"), Object{"extraArgs": []string{"--dtype=half", "--enforce-eager"}, "trustRemoteCode": true})
		manifestTestEqual(t, get(result, "spec", "gateway"), Object{"enabled": true})
	})
	t.Run("bundled image", func(t *testing.T) {
		result := manifestTestModel(t, Flags{"id": nil, "image": {"registry.example.test/bundled:v1"}, "model-path": {"/models/chat"}})
		manifestTestEqual(t, get(result, "spec", "model"), Object{"source": "custom", "id": "/models/chat"})
		manifestTestEqual(t, get(result, "spec", "engine"), Object{"type": "vllm", "image": "registry.example.test/bundled:v1"})
		manifestTestEqual(t, get(result, "spec", "provider"), Object{"name": "vllm"})
	})
	t.Run("HF credential key", func(t *testing.T) {
		manifestTestEqual(t, get(manifestTestModel(t, Flags{"credential": {"hf-token/HF_TOKEN"}}), "spec", "secrets"), Object{"huggingFaceToken": "hf-token"})
		_, err := manifestTestModelResult(Flags{"credential": {"hf-token/other-key"}}, nil)
		manifestTestUsage(t, err, "HF_TOKEN")
	})
}

func TestManifestModelValidation(t *testing.T) {
	invalid := []Flags{
		{"id": nil}, {"id": {""}}, {"engine": {"not-an-engine"}}, {"provider": {"../bad"}},
		{"image": {"https://registry/image"}}, {"engine-arg": {""}}, {"engine-arg": {"bad\x00arg"}},
		{"prompt": {"not a model option"}}, {"trust-remote-code": {"yes"}}, {"gateway": {"0"}}, {"served-name": {" "}},
		{"model-path": {"/models/demo"}}, {"id": nil, "image": {"demo:v1"}, "model-path": {"relative"}},
		{"id": nil, "image": {"demo:v1"}, "model-path": {"/models/../private"}},
		{"id": nil, "image": {"demo:v1"}, "model-path": {"/models/demo"}, "provider": {"kaito"}},
		{"id": nil, "image": {"demo:v1"}, "model-path": {"/models/demo"}, "served-name": {"not-supported"}},
		{"id": nil, "image": {"demo:v1"}, "model-path": {"/models/demo"}, "credential": {"credentials"}},
		{"id": nil, "model-path": {"/models/demo"}}, {"id": nil, "image": {"demo:v1"}, "model-path": {"/"}},
		{"served-name": {"chat\nmodel"}}, {"credential": {"token/"}}, {"credential": {"BadName"}},
	}
	for i, flags := range invalid {
		t.Run(fmt.Sprintf("flags %d", i), func(t *testing.T) { _, err := manifestTestModelResult(flags, nil); manifestTestUsage(t, err, "") })
	}
	for _, key := range []string{"gpus", "replicas", "context-length"} {
		for _, v := range []string{"-1", "+1", "1.5", "nope", "Infinity", "2147483648", " 1", "1 ", ""} {
			t.Run(key+"="+v, func(t *testing.T) {
				_, err := manifestTestModelResult(Flags{key: {v}}, nil)
				manifestTestUsage(t, err, "--"+key)
			})
		}
	}
	_, err := manifestTestModelResult(Flags{"context-length": {"0"}}, nil)
	manifestTestUsage(t, err, "--context-length")
	for _, key := range []string{"cpu", "memory"} {
		for _, v := range []string{"0", "-1", "NaN", "1GB", " 2", "2 Gi", strings.Repeat("9", 400)} {
			t.Run(key+"="+v, func(t *testing.T) {
				_, err := manifestTestModelResult(Flags{key: {v}}, nil)
				manifestTestUsage(t, err, "--"+key)
			})
		}
		for _, v := range []string{"500m", "4", "8Gi", ".5", "1.", "1e3", "1E-3", "100u", "10n"} {
			t.Run("valid "+key+v, func(t *testing.T) {
				manifestTestEqual(t, get(manifestTestModel(t, Flags{key: {v}}), "spec", "resources", key), v)
			})
		}
	}
	for _, pair := range [][2]string{{"BadName", "team"}, {"demo", "../team"}, {"1demo", "team"}, {"demo", "bad.namespace"}, {strings.Repeat("a", 64), "team"}, {"demo", ""}} {
		t.Run("name "+pair[0]+" namespace "+pair[1], func(t *testing.T) {
			_, err := buildModel(pair[0], Flags{"id": {"org/model"}}, pair[1], nil)
			manifestTestUsage(t, err, "Provide")
		})
	}
}

func TestManifestModelSources(t *testing.T) {
	for _, uri := range []string{"s3://bucket/models/demo", "s3://bucket/", "gs://bucket/models/demo", "oci://registry.example.test/models/demo:v1", "oci://registry.example.test/models/demo@sha256:" + strings.Repeat("a", 64), "oci://localhost:5000/model:v1"} {
		t.Run(uri, func(t *testing.T) {
			result := manifestTestModel(t, Flags{"id": {uri}})
			manifestTestEqual(t, get(result, "spec", "model"), Object{"source": "custom", "id": "/model-cache/artifacts", "artifact": Object{"uri": uri}, "storage": Object{"volumes": []any{Object{"name": "model-cache", "purpose": "modelCache", "mountPath": "/model-cache", "readOnly": false, "size": "100Gi"}}}})
			manifestTestEqual(t, get(result, "spec", "provider"), Object{"name": "vllm"})
			manifestTestEqual(t, get(result, "spec", "engine", "type"), "vllm")
		})
	}
	t.Run("HTTPS file names", func(t *testing.T) {
		uri := "https://models.example.test/files/model.gguf"
		manifestTestEqual(t, get(manifestTestModel(t, Flags{"id": {uri}}), "spec", "model", "id"), "/model-cache/artifacts/model.gguf")
		result := manifestTestModel(t, Flags{"id": {uri}, "file": {"quantized/chat.gguf"}})
		manifestTestEqual(t, get(result, "spec", "model", "id"), "/model-cache/artifacts/quantized/chat.gguf")
		manifestTestEqual(t, get(result, "spec", "model", "artifact"), Object{"uri": uri, "file": "quantized/chat.gguf"})
		manifestTestEqual(t, get(manifestTestModel(t, Flags{"id": {"https://models.example.test/模型.gguf"}}), "spec", "model", "id"), "/model-cache/artifacts/模型.gguf")
	})
	t.Run("separate source and runtime options", func(t *testing.T) {
		result := manifestTestModel(t, Flags{"id": {"s3://bucket/prefix"}, "file": {"weights/chat.gguf"}, "credential": {"cloud/key.json"}, "service-account": {"model-loader"}, "image": {"vllm/runtime:v1"}, "artifact-image": {"registry.example.test/loader:v2"}, "storage-size": {"250Gi"}, "storage-class": {"fast-rwx"}, "served-name": {"chat"}})
		manifestTestEqual(t, get(result, "spec", "model", "artifact"), Object{"uri": "s3://bucket/prefix", "file": "weights/chat.gguf", "credentialsRef": Object{"name": "cloud", "key": "key.json"}, "image": "registry.example.test/loader:v2", "serviceAccountName": "model-loader"})
		manifestTestEqual(t, get(result, "spec", "model", "id"), "/model-cache/artifacts/weights/chat.gguf")
		manifestTestEqual(t, get(result, "spec", "model", "servedName"), "chat")
		manifestTestEqual(t, array(get(result, "spec", "model", "storage", "volumes"))[0], Object{"name": "model-cache", "purpose": "modelCache", "mountPath": "/model-cache", "readOnly": false, "size": "250Gi", "storageClassName": "fast-rwx"})
		manifestTestEqual(t, get(result, "spec", "engine", "image"), "vllm/runtime:v1")
		manifestTestEqual(t, get(result, "spec", "secrets"), nil)
	})
	t.Run("default credential key and empty storage class", func(t *testing.T) {
		result := manifestTestModel(t, Flags{"id": {"gs://bucket/model"}, "credential": {"cloud"}, "storage-class": {""}})
		manifestTestEqual(t, get(result, "spec", "model", "artifact", "credentialsRef"), Object{"name": "cloud"})
		volume := object(array(get(result, "spec", "model", "storage", "volumes"))[0])
		if v, ok := volume["storageClassName"]; !ok || v != "" {
			t.Fatal("empty storage class was omitted")
		}
	})
	for _, flags := range []Flags{{"revision": {"main"}}, {"file": {"weights/model.gguf"}}, {"revision": {"refs/pr/1"}, "file": {"weights/model.gguf"}}, {"id": {"hf://Qwen/Qwen3-8B"}, "revision": {"main"}}} {
		t.Run("staged HF "+flags.Text("id")+flags.Text("revision")+flags.Text("file"), func(t *testing.T) {
			flags = flags.Copy()
			flags["credential"] = []string{"hf-token"}
			result := manifestTestModel(t, flags)
			uri := "hf://org/model"
			if flags.Has("id") {
				uri = flags.Text("id")
			}
			artifact := Object{"uri": uri}
			for _, k := range []string{"revision", "file"} {
				if flags.Has(k) {
					artifact[k] = flags.Text(k)
				}
			}
			id := "/model-cache/artifacts"
			if flags.Has("file") {
				id += "/" + flags.Text("file")
			}
			manifestTestEqual(t, get(result, "spec", "model", "artifact"), artifact)
			manifestTestEqual(t, get(result, "spec", "model", "id"), id)
			manifestTestEqual(t, get(result, "spec", "secrets"), Object{"huggingFaceToken": "hf-token"})
		})
	}
	t.Run("staged HF JSON credential", func(t *testing.T) {
		result := manifestTestModel(t, Flags{"revision": {"main"}, "credential": {"cloud/credentials"}})
		manifestTestEqual(t, get(result, "spec", "model", "artifact", "credentialsRef"), Object{"name": "cloud", "key": "credentials"})
		manifestTestEqual(t, get(result, "spec", "secrets"), nil)
	})
	for _, pair := range [][2]string{{"pvc://weights/models/chat", "/model-cache/models/chat"}, {"pvc://weights", "/model-cache"}, {"pvc://weights/", "/model-cache"}, {"pvc://weights/模型", "/model-cache/模型"}} {
		t.Run(pair[0], func(t *testing.T) {
			result := manifestTestModel(t, Flags{"id": {pair[0]}})
			manifestTestEqual(t, get(result, "spec", "model"), Object{"source": "custom", "id": pair[1], "storage": Object{"volumes": []any{Object{"name": "model-cache", "purpose": "modelCache", "mountPath": "/model-cache", "claimName": "weights", "readOnly": true}}}})
			manifestTestEqual(t, get(result, "spec", "provider", "name"), "vllm")
		})
	}
}

func TestManifestModelSourceValidation(t *testing.T) {
	invalid := []Flags{
		{"storage-size": {"100Gi"}}, {"storage-class": {"fast"}}, {"artifact-image": {"loader:v1"}}, {"service-account": {"loader"}},
		{"id": {"s3://bucket/model"}, "provider": {"kaito"}}, {"id": {"s3://bucket/model"}, "engine": {"sglang"}},
		{"revision": {"main"}, "provider": {"dynamo"}}, {"revision": {"main"}, "engine": {"llamacpp"}},
		{"id": {"pvc://claim/path"}, "file": {"model.gguf"}}, {"id": {"pvc://claim/path"}, "credential": {"cloud"}},
		{"id": {"pvc://claim/path"}, "storage-size": {"100Gi"}}, {"id": {"pvc://claim/path"}, "service-account": {"loader"}},
		{"id": {"pvc://claim/path"}, "served-name": {"chat"}}, {"id": {"s3://bucket/model"}, "revision": {"main"}},
		{"revision": {strings.Repeat("a", 257)}}, {"id": {"gs://bucket/model"}, "storage-size": {"0"}},
		{"id": {"gs://bucket/model"}, "storage-size": {"100GB"}}, {"id": {"gs://bucket/model"}, "storage-class": {"../fast"}},
		{"id": {"gs://bucket/model"}, "service-account": {"../loader"}}, {"id": {"gs://bucket/model"}, "artifact-image": {"loader"}},
		{"id": {"gs://bucket/model"}, "credential": {"cloud/key/extra"}},
		{"id": {"s3://bucket/model"}, "artifact-image": {"invalid..example.test/loader:v1"}},
		{"id": {"s3://bucket/model"}, "artifact-image": {"loader@sha256:bad"}},
	}
	for i, flags := range invalid {
		t.Run(fmt.Sprintf("combination %d", i), func(t *testing.T) { _, err := manifestTestModelResult(flags, nil); manifestTestUsage(t, err, "") })
	}
	uris := []string{
		"ftp://example.test/model", "http://example.test/model", "file:///models/chat", "/tmp/model", "../model",
		"hf://org/model/file", "hf://org/../model", "hf://org/model?token=private-value",
		"s3://bucket/../model", "s3://bucket/a/./model", "gs://bucket/a//model", "gs://bucket/%2e%2e/model",
		"s3://bucket/model?signature=private-value", manifestTestCredentialURL("s3://bucket/model"),
		manifestTestCredentialURL("https://example.test/model.gguf"), "https://example.test/model?sig=private-value",
		"https://example.test/model#private-value", "https://example.test/../model", "https://example.test/%252e%252e/model",
		"https://example.test/", "https://example.test/models/", "https://example.test/a\\..\\model",
		"oci://registry.example.test/models/demo", "oci://registry.example.test/models/demo@sha256:bad",
		manifestTestCredentialURL("oci://registry.example.test/model:v1"), "oci://registry.example.test/../model:v1",
		"pvc://claim/../../private", "pvc://claim/%2fprivate", "pvc://claim/path?token=private-value",
		"s3://bad_bucket/model", "s3://bucket:123/model", "s3://bucket//", "oci://invalid..example.test/model:v1",
		"https://example.test:99999/model", "https://[invalid]/model", "https://example.test/model\x00",
		"s3://bucket/" + strings.Repeat("a", 1025), "hf://org/" + strings.Repeat("a", 4090),
	}
	for i, uri := range uris {
		t.Run(fmt.Sprintf("unsafe source %d", i), func(t *testing.T) {
			streams, reader, out := manifestTestIO("")
			_, err := manifestTestModelResult(Flags{"id": {uri}}, streams)
			manifestTestUsage(t, err, "")
			if reader.reads != 0 || out.Len() != 0 {
				t.Fatal("model validation accessed IO")
			}
		})
	}
	for _, path := range []string{"../model.gguf", "/model.gguf", "a/../model", "a//model", "a/./model", "%2e%2e/model", "a\\model", "", "model\x00file", strings.Repeat("a", 1025), "._airunway_complete.json", "a/._airunway_complete.json"} {
		t.Run("unsafe file "+path, func(t *testing.T) {
			_, err := manifestTestModelResult(Flags{"file": {path}}, nil)
			manifestTestUsage(t, err, "")
		})
	}
}

func TestManifestAgentConstruction(t *testing.T) {
	t.Run("minimal", func(t *testing.T) {
		manifestTestEqual(t, manifestTestAgent(t, nil), Object{
			"apiVersion": "airunway.ai/v1alpha1", "kind": "AgentDeployment",
			"metadata": Object{"name": "helper", "namespace": "team", "annotations": Object{"airunway.ai/managed-by": "cli"}},
			"spec":     Object{"framework": Object{"name": "langgraph"}, "lifecycle": "deployment", "model": Object{"deploymentRef": Object{"name": "demo"}}},
		})
	})
	t.Run("cross namespace references", func(t *testing.T) {
		manifestTestEqual(t, get(manifestTestAgent(t, Flags{"model-ref": {"models/demo"}}), "spec", "model"), Object{"deploymentRef": Object{"namespace": "models", "name": "demo"}})
		manifestTestEqual(t, get(manifestTestAgent(t, manifestTestGateway(Flags{"model-gateway": {"edge/shared"}, "gateway-listener": {"https"}})), "spec", "model"), Object{"gatewayEndpoint": Object{"gatewayRef": Object{"name": "shared", "namespace": "edge", "listenerName": "https"}, "modelName": "served-model"}})
	})
	for _, pair := range [][2]string{{"openai", "openai"}, {"anthropic", "anthropic"}, {"azure-openai", "azureOpenAI"}, {"azureOpenAI", "azureOpenAI"}, {"custom", "custom"}} {
		t.Run(pair[0], func(t *testing.T) {
			result := manifestTestAgent(t, manifestTestExternal(Flags{"model-api": {pair[0]}, "model-credential": {"api-key/token"}}))
			manifestTestEqual(t, get(result, "spec", "model"), Object{"externalAPI": Object{"baseURL": "https://api.example.test/v1", "type": pair[1], "modelName": "remote-model", "credentialsRef": Object{"name": "api-key", "key": "token"}}})
		})
	}
	for _, endpoint := range []string{"http://model.team.svc:8000/v1", "https://[::1]:8443/v1", "https://api.example.test"} {
		t.Run(endpoint, func(t *testing.T) {
			result := manifestTestAgent(t, manifestTestExternal(Flags{"model-url": {endpoint}}))
			manifestTestEqual(t, get(result, "spec", "model", "externalAPI", "baseURL"), endpoint)
			manifestTestEqual(t, get(result, "spec", "model", "externalAPI", "credentialsRef"), nil)
		})
	}
	t.Run("prompt file and container settings", func(t *testing.T) {
		prompt := "Keep this prompt.\nIncluding trailing newline.\n"
		result := manifestTestAgent(t, Flags{"prompt-file": {manifestTestFixture(t, prompt)}, "image": {"registry.example.test/agent:v1"}, "cpu": {"500m"}, "memory": {"2Gi"}})
		manifestTestEqual(t, get(result, "spec", "config"), Object{"systemPrompt": prompt, "image": "registry.example.test/agent:v1"})
		manifestTestEqual(t, get(result, "spec", "resources"), Object{"requests": Object{"cpu": "500m", "memory": "2Gi"}})
		manifestTestEqual(t, get(result, "spec", "image"), nil)
	})
	t.Run("once task from stdin", func(t *testing.T) {
		streams, reader, out := manifestTestIO("Summarize the incident.\n")
		result, err := manifestTestAgentResult(Flags{"mode": {"once"}, "prompt": {"Be concise."}, "task-file": {"-"}}, streams)
		if err != nil {
			t.Fatal(err)
		}
		manifestTestEqual(t, get(result, "spec", "lifecycle"), "job")
		manifestTestEqual(t, get(result, "spec", "config"), Object{"systemPrompt": "Be concise.", "task": "Summarize the incident.\n"})
		if reader.reads == 0 || out.Len() != 0 {
			t.Fatal("unexpected task IO")
		}
	})
	t.Run("job task file", func(t *testing.T) {
		result := manifestTestAgent(t, Flags{"mode": {"job"}, "task-file": {manifestTestFixture(t, "Complete the task.")}})
		manifestTestEqual(t, get(result, "spec", "lifecycle"), "job")
		manifestTestEqual(t, get(result, "spec", "config", "task"), "Complete the task.")
	})
}

func TestManifestAgentConfiguration(t *testing.T) {
	t.Run("merge nested config", func(t *testing.T) {
		path := manifestTestFixture(t, `{"nested":{"second":2},"command":["python","agent.py"]}`)
		result := manifestTestAgent(t, Flags{"__preset-config": {`{"nested":{"first":1},"image":"example/agent:v1"}`}, "config-file": {path}, "image": {"example/agent:v1"}, "prompt": {"Instructions."}})
		manifestTestEqual(t, get(result, "spec", "config"), Object{"nested": Object{"first": 1, "second": 2}, "command": []string{"python", "agent.py"}, "image": "example/agent:v1", "systemPrompt": "Instructions."})
	})
	t.Run("identical nested config and arrays", func(t *testing.T) {
		path := manifestTestFixture(t, `{"nested":{"second":2,"first":1},"items":[{"b":2,"a":1}]}`)
		result := manifestTestAgent(t, Flags{"__preset-config": {`{"nested":{"first":1,"second":2},"items":[{"a":1,"b":2}]}`}, "config-file": {path}})
		manifestTestEqual(t, get(result, "spec", "config"), Object{"nested": Object{"first": 1, "second": 2}, "items": []any{Object{"a": 1, "b": 2}}})
	})
	t.Run("job task from config", func(t *testing.T) {
		result := manifestTestAgent(t, Flags{"mode": {"once"}, "config-file": {manifestTestFixture(t, `{"task":"Do the work."}`)}})
		manifestTestEqual(t, get(result, "spec", "config", "task"), "Do the work.")
		result = manifestTestAgent(t, Flags{"mode": {"once"}, "__preset-config": {`{"prompt":"Legacy task."}`}})
		manifestTestEqual(t, get(result, "spec", "config", "prompt"), "Legacy task.")
	})
	t.Run("unsafe config keys match declarative validation", func(t *testing.T) {
		streams, _, _ := manifestTestIO("")
		_, err := manifestTestAgentResult(Flags{"__preset-config": {`{"__proto__":{"polluted":true},"constructor":{"name":"data"}}`}}, streams)
		manifestTestUsage(t, err, "Unsafe document key")
	})
	for _, content := range []string{"plain: yaml", "[]", "null", "42", `"string"`, "{broken-json", `{} {}`} {
		t.Run("invalid JSON "+content, func(t *testing.T) {
			streams, _, _ := manifestTestIO("")
			_, err := manifestTestAgentResult(Flags{"config-file": {manifestTestFixture(t, content)}}, streams)
			manifestTestUsage(t, err, "--config-file")
		})
	}
	for _, content := range []string{`{"systemPrompt":42}`, `{"task":{}}`, `{"image":null}`, `{"image":"https://example.test/image"}`} {
		t.Run("managed field "+content, func(t *testing.T) {
			streams, _, _ := manifestTestIO("")
			_, err := manifestTestAgentResult(Flags{"__preset-config": {content}}, streams)
			manifestTestUsage(t, err, "")
		})
	}
	conflicts := []struct {
		config string
		flags  Flags
	}{
		{`{"systemPrompt":"from file"}`, Flags{"prompt": {"explicit"}}},
		{`{"task":"from file"}`, Flags{"task": {"explicit"}, "mode": {"once"}}},
		{`{"image":"from-file:v1"}`, Flags{"image": {"explicit:v1"}}},
		{`{"nested":{"setting":2}}`, Flags{"__preset-config": {`{"nested":{"setting":1}}`}}},
		{`{"nested":null}`, Flags{"__preset-config": {`{"nested":{}}`}}},
		{`{"items":[2,1]}`, Flags{"__preset-config": {`{"items":[1,2]}`}}},
	}
	for i, item := range conflicts {
		t.Run(fmt.Sprintf("conflict %d", i), func(t *testing.T) {
			flags := item.flags.Copy()
			flags["config-file"] = []string{manifestTestFixture(t, item.config)}
			streams, _, _ := manifestTestIO("")
			_, err := manifestTestAgentResult(flags, streams)
			manifestTestUsage(t, err, "conflicts")
		})
	}
}

func TestManifestAgentValidation(t *testing.T) {
	invalid := []Flags{
		{"framework": nil}, {"model-ref": nil}, {"model-url": {"https://api.example.test/v1"}}, {"model-gateway": {"gateway"}}, {"model-api": {"openai"}},
		manifestTestExternal(Flags{"model-gateway": {"edge"}}), manifestTestExternal(Flags{"model-api": nil}), manifestTestExternal(Flags{"model-id": nil}),
		manifestTestExternal(Flags{"model-api": {"unsupported"}}), manifestTestExternal(Flags{"model-credential": {"key-without-field"}}),
		manifestTestExternal(Flags{"model-credential": {"name/"}}), manifestTestExternal(Flags{"model-credential": {"name/key/extra"}}),
		manifestTestExternal(Flags{"gateway-listener": {"https"}}), manifestTestExternal(Flags{"model-url": {manifestTestCredentialURL("https://example.test/v1")}}),
		manifestTestExternal(Flags{"model-url": {"https://example.test/v1?api_key=private-value"}}), manifestTestExternal(Flags{"model-url": {"ftp://example.test/v1"}}),
		manifestTestGateway(Flags{"model-id": nil}), manifestTestGateway(Flags{"model-credential": {"key/token"}}), manifestTestGateway(Flags{"model-api": {"openai"}}),
		{"model-ref": {"too/many/segments"}}, {"framework": {"../invalid"}}, {"mode": {"unknown"}}, {"mode": {"once"}}, {"mode": {"once"}, "task": {" "}},
		{"task": {"only jobs"}}, {"gpus": {"1"}}, {"replicas": {"2"}}, {"provider": {"vllm"}}, {"cpu": {"-1"}},
		{"prompt": {"inline"}, "prompt-file": {"-"}}, {"task": {"inline"}, "task-file": {"-"}, "mode": {"once"}},
		{"__preset-config": {"[]"}}, {"__preset-config": {"invalid-json"}}, {"framework": {"bad.framework"}},
		{"model-ref": {"bad.namespace/model"}}, manifestTestGateway(Flags{"model-gateway": {"bad.namespace/gateway"}}),
		{"mode": {"once"}, "__preset-config": {`{"task":"","prompt":"not a fallback"}`}},
		{"framework": {strings.Repeat("a", 64)}}, {"model-ref": {"/demo"}}, {"model-ref": {"team/"}},
	}
	for i, flags := range invalid {
		t.Run(fmt.Sprintf("flags %d", i), func(t *testing.T) {
			streams, _, out := manifestTestIO("")
			_, err := manifestTestAgentResult(flags, streams)
			manifestTestUsage(t, err, "")
			if out.Len() != 0 {
				t.Fatal("invalid input was logged")
			}
		})
	}
	for _, flags := range []Flags{{"config-file": {"-"}, "prompt-file": {"-"}}, {"config-file": {"-"}, "task-file": {"-"}, "mode": {"once"}}, {"prompt-file": {"-"}, "task-file": {"-"}, "mode": {"once"}}} {
		t.Run("stdin single use "+fmt.Sprint(flags), func(t *testing.T) {
			streams, reader, _ := manifestTestIO("{}")
			_, err := manifestTestAgentResult(flags, streams)
			manifestTestUsage(t, err, "Only one")
			if reader.reads != 0 {
				t.Fatal("stdin consumed before conflict check")
			}
		})
	}
	t.Run("missing and oversized files", func(t *testing.T) {
		streams, _, _ := manifestTestIO("")
		_, err := manifestTestAgentResult(Flags{"prompt-file": {filepath.Join(t.TempDir(), "missing")}}, streams)
		manifestTestUsage(t, err, "Cannot read")
		_, err = manifestTestAgentResult(Flags{"prompt-file": {manifestTestFixture(t, strings.Repeat("x", maxInput+1))}}, streams)
		manifestTestUsage(t, err, "size limit")
		streams, _, _ = manifestTestIO(strings.Repeat("x", maxInput+1))
		_, err = manifestTestAgentResult(Flags{"prompt-file": {"-"}}, streams)
		manifestTestUsage(t, err, "size limit")
	})
}

func TestManifestSparseModelUpdates(t *testing.T) {
	t.Run("preserve concurrency and omitted fields", func(t *testing.T) {
		existing := manifestTestServerFields(manifestTestModel(t, Flags{"provider": {"vllm"}, "engine": {"vllm"}, "gpus": {"4"}, "cpu": {"8"}, "memory": {"32Gi"}, "replicas": {"3"}, "context-length": {"4096"}}))
		object(get(existing, "spec", "resources", "gpu"))["type"] = "vendor.example/gpu"
		object(existing["spec"])["futureSetting"] = Object{"enabled": true}
		before := cloneObject(existing)
		patch := manifestTestUpdate(t, "model", existing, Flags{"memory": {"64Gi"}}, "")
		manifestTestEqual(t, patch, Object{"apiVersion": existing["apiVersion"], "kind": existing["kind"], "metadata": Object{"name": "demo", "namespace": "team", "resourceVersion": "42"}, "spec": Object{"resources": Object{"memory": "64Gi"}}})
		manifestTestEqual(t, existing, before)
		merged := manifestTestMergePatch(existing, patch)
		manifestTestEqual(t, get(merged, "spec", "resources"), Object{"gpu": Object{"count": 4, "type": "vendor.example/gpu"}, "cpu": "8", "memory": "64Gi"})
		for _, path := range [][]string{{"spec", "scaling"}, {"spec", "engine"}, {"spec", "futureSetting"}, {"metadata", "annotations"}, {"status"}} {
			manifestTestEqual(t, get(merged, path...), get(existing, path...))
		}
	})
	t.Run("explicit zero false and argument clear", func(t *testing.T) {
		existing := manifestTestServerFields(manifestTestModel(t, Flags{"engine-arg": {"--old"}, "trust-remote-code": {"true"}}))
		patch := manifestTestUpdate(t, "model", existing, Flags{"gpus": {"0"}, "replicas": {"0"}, "gateway": {"false"}, "trust-remote-code": {"false"}, "engine-arg": {}}, "")
		manifestTestEqual(t, patch["spec"], Object{"resources": Object{"gpu": Object{"count": 0}}, "scaling": Object{"replicas": 0}, "engine": Object{"trustRemoteCode": false, "extraArgs": []any{}}, "gateway": Object{"enabled": false}})
		manifestTestEqual(t, get(manifestTestMergePatch(existing, patch), "spec", "engine", "extraArgs"), []any{})
		manifestTestEqual(t, patch["status"], nil)
		manifestTestEqual(t, get(patch, "metadata", "uid"), nil)
		manifestTestEqual(t, get(patch, "metadata", "managedFields"), nil)
	})
	t.Run("all runtime fields", func(t *testing.T) {
		patch := manifestTestUpdate(t, "model", manifestTestModel(t, nil), Flags{"served-name": {"chat"}, "context-length": {"2048"}, "image": {"runtime:v2"}, "engine-arg": {"--dtype=half"}, "credential": {"next-token"}, "cpu": {"2"}}, "")
		manifestTestEqual(t, patch["spec"], Object{"model": Object{"servedName": "chat"}, "engine": Object{"contextLength": 2048, "image": "runtime:v2", "extraArgs": []string{"--dtype=half"}}, "secrets": Object{"huggingFaceToken": "next-token"}, "resources": Object{"cpu": "2"}})
	})
	t.Run("artifact state preserved", func(t *testing.T) {
		existing := manifestTestServerFields(manifestTestModel(t, Flags{"id": {"s3://bucket/model"}, "credential": {"cloud"}}))
		patch := manifestTestUpdate(t, "model", existing, Flags{"context-length": {"16384"}, "served-name": {"chat"}}, "")
		manifestTestEqual(t, patch["spec"], Object{"engine": Object{"contextLength": 16384}, "model": Object{"servedName": "chat"}})
		manifestTestEqual(t, get(manifestTestMergePatch(existing, patch), "spec", "model", "artifact"), get(existing, "spec", "model", "artifact"))
		_, err := updateResource("model", existing, Flags{"credential": {"replacement"}}, nil)
		manifestTestUsage(t, err, "immutable")
	})
	t.Run("custom credential and served name rejected", func(t *testing.T) {
		existing := manifestTestModel(t, Flags{"id": {"pvc://claim/model"}})
		for _, flags := range []Flags{{"credential": {"new-token"}}, {"served-name": {"chat"}}} {
			_, err := updateResource("model", existing, flags, nil)
			manifestTestUsage(t, err, "")
		}
	})
	t.Run("optional server metadata omitted", func(t *testing.T) {
		existing := manifestTestModel(t, nil)
		delete(object(existing["metadata"]), "namespace")
		patch := manifestTestUpdate(t, "model", existing, Flags{"cpu": {"1"}}, "")
		manifestTestEqual(t, patch["metadata"], Object{"name": "demo"})
	})
}

func TestManifestSparseAgentUpdates(t *testing.T) {
	t.Run("prompt update preserves config", func(t *testing.T) {
		existing := manifestTestServerFields(manifestTestAgent(t, Flags{"prompt": {"old"}, "image": {"agent:v1"}, "__preset-config": {`{"skills":["search"],"nested":{"setting":true}}`}}))
		before := cloneObject(existing)
		patch := manifestTestUpdate(t, "agent", existing, Flags{"prompt-file": {"-"}}, "new\n")
		manifestTestEqual(t, patch["spec"], Object{"config": Object{"systemPrompt": "new\n"}})
		manifestTestEqual(t, get(patch, "metadata", "resourceVersion"), "42")
		manifestTestEqual(t, patch["status"], nil)
		manifestTestEqual(t, existing, before)
		manifestTestEqual(t, get(manifestTestMergePatch(existing, patch), "spec", "config"), Object{"skills": []string{"search"}, "nested": Object{"setting": true}, "systemPrompt": "new\n", "image": "agent:v1"})
	})
	t.Run("empty prompt clears", func(t *testing.T) {
		patch := manifestTestUpdate(t, "agent", manifestTestAgent(t, Flags{"prompt": {"old"}}), Flags{"prompt": {""}}, "")
		manifestTestEqual(t, patch["spec"], Object{"config": Object{"systemPrompt": ""}})
	})
	modes := []struct {
		kind  string
		flags Flags
	}{{"deploymentRef", Flags{"model-ref": {"replacement"}}}, {"externalAPI", manifestTestExternal(nil)}, {"gatewayEndpoint", manifestTestGateway(nil)}}
	for _, old := range modes {
		for _, next := range modes {
			if old.kind == next.kind {
				continue
			}
			t.Run(old.kind+" to "+next.kind, func(t *testing.T) {
				existing := manifestTestServerFields(manifestTestAgent(t, old.flags))
				before := cloneObject(existing)
				patch := manifestTestUpdate(t, "agent", existing, manifestTestDefaults(Flags{}, next.flags), "")
				model := object(get(patch, "spec", "model"))
				if v, ok := model[old.kind]; !ok || v != nil {
					t.Fatal("old binding not explicitly cleared")
				}
				merged := object(get(manifestTestMergePatch(existing, patch), "spec", "model"))
				if len(merged) != 1 || merged[next.kind] == nil {
					t.Fatalf("invalid union after merge: %v", merged)
				}
				for _, key := range []string{"framework", "lifecycle", "config"} {
					manifestTestEqual(t, get(patch, "spec", key), nil)
				}
				manifestTestEqual(t, existing, before)
			})
		}
	}
	t.Run("partial external updates", func(t *testing.T) {
		existing := manifestTestAgent(t, manifestTestExternal(Flags{"model-credential": {"existing/key"}}))
		patch := manifestTestUpdate(t, "agent", existing, Flags{"model-id": {"new-model"}}, "")
		manifestTestEqual(t, patch["spec"], Object{"model": Object{"externalAPI": Object{"modelName": "new-model"}}})
		want := cloneObject(object(get(existing, "spec", "model", "externalAPI")))
		want["modelName"] = "new-model"
		manifestTestEqual(t, get(manifestTestMergePatch(existing, patch), "spec", "model", "externalAPI"), want)
		patch = manifestTestUpdate(t, "agent", existing, Flags{"model-api": {"azure-openai"}, "model-credential": {"new/key"}}, "")
		manifestTestEqual(t, patch["spec"], Object{"model": Object{"externalAPI": Object{"type": "azureOpenAI", "credentialsRef": Object{"name": "new", "key": "key"}}}})
		patch = manifestTestUpdate(t, "agent", existing, Flags{"model-url": {"https://next.example.test/v2"}}, "")
		manifestTestEqual(t, patch["spec"], Object{"model": Object{"externalAPI": Object{"baseURL": "https://next.example.test/v2"}}})
	})
	t.Run("partial gateway updates", func(t *testing.T) {
		existing := manifestTestAgent(t, manifestTestGateway(nil))
		patch := manifestTestUpdate(t, "agent", existing, Flags{"gateway-listener": {"https"}, "model-id": {"new-model"}}, "")
		manifestTestEqual(t, patch["spec"], Object{"model": Object{"gatewayEndpoint": Object{"gatewayRef": Object{"listenerName": "https"}, "modelName": "new-model"}}})
		manifestTestEqual(t, get(manifestTestMergePatch(existing, patch), "spec", "model", "gatewayEndpoint", "gatewayRef"), Object{"name": "edge", "listenerName": "https"})
	})
	t.Run("unqualified refs clear namespace and listener", func(t *testing.T) {
		existing := manifestTestAgent(t, Flags{"model-ref": {"other/demo"}})
		patch := manifestTestUpdate(t, "agent", existing, Flags{"model-ref": {"local"}}, "")
		manifestTestEqual(t, get(patch, "spec", "model", "deploymentRef"), Object{"name": "local", "namespace": nil})
		manifestTestEqual(t, get(manifestTestMergePatch(existing, patch), "spec", "model", "deploymentRef"), Object{"name": "local"})
		existing = manifestTestAgent(t, manifestTestGateway(Flags{"model-gateway": {"other/edge"}, "gateway-listener": {"old"}}))
		before := cloneObject(existing)
		patch = manifestTestUpdate(t, "agent", existing, Flags{"model-gateway": {"new-edge"}}, "")
		manifestTestEqual(t, get(patch, "spec", "model", "gatewayEndpoint", "gatewayRef"), Object{"name": "new-edge", "namespace": nil, "listenerName": nil})
		manifestTestEqual(t, get(manifestTestMergePatch(existing, patch), "spec", "model", "gatewayEndpoint", "gatewayRef"), Object{"name": "new-edge"})
		manifestTestEqual(t, existing, before)
		patch = manifestTestUpdate(t, "agent", existing, Flags{"model-gateway": {"next/new-edge"}, "gateway-listener": {"new"}}, "")
		manifestTestEqual(t, get(manifestTestMergePatch(existing, patch), "spec", "model", "gatewayEndpoint", "gatewayRef"), Object{"name": "new-edge", "namespace": "next", "listenerName": "new"})
	})
	t.Run("repair invalid union", func(t *testing.T) {
		existing := manifestTestAgent(t, manifestTestExternal(nil))
		object(get(existing, "spec", "model"))["deploymentRef"] = Object{"name": "old"}
		patch := manifestTestUpdate(t, "agent", existing, manifestTestDefaults(Flags{}, manifestTestGateway(nil)), "")
		if len(object(get(manifestTestMergePatch(existing, patch), "spec", "model"))) != 1 {
			t.Fatal("invalid union not repaired")
		}
	})
}

func TestManifestUpdateValidation(t *testing.T) {
	for _, noun := range []string{"model", "agent"} {
		existing := manifestTestModel(t, nil)
		if noun == "agent" {
			existing = manifestTestAgent(t, nil)
		}
		for _, key := range manifestImmutableFlags {
			t.Run(noun+" immutable "+key, func(t *testing.T) {
				_, err := updateResource(noun, existing, Flags{key: {"same"}}, nil)
				manifestTestUsage(t, err, "immutable")
			})
		}
		for _, flags := range []Flags{{}, {"wait": {"false"}, "output": {"json"}, "namespace": {"elsewhere"}, "timeout": {"1m"}}} {
			t.Run(noun+" no mutable flags "+fmt.Sprint(flags), func(t *testing.T) {
				_, err := updateResource(noun, existing, flags, nil)
				manifestTestUsage(t, err, "mutable")
			})
		}
	}
	existing := manifestTestAgent(t, nil)
	for _, flags := range []Flags{{"image": {"agent:v2"}}, {"cpu": {"2"}}, {"replicas": {"2"}}, {"task": {"new"}}, {"config-file": {"-"}}, {"__preset-config": {"{}"}},
		{"model-url": {"https://api.example.test/v1"}}, {"model-id": {"no-endpoint"}}, {"model-ref": {"new"}, "model-gateway": {"edge"}}} {
		t.Run("invalid agent update "+fmt.Sprint(flags), func(t *testing.T) {
			_, err := updateResource("agent", existing, flags, nil)
			manifestTestUsage(t, err, "")
		})
	}
	_, err := updateResource("agent", manifestTestAgent(t, Flags{"mode": {"once"}, "task": {"Do the work."}}), Flags{"prompt": {"new"}}, nil)
	manifestTestUsage(t, err, "One-shot")
	_, err = updateResource("agent", manifestTestModel(t, nil), Flags{"prompt": {"new"}}, nil)
	manifestTestUsage(t, err, "not a agent")
	_, err = updateResource("provider", Object{}, Flags{}, nil)
	manifestTestUsage(t, err, "Only model and agent")
	for _, key := range []string{"gateway", "trust-remote-code"} {
		_, err := updateResource("model", manifestTestModel(t, nil), Flags{key: {"no"}}, nil)
		manifestTestUsage(t, err, "true or false")
	}
}

func TestManifestHelpersDoNotMutateInputs(t *testing.T) {
	target := Object{"nested": Object{"one": 1}, "array": []any{1, 2}}
	source := Object{"nested": Object{"two": 2}, "array": []any{1, 2}}
	beforeTarget, beforeSource := cloneObject(target), cloneObject(source)
	merged, err := manifestMergeConfig(target, source, "configuration")
	if err != nil {
		t.Fatal(err)
	}
	manifestTestEqual(t, target, beforeTarget)
	manifestTestEqual(t, source, beforeSource)
	manifestTestEqual(t, merged, Object{"nested": Object{"one": 1, "two": 2}, "array": []any{1, 2}})
	flags := Flags{"image": {"runtime:v1"}, "engine-arg": {"--dtype=half"}, "trust-remote-code": {"false"}}
	beforeFlags := flags.Copy()
	manifestTestModel(t, flags)
	if !reflect.DeepEqual(flags, beforeFlags) {
		t.Fatal("manifest construction mutated flags")
	}
}

func TestManifestParsedRawEngineArguments(t *testing.T) {
	_, flags, err := parseArgs([]string{"--id", "org/model", "--engine-arg=--dtype=half", "--engine-arg=--enforce-eager", "--trust-remote-code=true"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := buildModel("demo", flags, "team", nil)
	if err != nil {
		t.Fatal(err)
	}
	manifestTestEqual(t, get(result, "spec", "engine"), Object{"extraArgs": []string{"--dtype=half", "--enforce-eager"}, "trustRemoteCode": true})
}

func TestManifestWhitespaceAndLimits(t *testing.T) {
	for _, flags := range []Flags{{"served-name": {"\ufeffchat"}}, {"id": {"https://models.example.test/a\ufeffb"}}, {"image": {"runtime:v1\ufeff"}}} {
		_, err := manifestTestModelResult(flags, nil)
		manifestTestUsage(t, err, "")
	}
	_, err := manifestTestAgentResult(Flags{"mode": {"once"}, "task": {"\ufeff"}}, nil)
	manifestTestUsage(t, err, "requires a task")
	result := manifestTestModel(t, Flags{"gpus": {"2147483647"}, "replicas": {"2147483647"}, "context-length": {"2147483647"}})
	manifestTestEqual(t, get(result, "spec", "resources", "gpu", "count"), 2147483647)
	manifestTestEqual(t, get(result, "spec", "scaling", "replicas"), 2147483647)
	manifestTestEqual(t, get(result, "spec", "engine", "contextLength"), 2147483647)
	_, err = buildModel(strings.Repeat("a", 63), Flags{"id": {"org/model"}}, strings.Repeat("b", 63), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"gateway", "trust-remote-code", "image"} {
		_, err := buildModel("demo", Flags{"id": {"org/model"}, key: nil}, "team", nil)
		manifestTestUsage(t, err, "--"+key)
	}
}
