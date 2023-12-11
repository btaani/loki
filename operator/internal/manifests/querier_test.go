package manifests

import (
	"testing"

	lokiv1 "github.com/grafana/loki/operator/apis/loki/v1"
	"github.com/grafana/loki/operator/internal/manifests/storage"
	"github.com/stretchr/testify/require"
)

func TestNewQuerierDeployment_HasTemplateConfigHashAnnotation(t *testing.T) {
	ss := NewQuerierDeployment(Options{
		Name:       "abcd",
		Namespace:  "efgh",
		ConfigSHA1: "deadbeef",
		Stack: lokiv1.LokiStackSpec{
			StorageClassName: "standard",
			Template: &lokiv1.LokiTemplateSpec{
				Querier: &lokiv1.LokiComponentSpec{
					Replicas: 1,
				},
			},
		},
	})

	annotations := ss.Spec.Template.Annotations
	require.Contains(t, annotations, AnnotationLokiConfigHash)
	require.Equal(t, annotations[AnnotationLokiConfigHash], "deadbeef")
}

func TestNewQuerierDeployment_HasTemplateObjectStoreHashAnnotation(t *testing.T) {
	ss := NewQuerierDeployment(Options{
		Name:      "abcd",
		Namespace: "efgh",
		ObjectStorage: storage.Options{
			SecretSHA1: "deadbeef",
		},
		Stack: lokiv1.LokiStackSpec{
			StorageClassName: "standard",
			Template: &lokiv1.LokiTemplateSpec{
				Querier: &lokiv1.LokiComponentSpec{
					Replicas: 1,
				},
			},
		},
	})

	annotations := ss.Spec.Template.Annotations
	require.Contains(t, annotations, AnnotationLokiObjectStoreHash)
	require.Equal(t, annotations[AnnotationLokiObjectStoreHash], "deadbeef")
}

func TestNewQuerierDeployment_HasTemplateCertRotationRequiredAtAnnotation(t *testing.T) {
	ss := NewQuerierDeployment(Options{
		Name:                   "abcd",
		Namespace:              "efgh",
		CertRotationRequiredAt: "deadbeef",
		Stack: lokiv1.LokiStackSpec{
			Template: &lokiv1.LokiTemplateSpec{
				Querier: &lokiv1.LokiComponentSpec{
					Replicas: 1,
				},
			},
		},
	})

	annotations := ss.Spec.Template.Annotations
	require.Contains(t, annotations, AnnotationCertRotationRequiredAt)
	require.Equal(t, annotations[AnnotationCertRotationRequiredAt], "deadbeef")
}

func TestNewQuerierDeployment_SelectorMatchesLabels(t *testing.T) {
	// You must set the .spec.selector field of a Deployment to match the labels of
	// its .spec.template.metadata.labels. Prior to Kubernetes 1.8, the
	// .spec.selector field was defaulted when omitted. In 1.8 and later versions,
	// failing to specify a matching Pod Selector will result in a validation error
	// during Deployment creation.
	// See https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#pod-selector
	ss := NewQuerierDeployment(Options{
		Name:      "abcd",
		Namespace: "efgh",
		Stack: lokiv1.LokiStackSpec{
			StorageClassName: "standard",
			Template: &lokiv1.LokiTemplateSpec{
				Querier: &lokiv1.LokiComponentSpec{
					Replicas: 1,
				},
			},
		},
	})

	l := ss.Spec.Template.GetObjectMeta().GetLabels()
	for key, value := range ss.Spec.Selector.MatchLabels {
		require.Contains(t, l, key)
		require.Equal(t, l[key], value)
	}
}
