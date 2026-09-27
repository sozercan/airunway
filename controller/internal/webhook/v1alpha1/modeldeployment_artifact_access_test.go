package v1alpha1

import (
	"context"
	"errors"
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
