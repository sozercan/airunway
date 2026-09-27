package v1alpha1

import (
	"context"
	"fmt"
	"reflect"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ArtifactPodAccessReviewer checks whether the caller can already delegate a
// namespace's pod identities/images. Reading a ServiceAccount does NOT grant
// permission to use its workload identity or mint its tokens.
type ArtifactPodAccessReviewer interface {
	CanCreateArtifactPod(context.Context, admission.Request, string) (bool, error)
}

type artifactPodSARReviewer struct{ client client.Client }

func (r *artifactPodSARReviewer) CanCreateArtifactPod(ctx context.Context, req admission.Request, namespace string) (bool, error) {
	sar := &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
		User: req.UserInfo.Username, UID: req.UserInfo.UID, Groups: req.UserInfo.Groups,
		ResourceAttributes: &authzv1.ResourceAttributes{Namespace: namespace, Verb: "create", Group: "", Resource: "pods"},
	}}
	sar.Spec.Extra = artifactReviewExtras(req.UserInfo.Extra)
	if err := r.client.Create(ctx, sar); err != nil {
		return false, err
	}
	return sar.Status.Allowed, nil
}

func artifactReviewExtras(extra map[string]authnv1.ExtraValue) map[string]authzv1.ExtraValue {
	if len(extra) == 0 {
		return nil
	}
	converted := make(map[string]authzv1.ExtraValue, len(extra))
	for key, values := range extra {
		converted[key] = authzv1.ExtraValue(values)
	}
	return converted
}

// validateArtifactAccess always checks the requesting principal, not the
// controller's privileges or a prior author's authorization. Artifact identity
// is immutable, but edits to other workload fields can still expose cached data.
func (v *ModelDeploymentCustomValidator) validateArtifactAccess(ctx context.Context, obj *airunwayv1alpha1.ModelDeployment) field.ErrorList {
	a := obj.Spec.Model.Artifact
	if a == nil {
		return nil
	}
	p := field.NewPath("spec", "model", "artifact")
	names := map[string]bool{}
	if a.CredentialsRef != nil {
		names[a.CredentialsRef.Name] = true
	}
	if obj.Spec.Secrets != nil && obj.Spec.Secrets.HuggingFaceToken != "" {
		names[obj.Spec.Secrets.HuggingFaceToken] = true
	}
	needsPodAccess := a.ServiceAccountName != "" || a.Image != ""
	if len(names) == 0 && !needsPodAccess {
		return nil
	}
	req, err := admission.RequestFromContext(ctx)
	if err != nil || req.UserInfo.Username == "" {
		return field.ErrorList{field.Forbidden(p, "artifact access cannot be authorized without the requesting user")}
	}
	var allErrs field.ErrorList
	for name := range names {
		if err := v.validateArtifactSecretAccess(ctx, req, obj.Namespace, name, p); err != nil {
			allErrs = append(allErrs, err)
		}
	}
	if needsPodAccess {
		if err := v.validateArtifactPodAccess(ctx, req, obj.Namespace, p); err != nil {
			allErrs = append(allErrs, err)
		}
	}
	return allErrs
}

func (v *ModelDeploymentCustomValidator) validateArtifactSecretAccess(ctx context.Context, req admission.Request, namespace, name string, p *field.Path) *field.Error {
	if v.SecretAccess == nil {
		return field.InternalError(p, fmt.Errorf("artifact Secret authorizer is not configured"))
	}
	allowed, _, err := v.SecretAccess.CanGetSecret(ctx, req, namespace, name)
	if err != nil {
		return field.InternalError(p, fmt.Errorf("artifact Secret authorization failed"))
	}
	if !allowed {
		return field.Forbidden(p, "caller must have get permission for every Secret referenced by an artifact deployment")
	}
	return nil
}

func (v *ModelDeploymentCustomValidator) validateArtifactPodAccess(ctx context.Context, req admission.Request, namespace string, p *field.Path) *field.Error {
	if v.ArtifactPodAccess == nil {
		return field.InternalError(p, fmt.Errorf("artifact pod authorizer is not configured"))
	}
	allowed, err := v.ArtifactPodAccess.CanCreateArtifactPod(ctx, req, namespace)
	if err != nil {
		return field.InternalError(p, fmt.Errorf("artifact pod authorization failed"))
	}
	if !allowed {
		return field.Forbidden(p, "caller must have create pods permission in this namespace to select an artifact service account or downloader image")
	}
	return nil
}

// artifactBookkeepingOnly excludes only updates with no workload-affecting
// changes. In particular, keeping credentialsRef unchanged does NOT bypass SAR
// when the image, spec, labels, annotations, or ownership are edited.
func artifactBookkeepingOnly(oldObj, newObj *airunwayv1alpha1.ModelDeployment) bool {
	return reflect.DeepEqual(oldObj.Spec, newObj.Spec) &&
		reflect.DeepEqual(oldObj.Labels, newObj.Labels) &&
		reflect.DeepEqual(oldObj.Annotations, newObj.Annotations) &&
		reflect.DeepEqual(oldObj.OwnerReferences, newObj.OwnerReferences)
}
