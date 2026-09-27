package vllm

import (
	"context"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/storage"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const storageTestVolume = "model-cache"
const storageTestMount = "/" + storageTestVolume

func stagedModel() *airunwayv1alpha1.ModelDeployment {
	md := newMDForController("staged", "team")
	md.UID = types.UID("staged-owner")
	md.Generation = 1
	md.Finalizers = []string{FinalizerName}
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{Name: ProviderName}
	md.Spec.Model.Source = airunwayv1alpha1.ModelSourceCustom
	md.Spec.Model.ID = storageTestMount + "/artifacts"
	md.Spec.Model.Artifact = &airunwayv1alpha1.ModelArtifactSpec{URI: "s3://model-bucket/weights/", Image: "example.test/downloader:fixture"}
	size := resource.MustParse("1Gi")
	md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{
		Name: storageTestVolume, Purpose: airunwayv1alpha1.VolumePurposeModelCache, MountPath: storageTestMount, Size: &size,
	}}}
	return md
}

func storageReconciler(t *testing.T, md *airunwayv1alpha1.ModelDeployment, objects ...client.Object) (*VLLMProviderReconciler, client.Client, ctrl.Request) {
	t.Helper()
	scheme := newScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects = append(objects, md)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(md, &corev1.PersistentVolumeClaim{}, &batchv1.Job{}).Build()
	r := NewVLLMProviderReconciler(c, scheme)
	r.ImageResolver = successfulFakeResolver(fakeResolvedImage(DefaultVLLMImage, "sha256:default"))
	return r, c, ctrl.Request{NamespacedName: types.NamespacedName{Name: md.Name, Namespace: md.Namespace}}
}

func requireNoServingDeployment(t *testing.T, c client.Client, key types.NamespacedName) {
	t.Helper()
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(deploymentGVK)
	if err := c.Get(context.Background(), key, deployment); !errors.IsNotFound(err) {
		t.Fatalf("serving must not start before download completion: %v", err)
	}
}

func TestReconcileStagesArtifactsBeforeServing(t *testing.T) {
	ctx := context.Background()
	md := stagedModel()
	r, c, req := storageReconciler(t, md)
	r.DownloadJobImage = "mirror.example.test/downloader:v1"
	result, err := r.Reconcile(ctx, req)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("expected storage requeue: %+v, %v", result, err)
	}
	requireNoServingDeployment(t, c, req.NamespacedName)
	pvc := &corev1.PersistentVolumeClaim{}
	pvcKey := types.NamespacedName{Name: "staged-" + storageTestVolume, Namespace: md.Namespace}
	if err := c.Get(ctx, pvcKey, pvc); err != nil {
		t.Fatal(err)
	}
	if len(pvc.OwnerReferences) != 1 || pvc.OwnerReferences[0].UID != md.UID {
		t.Fatal("cache must be owned by the model")
	}
	job := startArtifactDownload(t, r, c, req, pvc, md)

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := c.List(ctx, jobs, client.InNamespace(md.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatal("reconciliation must not resubmit the download")
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(deploymentGVK)
	if err := c.Get(ctx, req.NamespacedName, deployment); err != nil {
		t.Fatalf("completed download must allow serving: %v", err)
	}
	var got airunwayv1alpha1.ModelDeployment
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, airunwayv1alpha1.ConditionTypeModelDownloaded) {
		t.Fatal("download completion must be recorded")
	}
}

func startArtifactDownload(
	t *testing.T, r *VLLMProviderReconciler, c client.Client, req ctrl.Request,
	pvc *corev1.PersistentVolumeClaim, md *airunwayv1alpha1.ModelDeployment,
) *batchv1.Job {
	t.Helper()
	ctx := context.Background()
	// Pending storage must progress so the Job can trigger WaitForFirstConsumer binding.
	pvc.Status.Phase = corev1.ClaimPending
	if err := c.Status().Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	result, err := r.Reconcile(ctx, req)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("expected download requeue: %+v, %v", result, err)
	}
	requireNoServingDeployment(t, c, req.NamespacedName)
	jobs := &batchv1.JobList{}
	if err := c.List(ctx, jobs, client.InNamespace(md.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected one download Job, got %d", len(jobs.Items))
	}
	job := &jobs.Items[0]
	if job.Spec.Template.Spec.Containers[0].Image != md.Spec.Model.Artifact.Image || job.Spec.Template.Spec.Containers[0].Args[0] != "artifact" {
		t.Fatal("Job must use the artifact downloader")
	}
	return job
}

func TestFailedArtifactDownloadNeverStartsServing(t *testing.T) {
	ctx := context.Background()
	md := stagedModel()
	r, c, req := storageReconciler(t, md)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, types.NamespacedName{Name: "staged-" + storageTestVolume, Namespace: md.Namespace}, pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := c.List(ctx, jobs); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("missing download Job: %v", err)
	}
	job := &jobs.Items[0]
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "private upstream detail"}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	requireNoServingDeployment(t, c, req.NamespacedName)
	var got airunwayv1alpha1.ModelDeployment
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed || !meta.IsStatusConditionFalse(got.Status.Conditions, airunwayv1alpha1.ConditionTypeReady) {
		t.Fatal("failed download must not report ready")
	}
	if got.Status.Message != "Model download failed. Inspect the download Job and its credentials." {
		t.Fatal("Job details must not leak through status")
	}
}

func TestExistingModelVolumeIsNotDownloadedOrAdopted(t *testing.T) {
	ctx := context.Background()
	md := stagedModel()
	md.Spec.Model.Artifact = nil
	md.Spec.Model.ID = storageTestMount + "/weights"
	md.Spec.Model.Storage.Volumes[0].Size = nil
	md.Spec.Model.Storage.Volumes[0].ClaimName = "existing-weights"
	md.Spec.Model.Storage.Volumes[0].ReadOnly = true
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "existing-weights", Namespace: md.Namespace}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	r, c, req := storageReconciler(t, md, pvc)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := c.List(ctx, jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatal("prepopulated volumes must not be downloaded")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
		t.Fatal(err)
	}
	if len(pvc.OwnerReferences) != 0 {
		t.Fatal("existing storage must not be adopted")
	}
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(deploymentGVK)
	if err := c.Get(ctx, req.NamespacedName, deployment); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileUsesConfiguredDownloadJobImage(t *testing.T) {
	const mirrorImage = "mirror.example.test/downloader:v1"
	for _, tt := range []struct {
		name          string
		staged        bool
		providerImage string
		artifactImage string
		wantImage     string
	}{
		{name: "HF default", wantImage: storage.DefaultDownloadJobImage},
		{name: "HF mirror", providerImage: mirrorImage, wantImage: mirrorImage},
		{name: "artifact provider default", staged: true, wantImage: storage.DefaultDownloadJobImage},
		{name: "artifact mirror fallback", staged: true, providerImage: mirrorImage, wantImage: mirrorImage},
		{name: "artifact image wins", staged: true, providerImage: mirrorImage, artifactImage: "artifact.example.test/downloader:v2", wantImage: "artifact.example.test/downloader:v2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			md := stagedModel()
			if tt.staged {
				md.Spec.Model.Artifact.Image = tt.artifactImage
			} else {
				md.Spec.Model.Artifact = nil
				md.Spec.Model.Source = airunwayv1alpha1.ModelSourceHuggingFace
				md.Spec.Model.ID = "org/model"
				md.Spec.Model.Storage.Volumes[0].MountPath = "/weights/hf"
			}
			r, c, req := storageReconciler(t, md)
			if r.DownloadJobImage != storage.DefaultDownloadJobImage {
				t.Fatalf("unexpected default downloader image %q", r.DownloadJobImage)
			}
			if tt.providerImage != "" {
				r.DownloadJobImage = tt.providerImage
			}
			// First prepare the PVC, then simulate the provisioner's Pending phase
			// so the download Job can trigger WaitForFirstConsumer binding.
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			pvc := &corev1.PersistentVolumeClaim{}
			pvcKey := types.NamespacedName{Name: md.Spec.Model.Storage.Volumes[0].ResolvedClaimName(md.Name), Namespace: md.Namespace}
			if err := c.Get(ctx, pvcKey, pvc); err != nil {
				t.Fatal(err)
			}
			pvc.Status.Phase = corev1.ClaimPending
			if err := c.Status().Update(ctx, pvc); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			requireNoServingDeployment(t, c, req.NamespacedName)
			jobs := &batchv1.JobList{}
			if err := c.List(ctx, jobs, client.InNamespace(md.Namespace)); err != nil {
				t.Fatal(err)
			}
			if len(jobs.Items) != 1 {
				t.Fatalf("expected one download Job, got %d", len(jobs.Items))
			}
			job := &jobs.Items[0]
			container := job.Spec.Template.Spec.Containers[0]
			if container.Image != tt.wantImage {
				t.Fatalf("download image = %q, want %q", container.Image, tt.wantImage)
			}
			if tt.staged {
				if len(container.Args) != 1 || container.Args[0] != "artifact" {
					t.Fatalf("artifact download invocation changed: %v", container.Args)
				}
			} else {
				if len(container.Args) != 2 || container.Args[0] != "download" || container.Args[1] != md.Spec.Model.ID {
					t.Fatalf("HF download invocation changed: %v", container.Args)
				}
				assertStorageHFHome(t, container, "/weights/hf")
			}
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			if err := c.Status().Update(ctx, job); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			serving := storageServingContainer(t, c, req.NamespacedName)
			if tt.staged {
				for _, env := range serving.Env {
					if env.Name == "HF_HOME" {
						t.Fatal("staged artifacts must keep their local model path instead of using HF cache layout")
					}
				}
			} else {
				assertStorageHFHome(t, serving, "/weights/hf")
			}
		})
	}
}

func TestReadOnlyHuggingFaceCacheDoesNotDownload(t *testing.T) {
	ctx := context.Background()
	md := stagedModel()
	md.Spec.Model.Artifact = nil
	md.Spec.Model.Source = airunwayv1alpha1.ModelSourceHuggingFace
	md.Spec.Model.ID = "org/model"
	volume := &md.Spec.Model.Storage.Volumes[0]
	volume.Size = nil
	volume.ClaimName = "existing-cache"
	volume.ReadOnly = true
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: volume.ClaimName, Namespace: md.Namespace},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	r, c, req := storageReconciler(t, md, pvc)
	r.DownloadJobImage = "mirror.example.test/downloader:v1"
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := c.List(ctx, jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("read-only caches must not be downloaded: jobs=%d, error=%v", len(jobs.Items), err)
	}
	serving := storageServingContainer(t, c, req.NamespacedName)
	assertStorageHFHome(t, serving, storageTestMount)
	if len(serving.VolumeMounts) != 1 || !serving.VolumeMounts[0].ReadOnly {
		t.Fatalf("read-only cache mount changed: %+v", serving.VolumeMounts)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
		t.Fatal(err)
	}
	if len(pvc.OwnerReferences) != 0 {
		t.Fatal("pre-populated cache must not be adopted")
	}
}

func storageServingContainer(t *testing.T, c client.Client, key types.NamespacedName) corev1.Container {
	t.Helper()
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(deploymentGVK)
	if err := c.Get(context.Background(), key, deployment); err != nil {
		t.Fatal(err)
	}
	var container corev1.Container
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(getContainer(t, deployment), &container); err != nil {
		t.Fatal(err)
	}
	return container
}

func assertStorageHFHome(t *testing.T, container corev1.Container, mountPath string) {
	t.Helper()
	count := 0
	for _, env := range container.Env {
		if env.Name == "HF_HOME" {
			count++
			if env.Value != mountPath || env.ValueFrom != nil {
				t.Fatalf("HF_HOME must use mounted cache %q: %+v", mountPath, env)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected one HF_HOME entry, got %d", count)
	}
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == mountPath {
			return
		}
	}
	t.Fatalf("HF_HOME directory %q is not mounted", mountPath)
}
