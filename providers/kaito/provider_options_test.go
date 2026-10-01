package kaito

import (
	"context"
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTransformRejectsRawEngineArgs(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"--max-num-seqs=8"}, {"--unknown", "value"}} {
		md := newTestMD("raw-flags", "team")
		md.Spec.Engine.ExtraArgs = args
		objects, err := NewTransformer().Transform(context.Background(), md)
		if len(args) == 0 {
			if err != nil || len(objects) == 0 {
				t.Fatalf("default arguments rejected: %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "KAITO does not support spec.engine.extraArgs") || len(objects) != 0 {
			t.Fatalf("raw arguments must fail before rendering resources: objects=%d, error=%v", len(objects), err)
		}
	}
}

func TestAutoSelectedProviderRejectsRawEngineArgs(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	md := newMDForController("raw-flags", "team")
	md.Spec.Provider = nil // Controller-owned auto-selection is published only in status.
	md.Spec.Engine.ExtraArgs = []string{"--max-num-seqs=8"}
	md.Finalizers = []string{FinalizerName}
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseRunning
	md.Status.Conditions = []metav1.Condition{{Type: airunwayv1alpha1.ConditionTypeReady, Status: metav1.ConditionTrue}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md).WithStatusSubresource(md).Build()
	direct := probeClientBuilderWithWorkspace(t).WithObjects(newReadyKaitoDeployment()).Build()
	r := NewKaitoProviderReconciler(c, scheme, direct, record.NewFakeRecorder(10))
	key := types.NamespacedName{Name: md.Name, Namespace: md.Namespace}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got airunwayv1alpha1.ModelDeployment
	if err := c.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed ||
		!meta.IsStatusConditionFalse(got.Status.Conditions, airunwayv1alpha1.ConditionTypeReady) ||
		!strings.Contains(got.Status.Message, "KAITO does not support spec.engine.extraArgs") {
		t.Fatalf("auto-selected provider silently accepted raw arguments: %+v", got.Status)
	}
}
