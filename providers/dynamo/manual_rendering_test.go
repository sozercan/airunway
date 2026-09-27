package dynamo

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func manualRenderFixture(t *testing.T, apiVersion, release string) (*api.ModelDeployment, *unstructured.Unstructured, releasedContract) {
	t.Helper()
	md := newMDForController("model", "models")
	setRenderingOverrides(t, md, map[string]any{"spec": map[string]any{"services": map[string]any{
		"VllmWorker": map[string]any{"extraPodSpec": map[string]any{"mainContainer": map[string]any{
			"ports": []any{map[string]any{"name": "metrics", "containerPort": int64(9090)}},
		}}},
	}}})
	objects, err := NewTransformer().TransformForVersion(context.Background(), md, apiVersion, release)
	if err != nil {
		t.Fatal(err)
	}
	live := objects[0]
	live.SetName("existing-workload")
	live.SetUID("existing-uid")
	live.SetResourceVersion("41")
	live.SetGeneration(3)
	live.SetFinalizers([]string{"nvidia.com/operator-cleanup"})
	live.SetAnnotations(map[string]string{
		manualInputHashAnnotation:      manualFingerprint(md),
		runtimeVersionAnnotation:       release,
		"nvidia.com/workload-provider": "grove",
		"operator-added":               "preserved",
	})
	labels := live.GetLabels()
	labels["operator-added"] = "preserved"
	live.SetLabels(labels)
	live.Object["status"] = map[string]any{"state": "successful"}
	md.Status.Provider.WorkloadRef = resourceReference(live)

	// Observed digests differ from Runway's default image tags. A newly installed
	// operator must not move an existing workload's API, runtime, or images.
	pinManualImages(live.Object["spec"])
	contract := readReleasedContract(t, "v"+release, "dynamographdeployments", apiVersion)
	defaultManualTestDGD(t, contract, live)
	return md, live, contract
}

func pinManualImages(value any) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "image" {
				value[key] = child.(string) + "@sha256:" + strings.Repeat("a", 64)
			} else {
				pinManualImages(child)
			}
		}
	case []any:
		for _, child := range value {
			pinManualImages(child)
		}
	}
}

func defaultManualTestDGD(t *testing.T, contract releasedContract, obj *unstructured.Unstructured) {
	t.Helper()
	defaulting.Default(obj.Object, contract.schema)
	// Pinned Dynamo 1.5.0 defaulting/dynamographdeployment_handler.go:95-103
	// fills omitted replicas and Grove minAvailable on UPDATE. The 1.1.1
	// mutating webhook is CREATE-only; Runway already supplies all replicas.
	if obj.GroupVersionKind().Version == dynamoBetaVersion {
		components, _, err := unstructured.NestedSlice(obj.Object, "spec", "components")
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range components {
			component := raw.(map[string]any)
			if _, exists := component["replicas"]; !exists {
				component["replicas"] = int64(1)
			}
			if _, exists := component["minAvailable"]; !exists && obj.GetAnnotations()["nvidia.com/workload-provider"] == "grove" {
				component["minAvailable"] = int64(1)
			}
		}
		if err := unstructured.SetNestedSlice(obj.Object, components, "spec", "components"); err != nil {
			t.Fatal(err)
		}
	}
	assertContract(t, contract, obj)
}

func changeManualWorker(t *testing.T, obj *unstructured.Unstructured, change func(component, main map[string]any)) {
	t.Helper()
	if obj.GroupVersionKind().Version == DynamoAPIVersion {
		component, found, err := unstructured.NestedMap(obj.Object, "spec", "services", "VllmWorker")
		if err != nil || !found {
			t.Fatalf("missing alpha worker: %v", err)
		}
		main := component["extraPodSpec"].(map[string]any)["mainContainer"].(map[string]any)
		change(component, main)
		if err := unstructured.SetNestedMap(obj.Object, component, "spec", "services", "VllmWorker"); err != nil {
			t.Fatal(err)
		}
		return
	}
	components, _, err := unstructured.NestedSlice(obj.Object, "spec", "components")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range components {
		component := raw.(map[string]any)
		if component["name"] != "VllmWorker" {
			continue
		}
		containers := component["podTemplate"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
		for _, raw := range containers {
			main := raw.(map[string]any)
			if main["name"] == "main" {
				change(component, main)
				found = true
			}
		}
	}
	if !found {
		t.Fatal("missing beta worker main container")
	}
	if err := unstructured.SetNestedSlice(obj.Object, components, "spec", "components"); err != nil {
		t.Fatal(err)
	}
}

func TestManualDriftConvergesWithoutDefaultChurn(t *testing.T) {
	ctx := context.Background()
	for _, target := range []struct{ api, release string }{{DynamoAPIVersion, "1.1.1"}, {dynamoBetaVersion, "1.5.0"}} {
		for _, drift := range []string{"defaults only", "replicas", "args", "input hash only"} {
			t.Run(target.api+"/"+drift, func(t *testing.T) {
				md, live, contract := manualRenderFixture(t, target.api, target.release)
				want := live.DeepCopy()
				changeManualWorker(t, live, func(component, main map[string]any) {
					switch drift {
					case "replicas":
						component["replicas"] = int64(9)
					case "args":
						main["args"] = []any{"--model", "wrong-model"}
					}
				})
				if drift == "input hash only" {
					annotations := live.GetAnnotations()
					annotations[manualInputHashAnnotation] = "older-input-hash"
					live.SetAnnotations(annotations)
				}
				dryRuns, updates, patches := 0, 0, 0
				c := fake.NewClientBuilder().WithScheme(newScheme()).WithRESTMapper(dynamoMapper(DynamoAPIVersion, dynamoBetaVersion)).
					WithObjects(live).WithObjects(operatorRuntimeFixtures("1.6.0", "")...).WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, inner client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						options := (&client.UpdateOptions{}).ApplyOptions(opts)
						if options.FieldValidation != metav1.FieldValidationStrict {
							t.Fatal("update omitted strict field validation")
						}
						u := obj.(*unstructured.Unstructured)
						if len(options.DryRun) != 0 {
							if !reflect.DeepEqual(options.DryRun, []string{metav1.DryRunAll}) {
								t.Fatalf("unexpected dry-run options: %v", options.DryRun)
							}
							dryRuns++
							defaultManualTestDGD(t, contract, u)
							// The real API client decodes JSON numbers into Unstructured.
							wire, err := u.MarshalJSON()
							if err != nil {
								t.Fatal(err)
							}
							if err := u.UnmarshalJSON(wire); err != nil {
								t.Fatal(err)
							}
							// These response fields must never leak into the persisted update.
							u.SetUID("dry-run-uid")
							u.SetResourceVersion("999")
							u.SetAnnotations(map[string]string{"dry-run-only": "discard"})
							u.SetLabels(map[string]string{"dry-run-only": "discard"})
							u.SetFinalizers(nil)
							u.SetOwnerReferences(nil)
							u.Object["status"] = map[string]any{"dry-run-only": true}
							return nil
						}
						updates++
						if u.GetResourceVersion() != live.GetResourceVersion() || u.GetUID() != live.GetUID() ||
							!reflect.DeepEqual(u.GetOwnerReferences(), live.GetOwnerReferences()) ||
							!sameJSON(t, u.Object["status"], live.Object["status"]) {
							t.Fatalf("dry-run response replaced original fields: RV=%s/%s UID=%s/%s owners=%v/%v status=%v/%v", u.GetResourceVersion(), live.GetResourceVersion(), u.GetUID(), live.GetUID(), u.GetOwnerReferences(), live.GetOwnerReferences(), u.Object["status"], live.Object["status"])
						}
						if !sameJSON(t, u.Object["spec"], want.Object["spec"]) {
							t.Fatal("persisted update did not use the normalized desired spec")
						}
						return inner.Update(ctx, obj, opts...)
					},
					Patch: func(ctx context.Context, inner client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						patches++
						options := (&client.PatchOptions{}).ApplyOptions(opts)
						if options.FieldValidation != metav1.FieldValidationStrict {
							t.Fatal("metadata patch omitted strict validation")
						}
						return inner.Patch(ctx, obj, patch, opts...)
					},
				}).Build()
				r := NewDynamoProviderReconciler(c, newScheme(), "")
				beforeMD := md.DeepCopy()
				previousRV := live.GetResourceVersion()
				for pass := 0; pass < 3; pass++ {
					desired, err := r.renderResources(ctx, md)
					if err != nil {
						t.Fatal(err)
					}
					if desired[0].GetAPIVersion() != live.GetAPIVersion() || desired[0].GetName() != live.GetName() || existingRuntimeVersion(desired[0]) != target.release {
						t.Fatal("re-render changed existing API, name, or runtime after operator upgrade")
					}
					oldImages, newImages := []string{}, []string{}
					collectImages(live.Object["spec"], &oldImages)
					collectImages(desired[0].Object["spec"], &newImages)
					sort.Strings(oldImages)
					sort.Strings(newImages)
					if !reflect.DeepEqual(oldImages, newImages) {
						t.Fatal("re-render changed pinned images")
					}
					if err := r.createOrUpdateResource(ctx, desired[0], md); err != nil {
						t.Fatal(err)
					}
					stored := live.DeepCopy()
					if err := c.Get(ctx, client.ObjectKeyFromObject(live), stored); err != nil {
						t.Fatal(err)
					}
					if !sameJSON(t, stored.Object["spec"], want.Object["spec"]) {
						t.Fatalf("controlled drift did not converge on pass %d", pass)
					}
					if !reflect.DeepEqual(stored.GetAnnotations(), want.GetAnnotations()) || !reflect.DeepEqual(stored.GetLabels(), want.GetLabels()) ||
						!reflect.DeepEqual(stored.GetFinalizers(), live.GetFinalizers()) || !sameJSON(t, stored.Object["status"], live.Object["status"]) {
						t.Fatal("operator metadata, finalizers, or status changed")
					}
					if pass > 0 && stored.GetResourceVersion() != previousRV {
						t.Fatal("repeated reconciliation persisted a write")
					}
					previousRV = stored.GetResourceVersion()
				}
				wantUpdates, wantPatches := 0, 0
				if drift == "replicas" || drift == "args" {
					wantUpdates = 1
				}
				if drift == "input hash only" {
					wantPatches = 1
				}
				if dryRuns != 3 || updates != wantUpdates || patches != wantPatches {
					t.Fatalf("calls: dry-run=%d update=%d patch=%d; want 3/%d/%d", dryRuns, updates, patches, wantUpdates, wantPatches)
				}
				if !reflect.DeepEqual(beforeMD, md) {
					t.Fatal("manual reconciliation mutated the ModelDeployment")
				}
			})
		}
	}
}

func TestManualDryRunErrorsDoNotPersist(t *testing.T) {
	for _, target := range []struct{ api, release string }{{DynamoAPIVersion, "1.1.1"}, {dynamoBetaVersion, "1.5.0"}} {
		for _, failure := range []struct {
			name string
			err  error
		}{
			{"schema rejection", apierrors.NewBadRequest(`strict decoding error: unknown field "spec.futureField"`)},
			{"conflict", apierrors.NewConflict(schema.GroupResource{Group: DynamoAPIGroup, Resource: "dynamographdeployments"}, "existing-workload", errors.New("stale RV"))},
			{"webhook unavailable", apierrors.NewServiceUnavailable("webhook unavailable")},
		} {
			t.Run(target.api+"/"+failure.name, func(t *testing.T) {
				md, live, _ := manualRenderFixture(t, target.api, target.release)
				changeManualWorker(t, live, func(component, _ map[string]any) { component["replicas"] = int64(9) })
				dryRuns, writes := 0, 0
				c := fake.NewClientBuilder().WithScheme(newScheme()).WithRESTMapper(dynamoMapper(target.api)).WithObjects(live).
					WithInterceptorFuncs(interceptor.Funcs{
						Update: func(_ context.Context, _ client.WithWatch, _ client.Object, opts ...client.UpdateOption) error {
							o := (&client.UpdateOptions{}).ApplyOptions(opts)
							if reflect.DeepEqual(o.DryRun, []string{metav1.DryRunAll}) && o.FieldValidation == metav1.FieldValidationStrict {
								dryRuns++
								return failure.err
							}
							writes++
							return nil
						},
						Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
							writes++
							return nil
						},
					}).Build()
				r := NewDynamoProviderReconciler(c, newScheme(), "")
				desired, err := r.renderResources(context.Background(), md)
				if err != nil {
					t.Fatal(err)
				}
				err = r.createOrUpdateResource(context.Background(), desired[0], md)
				if !errors.Is(err, failure.err) || dryRuns != 1 || writes != 0 {
					t.Fatalf("error=%v, dry-runs=%d writes=%d; want original error, one dry-run, no writes", err, dryRuns, writes)
				}
				stored := live.DeepCopy()
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(live), stored); err != nil {
					t.Fatal(err)
				}
				if !sameJSON(t, stored.Object, live.Object) {
					t.Fatal("failed dry-run changed persisted state")
				}
			})
		}
	}
}

func TestManualExactMatchSkipsDryRun(t *testing.T) {
	for _, target := range []struct{ api, release string }{{DynamoAPIVersion, "1.1.1"}, {dynamoBetaVersion, "1.5.0"}} {
		t.Run(target.api, func(t *testing.T) {
			md := newMDForController("model", "models")
			objects, err := NewTransformer().TransformForVersion(context.Background(), md, target.api, target.release)
			if err != nil {
				t.Fatal(err)
			}
			live := objects[0]
			live.SetAnnotations(map[string]string{manualInputHashAnnotation: manualFingerprint(md), runtimeVersionAnnotation: target.release})
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(live).WithInterceptorFuncs(interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					t.Fatal("unchanged spec made an update call")
					return nil
				},
				Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					t.Fatal("unchanged input made a patch call")
					return nil
				},
			}).Build()
			if err := NewDynamoProviderReconciler(c, newScheme(), "").createOrUpdateResource(context.Background(), live.DeepCopy(), md); err != nil {
				t.Fatal(err)
			}
		})
	}
}
