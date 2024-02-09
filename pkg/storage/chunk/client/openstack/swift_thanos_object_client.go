package openstack

import (
	"context"
	"io"
	"strings"

	"github.com/go-kit/log"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/thanos-io/objstore"

	"github.com/grafana/loki/pkg/storage/bucket"
	"github.com/grafana/loki/pkg/storage/chunk/client"
	"github.com/grafana/loki/pkg/storage/chunk/client/hedging"
)

type SwiftThanosObjectClient struct {
	client objstore.Bucket
}

func NewSwiftThanosObjectClient(ctx context.Context, cfg bucket.Config, component string, logger log.Logger, hedgingCfg hedging.Config, reg prometheus.Registerer) (*SwiftThanosObjectClient, error) {
	// TODO Add Hedging client once we are able to configure HTTP on Swift provider
	return newSwiftThanosObjectClient(ctx, cfg, component, logger, reg)
}

func newSwiftThanosObjectClient(ctx context.Context, cfg bucket.Config, component string, logger log.Logger, reg prometheus.Registerer) (*SwiftThanosObjectClient, error) {
	bucket, err := bucket.NewClient(ctx, cfg, component, logger, reg)
	if err != nil {
		return nil, err
	}
	return &SwiftThanosObjectClient{
		client: bucket,
	}, nil
}

func (s *SwiftThanosObjectClient) Stop() {}

// ObjectExists checks if a given objectKey exists in the Swift bucket
func (s *SwiftThanosObjectClient) ObjectExists(ctx context.Context, objectKey string) (bool, error) {
	return s.client.Exists(ctx, objectKey)
}

// PutObject puts the specified bytes into the configured Swift bucket at the provided key
func (s *SwiftThanosObjectClient) PutObject(ctx context.Context, objectKey string, object io.ReadSeeker) error {
	return s.client.Upload(ctx, objectKey, object)
}

// GetObject returns a reader and the size for the specified object key from the configured Swift bucket.
func (s *SwiftThanosObjectClient) GetObject(ctx context.Context, objectKey string) (io.ReadCloser, int64, error) {
	reader, err := s.client.Get(ctx, objectKey)
	if err != nil {
		return nil, 0, err
	}

	attr, err := s.client.Attributes(ctx, objectKey)
	if err != nil {
		return nil, 0, errors.Wrapf(err, "failed to get attributes for %s", objectKey)
	}

	return reader, attr.Size, err
}

// List objects with given prefix.
func (s *SwiftThanosObjectClient) List(ctx context.Context, prefix, delimiter string) ([]client.StorageObject, []client.StorageCommonPrefix, error) {
	var storageObjects []client.StorageObject
	var commonPrefixes []client.StorageCommonPrefix
	var iterParams []objstore.IterOption

	// If delimiter is empty we want to list all files
	if delimiter == "" {
		iterParams = append(iterParams, objstore.WithRecursiveIter)
	}

	s.client.Iter(ctx, prefix, func(objectKey string) error {
		// CommonPrefixes are keys that have the prefix and have the delimiter
		// as a suffix
		if delimiter != "" && strings.HasSuffix(objectKey, delimiter) {
			commonPrefixes = append(commonPrefixes, client.StorageCommonPrefix(objectKey))
			return nil
		}
		attr, err := s.client.Attributes(ctx, objectKey)
		if err != nil {
			return errors.Wrapf(err, "failed to get attributes for %s", objectKey)
		}

		storageObjects = append(storageObjects, client.StorageObject{
			Key:        objectKey,
			ModifiedAt: attr.LastModified,
		})

		return nil

	}, iterParams...)

	return storageObjects, commonPrefixes, nil
}

// DeleteObject deletes the specified object key from the configured Swift bucket.
func (s *SwiftThanosObjectClient) DeleteObject(ctx context.Context, objectKey string) error {
	return s.client.Delete(ctx, objectKey)
}

// IsObjectNotFoundErr returns true if error means that object is not found. Relevant to GetObject and DeleteObject operations.
func (s *SwiftThanosObjectClient) IsObjectNotFoundErr(err error) bool {
	return s.client.IsObjNotFoundErr(err)
}

// IsRetryableErr returns true if the request failed due to some retryable server-side scenario
func (s *SwiftThanosObjectClient) IsRetryableErr(err error) bool { return false }
