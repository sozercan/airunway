package dynamointent

import (
	"strings"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestToolParsers(t *testing.T) {
	for _, tc := range []struct {
		model, parser, reasoning, wantTool, wantReason string
		wantErr                                        bool
	}{
		{model: "Qwen/Qwen3-0.6B", wantTool: "hermes", wantReason: "qwen3"},
		{model: "Qwen/Qwen3-Coder-30B-A3B-Instruct", wantTool: "qwen3_coder"},
		{model: "Qwen/Qwen3.5-35B-A3B", wantTool: "qwen3_coder", wantReason: "qwen3"},
		{model: "qWeN/qWEN3-8B", wantTool: "hermes", wantReason: "qwen3"},
		{model: "Qwen/Qwen3-8B", reasoning: "none", wantErr: true},
		{model: "custom/model", parser: "llama3_json", wantTool: "llama3_json"},
		{model: "custom/model", parser: "hermes", reasoning: "qwen3", wantTool: "hermes", wantReason: "qwen3"},
		{model: "custom/model", wantErr: true},
		{model: "custom/qwen3-8B", wantErr: true},
		{model: "Qwen/Qwen30-8B", wantErr: true},
		{model: "Qwen/Qwen3-8B", parser: "auto", wantErr: true},
		{model: "Qwen/Qwen3-8B", reasoning: "auto", wantErr: true},
		{model: "Qwen/Qwen3-8B", parser: "hermes;echo", wantErr: true},
		{model: "Qwen/Qwen3-8B", reasoning: "Qwen3", wantErr: true},
		{model: "Qwen/Qwen3-8B", parser: strings.Repeat("a", 65), wantErr: true},
	} {
		t.Run(tc.model+"/"+tc.parser+"/"+tc.reasoning, func(t *testing.T) {
			md := fixture(valid)
			md.Spec.Model.ID = tc.model
			md.Spec.Engine.ToolCalling = true
			md.Spec.Engine.ToolCallParser = tc.parser
			md.Spec.Engine.ReasoningParser = tc.reasoning
			tool, reason, err := ToolParsers(md)
			if (err != nil) != tc.wantErr || tool != tc.wantTool || reason != tc.wantReason {
				t.Fatalf("got (%q,%q,%v), want (%q,%q,error=%v)", tool, reason, err, tc.wantTool, tc.wantReason, tc.wantErr)
			}
		})
	}
}

func TestToolCallingValidationAndConflicts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*api.ModelDeployment)
		wantErr bool
	}{
		{"valid intent", func(*api.ModelDeployment) {}, false},
		{"implicit intent provider", func(md *api.ModelDeployment) { md.Spec.Provider.Name = "" }, false},
		{"manual", func(md *api.ModelDeployment) { md.Spec.Provider.Overrides = nil }, false},
		{"wrong provider", func(md *api.ModelDeployment) { md.Spec.Provider = &api.ProviderSpec{Name: "vllm"} }, true},
		{"wrong resolved provider", func(md *api.ModelDeployment) { md.Status.Provider = &api.ProviderStatus{Name: "kaito"} }, true},
		{"missing manual provider", func(md *api.ModelDeployment) { md.Spec.Provider = nil }, true},
		{"unsupported engine", func(md *api.ModelDeployment) { md.Spec.Engine.Type = api.EngineTypeLlamaCpp }, true},
		{"disabled with parser", func(md *api.ModelDeployment) {
			md.Spec.Engine.ToolCalling = false
			md.Spec.Engine.ToolCallParser = "hermes"
		}, true},
		{"flag", func(md *api.ModelDeployment) { md.Spec.Engine.ExtraArgs = []string{"--dyn-tool-call-parser=hermes"} }, true},
		{"map flag", func(md *api.ModelDeployment) { md.Spec.Engine.Args = map[string]string{"tool-call-parser": "hermes"} }, true},
		{"env", func(md *api.ModelDeployment) {
			md.Spec.Env = []corev1.EnvVar{{Name: "DYN_TOOL_CALL_PARSER", Value: "hermes"}}
		}, true},
		{"native worker flags", func(md *api.ModelDeployment) {
			md.Spec.Provider.Overrides = &runtime.RawExtension{Raw: []byte(`{"spec":{"components":[{"name":"Frontend","podTemplate":{"spec":{"containers":[{"name":"main","args":["--dyn-chat-processor=vllm"]}]}}}]}}`)}
		}, true},
		{"unrelated native fields", func(md *api.ModelDeployment) {
			md.Spec.Provider.Overrides = &runtime.RawExtension{Raw: []byte(`{"spec":{"env":[{"name":"CUSTOM_ENV","value":"keep"}]}}`)}
		}, false},
		{"mocker", func(md *api.ModelDeployment) {
			md.Annotations = map[string]string{"airunway.ai/dynamo-test-backend": "mocker"}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := fixture(valid)
			md.Spec.Engine.ToolCalling = true
			tc.mutate(md)
			err := ValidateToolCalling(md)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantError=%v", err, tc.wantErr)
			}
		})
	}
}

func TestToolCallingFingerprintAndReconfigure(t *testing.T) {
	old := fixture(valid)
	old.Status.Provider = &api.ProviderStatus{Name: "dynamo", RequestRef: &api.ProviderResourceReference{Name: "request"}, Intent: &api.ProviderIntentStatus{Phase: "Deployed"}}
	before, err := Fingerprint(old)
	if err != nil {
		t.Fatal(err)
	}
	next := old.DeepCopy()
	next.Spec.Engine.ToolCalling = true
	after, err := Fingerprint(next)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("enabling tools must change request identity")
	}
	if err := ValidateUpdate(old, next); err == nil {
		t.Fatal("tool changes must respect request lock")
	}
	next.Annotations = map[string]string{AttemptAnnotation: "tools"}
	if err := ValidateUpdate(old, next); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*api.ModelDeployment){func(md *api.ModelDeployment) { md.Spec.Engine.ToolCallParser = "qwen3_coder" }, func(md *api.ModelDeployment) { md.Spec.Engine.ReasoningParser = "basic" }} {
		updated := next.DeepCopy()
		change(updated)
		hash, _ := Fingerprint(updated)
		if hash == after {
			t.Fatal("parser change must change request identity")
		}
	}
	disabled := old.DeepCopy()
	disabled.Spec.Engine.ToolCalling = false
	hash, _ := Fingerprint(disabled)
	if hash != before {
		t.Fatal("default false must preserve existing hashes")
	}
}
