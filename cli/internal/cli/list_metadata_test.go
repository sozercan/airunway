package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

const listMetadataNamespace = "credential-list-test"

func listMetadataClient(t *testing.T, typ ResourceType, items []Object) *KubernetesClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != resourcePath(typ, listMetadataNamespace, "") {
			t.Errorf("unexpected list request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(Object{"kind": typ.Kind + "List", "items": items})
	}))
	t.Cleanup(server.Close)
	client, err := NewKubernetesClient(&rest.Config{Host: server.URL}, listMetadataNamespace)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestClusterListRestoresOmittedTypeMeta(t *testing.T) {
	for _, noun := range []string{"credential", "model"} {
		t.Run(noun, func(t *testing.T) {
			typ := resourceTypes[noun]
			apiVersion := typ.Version
			if typ.Group != "" {
				apiVersion = typ.Group + "/" + typ.Version
			}
			items := []Object{
				{"metadata": Object{"name": "missing-type-meta"}},
				{"kind": "Unexpected", "apiVersion": "unexpected/v9"},
				{"kind": typ.Kind},
				{"apiVersion": apiVersion},
				{"kind": "", "apiVersion": nil},
			}
			client := listMetadataClient(t, typ, items)
			got, err := client.List(context.Background(), typ, listMetadataNamespace, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []Object{
				{"metadata": Object{"name": "missing-type-meta"}, "kind": typ.Kind, "apiVersion": apiVersion},
				items[1],
				{"kind": typ.Kind, "apiVersion": apiVersion},
				{"kind": typ.Kind, "apiVersion": apiVersion},
				items[4],
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("list type metadata: got %#v, want %#v", got, want)
			}
		})
	}
}

func listMetadataCredential(name, kind string) Object {
	return Object{
		"metadata": Object{
			"name": name, "namespace": listMetadataNamespace,
			"labels": Object{
				managementManagedBy: managementManager, managementCredentialType: kind,
				"private-label": "private-label-value",
			},
			"annotations": Object{"private-annotation": "private-annotation-value"},
		},
		"type":       "Opaque",
		"data":       Object{managementCredentialKeys[kind]: base64.StdEncoding.EncodeToString([]byte(managementTestToken))},
		"stringData": Object{"private-key": managementTestToken},
	}
}

func listMetadataInvalidCredentials() []Object {
	mutations := []func(Object){
		func(o Object) { object(get(o, "metadata", "labels"))[managementManagedBy] = "other-manager" },
		func(o Object) { object(get(o, "metadata", "labels"))[managementCredentialType] = "unknown" },
		func(o Object) { object(o["metadata"])["namespace"] = "other-namespace" },
		func(o Object) { delete(object(o["metadata"]), "namespace") },
		func(o Object) { object(o["metadata"])["ownerReferences"] = []Object{{"uid": "owner-uid"}} },
		func(o Object) { o["kind"] = "ConfigMap" },
		func(o Object) { o["apiVersion"] = "unexpected/v9" },
		func(o Object) { o["type"] = "kubernetes.io/service-account-token" },
	}
	items := make([]Object, 0, len(mutations))
	for _, mutate := range mutations {
		item := listMetadataCredential("excluded-credential", "huggingface")
		mutate(item)
		items = append(items, item)
	}
	return items
}

func TestCredentialListHandlesKubernetesSecretList(t *testing.T) {
	items := []Object{
		listMetadataCredential("managed-huggingface", "huggingface"),
		listMetadataCredential("managed-api-key", "api-key"),
		listMetadataCredential("managed-artifact", "artifact"),
	}
	items = append(items, listMetadataInvalidCredentials()...)
	client := listMetadataClient(t, resourceTypes["credential"], items)
	for _, format := range []string{"json", "yaml", "text"} {
		t.Run(format, func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := Run(context.Background(), []string{
				"credential", "list", "--namespace", listMetadataNamespace, "--output", format,
			}, RunOptions{Client: client, Config: &CLIConfig{}, IO: &IO{Out: &out, Err: &stderr}})
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("credential list failed: exit %d, stderr %q", code, &stderr)
			}
			listMetadataCheckOutput(t, out.String())
		})
	}
}

func listMetadataCheckOutput(t *testing.T, output string) {
	t.Helper()
	for _, name := range []string{"managed-huggingface", "managed-api-key", "managed-artifact"} {
		if !strings.Contains(output, name) {
			t.Errorf("managed credential %s absent from list", name)
		}
	}
	for _, forbidden := range []string{
		"excluded-credential", managementTestToken, base64.StdEncoding.EncodeToString([]byte(managementTestToken)),
		"private-key", "private-label", "private-annotation", "stringData",
	} {
		if strings.Contains(output, forbidden) {
			t.Error("credential list exposed excluded metadata or sensitive fields")
		}
	}
}
