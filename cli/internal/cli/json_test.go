package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func TestDecodeJSONRejectsDuplicateKeys(t *testing.T) {
	for _, raw := range []string{
		`{"replicas":0,"replicas":1}`,
		`{"spec":{"scaling":{"replicas":0,"replicas":0}}}`,
		`[{"name":"one","name":"two"}]`,
		`{"a":1,"\u0061":2}`,
		`{"config":{"secret":"first","secret":null}}`,
	} {
		var target Object
		if err := decodeJSON([]byte(raw), &target); err == nil {
			t.Fatalf("accepted ambiguous document: %s", raw)
		}
		if target != nil {
			t.Fatal("partial decode before rejecting duplicate")
		}
	}
	for _, raw := range []string{`{"a":1,"b":{"a":2}}`, `[{"a":1},{"a":2}]`, `{"seed":9007199254740993}`, `null`, `42`, `"value"`} {
		var target any
		if err := decodeJSON([]byte(raw), &target); err != nil {
			t.Fatalf("valid JSON rejected: %s: %v", raw, err)
		}
	}
}

func TestApplyDuplicateJSONFailsBeforeClusterWrite(t *testing.T) {
	file := managementTestWrite(t, t.TempDir(), "duplicate.json", `{"apiVersion":"airunway.ai/v1alpha1","kind":"ModelDeployment","metadata":{"name":"duplicate"},"spec":{"scaling":{"replicas":0,"replicas":1}}}`)
	for _, dry := range []string{"", "client", "server"} {
		h := managementTestContext("file", file)
		if dry != "" {
			h.ctx.Flags["dry-run"] = []string{dry}
		}
		managementTestError(t, h, "JSON", "apply")
		if h.connections != 0 || len(h.client.writes()) != 0 {
			t.Fatal("ambiguous manifest contacted cluster")
		}
	}
}

func TestForbiddenMessageDoesNotAssumeAuthorizationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"kind":"Status","message":"admission webhook denied a private-value"}`)
	}))
	defer server.Close()
	client, err := NewKubernetesClient(&rest.Config{Host: server.URL}, "default")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Request(context.Background(), "POST", "/api/resource", Object{}, RequestOptions{})
	if !accessHasCode(err, "HTTP_403") || !strings.Contains(err.Error(), "admission policies") || strings.Contains(err.Error(), "private-value") {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(Object{"error": err.Error()})
	if err != nil || !json.Valid(encoded) {
		t.Fatal("invalid diagnostic")
	}
}
