package dynamo

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestDeletionTimeoutCoversUpstreamFailuresAndPendingResources(t *testing.T) {
	for _, failure := range []string{"request-discovery", "workload-discovery", "workload-delete", "workload-pending"} {
		for _, expired := range []bool{false, true} {
			name := failure + "/before-timeout"
			if expired {
				name = failure + "/after-timeout"
			}
			t.Run(name, func(t *testing.T) {
				md := newMDForController("cleanup", "models")
				md.Finalizers = []string{FinalizerName, "example.com/other"}
				deletion := metav1.Now()
				if expired {
					deletion = metav1.NewTime(time.Now().Add(-FinalizerTimeout - time.Minute))
				}
				md.DeletionTimestamp = &deletion
				objects := []client.Object{md}
				if failure == "workload-delete" || failure == "workload-pending" {
					dgd := &unstructured.Unstructured{}
					setDGDGVK(dgd)
					dgd.SetName(md.Name)
					dgd.SetNamespace(md.Namespace)
					dgd.SetUID("workload")
					dgd.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "ModelDeployment", Name: md.Name, UID: md.UID}})
					dgd.SetFinalizers([]string{"upstream/hold"})
					objects = append(objects, dgd)
				}
				c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objects...).WithStatusSubresource(md).WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						kind := obj.GetObjectKind().GroupVersionKind().Kind
						if failure == "request-discovery" && kind == DynamoGraphDeploymentRequestKind || failure == "workload-discovery" && kind == DynamoGraphDeploymentKind {
							return errors.New("upstream API unavailable")
						}
						return cl.Get(ctx, key, obj, opts...)
					},
					Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if failure == "workload-delete" && obj.GetObjectKind().GroupVersionKind().Kind == DynamoGraphDeploymentKind {
							return errors.New("upstream delete forbidden")
						}
						return cl.Delete(ctx, obj, opts...)
					},
				}).Build()
				r := NewDynamoProviderReconciler(c, newScheme(), "")
				result, err := r.handleDeletion(context.Background(), md)
				if err != nil {
					t.Fatal(err)
				}
				current := &api.ModelDeployment{}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(md), current); err != nil {
					t.Fatal(err)
				}
				if !controllerutil.ContainsFinalizer(current, "example.com/other") {
					t.Fatal("removed another controller's finalizer")
				}
				if controllerutil.ContainsFinalizer(current, FinalizerName) == expired {
					t.Fatalf("provider finalizer state incorrect: expired=%v", expired)
				}
				if expired && result.RequeueAfter != 0 || !expired && result.RequeueAfter <= 0 {
					t.Fatalf("incorrect cleanup retry: %v", result)
				}
			})
		}
	}
}

func TestDeletionTimeoutCoversIdentityCheckpointFailure(t *testing.T) {
	for _, expired := range []bool{false, true} {
		md := newMDForController("checkpoint", "models")
		setIntentMode(md)
		md.Finalizers = []string{FinalizerName, "example.com/other"}
		deletion := metav1.Now()
		if expired {
			deletion = metav1.NewTime(time.Now().Add(-FinalizerTimeout - time.Minute))
		}
		md.DeletionTimestamp = &deletion
		request := requestFixture(t, md, "Deploying")
		workload := workloadFixture(request, "v1beta1")
		c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(md, request, workload).WithStatusSubresource(md).WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				return errors.New("status updates forbidden")
			},
		}).Build()
		r := NewDynamoProviderReconciler(c, newScheme(), "")
		result, err := r.handleDeletion(context.Background(), md)
		if err != nil {
			t.Fatal(err)
		}
		current := &api.ModelDeployment{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(md), current); err != nil {
			t.Fatal(err)
		}
		if controllerutil.ContainsFinalizer(current, FinalizerName) == expired {
			t.Fatalf("incorrect checkpoint timeout behavior, expired=%v", expired)
		}
		if !controllerutil.ContainsFinalizer(current, "example.com/other") {
			t.Fatal("removed another finalizer")
		}
		if expired && result.RequeueAfter != 0 || !expired && result.RequeueAfter <= 0 {
			t.Fatalf("incorrect retry: %v", result)
		}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(workload), workload); err != nil {
			t.Fatal("deleted workload before its identity was persisted")
		}
	}
}
