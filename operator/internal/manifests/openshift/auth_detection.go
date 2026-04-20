package openshift

import (
	"context"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/grafana/loki/operator/internal/external/k8s"
)

func IsAuthTypeOIDC(ctx context.Context, c k8s.Client, log logr.Logger) (bool, error) {
	auth := &configv1.Authentication{}
	key := types.NamespacedName{Name: "cluster"}

	if err := c.Get(ctx, key, auth); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Authentication CR not found, assuming native OAuth")
			return false, nil
		}
		return false, err
	}

	if auth.Spec.Type == configv1.AuthenticationTypeOIDC {
		log.Info("Detected external OIDC authentication")
		return true, nil
	}

	return false, nil
}
