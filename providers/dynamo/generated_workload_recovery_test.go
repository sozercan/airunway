package dynamo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func lostRequestMD() *api.ModelDeployment {
	md := newMDForController("lost", "models")
	setIntentMode(md)
	md.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	md.Finalizers = []string{FinalizerName, "example.com/other"}
	now := metav1.Now()
	md.DeletionTimestamp = &now
	md.Status.Provider = &api.ProviderStatus{Name: ProviderName, ResourceKind: DynamoGraphDeploymentRequestKind,
		RequestRef: &api.ProviderResourceReference{APIVersion: DynamoAPIGroup + "/" + DynamoGraphDeploymentRequestAPIVersion, Kind: DynamoGraphDeploymentRequestKind, Namespace: md.Namespace, Name: "lost-request", UID: "request-uid"}}
	return md
}

func lostRequestWorkload(version, name string) *unstructured.Unstructured {
	u := newDynamoResource(version, DynamoGraphDeploymentKind, name, "models")
	u.SetUID(types.UID("uid-" + name))
	u.SetCreationTimestamp(metav1.Now())
	u.SetLabels(map[string]string{dynamoDGDRNameLabel: "lost-request", dynamoDGDRNamespaceLabel: "models"})
	return u
}

func TestDeletionRecoversWorkloadAfterRequestDisappears(t *testing.T) {
	for _, target := range []struct {
		version string
		delayed bool
	}{
		{DynamoAPIVersion, false}, {dynamoBetaVersion, false},
		{DynamoAPIVersion, true}, {dynamoBetaVersion, true},
	} {
		name := target.version
		if target.delayed {
			name += "-late-discovery"
		}
		t.Run(name, func(t *testing.T) {
			md := lostRequestMD()
			workload := lostRequestWorkload(target.version, "generated")
			deletes, lists := 0, 0
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(md, workload).WithStatusSubresource(md).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lists++
					if target.delayed && lists <= 2 {
						return nil
					}
					return cl.List(ctx, list, opts...)
				},
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deletes++
					if obj.GetObjectKind().GroupVersionKind().Kind != DynamoGraphDeploymentKind {
						t.Fatal("synthetic request used as deletion target")
					}
					options := &client.DeleteOptions{}
					for _, option := range opts {
						option.ApplyToDelete(options)
					}
					if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != workload.GetUID() {
						t.Fatal("missing recovered UID precondition")
					}
					return cl.Delete(ctx, obj, opts...)
				},
			}).Build()
			r := NewDynamoProviderReconciler(c, newScheme(), "")
			if _, err := r.handleDeletion(context.Background(), md); err != nil {
				t.Fatal(err)
			}
			if deletes != 0 {
				t.Fatal("workload deleted before identity checkpoint")
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(md), md); err != nil {
				t.Fatal(err)
			}
			if md.Status.Provider.WorkloadRef == nil || md.Status.Provider.WorkloadRef.UID != string(workload.GetUID()) {
				t.Fatalf("workload identity not recovered and persisted: status=%+v provider=%+v", md.Status, md.Status.Provider)
			}
			if !controllerutil.ContainsFinalizer(md, FinalizerName) {
				t.Fatal("finalizer removed before cleanup")
			}
			if _, err := r.handleDeletion(context.Background(), md); err != nil {
				t.Fatal(err)
			}
			if deletes != 1 {
				t.Fatalf("expected one workload deletion, got %d", deletes)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(workload), workload); !apierrors.IsNotFound(err) {
				t.Fatalf("workload remains: %v", err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(md), md); err != nil {
				t.Fatal(err)
			}
			if _, err := r.handleDeletion(context.Background(), md); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(md), md); err != nil {
				t.Fatal(err)
			}
			if controllerutil.ContainsFinalizer(md, FinalizerName) || !controllerutil.ContainsFinalizer(md, "example.com/other") {
				t.Fatal("incorrect finalizer cleanup")
			}
		})
	}
}

func TestWorkloadRecoveryRefusesAmbiguityAndUnavailableDiscovery(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		md := lostRequestMD()
		first, second := lostRequestWorkload(DynamoAPIVersion, "first"), lostRequestWorkload(DynamoAPIVersion, "second")
		c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(md, first, second).WithStatusSubresource(md).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if unavailable {
					return errors.New("discovery forbidden")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
		r := NewDynamoProviderReconciler(c, newScheme(), "")
		result, err := r.handleDeletion(context.Background(), md)
		if err != nil || result.RequeueAfter <= 0 {
			t.Fatalf("unsafe cleanup result: %v %v", result, err)
		}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(md), md); err != nil {
			t.Fatal(err)
		}
		if !controllerutil.ContainsFinalizer(md, FinalizerName) || md.Status.Provider.WorkloadRef != nil {
			t.Fatal("uncertain workload identity accepted")
		}
		for _, workload := range []*unstructured.Unstructured{first, second} {
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(workload), workload); err != nil {
				t.Fatal("uncertain workload deleted")
			}
		}
	}
}

func TestRecoveryHonorsRequestUIDNamespaceAndCreationBoundary(t *testing.T) {
	for _, mismatch := range []string{"uid", "namespace", "older"} {
		md := lostRequestMD()
		workload := lostRequestWorkload(DynamoAPIVersion, "foreign")
		switch mismatch {
		case "uid":
			workload.SetAnnotations(map[string]string{"nvidia.com/dgdr-uid": "different-request"})
		case "namespace":
			workload.SetNamespace("other")
		case "older":
			workload.SetCreationTimestamp(metav1.NewTime(md.CreationTimestamp.Add(-time.Minute)))
		}
		c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(workload).Build()
		r := NewDynamoProviderReconciler(c, newScheme(), "")
		found, err := r.resolveCleanupWorkload(context.Background(), md, nil)
		if err != nil || found != nil || md.Status.Provider.WorkloadRef != nil {
			t.Fatalf("foreign %s workload accepted: %v", mismatch, err)
		}
	}
	md := lostRequestMD()
	md.Status.Provider.RequestRef.UID = ""
	r := NewDynamoProviderReconciler(fake.NewClientBuilder().WithScheme(newScheme()).Build(), newScheme(), "")
	if _, err := r.resolveCleanupWorkload(context.Background(), md, nil); err == nil || !strings.Contains(err.Error(), "recorded request identity") {
		t.Fatalf("missing request UID accepted: %v", err)
	}
}

func TestGeneratedWorkloadCanPrecedeRequestStatus(t *testing.T) {
	md := typedRenderingMD(t)
	request := requestFixture(t, md, "Deploying")
	workload := newDynamoResource(dynamoBetaVersion, DynamoGraphDeploymentKind, "early-workload", md.Namespace)
	workload.SetUID("early-uid")
	workload.SetLabels(map[string]string{dynamoDGDRNameLabel: request.GetName(), dynamoDGDRNamespaceLabel: request.GetNamespace()})
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(workload).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	found, err := r.resolveGeneratedDGD(context.Background(), md, request)
	if err != nil || found == nil || md.Status.Provider.WorkloadRef == nil || md.Status.Provider.WorkloadRef.UID != "early-uid" {
		t.Fatalf("workload not discovered before dgdName status: %v", err)
	}
}

func TestIntentDeletionWithoutAnyRequestCheckpointRequiresVerification(t *testing.T) {
	md := lostRequestMD()
	md.Status.Provider.RequestRef = nil
	md.Status.Provider.ResourceKind = ""
	workload := lostRequestWorkload(DynamoAPIVersion, "uncheckpointed")
	workload.SetLabels(map[string]string{dynamoDGDRNameLabel: intentAttemptName(md), dynamoDGDRNamespaceLabel: md.Namespace})
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(md, workload).WithStatusSubresource(md).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	result, err := r.handleDeletion(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(md), md); err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 || !controllerutil.ContainsFinalizer(md, FinalizerName) || !strings.Contains(md.Status.Message, "verification") {
		t.Fatal("unverified intent cleanup reported complete without any request identity")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workload), workload); err != nil {
		t.Fatal("workload deleted without recorded request identity")
	}
}
