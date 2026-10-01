package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"k8s.io/client-go/rest"
)

func TestWorkloadDiscoveryIncludesNonPreferredVersions(t *testing.T) {
	for _, tc := range []struct {
		kind, version string
		calls         []string
	}{
		{"ClusterPolicy", "v1", []string{"/apis", "/apis/nvidia.com/v1"}},
		{"DynamoGraphDeployment", "v1beta1", []string{"/apis", "/apis/nvidia.com/v1", "/apis/nvidia.com/v1beta1"}},
		{"DynamoModel", "v1alpha1", []string{"/apis", "/apis/nvidia.com/v1", "/apis/nvidia.com/v1beta1", "/apis/nvidia.com/v1alpha1"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			calls := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.Path)
				var reply Object
				switch r.URL.Path {
				case "/apis":
					reply = Object{"groups": []Object{{"name": "nvidia.com", "preferredVersion": Object{"groupVersion": "nvidia.com/v1"}, "versions": []Object{{"groupVersion": "nvidia.com/v1"}, {"groupVersion": "nvidia.com/v1beta1"}, {"groupVersion": "nvidia.com/v1alpha1"}}}}}
				case "/apis/nvidia.com/v1":
					reply = Object{"resources": []Object{{"name": "clusterpolicies", "kind": "ClusterPolicy", "namespaced": false}}}
				case "/apis/nvidia.com/v1beta1":
					reply = Object{"resources": []Object{{"name": "dynamographdeployments", "kind": "DynamoGraphDeployment", "namespaced": true}}}
				case "/apis/nvidia.com/v1alpha1":
					reply = Object{"resources": []Object{{"name": "dynamomodels", "kind": "DynamoModel", "namespaced": true}}}
				default:
					t.Errorf("unexpected discovery path %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(reply)
			}))
			defer server.Close()
			client, err := NewKubernetesClient(&rest.Config{Host: server.URL}, "default")
			if err != nil {
				t.Fatal(err)
			}
			typ, err := accessReferenceType(context.Background(), client, tc.kind, "")
			if err != nil || typ.Group != "nvidia.com" || typ.Version != tc.version || typ.Kind != tc.kind {
				t.Fatalf("unexpected type %+v: %v", typ, err)
			}
			if !reflect.DeepEqual(calls, tc.calls) {
				t.Fatalf("discovery calls: %v", calls)
			}
		})
	}
}
