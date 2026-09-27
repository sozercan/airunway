package v1alpha1

import (
	"context"
	"errors"
	"maps"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	artifactTestNamespace = "team-a"
	artifactTestCache     = "cache"
	artifactTestProvider  = "vllm"
)

type artifactPodReviewer struct {
	allowed bool
	err     error
	called  bool
}

func (f *artifactPodReviewer) CanCreateArtifactPod(_ context.Context, _ admission.Request, _ string) (bool, error) {
	f.called = true
	return f.allowed, f.err
}

func artifactWithAccess() *airunwayv1alpha1.ModelDeployment {
	return &airunwayv1alpha1.ModelDeployment{ObjectMeta: metav1.ObjectMeta{Name: "artifact", Namespace: artifactTestNamespace}, Spec: airunwayv1alpha1.ModelDeploymentSpec{
		Model:    airunwayv1alpha1.ModelSpec{Source: airunwayv1alpha1.ModelSourceCustom, ID: "/model-cache/artifacts", Artifact: &airunwayv1alpha1.ModelArtifactSpec{URI: "s3://bucket/prefix", CredentialsRef: &airunwayv1alpha1.ArtifactCredentialsRef{Name: "private-secret"}}, Storage: &airunwayv1alpha1.StorageSpec{Volumes: []airunwayv1alpha1.StorageVolume{{Name: artifactTestCache, ClaimName: artifactTestCache, Purpose: airunwayv1alpha1.VolumePurposeModelCache}}}},
		Provider: &airunwayv1alpha1.ProviderSpec{Name: artifactTestProvider},
	}}
}

func TestArtifactSecretAccessAlwaysChecked(t *testing.T) {
	obj := artifactWithAccess()
	reviewer := &fakeReviewer{allowed: true}
	validator := &ModelDeploymentCustomValidator{SecretAccess: reviewer}
	if errs := validator.validateArtifactAccess(requestAs("alice"), obj); len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(reviewer.asked) != 1 || reviewer.asked[0] != "alice/team-a/private-secret" {
		t.Fatal("wrong principal", reviewer.asked)
	}
	reviewer.allowed = false
	if _, err := validator.ValidateCreate(requestAs("mallory"), obj); err == nil {
		t.Fatal("create bypassed authorization")
	}
	edited := obj.DeepCopy()
	edited.Spec.Engine.Image = "attacker/image:v1"
	if _, err := validator.ValidateUpdate(requestAs("mallory"), obj, edited); err == nil {
		t.Fatal("unchanged Secret reference bypassed authorization on image edit")
	}
	if errs := validator.validateArtifactAccess(context.Background(), obj); len(errs) == 0 {
		t.Fatal("missing request failed open")
	}
	validator.SecretAccess = nil
	if errs := validator.validateArtifactAccess(requestAs("alice"), obj); len(errs) == 0 {
		t.Fatal("missing reviewer failed open")
	}
	validator.SecretAccess = &fakeReviewer{err: errors.New("unavailable")}
	if errs := validator.validateArtifactAccess(requestAs("alice"), obj); len(errs) == 0 {
		t.Fatal("review error failed open")
	}
	obj.Spec.Model.Artifact.CredentialsRef = nil
	obj.Spec.Secrets = &airunwayv1alpha1.SecretsSpec{HuggingFaceToken: "hf-secret"}
	if errs := validator.validateArtifactAccess(requestAs("mallory"), obj); len(errs) == 0 {
		t.Fatal("HF token bypassed authorization")
	}
	obj.Spec.Model.Artifact = nil
	if errs := validator.validateArtifactAccess(context.Background(), obj); len(errs) != 0 {
		t.Fatal("plain HF behavior changed", errs)
	}
}

func TestArtifactIdentityRequiresPodCreationRights(t *testing.T) {
	for _, kind := range []string{"image", "account"} {
		t.Run(kind, func(t *testing.T) {
			obj := artifactWithAccess()
			obj.Spec.Model.Artifact.CredentialsRef = nil
			if kind == "image" {
				obj.Spec.Model.Artifact.Image = "image:v1"
			} else {
				obj.Spec.Model.Artifact.ServiceAccountName = "powerful"
			}
			pod := &artifactPodReviewer{}
			validator := &ModelDeploymentCustomValidator{ArtifactPodAccess: pod}
			if errs := validator.validateArtifactAccess(requestAs("mallory"), obj); len(errs) == 0 || !pod.called {
				t.Fatal("identity delegation bypass")
			}
			pod.allowed = true
			if errs := validator.validateArtifactAccess(requestAs("alice"), obj); len(errs) != 0 {
				t.Fatal(errs)
			}
			pod.err = errors.New("unavailable")
			if errs := validator.validateArtifactAccess(requestAs("alice"), obj); len(errs) == 0 {
				t.Fatal("review error failed open")
			}
			validator.ArtifactPodAccess = nil
			if errs := validator.validateArtifactAccess(requestAs("alice"), obj); len(errs) == 0 {
				t.Fatal("missing reviewer failed open")
			}
		})
	}
}

func TestArtifactBookkeepingDoesNotDelegateNewAccess(t *testing.T) {
	old := artifactWithAccess()
	next := old.DeepCopy()
	next.Finalizers = []string{"airunway.ai/finalizer"}
	if !artifactBookkeepingOnly(old, next) {
		t.Fatal("finalizer update requires credential escalation")
	}
	if _, err := (&ModelDeploymentCustomValidator{}).ValidateUpdate(context.Background(), old, next); err != nil {
		t.Fatal("finalizer update blocked", err)
	}
	next.Annotations = map[string]string{"arbitrary": "change"}
	if artifactBookkeepingOnly(old, next) {
		t.Fatal("annotation edit bypassed authorization")
	}
	next = old.DeepCopy()
	next.Spec.Model.ID = "/other"
	if artifactBookkeepingOnly(old, next) {
		t.Fatal("spec edit bypassed authorization")
	}
}

func artifactGatewayBookkeepingPair(kind string, remove bool) (*airunwayv1alpha1.ModelDeployment, *airunwayv1alpha1.ModelDeployment) {
	old := artifactWithAccess()
	if kind == "image" {
		old.Spec.Model.Artifact.Image = "downloader:v1"
	} else {
		old.Spec.Model.Artifact.ServiceAccountName = "downloader"
	}
	if remove {
		old.Annotations = map[string]string{airunwayv1alpha1.HTTPRouteCreated: "true"}
	}
	next := old.DeepCopy()
	if remove {
		delete(next.Annotations, airunwayv1alpha1.HTTPRouteCreated)
	} else {
		next.Annotations = map[string]string{airunwayv1alpha1.HTTPRouteCreated: "true"}
	}
	return old, next
}

func TestArtifactGatewayBookkeepingDoesNotRequirePodAccess(t *testing.T) {
	cases := []struct {
		name, kind string
		remove     bool
	}{
		{"image/add", "image", false}, {"image/remove", "image", true},
		{"account/add", "account", false}, {"account/remove", "account", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old, next := artifactGatewayBookkeepingPair(tc.kind, tc.remove)
			oldAnnotations, newAnnotations := maps.Clone(old.Annotations), maps.Clone(next.Annotations)
			if !artifactBookkeepingOnly(old, next) {
				t.Fatal("gateway bookkeeping requires new artifact privileges")
			}
			if _, err := (&ModelDeploymentCustomValidator{}).ValidateUpdate(context.Background(), old, next); err != nil {
				t.Fatal("gateway bookkeeping rejected", err)
			}
			if !maps.Equal(old.Annotations, oldAnnotations) || !maps.Equal(next.Annotations, newAnnotations) {
				t.Fatal("annotations mutated during validation")
			}
		})
	}
}

func TestArtifactGatewayMarkerCannotHideWorkloadChanges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*airunwayv1alpha1.ModelDeployment)
	}{
		{"annotation", func(obj *airunwayv1alpha1.ModelDeployment) {
			obj.Annotations["azure.workload.identity/client-id"] = "other-identity"
		}},
		{"labels", func(obj *airunwayv1alpha1.ModelDeployment) { obj.Labels = map[string]string{"workload": "other"} }},
		{"image", func(obj *airunwayv1alpha1.ModelDeployment) { obj.Spec.Engine.Image = "other/image:v1" }},
		{"ownership", func(obj *airunwayv1alpha1.ModelDeployment) {
			obj.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Secret", Name: "other", UID: "other"}}
		}},
		{"invalid marker", func(obj *airunwayv1alpha1.ModelDeployment) {
			obj.Annotations[airunwayv1alpha1.HTTPRouteCreated] = "unexpected"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old, edited := artifactGatewayBookkeepingPair("account", false)
			tc.mutate(edited)
			if artifactBookkeepingOnly(old, edited) {
				t.Fatal("non-bookkeeping change bypassed authorization")
			}
			if _, err := (&ModelDeploymentCustomValidator{}).ValidateUpdate(context.Background(), old, edited); err == nil {
				t.Fatal("non-bookkeeping update admitted without authorization")
			}
		})
	}
}

func TestDeletingInvalidArtifactAllowsFinalizerCleanup(t *testing.T) {
	old := artifactWithAccess()
	old.Spec.Model.Artifact.URI = "s3://a/prefix" // Admitted by the older bucket validator.
	old.Finalizers = []string{"airunway.ai/provider-cleanup"}
	now := metav1.Now()
	old.DeletionTimestamp = &now
	next := old.DeepCopy()
	next.Finalizers = nil
	validator := &ModelDeploymentCustomValidator{}
	if _, err := validator.ValidateUpdate(context.Background(), old, next); err != nil {
		t.Fatalf("new validation trapped an existing deleting resource: %v", err)
	}
	changed := next.DeepCopy()
	changed.Spec.Engine.Image = "changed:v1"
	if _, err := validator.ValidateUpdate(context.Background(), old, changed); err == nil {
		t.Fatal("deletion allowed a workload-affecting spec change")
	}
	changed = next.DeepCopy()
	changed.Annotations = map[string]string{"workload-setting": "changed"}
	if _, err := validator.ValidateUpdate(context.Background(), old, changed); err == nil {
		t.Fatal("deletion allowed workload annotation changes")
	}
	old.DeletionTimestamp = nil
	if _, err := validator.ValidateUpdate(context.Background(), old, next); err == nil {
		t.Fatal("newly supplied deletion timestamp bypassed validation")
	}
}
