package vllm

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Exercise the real builder and event handlers without starting an API server.
type storageWatchInformer struct {
	controllertest.FakeInformer
	registered chan struct{}
}

func (i *storageWatchInformer) AddEventHandlerWithOptions(
	handler toolscache.ResourceEventHandler, options toolscache.HandlerOptions,
) (toolscache.ResourceEventHandlerRegistration, error) {
	registration, err := i.FakeInformer.AddEventHandlerWithOptions(handler, options)
	close(i.registered)
	return registration, err
}

type storageWatchCache struct {
	cache.Cache
	informers map[reflect.Type]*storageWatchInformer
}

func (c *storageWatchCache) GetInformer(
	_ context.Context, obj client.Object, _ ...cache.InformerGetOption,
) (cache.Informer, error) {
	informer, ok := c.informers[reflect.TypeOf(obj)]
	if !ok {
		return nil, fmt.Errorf("unexpected informer type %T", obj)
	}
	return informer, nil
}

func (*storageWatchCache) WaitForCacheSync(context.Context) bool { return true }

type storageWatchManager struct {
	ctrl.Manager
	scheme   *runtime.Scheme
	cache    *storageWatchCache
	mapper   meta.RESTMapper
	runnable manager.Runnable
}

func (m *storageWatchManager) GetScheme() *runtime.Scheme     { return m.scheme }
func (m *storageWatchManager) GetCache() cache.Cache          { return m.cache }
func (m *storageWatchManager) GetRESTMapper() meta.RESTMapper { return m.mapper }
func (*storageWatchManager) GetLogger() logr.Logger           { return logr.Discard() }
func (*storageWatchManager) GetControllerOptions() config.Controller {
	skipNameValidation := true
	return config.Controller{SkipNameValidation: &skipNameValidation}
}
func (m *storageWatchManager) Add(runnable manager.Runnable) error {
	m.runnable = runnable
	return nil
}

func startStorageWatchController(t *testing.T) (*storageWatchCache, <-chan types.NamespacedName) {
	t.Helper()
	scheme := newScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	md := newMDForController("watched", "team")
	md.UID = "watched-owner"
	// Stop after the initial read so assertions observe enqueueing, not storage/image I/O.
	md.Annotations = map[string]string{"airunway.ai/reconcile-paused": "true"}
	reads := make(chan types.NamespacedName, 16)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*airunwayv1alpha1.ModelDeployment); ok {
				reads <- key
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	watchCache := &storageWatchCache{informers: map[reflect.Type]*storageWatchInformer{}}
	for _, obj := range []client.Object{&airunwayv1alpha1.ModelDeployment{}, &corev1.PersistentVolumeClaim{}, &batchv1.Job{}} {
		watchCache.informers[reflect.TypeOf(obj)] = &storageWatchInformer{
			FakeInformer: controllertest.FakeInformer{Synced: true},
			registered:   make(chan struct{}),
		}
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{airunwayv1alpha1.GroupVersion})
	mapper.Add(airunwayv1alpha1.GroupVersion.WithKind("ModelDeployment"), meta.RESTScopeNamespace)
	mgr := &storageWatchManager{scheme: scheme, cache: watchCache, mapper: mapper}
	r := NewVLLMProviderReconciler(c, scheme)
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.runnable.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("controller shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("controller did not stop")
		}
	})
	for typ, informer := range watchCache.informers {
		select {
		case <-informer.registered:
		case <-time.After(2 * time.Second):
			t.Fatalf("no watch registered for %v", typ)
		}
	}
	return watchCache, reads
}

func expectStorageWatchRead(t *testing.T, reads <-chan types.NamespacedName, want bool) {
	t.Helper()
	timeout := 100 * time.Millisecond
	if want {
		timeout = 5 * time.Second
	}
	select {
	case key := <-reads:
		if !want || key != (types.NamespacedName{Name: "watched", Namespace: "team"}) {
			t.Fatalf("unexpected reconcile request %v, want enqueue=%v", key, want)
		}
	case <-time.After(timeout):
		if want {
			t.Fatal("storage event did not enqueue its ModelDeployment owner")
		}
	}
}

func TestStorageWatchesEnqueueStatusChanges(t *testing.T) {
	watchCache, reads := startStorageWatchController(t)
	controllerOwner := true
	owner := metav1.OwnerReference{
		APIVersion: airunwayv1alpha1.GroupVersion.String(), Kind: "ModelDeployment",
		Name: "watched", UID: "watched-owner", Controller: &controllerOwner,
	}
	metadata := metav1.ObjectMeta{
		Name: "watched-model-cache", Namespace: "team", Generation: 1, ResourceVersion: "1",
		OwnerReferences: []metav1.OwnerReference{owner},
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metadata, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}}
	bound := pvc.DeepCopy()
	bound.ResourceVersion = "2"
	bound.Status.Phase = corev1.ClaimBound
	job := &batchv1.Job{ObjectMeta: metadata}
	complete := job.DeepCopy()
	complete.ResourceVersion = "2"
	complete.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	failed := job.DeepCopy()
	failed.ResourceVersion = "2"
	failed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}

	for _, tt := range []struct {
		name     string
		old, new client.Object
	}{
		{"PVC bound without generation change", pvc, bound},
		{"Job completed without generation change", job, complete},
		{"Job failed without generation change", job, failed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			informer := watchCache.informers[reflect.TypeOf(tt.new)]
			informer.Update(tt.old, tt.new)
			expectStorageWatchRead(t, reads, true)
			informer.Update(tt.new, tt.new.DeepCopyObject().(client.Object))
			expectStorageWatchRead(t, reads, false)
		})
	}
	t.Run("unowned existing claim is ignored", func(t *testing.T) {
		oldClaim, newClaim := pvc.DeepCopy(), bound.DeepCopy()
		oldClaim.OwnerReferences, newClaim.OwnerReferences = nil, nil
		watchCache.informers[reflect.TypeFor[*corev1.PersistentVolumeClaim]()].Update(oldClaim, newClaim)
		expectStorageWatchRead(t, reads, false)
	})
	t.Run("owned storage deletion enqueues the model", func(t *testing.T) {
		watchCache.informers[reflect.TypeFor[*corev1.PersistentVolumeClaim]()].Delete(bound)
		expectStorageWatchRead(t, reads, true)
	})
	t.Run("primary provider filtering remains intact", func(t *testing.T) {
		oldMD := newMDForController("watched", "team")
		oldMD.Status.Provider = nil
		selected := oldMD.DeepCopy()
		selected.Status.Provider = &airunwayv1alpha1.ProviderStatus{Name: ProviderName}
		informer := watchCache.informers[reflect.TypeFor[*airunwayv1alpha1.ModelDeployment]()]
		informer.Update(oldMD, selected)
		expectStorageWatchRead(t, reads, true)
		other := oldMD.DeepCopy()
		other.Status.Provider = &airunwayv1alpha1.ProviderStatus{Name: "dynamo"}
		informer.Update(oldMD, other)
		expectStorageWatchRead(t, reads, false)
		other.Finalizers = []string{FinalizerName}
		informer.Update(oldMD, other)
		expectStorageWatchRead(t, reads, true)
	})
}
