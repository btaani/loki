package status

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	lokiv1 "github.com/grafana/loki/operator/apis/loki/v1"
	"github.com/grafana/loki/operator/internal/manifests"
)

func TestRefreshSuccess(t *testing.T) {
	now := time.Now()
	stack := &lokiv1.LokiStack{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-stack",
			Namespace: "some-ns",
		},
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "my-stack",
			Namespace: "some-ns",
		},
	}

	componentPods := map[string]*corev1.PodList{
		manifests.LabelCompactorComponent:     createPodList(manifests.LabelCompactorComponent, true, corev1.PodRunning),
		manifests.LabelDistributorComponent:   createPodList(manifests.LabelDistributorComponent, true, corev1.PodRunning),
		manifests.LabelIngesterComponent:      createPodList(manifests.LabelIngesterComponent, true, corev1.PodRunning),
		manifests.LabelQuerierComponent:       createPodList(manifests.LabelQuerierComponent, true, corev1.PodRunning),
		manifests.LabelQueryFrontendComponent: createPodList(manifests.LabelQueryFrontendComponent, true, corev1.PodRunning),
		manifests.LabelIndexGatewayComponent:  createPodList(manifests.LabelIndexGatewayComponent, true, corev1.PodRunning),
		manifests.LabelRulerComponent:         createPodList(manifests.LabelRulerComponent, true, corev1.PodRunning),
		manifests.LabelGatewayComponent:       createPodList(manifests.LabelGatewayComponent, true, corev1.PodRunning),
	}

	wantStatus := lokiv1.LokiStackStatus{
		Components: lokiv1.LokiStackComponentStatus{
			Compactor:     lokiv1.PodStatusMap{lokiv1.PodReady: {"compactor-pod-0"}},
			Distributor:   lokiv1.PodStatusMap{lokiv1.PodReady: {"distributor-pod-0"}},
			IndexGateway:  lokiv1.PodStatusMap{lokiv1.PodReady: {"index-gateway-pod-0"}},
			Ingester:      lokiv1.PodStatusMap{lokiv1.PodReady: {"ingester-pod-0"}},
			Querier:       lokiv1.PodStatusMap{lokiv1.PodReady: {"querier-pod-0"}},
			QueryFrontend: lokiv1.PodStatusMap{lokiv1.PodReady: {"query-frontend-pod-0"}},
			Gateway:       lokiv1.PodStatusMap{lokiv1.PodReady: {"lokistack-gateway-pod-0"}},
			Ruler:         lokiv1.PodStatusMap{lokiv1.PodReady: {"ruler-pod-0"}},
		},
		Storage: lokiv1.LokiStackStorageStatus{},
		Conditions: []metav1.Condition{
			{
				Type:               string(lokiv1.ConditionReady),
				Reason:             string(lokiv1.ReasonReadyComponents),
				Message:            messageReady,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(now),
			},
		},
	}

	k, sw := setupListClient(t, stack, componentPods)

	err := Refresh(context.Background(), k, req, now)

	require.NoError(t, err)
	require.Equal(t, 1, k.GetCallCount())
	require.Equal(t, 8, k.ListCallCount())

	require.Equal(t, 1, sw.UpdateCallCount())
	_, updated, _ := sw.UpdateArgsForCall(0)
	updatedStack, ok := updated.(*lokiv1.LokiStack)
	if !ok {
		t.Fatalf("not a LokiStack: %T", updatedStack)
	}

	require.Equal(t, wantStatus, updatedStack.Status)
}
