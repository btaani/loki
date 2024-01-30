package storage

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lokiv1 "github.com/grafana/loki/operator/apis/loki/v1"
	"github.com/grafana/loki/operator/internal/external/k8s"
	"github.com/grafana/loki/operator/internal/manifests/storage"
	"github.com/grafana/loki/operator/internal/status"
)

var (
	hashSeparator = []byte(",")

	errSecretUnknownType  = errors.New("unknown secret type")
	errSecretMissingField = errors.New("missing secret field")
	errSecretHashError    = errors.New("error calculating hash for secret")
)

func getSecret(ctx context.Context, k k8s.Client, stack *lokiv1.LokiStack) (*corev1.Secret, error) {
	var storageSecret corev1.Secret
	key := client.ObjectKey{Name: stack.Spec.Storage.Secret.Name, Namespace: stack.Namespace}
	if err := k.Get(ctx, key, &storageSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &status.DegradedError{
				Message: "Missing object storage secret",
				Reason:  lokiv1.ReasonMissingObjectStorageSecret,
				Requeue: false,
			}
		}
		return nil, fmt.Errorf("failed to lookup lokistack storage secret: %w", err)
	}

	return &storageSecret, nil
}

// extractSecret reads a k8s secret into a manifest object storage struct if valid.
func extractSecret(s *corev1.Secret, secretType lokiv1.ObjectStorageSecretType) (storage.Options, error) {
	hash, err := hashSecretData(s)
	if err != nil {
		return storage.Options{}, errSecretHashError
	}

	storageOpts := storage.Options{
		SecretName:  s.Name,
		SecretSHA1:  hash,
		SharedStore: secretType,
	}

	switch secretType {
	case lokiv1.ObjectStorageSecretAzure:
		storageOpts.Azure, err = extractAzureConfigSecret(s)
	case lokiv1.ObjectStorageSecretGCS:
		storageOpts.GCS, err = extractGCSConfigSecret(s)
	case lokiv1.ObjectStorageSecretS3:
		storageOpts.S3, err = extractS3ConfigSecret(s)
	case lokiv1.ObjectStorageSecretSwift:
		storageOpts.Swift, err = extractSwiftConfigSecret(s)
	default:
		return storage.Options{}, fmt.Errorf("%w: %s", errSecretUnknownType, secretType)
	}

	if err != nil {
		return storage.Options{}, err
	}
	return storageOpts, nil
}

func hashSecretData(s *corev1.Secret) (string, error) {
	keys := make([]string, 0, len(s.Data))
	for k := range s.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha1.New()
	for _, k := range keys {
		if _, err := h.Write([]byte(k)); err != nil {
			return "", err
		}

		if _, err := h.Write(hashSeparator); err != nil {
			return "", err
		}

		if _, err := h.Write(s.Data[k]); err != nil {
			return "", err
		}

		if _, err := h.Write(hashSeparator); err != nil {
			return "", err
		}
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func extractAzureConfigSecret(s *corev1.Secret) (*storage.AzureStorageConfig, error) {
	// Extract and validate mandatory fields
	env := s.Data[storage.KeyAzureEnvironmentName]
	if len(env) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeyAzureEnvironmentName)
	}
	container := s.Data[storage.KeyAzureStorageContainerName]
	if len(container) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeyAzureStorageContainerName)
	}
	name := s.Data[storage.KeyAzureStorageAccountName]
	if len(name) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeyAzureStorageAccountName)
	}
	key := s.Data[storage.KeyAzureStorageAccountKey]
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeyAzureStorageAccountKey)
	}

	return &storage.AzureStorageConfig{
		Env:       string(env),
		Container: string(container),
	}, nil
}

func extractGCSConfigSecret(s *corev1.Secret) (*storage.GCSStorageConfig, error) {
	// Extract and validate mandatory fields
	bucket := s.Data[storage.KeyGCPStorageBucketName]
	if len(bucket) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeyGCPStorageBucketName)
	}

	// Check if google authentication credentials is provided
	keyJSON := s.Data[storage.KeyGCPServiceAccountKeyFilename]
	if len(keyJSON) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeyGCPServiceAccountKeyFilename)
	}

	return &storage.GCSStorageConfig{
		Bucket: string(bucket),
	}, nil
}

func extractS3ConfigSecret(s *corev1.Secret) (*storage.S3StorageConfig, error) {
	// Extract and validate mandatory fields
	buckets := s.Data[storage.KeyAWSBucketNames]
	if len(buckets) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeyAWSBucketNames)
	}
	endpoint := s.Data[storage.KeyAWSEndpoint]
	if len(endpoint) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeyAWSEndpoint)
	}
	id := s.Data[storage.KeyAWSAccessKeyID]
	if len(id) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeyAWSAccessKeyID)
	}
	secret := s.Data[storage.KeyAWSAccessKeySecret]
	if len(secret) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeyAWSAccessKeySecret)
	}

	// Extract and validate optional fields
	region := s.Data[storage.KeyAWSRegion]

	return &storage.S3StorageConfig{
		Endpoint: string(endpoint),
		Buckets:  string(buckets),
		Region:   string(region),
	}, nil
}

func extractSwiftConfigSecret(s *corev1.Secret) (*storage.SwiftStorageConfig, error) {
	// Extract and validate mandatory fields
	url := s.Data[storage.KeySwiftAuthURL]
	if len(url) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeySwiftAuthURL)
	}
	username := s.Data[storage.KeySwiftUsername]
	if len(username) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeySwiftUsername)
	}
	userDomainName := s.Data[storage.KeySwiftUserDomainName]
	if len(userDomainName) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeySwiftUserDomainName)
	}
	userDomainID := s.Data[storage.KeySwiftUserDomainID]
	if len(userDomainID) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeySwiftUserDomainID)
	}
	userID := s.Data[storage.KeySwiftUserID]
	if len(userID) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeySwiftUserID)
	}
	password := s.Data[storage.KeySwiftPassword]
	if len(password) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeySwiftPassword)
	}
	domainID := s.Data[storage.KeySwiftDomainID]
	if len(domainID) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeySwiftDomainID)
	}
	domainName := s.Data[storage.KeySwiftDomainName]
	if len(domainName) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeySwiftDomainName)
	}
	containerName := s.Data[storage.KeySwiftContainerName]
	if len(containerName) == 0 {
		return nil, fmt.Errorf("%w: %s", errSecretMissingField, storage.KeySwiftContainerName)
	}

	// Extract and validate optional fields
	projectID := s.Data[storage.KeySwiftProjectID]
	projectName := s.Data[storage.KeySwiftProjectName]
	projectDomainID := s.Data[storage.KeySwiftProjectDomainId]
	projectDomainName := s.Data[storage.KeySwiftProjectDomainName]
	region := s.Data[storage.KeySwiftRegion]

	return &storage.SwiftStorageConfig{
		AuthURL:           string(url),
		UserDomainName:    string(userDomainName),
		UserDomainID:      string(userDomainID),
		UserID:            string(userID),
		DomainID:          string(domainID),
		DomainName:        string(domainName),
		ProjectID:         string(projectID),
		ProjectName:       string(projectName),
		ProjectDomainID:   string(projectDomainID),
		ProjectDomainName: string(projectDomainName),
		Region:            string(region),
		Container:         string(containerName),
	}, nil
}
