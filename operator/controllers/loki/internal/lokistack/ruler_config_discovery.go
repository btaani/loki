package lokistack

import (
	"context"
	"time"

	"github.com/ViaQ/logerr/v2/kverrors"
	lokiv1 "github.com/grafana/loki/operator/apis/loki/v1"
	"github.com/grafana/loki/operator/internal/external/k8s"
	"github.com/grafana/loki/operator/internal/manifests"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	annotationRulerConfigDiscoveredAt = "loki.grafana.com/rulerConfigDiscoveredAt"
)

// AnnotateForRulerConfig adds/updates the `loki.grafana.com/rulerConfigDiscoveredAt` annotation
// to the named Lokistack in the same namespace of the RulerConfig. If no LokiStack is found, then
// skip reconciliation.
func AnnotateForRulerConfig(ctx context.Context, k k8s.Client, name, namespace string) error {
	key := client.ObjectKey{Name: name, Namespace: namespace}
	ss, err := getLokiStack(ctx, k, key)
	if ss == nil || err != nil {
		return err
	}

	timeStamp := time.Now().UTC().Format(time.RFC3339)
	if err := updateAnnotation(ctx, k, ss, annotationRulerConfigDiscoveredAt, timeStamp); err != nil {
		return kverrors.Wrap(err, "failed to update lokistack `rulerConfigDiscoveredAt` annotation", "key", key)
	}

	return nil
}

// RemoveRulerConfigAnnotation removes the `loki.grafana.com/rulerConfigDiscoveredAt` annotation
// from the named Lokistack in the same namespace of the RulerConfig. If no LokiStack is found, then
// skip reconciliation.
func RemoveRulerConfigAnnotation(ctx context.Context, k k8s.Client, name, namespace string) error {
	key := client.ObjectKey{Name: name, Namespace: namespace}
	ss, err := getLokiStack(ctx, k, key)
	if ss == nil || err != nil {
		return err
	}

	if err := removeAnnotation(ctx, k, ss, annotationRulerConfigDiscoveredAt); err != nil {
		return kverrors.Wrap(err, "failed to update lokistack `rulerConfigDiscoveredAt` annotation", "key", key)
	}

	return nil
}

func getLokiStack(ctx context.Context, k k8s.Client, key client.ObjectKey) (*lokiv1.LokiStack, error) {
	var s lokiv1.LokiStack

	if err := k.Get(ctx, key, &s); err != nil {
		if apierrors.IsNotFound(err) {
			// Do nothing
			return nil, nil
		}

		return nil, kverrors.Wrap(err, "failed to get lokistack", "key", key)
	}

	return s.DeepCopy(), nil
}

// RestartOnRulerConfigUpdate restarts the ruler pod after changes done on the rulerConfig
func RestartRulerOnRulerConfigUpdate(ctx context.Context, k k8s.Client, name, namespace string) error {
	var rulerPods corev1.PodList
	err := k.List(ctx, &rulerPods, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			"app.kubernetes.io/component": manifests.LabelRulerComponent,
			"app.kubernetes.io/instance":  name,
		}),
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return kverrors.Wrap(err, "failed to list any ruler instances", "req")
	}

	// restarting ruler by deleting the pod
	for _, rp := range rulerPods.Items {
		if err := k.Delete(ctx, &rp, &client.DeleteOptions{}); err != nil {
			return kverrors.Wrap(err, "failed to update ruler pod", "name", rp.Name, "namespace", rp.Namespace)
		}
	}

	return nil
}
