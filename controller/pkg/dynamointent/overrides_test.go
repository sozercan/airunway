package dynamointent

import (
	"encoding/json"
	"strings"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

const nativeOverrides = `{"profilingJob":{"activeDeadlineSeconds":1800},"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"DynamoGraphDeployment","spec":{"components":[{"name":"VllmDecodeWorker","podTemplate":{"spec":{"containers":[{"name":"main","$patch":{"args":"append"},"args":["--dyn-tool-call-parser","hermes"]}]}}}]}}}`

func overrideFixture(raw string) *api.ModelDeployment {
	return fixture(`{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":2},"overrides":` + raw + `}}`)
}

func TestNativeOverridesValidation(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantError bool
	}{
		{"both", nativeOverrides, false},
		{"empty", `{}`, false},
		{"job only", `{"profilingJob":{"activeDeadlineSeconds":1800}}`, false},
		{"alpha", `{"dgd":{"apiVersion":"nvidia.com/v1alpha1","kind":"DynamoGraphDeployment","metadata":{"labels":{"example":"value"}},"spec":{"services":{"VllmDecodeWorker":{"extraPodSpec":{"mainContainer":{"args":["--dyn-tool-call-parser","hermes"]}}}}}}}`, false},
		{"null", `null`, true},
		{"array", `[]`, true},
		{"string", `"bad"`, true},
		{"unknown", `{"model":"other/model"}`, true},
		{"job null", `{"profilingJob":null}`, true},
		{"job array", `{"profilingJob":[]}`, true},
		{"job scalar", `{"profilingJob":1}`, true},
		{"dgd null", `{"dgd":null}`, true},
		{"dgd array", `{"dgd":[]}`, true},
		{"missing version", `{"dgd":{"kind":"DynamoGraphDeployment","spec":{}}}`, true},
		{"wrong version", `{"dgd":{"apiVersion":"nvidia.com/v1","kind":"DynamoGraphDeployment","spec":{}}}`, true},
		{"wrong kind", `{"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"Deployment","spec":{}}}`, true},
		{"missing spec", `{"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"DynamoGraphDeployment"}}`, true},
		{"null spec", `{"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"DynamoGraphDeployment","spec":null}}`, true},
		{"array spec", `{"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"DynamoGraphDeployment","spec":[]}}`, true},
		{"null metadata", `{"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"DynamoGraphDeployment","spec":{},"metadata":null}}`, true},
		{"unknown dgd root", `{"dgd":{"apiVersion":"nvidia.com/v1beta1","kind":"DynamoGraphDeployment","spec":{},"status":{}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(overrideFixture(tc.raw))
			if (err != nil) != tc.wantError {
				t.Fatalf("Validate()=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestNativeOverridesCannotBypassPolicy(t *testing.T) {
	for _, key := range []string{"securityContext", "serviceAccountName", "serviceAccount", "hostNetwork", "hostPID", "hostIPC", "automountServiceAccountToken", "nodeName", "priorityClassName", "runtimeClassName", "resources", "replicas", "SecurityContext", "Resources"} {
		t.Run(key, func(t *testing.T) {
			raw := `{"profilingJob":{"template":{"spec":{"containers":[{"name":"main","` + key + `":{}}]}}}}`
			if err := Validate(overrideFixture(raw)); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("forbidden nested key accepted: %v", err)
			}
		})
	}
	// Flag values and environment variable names are not JSON field names.
	safe := `{"profilingJob":{"template":{"spec":{"containers":[{"name":"main","env":[{"name":"securityContext","value":"resources"}]}]}}}}`
	if err := Validate(overrideFixture(safe)); err != nil {
		t.Fatal(err)
	}
}

func TestNativeOverrideFingerprintAndAttempts(t *testing.T) {
	old := overrideFixture(nativeOverrides)
	old.Status.Provider = &api.ProviderStatus{RequestRef: &api.ProviderResourceReference{Name: "request"}, Intent: &api.ProviderIntentStatus{Phase: "Profiling"}}
	before, err := Fingerprint(old)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal([]byte(nativeOverrides), &decoded); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if after, err := Fingerprint(overrideFixture(string(canonical))); err != nil || after != before {
		t.Fatalf("formatting or key order changed fingerprint: %v", err)
	}
	for _, tc := range []struct{ name, raw string }{
		{"parser", strings.ReplaceAll(nativeOverrides, "hermes", "qwen3_coder")},
		{"deadline", strings.ReplaceAll(nativeOverrides, "1800", "3600")},
		{"removed", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := overrideFixture(tc.raw)
			after, err := Fingerprint(next)
			if err != nil || after == before {
				t.Fatalf("override change not included in hash: %v", err)
			}
			if err := ValidateUpdate(old, next); err == nil {
				t.Fatal("changed override accepted without a new attempt")
			}
			next.Annotations = map[string]string{AttemptAnnotation: "next"}
			if err := ValidateUpdate(old, next); err != nil {
				t.Fatalf("explicit override reconfiguration rejected: %v", err)
			}
		})
	}
}
