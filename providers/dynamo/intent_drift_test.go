package dynamo

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func driftIntentMD(t *testing.T) *api.ModelDeployment {
	md := typedRenderingMD(t)
	var root map[string]any
	if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &root); err != nil {
		t.Fatal(err)
	}
	root["intent"].(map[string]any)["hardware"] = map[string]any{"totalGpus": 4}
	setRenderingOverrides(t, md, root)
	return md
}

func TestAutomaticRequestDriftIgnoresMatchingHash(t *testing.T) {
	for _, phase := range []string{"Pending", "Profiling", "Deployed"} {
		for _, drift := range []string{"gpu budget", "injected overrides", "defaults only"} {
			t.Run(phase+"/"+drift, func(t *testing.T) {
				md := driftIntentMD(t)
				objects, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.5.0")
				if err != nil {
					t.Fatal(err)
				}
				request := objects[0]
				request.SetUID("request-uid")
				hash, err := dynamointent.Fingerprint(md)
				if err != nil {
					t.Fatal(err)
				}
				request.SetAnnotations(map[string]string{dynamointent.HashAnnotation: hash, dynamointent.AttemptAnnotation: ""})
				request.Object["status"] = map[string]any{"phase": phase}
				_ = unstructured.SetNestedField(request.Object, "discovered", "spec", "hardware", "gpuSku")
				if drift == "gpu budget" {
					_ = unstructured.SetNestedField(request.Object, int64(99), "spec", "hardware", "totalGpus")
				}
				if drift == "injected overrides" {
					_ = unstructured.SetNestedMap(request.Object, map[string]any{"profilingJob": map[string]any{"activeDeadlineSeconds": int64(1)}}, "spec", "overrides")
				}
				c := fake.NewClientBuilder().WithScheme(newScheme()).WithRESTMapper(dynamoMapper(DynamoAPIVersion, dynamoBetaVersion)).WithObjects(request).WithObjects(operatorRuntimeFixtures("1.5.0", "")...).Build()
				r := NewDynamoProviderReconciler(c, newScheme(), "")
				desired, err := r.renderResources(context.Background(), md)
				if err != nil {
					t.Fatal(err)
				}
				err = r.createOrUpdateResource(context.Background(), desired[0], md)
				if phase != "Pending" && drift != "defaults only" {
					var locked *intentLockedError
					if !errors.As(err, &locked) {
						t.Fatalf("active drift not surfaced: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				got := request.DeepCopy()
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(request), got); err != nil {
					t.Fatal(err)
				}
				if phase == "Pending" && drift != "defaults only" {
					if got.GetUID() != request.GetUID() {
						t.Fatal("mutable correction replaced request")
					}
					budget, _, _ := unstructured.NestedInt64(got.Object, "spec", "hardware", "totalGpus")
					if budget != 4 {
						t.Fatalf("budget drift remains: %d", budget)
					}
					if _, found, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "overrides"); found {
						t.Fatal("undeclared override remains")
					}
				} else if !sameJSON(t, got.Object["spec"], request.Object["spec"]) {
					t.Fatal("immutable request or defaults were modified")
				}
				if sku, _, _ := unstructured.NestedString(got.Object, "spec", "hardware", "gpuSku"); sku != "discovered" {
					t.Fatal("discovered hardware lost")
				}
			})
		}
	}
}

func TestAutomaticNativeOverrideDriftAndDefaults(t *testing.T) {
	md := intentOverridesMD(t, "v1beta1")
	objects, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.5.0")
	if err != nil {
		t.Fatal(err)
	}
	want, _, _ := unstructured.NestedMap(objects[0].Object, "spec")
	have, _, _ := unstructured.NestedMap(objects[0].Object, "spec")
	job := have["overrides"].(map[string]any)["profilingJob"].(map[string]any)
	job["template"].(map[string]any)["metadata"] = map[string]any{}
	if different, err := requestSpecDiffers(have, want); err != nil || different {
		t.Fatalf("job serialization defaults treated as drift: %v", err)
	}
	job["activeDeadlineSeconds"] = 900
	if different, err := requestSpecDiffers(have, want); err != nil || !different {
		t.Fatalf("job override drift not detected: %v", err)
	}
	components := have["overrides"].(map[string]any)["dgd"].(map[string]any)["spec"].(map[string]any)["components"].([]any)
	components[0].(map[string]any)["podTemplate"].(map[string]any)["spec"].(map[string]any)["extraDrift"] = true
	job["activeDeadlineSeconds"] = 1800
	if different, err := requestSpecDiffers(have, want); err != nil || !different {
		t.Fatalf("opaque DGD override drift not detected: %v", err)
	}
}

func TestNewIntentAttemptUsesInstalledRuntime(t *testing.T) {
	md := typedRenderingMD(t)
	old, err := NewTransformer().TransformForVersion(context.Background(), md, "v1alpha1", "1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	old[0].SetUID("old-request")
	hash, err := dynamointent.Fingerprint(md)
	if err != nil {
		t.Fatal(err)
	}
	old[0].SetAnnotations(map[string]string{dynamointent.HashAnnotation: hash, dynamointent.AttemptAnnotation: ""})
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRESTMapper(dynamoMapper(DynamoAPIVersion, dynamoBetaVersion)).WithObjects(old[0]).WithObjects(operatorRuntimeFixtures("1.5.0", "")...).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	for _, token := range []string{"", "next"} {
		md.Annotations = map[string]string{dynamointent.AttemptAnnotation: token}
		objects, err := r.renderResources(context.Background(), md)
		if err != nil {
			t.Fatal(err)
		}
		image, _, _ := unstructured.NestedString(objects[0].Object, "spec", "image")
		wantVersion := "1.1.1"
		if token != "" {
			wantVersion = "1.5.0"
		}
		if semanticImageTag(image) != wantVersion {
			t.Fatalf("attempt %q used %q, want %s", token, image, wantVersion)
		}
	}
}
