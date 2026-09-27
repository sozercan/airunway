package dynamo

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func renderDGDForDefaultsTest(t *testing.T, md *airunwayv1alpha1.ModelDeployment) *unstructured.Unstructured {
	t.Helper()
	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	return resources[0]
}

func servicesForDefaultsTest(dgd *unstructured.Unstructured) map[string]any {
	return dgd.Object["spec"].(map[string]any)["services"].(map[string]any)
}

func addMinAvailableDefaultsForTest(dgd *unstructured.Unstructured) {
	for _, service := range servicesForDefaultsTest(dgd) {
		service.(map[string]any)["minAvailable"] = int64(1)
	}
}

func requireStrictUpdateForDefaultsTest(t *testing.T, opts []client.UpdateOption) {
	t.Helper()
	o := &client.UpdateOptions{}
	for _, opt := range opts {
		opt.ApplyToUpdate(o)
	}
	if o.FieldValidation != metav1.FieldValidationStrict {
		t.Fatalf("FieldValidation = %q, want Strict", o.FieldValidation)
	}
}

func requireDefaultPresence(t *testing.T, dgd *unstructured.Unstructured, present bool) {
	t.Helper()
	for name, service := range servicesForDefaultsTest(dgd) {
		value, found := service.(map[string]any)["minAvailable"]
		if found != present || (present && value != int64(1)) {
			t.Fatalf("%s.minAvailable = %v, present %v; want present %v", name, value, found, present)
		}
	}
}

func reconcileDefaultsOnce(t *testing.T, r *DynamoProviderReconciler, c client.Client, md *airunwayv1alpha1.ModelDeployment) {
	t.Helper()
	key := client.ObjectKeyFromObject(md)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), key, md); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, md.Status.Conditions, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionTrue, "ResourceCreated")
}

// The fake client has no admission or CRD validation. Model the captured Grove-backed
// v1alpha1 defaults here and check the exact object submitted on every update.
func TestReconcilePreservesDefaultedMinAvailable(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	md := newMDForController("test", "default")
	md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{Enabled: boolPtr(false)}
	controllerutil.AddFinalizer(md, FinalizerName)
	creates, updates := 0, 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				dgd, ok := obj.(*unstructured.Unstructured)
				if ok && dgd.GetKind() == DynamoGraphDeploymentKind {
					creates++
					requireDefaultPresence(t, dgd, false)
					addMinAvailableDefaultsForTest(dgd)
				}
				return cl.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				dgd, ok := obj.(*unstructured.Unstructured)
				if ok && dgd.GetKind() == DynamoGraphDeploymentKind {
					updates++
					requireStrictUpdateForDefaultsTest(t, opts)
					requireDefaultPresence(t, dgd, true)
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := NewDynamoProviderReconciler(c, scheme, "")
	key := client.ObjectKeyFromObject(md)

	reconcileDefaultsOnce(t, r, c, md)
	reconcileDefaultsOnce(t, r, c, md)
	if creates != 1 || updates != 0 {
		t.Fatalf("unchanged reconcile: creates = %d, updates = %d; want 1, 0", creates, updates)
	}

	md.Spec.Engine.Image = "example.invalid/dynamo:v2"
	if err := c.Update(ctx, md); err != nil {
		t.Fatal(err)
	}
	reconcileDefaultsOnce(t, r, c, md)
	reconcileDefaultsOnce(t, r, c, md)
	if creates != 1 || updates != 1 {
		t.Fatalf("image change: creates = %d, updates = %d; want 1, 1", creates, updates)
	}
	stored := &unstructured.Unstructured{}
	setDGDGVK(stored)
	if err := c.Get(ctx, key, stored); err != nil {
		t.Fatal(err)
	}
	for name, service := range servicesForDefaultsTest(stored) {
		image, _, err := unstructured.NestedString(service.(map[string]any), "extraPodSpec", "mainContainer", "image")
		if err != nil || image != md.Spec.Engine.Image {
			t.Errorf("%s image = %q, err = %v; want %q", name, image, err, md.Spec.Engine.Image)
		}
	}
}

func TestReconcileMinAvailableOverridesReachValidation(t *testing.T) {
	for _, value := range []string{"2", "0", "null", `""`, `{}`} {
		t.Run(value, func(t *testing.T) {
			ctx := context.Background()
			scheme := newScheme()
			md := newMDForController("test", "default")
			md.Spec.Gateway = &airunwayv1alpha1.GatewaySpec{Enabled: boolPtr(false)}
			controllerutil.AddFinalizer(md, FinalizerName)
			existing := renderDGDForDefaultsTest(t, md)
			addMinAvailableDefaultsForTest(existing)
			md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
				Name: ProviderName,
				Overrides: &runtime.RawExtension{Raw: fmt.Appendf(nil,
					`{"spec":{"services":{"VllmWorker":{"minAvailable":%s}}}}`, value)},
			}
			desired := renderDGDForDefaultsTest(t, md)
			want := servicesForDefaultsTest(desired)["VllmWorker"].(map[string]any)["minAvailable"]
			updates := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, existing).WithStatusSubresource(md).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(_ context.Context, _ client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						updates++
						requireStrictUpdateForDefaultsTest(t, opts)
						services := servicesForDefaultsTest(obj.(*unstructured.Unstructured))
						got, found := services["VllmWorker"].(map[string]any)["minAvailable"]
						if !found || !reflect.DeepEqual(got, want) {
							t.Fatalf("override changed: got %v, present %v; want %v", got, found, want)
						}
						if got := services["Frontend"].(map[string]any)["minAvailable"]; got != int64(1) {
							t.Fatalf("Frontend.minAvailable = %v, want 1", got)
						}
						return apierrors.NewInvalid(schema.GroupKind{Group: DynamoAPIGroup, Kind: DynamoGraphDeploymentKind},
							obj.GetName(), field.ErrorList{field.Invalid(
								field.NewPath("spec", "services", "VllmWorker", "minAvailable"), got,
								"minAvailable is immutable after creation")})
					},
				}).Build()
			r := NewDynamoProviderReconciler(c, scheme, "")
			key := client.ObjectKeyFromObject(md)
			result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			if err != nil || result.RequeueAfter != ExternalRecoveryInterval || updates != 1 {
				t.Fatalf("result = %+v, err = %v, updates = %d; want validation retry and one update", result, err, updates)
			}
			if err := c.Get(ctx, key, md); err != nil {
				t.Fatal(err)
			}
			assertCondition(t, md.Status.Conditions, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "CreateFailed")
			assertCondition(t, md.Status.Conditions, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "CreateFailed")
		})
	}
}

func TestCreateOrUpdateMinAvailableServiceChanges(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]any
		desired  map[string]any
		want     map[string]any
		updates  int
	}{
		{
			name:     "no upstream default",
			existing: map[string]any{"Worker": map[string]any{"replicas": int64(1)}},
			desired:  map[string]any{"Worker": map[string]any{"replicas": int64(1)}},
			want:     map[string]any{"Worker": map[string]any{"replicas": int64(1)}},
		},
		{
			name:     "retain observed value rather than hardcoding one",
			existing: map[string]any{"Worker": map[string]any{"minAvailable": int64(2)}},
			desired:  map[string]any{"Worker": map[string]any{}},
			want:     map[string]any{"Worker": map[string]any{"minAvailable": int64(2)}},
		},
		{
			name:     "new service does not inherit a default",
			existing: map[string]any{"Worker": map[string]any{"minAvailable": int64(1)}},
			desired:  map[string]any{"Worker": map[string]any{}, "New": map[string]any{"replicas": int64(1)}},
			want:     map[string]any{"Worker": map[string]any{"minAvailable": int64(1)}, "New": map[string]any{"replicas": int64(1)}},
			updates:  1,
		},
		{
			name:     "removed service is not restored",
			existing: map[string]any{"Worker": map[string]any{"minAvailable": int64(1)}, "Old": map[string]any{"minAvailable": int64(2)}},
			desired:  map[string]any{"Worker": map[string]any{}},
			want:     map[string]any{"Worker": map[string]any{"minAvailable": int64(1)}},
			updates:  1,
		},
		{
			name:     "renamed service does not inherit a default",
			existing: map[string]any{"Old": map[string]any{"minAvailable": int64(2)}},
			desired:  map[string]any{"New": map[string]any{"replicas": int64(1)}},
			want:     map[string]any{"New": map[string]any{"replicas": int64(1)}},
			updates:  1,
		},
		{
			name:     "explicit null service is not restored",
			existing: map[string]any{"Worker": map[string]any{"minAvailable": int64(1)}},
			desired:  map[string]any{"Worker": nil},
			want:     map[string]any{"Worker": nil},
			updates:  1,
		},
		{
			name:     "unknown server fields are not preserved",
			existing: map[string]any{"Worker": map[string]any{"minAvailable": int64(1), "unknown": "server-value"}},
			desired:  map[string]any{"Worker": map[string]any{}},
			want:     map[string]any{"Worker": map[string]any{"minAvailable": int64(1)}},
			updates:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := newScheme()
			md := newMDForController("test", "default")
			existing := renderDGDForDefaultsTest(t, md)
			existing.Object["spec"] = map[string]any{"services": tt.existing}
			desired := existing.DeepCopy()
			desired.Object["spec"] = map[string]any{"services": tt.desired}
			updates := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						updates++
						requireStrictUpdateForDefaultsTest(t, opts)
						return cl.Update(ctx, obj, opts...)
					},
				}).Build()
			r := NewDynamoProviderReconciler(c, scheme, "")
			if err := r.createOrUpdateResource(ctx, desired, md); err != nil {
				t.Fatal(err)
			}
			if updates != tt.updates {
				t.Fatalf("updates = %d, want %d", updates, tt.updates)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
				t.Fatal(err)
			}
			if got := servicesForDefaultsTest(existing); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("stored services = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMinAvailableDefaultsRequireOwnership(t *testing.T) {
	for _, uid := range []string{"", "another-model-deployment"} {
		t.Run(uid, func(t *testing.T) {
			md := newMDForController("test", "default")
			desired := renderDGDForDefaultsTest(t, md)
			existing := desired.DeepCopy()
			addMinAvailableDefaultsForTest(existing)
			refs := existing.GetOwnerReferences()
			refs[0].UID = "another-model-deployment"
			if uid == "" {
				refs = nil
			}
			existing.SetOwnerReferences(refs)
			scheme := newScheme()
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
						t.Fatal("attempted an update without ownership")
						return nil
					},
				}).Build()
			r := NewDynamoProviderReconciler(c, scheme, "")
			before := desired.DeepCopy()
			if err := r.createOrUpdateResource(context.Background(), desired, md); !isResourceConflict(err) {
				t.Fatalf("error = %v, want ownership conflict", err)
			}
			if !reflect.DeepEqual(desired, before) {
				t.Fatal("desired object was changed before ownership verification")
			}
		})
	}
}

func TestSpecUpdatePreservesOperatorMetadata(t *testing.T) {
	const keep = "keep"
	scheme := newScheme()
	md := newMDForController("metadata-preserved", "default")
	existing := renderDGDForDefaultsTest(t, md)
	existing.SetAnnotations(map[string]string{"nvidia.com/workload-provider": "grove", "operator-owned": keep})
	labels := existing.GetLabels()
	labels["operator-label"] = keep
	existing.SetLabels(labels)
	existing.SetFinalizers([]string{"nvidia.com/dynamo-finalizer"})
	addMinAvailableDefaultsForTest(existing)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	md.Spec.Engine.Image = "example/runtime:updated"
	desired := renderDGDForDefaultsTest(t, md)
	desired.SetAnnotations(map[string]string{"desired-annotation": "new"})
	r := NewDynamoProviderReconciler(c, scheme, "")
	if err := r.createOrUpdateResource(context.Background(), desired, md); err != nil {
		t.Fatal(err)
	}
	got := &unstructured.Unstructured{}
	setDGDGVK(got)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(existing), got); err != nil {
		t.Fatal(err)
	}
	if got.GetAnnotations()["nvidia.com/workload-provider"] != "grove" || got.GetAnnotations()["operator-owned"] != keep || got.GetAnnotations()["desired-annotation"] != "new" {
		t.Fatal("lost annotations", got.GetAnnotations())
	}
	if got.GetLabels()["operator-label"] != keep {
		t.Fatal("lost operator label")
	}
	if !reflect.DeepEqual(got.GetFinalizers(), existing.GetFinalizers()) || !reflect.DeepEqual(got.GetOwnerReferences(), existing.GetOwnerReferences()) {
		t.Fatal("lost lifecycle metadata")
	}
	if reflect.DeepEqual(got.Object["spec"], existing.Object["spec"]) {
		t.Fatal("spec change was not applied")
	}
}
