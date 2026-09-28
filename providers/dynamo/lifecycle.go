package dynamo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// An input mismatch is not a serving failure and never authorizes teardown.
type intentLockedError struct{ message string }

func (e *intentLockedError) Error() string { return e.message }

func intentAttemptName(md *api.ModelDeployment) string {
	// The profiler limits DGD name + component name to 45 characters. Reserve
	// four for "-dgd" and nineteen for the longest stock prefill worker name.
	return intentNameWithPrefix(md, 5) // at most 22 characters, including the hash
}

// Keep the previous generated name discoverable after an upgrade, including
// recovery when creation succeeded but the ModelDeployment status write did not.
func previousIntentAttemptName(md *api.ModelDeployment) string {
	return intentNameWithPrefix(md, 15)
}

func intentNameWithPrefix(md *api.ModelDeployment, prefixLength int) string {
	sum := sha256.Sum256([]byte(string(md.UID) + "\x00" + md.Annotations[dynamointent.AttemptAnnotation]))
	prefix := strings.TrimRight(strings.ReplaceAll(md.Name[:min(len(md.Name), prefixLength)], ".", "-"), "-")
	return fmt.Sprintf("%s-%x", prefix, sum[:8])
}

func ensureProviderStatus(md *api.ModelDeployment) *api.ProviderStatus {
	if md.Status.Provider == nil {
		md.Status.Provider = &api.ProviderStatus{Name: ProviderName}
	}
	return md.Status.Provider
}

func (r *DynamoProviderReconciler) findRequest(ctx context.Context, md *api.ModelDeployment) (*unstructured.Unstructured, error) {
	p := ensureProviderStatus(md)
	names := []string{md.Name, previousIntentAttemptName(md), intentAttemptName(md)}
	if p.RequestRef != nil {
		if p.RequestRef.Namespace != md.Namespace || p.RequestRef.Kind != DynamoGraphDeploymentRequestKind || p.RequestRef.APIVersion != DynamoAPIGroup+"/"+DynamoGraphDeploymentRequestAPIVersion {
			return nil, fmt.Errorf("invalid Dynamo request reference")
		}
		names = []string{p.RequestRef.Name, previousIntentAttemptName(md), intentAttemptName(md)}
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		u := newDynamoResource(DynamoGraphDeploymentRequestAPIVersion, DynamoGraphDeploymentRequestKind, name, md.Namespace)
		if err := r.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
			if upstreamResourceUnavailable(err) {
				continue
			}
			return nil, err
		}
		if err := verifyDynamoOwnership(u, md.UID); err != nil {
			// A colliding legacy name is not ours to migrate.
			if name == md.Name && p.RequestRef == nil {
				continue
			}
			return nil, err
		}
		if p.RequestRef != nil && p.RequestRef.Name == name && p.RequestRef.UID != "" && p.RequestRef.UID != string(u.GetUID()) {
			return nil, &resourceConflictError{namespace: md.Namespace, name: name}
		}
		return u, nil
	}
	return nil, nil
}

func (r *DynamoProviderReconciler) reconcileIntent(ctx context.Context, desired *unstructured.Unstructured, md *api.ModelDeployment) error {
	p := ensureProviderStatus(md)
	hash, err := dynamointent.Fingerprint(md)
	if err != nil {
		return err
	}
	attempt := md.Annotations[dynamointent.AttemptAnnotation]
	existing, err := r.findRequest(ctx, md)
	if err != nil {
		return err
	}
	acceptedAttempt := ""
	if p.Intent != nil {
		acceptedAttempt = p.Intent.Attempt
	}
	if existing != nil {
		acceptedAttempt = existing.GetAnnotations()[dynamointent.AttemptAnnotation]
		// Recover a successful create followed by a failed MD status write. The
		// new deterministic request already carries its accepted attempt identity.
		if p.RequestRef != nil && p.RequestRef.Name != existing.GetName() {
			if p.WorkloadRef != nil {
				old, err := r.findDGD(ctx, p.WorkloadRef.Namespace, p.WorkloadRef.Name, p.WorkloadRef)
				if err != nil {
					return err
				}
				if old != nil {
					return fmt.Errorf("new request overlaps recorded previous workload")
				}
			}
			p.WorkloadRef = nil
			p.RequestRef = resourceReference(existing)
		}
	}
	if (existing != nil || p.RequestRef != nil || p.WorkloadRef != nil) && acceptedAttempt != attempt {
		if p.Intent == nil {
			p.Intent = &api.ProviderIntentStatus{Attempt: acceptedAttempt}
		}
		// Save the exact resource identities and stop advertising old serving data
		// before the first delete. The outer reconcile persists this checkpoint.
		beforeRequest, beforeWorkload := p.RequestRef, p.WorkloadRef
		if existing != nil {
			p.RequestRef = resourceReference(existing)
			if _, err := r.resolveGeneratedDGD(ctx, md, existing); err != nil {
				return err
			}
		}
		needsCheckpoint := p.Intent.Phase != "Replacing" || !reflect.DeepEqual(beforeRequest, p.RequestRef) || !reflect.DeepEqual(beforeWorkload, p.WorkloadRef)
		p.Intent.Phase = "Replacing"
		md.Status.Phase = api.DeploymentPhaseDeploying
		md.Status.Message = "Replacing the previous Dynamo configuration attempt"
		md.Status.Endpoint = nil
		md.Status.Replicas = nil
		p.InferencePoolRef = nil
		r.setCondition(md, api.ConditionTypeReady, metav1.ConditionFalse, "Reconfiguring", md.Status.Message)
		if needsCheckpoint {
			return errIntentResourceReplacing
		}
		pending, err := r.deleteIntentResource(ctx, md, existing)
		if err != nil {
			return err
		}
		if pending {
			return errIntentResourceReplacing
		}
		p.RequestRef = nil
		p.WorkloadRef = nil
		p.Intent = nil
	}
	if existing != nil {
		desired.SetName(existing.GetName()) // adopt legacy same-name requests in place
		if existing.GetDeletionTimestamp() != nil {
			return errIntentResourceReplacing
		}
		phase, _, _ := unstructured.NestedString(existing.Object, "status", "phase")
		acceptedHash := existing.GetAnnotations()[dynamointent.HashAnnotation]
		if acceptedHash == "" && p.Intent != nil {
			acceptedHash = p.Intent.InputHash
		}
		changed := acceptedHash != "" && acceptedHash != hash
		legacyUnscopedHash := false
		if changed && md.Spec.Provider != nil {
			// Older controllers omitted legacy overrides from the hash when the
			// provider name was unset. Verify the live inputs before migrating it.
			if typed, err := dynamointent.Parse(md); err != nil {
				return err
			} else if typed == nil {
				oldInput := md.DeepCopy()
				oldInput.Spec.Provider.Overrides = nil
				oldHash, err := dynamointent.Fingerprint(oldInput)
				if err != nil {
					return err
				}
				legacyUnscopedHash = acceptedHash == oldHash
			}
		}
		// Hashes describe accepted parent inputs, not proof of current child state.
		have, _, _ := unstructured.NestedMap(existing.Object, "spec")
		want, _, _ := unstructured.NestedMap(desired.Object, "spec")
		drifted, err := requestSpecDiffers(have, want)
		if err != nil {
			return err
		}
		if acceptedHash == "" || legacyUnscopedHash {
			changed = drifted
		} else {
			changed = changed || drifted
		}
		if changed && (phase == "Profiling" || phase == "Ready" || phase == "Deploying" || phase == "Deployed") {
			p.RequestRef = resourceReference(existing)
			return &intentLockedError{message: "Dynamo profiling inputs are locked; change the airunway.ai/dynamo-attempt annotation to explicitly reconfigure"}
		}
		next := existing.DeepCopy()
		annotations := next.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[dynamointent.HashAnnotation] = hash
		annotations[dynamointent.AttemptAnnotation] = attempt
		next.SetAnnotations(annotations)
		if changed {
			if acceptedHash == hash {
				// Correct child drift without discarding operator-discovered inputs.
				corrected := deepMerge(have, want)
				if overrides, supplied := want["overrides"]; supplied {
					corrected["overrides"] = overrides
				} else {
					delete(corrected, "overrides")
				}
				next.Object["spec"] = corrected
			} else {
				next.Object["spec"] = desired.Object["spec"]
			}
		}
		// Optimistic metadata patch preserves operator defaults and concurrent status.
		if !reflect.DeepEqual(existing.Object, next.Object) {
			if changed {
				err = r.Update(ctx, next, strictFieldValidation)
			} else {
				err = r.Patch(ctx, next, client.MergeFromWithOptions(existing, client.MergeFromWithOptimisticLock{}), strictFieldValidation)
			}
			if err != nil {
				return wrapResourceWriteError(err, false)
			}
		}
		p.RequestRef = resourceReference(next)
		p.Intent = &api.ProviderIntentStatus{Phase: phase, InputHash: hash, Attempt: attempt}
		return nil
	}
	// The request may have been removed out of band. Never reuse its name while
	// the same attempt's workload still exists, or silently reprofile that attempt.
	if p.RequestRef != nil && acceptedAttempt == attempt {
		return &intentLockedError{message: "The Dynamo request is missing; change airunway.ai/dynamo-attempt to explicitly retry"}
	}
	desired.SetName(intentAttemptName(md))
	annotations := desired.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[dynamointent.HashAnnotation] = hash
	annotations[dynamointent.AttemptAnnotation] = attempt
	desired.SetAnnotations(annotations)
	if err := r.Create(ctx, desired, strictFieldValidation); err != nil {
		return wrapResourceWriteError(err, false)
	}
	p.RequestRef = resourceReference(desired)
	p.Intent = &api.ProviderIntentStatus{Phase: "Pending", InputHash: hash, Attempt: attempt}
	return nil
}

// Compare the declared request inputs, retaining upstream-derived fields such
// as discovered hardware. An opaque DGD override and override membership are
// owned by Runway; native JobSpec additions/defaults remain upstream-owned.
func requestSpecDiffers(have, want map[string]any) (bool, error) {
	raw, err := json.Marshal([]any{have, want})
	if err != nil {
		return false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var specs []map[string]any
	if err := decoder.Decode(&specs); err != nil {
		return false, err
	}
	for _, spec := range specs {
		for key, value := range spec {
			if value == nil {
				delete(spec, key)
			}
		}
		if overrides, ok := spec["overrides"].(map[string]any); ok {
			for key, value := range overrides {
				if value == nil {
					delete(overrides, key)
				}
			}
			if job, present := overrides["profilingJob"]; present {
				encoded, err := json.Marshal(job)
				if err != nil {
					return false, err
				}
				dec := json.NewDecoder(bytes.NewReader(encoded))
				dec.DisallowUnknownFields()
				var typed batchv1.JobSpec
				// Normalize known native omitempty fields without discarding future
				// fields unknown to this client's Kubernetes version.
				if dec.Decode(&typed) == nil {
					encoded, err = json.Marshal(typed)
					if err != nil {
						return false, err
					}
					dec = json.NewDecoder(bytes.NewReader(encoded))
					dec.UseNumber()
					var normalized map[string]any
					if err := dec.Decode(&normalized); err != nil {
						return false, err
					}
					overrides["profilingJob"] = normalized
				}
			}
		}
	}
	if selectedOverrideValuesDiffer(specs[0], specs[1], specs[1]) {
		return true, nil
	}
	actual, _ := specs[0]["overrides"].(map[string]any)
	desired, _ := specs[1]["overrides"].(map[string]any)
	for key := range actual {
		if _, present := desired[key]; !present {
			return true, nil
		}
	}
	if dgd, present := desired["dgd"]; present && !reflect.DeepEqual(actual["dgd"], dgd) {
		return true, nil
	}
	return false, nil
}

// Releases 1.1.1 and 1.5 bind by name and namespace. Newer releases may also
// supply a request UID. Never ignore that stronger identity when it is present.
func generatedByRequest(dgd, dgdr *unstructured.Unstructured) bool {
	if dgdr == nil {
		return false
	}
	labels := dgd.GetLabels()
	if labels[dynamoDGDRNameLabel] != dgdr.GetName() || labels[dynamoDGDRNamespaceLabel] != dgdr.GetNamespace() {
		return false
	}
	if uid := dgd.GetAnnotations()["nvidia.com/dgdr-uid"]; uid != "" && uid != string(dgdr.GetUID()) {
		return false
	}
	created, requested := dgd.GetCreationTimestamp(), dgdr.GetCreationTimestamp()
	if !created.IsZero() && !requested.IsZero() && created.Before(&requested) {
		return false
	}
	return true
}

func (r *DynamoProviderReconciler) resolveGeneratedDGD(ctx context.Context, md *api.ModelDeployment, request *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	p := ensureProviderStatus(md)
	name, _, _ := unstructured.NestedString(request.Object, "status", "dgdName")
	if name == "" {
		if p.WorkloadRef == nil {
			return r.discoverGeneratedDGD(ctx, md, request)
		}
		name = p.WorkloadRef.Name
	}
	if p.WorkloadRef != nil && p.WorkloadRef.Name != name {
		return nil, fmt.Errorf("generated Dynamo workload identity changed from %s to %s", p.WorkloadRef.Name, name)
	}
	dgd, err := r.findDGD(ctx, md.Namespace, name, p.WorkloadRef)
	if err != nil || dgd == nil {
		return dgd, err
	}
	if !generatedByRequest(dgd, request) {
		return nil, &resourceConflictError{namespace: md.Namespace, name: name}
	}
	p.WorkloadRef = resourceReference(dgd)
	return dgd, nil
}

// A generated DGD can exist before the DGDR's status is checkpointed. Search
// only the request's namespace/relationship labels and retain a concrete UID.
func (r *DynamoProviderReconciler) discoverGeneratedDGD(ctx context.Context, md *api.ModelDeployment, request *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	var found *unstructured.Unstructured
	served := false
	for _, version := range []string{DynamoAPIVersion, dynamoBetaVersion} {
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion(DynamoAPIGroup + "/" + version)
		list.SetKind(DynamoGraphDeploymentKind + "List")
		if err := r.List(ctx, list, client.InNamespace(md.Namespace), client.MatchingLabels{
			dynamoDGDRNameLabel: request.GetName(), dynamoDGDRNamespaceLabel: md.Namespace,
		}); err != nil {
			if upstreamResourceUnavailable(err) {
				continue
			}
			return nil, err
		}
		served = true
		for i := range list.Items {
			candidate := &list.Items[i]
			if !generatedByRequest(candidate, request) {
				continue
			}
			if candidate.GetUID() == "" {
				return nil, fmt.Errorf("generated Dynamo workload has no UID")
			}
			if found != nil && (found.GetUID() != candidate.GetUID() || found.GetName() != candidate.GetName()) {
				return nil, fmt.Errorf("multiple Dynamo workloads match request %s; cleanup requires verification", request.GetName())
			}
			if found == nil {
				found = candidate.DeepCopy()
				// List items may omit TypeMeta; the collection defines it.
				found.SetAPIVersion(DynamoAPIGroup + "/" + version)
				found.SetKind(DynamoGraphDeploymentKind)
			}
		}
	}
	if !served {
		return nil, fmt.Errorf("no served Dynamo workload API for request discovery")
	}
	if found != nil {
		ensureProviderStatus(md).WorkloadRef = resourceReference(found)
	}
	return found, nil
}

// A recorded request identity remains useful after garbage collection removes
// the request. It is discovery evidence only, never a synthetic deletion target.
func (r *DynamoProviderReconciler) resolveCleanupWorkload(ctx context.Context, md *api.ModelDeployment, request *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	p := ensureProviderStatus(md)
	if request == nil {
		if p.WorkloadRef != nil || p.RequestRef == nil {
			return nil, nil
		}
		ref := p.RequestRef
		if ref.Name == "" || ref.UID == "" || ref.Namespace != md.Namespace || ref.Kind != DynamoGraphDeploymentRequestKind || ref.APIVersion != DynamoAPIGroup+"/"+DynamoGraphDeploymentRequestAPIVersion {
			return nil, fmt.Errorf("cannot discover a generated workload without a valid recorded request identity")
		}
		request = referenceResource(ref)
		request.SetUID(types.UID(ref.UID))
		// Requests cannot predate their owning ModelDeployment.
		request.SetCreationTimestamp(md.CreationTimestamp)
	}
	return r.resolveGeneratedDGD(ctx, md, request)
}

func (r *DynamoProviderReconciler) deleteRecordedWorkload(ctx context.Context, md *api.ModelDeployment) (bool, error) {
	p := ensureProviderStatus(md)
	if p.WorkloadRef == nil {
		return false, nil
	}
	ref := p.WorkloadRef
	if ref.Namespace != md.Namespace || ref.UID == "" {
		return false, fmt.Errorf("refusing cleanup without a recorded Dynamo workload UID")
	}
	dgd, err := r.findDGD(ctx, ref.Namespace, ref.Name, ref)
	if err != nil || dgd == nil {
		return false, err
	}
	if dgd.GetDeletionTimestamp() == nil {
		if err := r.deleteWithIdentityPreconditions(ctx, dgd); err != nil && !errors.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}

// Generated DGDs intentionally have no request ownerReference. Follow their
// relationship labels back to the owned request, or a persisted workload UID.
func (r *DynamoProviderReconciler) mapDynamoWorkload(ctx context.Context, obj client.Object) []reconcile.Request {
	for _, owner := range obj.GetOwnerReferences() {
		if owner.APIVersion == api.GroupVersion.String() && owner.Kind == "ModelDeployment" {
			return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: owner.Name}}}
		}
	}
	labels := obj.GetLabels()
	if name := labels[dynamoDGDRNameLabel]; name != "" && labels[dynamoDGDRNamespaceLabel] == obj.GetNamespace() {
		request := newDynamoResource(DynamoGraphDeploymentRequestAPIVersion, DynamoGraphDeploymentRequestKind, name, obj.GetNamespace())
		if err := r.Get(ctx, client.ObjectKeyFromObject(request), request); err == nil {
			for _, owner := range request.GetOwnerReferences() {
				if owner.APIVersion == api.GroupVersion.String() && owner.Kind == "ModelDeployment" {
					return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: owner.Name}}}
				}
			}
		}
	}
	var deployments api.ModelDeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	for _, md := range deployments.Items {
		if md.Status.Provider == nil || md.Status.Provider.Name != ProviderName || md.Status.Provider.WorkloadRef == nil {
			continue
		}
		ref := md.Status.Provider.WorkloadRef
		if ref.UID != "" && ref.UID == string(obj.GetUID()) && ref.Name == obj.GetName() {
			return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(&md)}}
		}
	}
	return nil
}
