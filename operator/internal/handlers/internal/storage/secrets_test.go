package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lokiv1 "github.com/grafana/loki/operator/apis/loki/v1"
)

func TestHashSecretData(t *testing.T) {
	tt := []struct {
		desc     string
		data     map[string][]byte
		wantHash string
	}{
		{
			desc:     "nil",
			data:     nil,
			wantHash: "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		},
		{
			desc:     "empty",
			data:     map[string][]byte{},
			wantHash: "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		},
		{
			desc: "single entry",
			data: map[string][]byte{
				"key": []byte("value"),
			},
			wantHash: "a8973b2094d3af1e43931132dee228909bf2b02a",
		},
		{
			desc: "multiple entries",
			data: map[string][]byte{
				"key":  []byte("value"),
				"key3": []byte("value3"),
				"key2": []byte("value2"),
			},
			wantHash: "a3341093891ad4df9f07db586029be48e9e6e884",
		},
	}

	for _, tc := range tt {
		tc := tc

		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			s := &corev1.Secret{
				Data: tc.data,
			}

			hash, err := hashSecretData(s)
			require.NoError(t, err)
			require.Equal(t, tc.wantHash, hash)
		})
	}
}

func TestUnknownType(t *testing.T) {
	wantError := "unknown secret type: test-unknown-type"

	_, err := extractSecret(&corev1.Secret{}, "test-unknown-type")
	require.EqualError(t, err, wantError)
}

func TestAzureExtract(t *testing.T) {
	type test struct {
		name      string
		secret    *corev1.Secret
		wantError string
	}
	table := []test{
		{
			name:      "missing environment",
			secret:    &corev1.Secret{},
			wantError: "missing secret field: environment",
		},
		{
			name: "missing container",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"environment": []byte("here"),
				},
			},
			wantError: "missing secret field: container",
		},
		{
			name: "missing account_name",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"environment": []byte("here"),
					"container":   []byte("this,that"),
				},
			},
			wantError: "missing secret field: account_name",
		},
		{
			name: "missing account_key",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Data: map[string][]byte{
					"environment":  []byte("here"),
					"container":    []byte("this,that"),
					"account_name": []byte("id"),
				},
			},
			wantError: "missing secret field: account_key",
		},
		{
			name: "all set",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Data: map[string][]byte{
					"environment":  []byte("here"),
					"container":    []byte("this,that"),
					"account_name": []byte("id"),
					"account_key":  []byte("secret"),
				},
			},
		},
	}
	for _, tst := range table {
		tst := tst
		t.Run(tst.name, func(t *testing.T) {
			t.Parallel()

			opts, err := extractSecret(tst.secret, lokiv1.ObjectStorageSecretAzure)
			if tst.wantError == "" {
				require.NoError(t, err)
				require.NotEmpty(t, opts.SecretName)
				require.NotEmpty(t, opts.SecretSHA1)
				require.Equal(t, opts.SharedStore, lokiv1.ObjectStorageSecretAzure)
			} else {
				require.EqualError(t, err, tst.wantError)
			}
		})
	}
}

func TestGCSExtract(t *testing.T) {
	type test struct {
		name      string
		secret    *corev1.Secret
		wantError string
	}
	table := []test{
		{
			name:      "missing bucketname",
			secret:    &corev1.Secret{},
			wantError: "missing secret field: bucketname",
		},
		{
			name: "missing key.json",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"bucketname": []byte("here"),
				},
			},
			wantError: "missing secret field: key.json",
		},
		{
			name: "all set",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Data: map[string][]byte{
					"bucketname": []byte("here"),
					"key.json":   []byte("{\"type\": \"SA\"}"),
				},
			},
		},
	}
	for _, tst := range table {
		tst := tst
		t.Run(tst.name, func(t *testing.T) {
			t.Parallel()

			_, err := extractSecret(tst.secret, lokiv1.ObjectStorageSecretGCS)
			if tst.wantError == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tst.wantError)
			}
		})
	}
}

func TestS3Extract(t *testing.T) {
	type test struct {
		name      string
		secret    *corev1.Secret
		wantError string
	}
	table := []test{
		{
			name:      "missing bucketnames",
			secret:    &corev1.Secret{},
			wantError: "missing secret field: bucketnames",
		},
		{
			name: "missing endpoint",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"bucketnames": []byte("this,that"),
				},
			},
			wantError: "missing secret field: endpoint",
		},
		{
			name: "missing access_key_id",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"endpoint":    []byte("here"),
					"bucketnames": []byte("this,that"),
				},
			},
			wantError: "missing secret field: access_key_id",
		},
		{
			name: "missing access_key_secret",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"endpoint":      []byte("here"),
					"bucketnames":   []byte("this,that"),
					"access_key_id": []byte("id"),
				},
			},
			wantError: "missing secret field: access_key_secret",
		},
		{
			name: "all set",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Data: map[string][]byte{
					"endpoint":          []byte("here"),
					"bucketnames":       []byte("this,that"),
					"access_key_id":     []byte("id"),
					"access_key_secret": []byte("secret"),
				},
			},
		},
	}
	for _, tst := range table {
		tst := tst
		t.Run(tst.name, func(t *testing.T) {
			t.Parallel()

			opts, err := extractSecret(tst.secret, lokiv1.ObjectStorageSecretS3)
			if tst.wantError == "" {
				require.NoError(t, err)
				require.NotEmpty(t, opts.SecretName)
				require.NotEmpty(t, opts.SecretSHA1)
				require.Equal(t, opts.SharedStore, lokiv1.ObjectStorageSecretS3)
			} else {
				require.EqualError(t, err, tst.wantError)
			}
		})
	}
}

func TestSwiftExtract(t *testing.T) {
	type test struct {
		name      string
		secret    *corev1.Secret
		wantError string
	}
	table := []test{
		{
			name:      "missing auth_url",
			secret:    &corev1.Secret{},
			wantError: "missing secret field: auth_url",
		},
		{
			name: "missing username",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"auth_url": []byte("here"),
				},
			},
			wantError: "missing secret field: username",
		},
		{
			name: "missing user_domain_name",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"auth_url": []byte("here"),
					"username": []byte("this,that"),
				},
			},
			wantError: "missing secret field: user_domain_name",
		},
		{
			name: "missing user_domain_id",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"auth_url":         []byte("here"),
					"username":         []byte("this,that"),
					"user_domain_name": []byte("id"),
				},
			},
			wantError: "missing secret field: user_domain_id",
		},
		{
			name: "missing user_id",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"auth_url":         []byte("here"),
					"username":         []byte("this,that"),
					"user_domain_name": []byte("id"),
					"user_domain_id":   []byte("secret"),
				},
			},
			wantError: "missing secret field: user_id",
		},
		{
			name: "missing password",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"auth_url":         []byte("here"),
					"username":         []byte("this,that"),
					"user_domain_name": []byte("id"),
					"user_domain_id":   []byte("secret"),
					"user_id":          []byte("there"),
				},
			},
			wantError: "missing secret field: password",
		},
		{
			name: "missing domain_id",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"auth_url":         []byte("here"),
					"username":         []byte("this,that"),
					"user_domain_name": []byte("id"),
					"user_domain_id":   []byte("secret"),
					"user_id":          []byte("there"),
					"password":         []byte("cred"),
				},
			},
			wantError: "missing secret field: domain_id",
		},
		{
			name: "missing domain_name",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"auth_url":         []byte("here"),
					"username":         []byte("this,that"),
					"user_domain_name": []byte("id"),
					"user_domain_id":   []byte("secret"),
					"user_id":          []byte("there"),
					"password":         []byte("cred"),
					"domain_id":        []byte("text"),
				},
			},
			wantError: "missing secret field: domain_name",
		},
		{
			name: "missing container_name",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"auth_url":         []byte("here"),
					"username":         []byte("this,that"),
					"user_domain_name": []byte("id"),
					"user_domain_id":   []byte("secret"),
					"user_id":          []byte("there"),
					"password":         []byte("cred"),
					"domain_id":        []byte("text"),
					"domain_name":      []byte("where"),
				},
			},
			wantError: "missing secret field: container_name",
		},
		{
			name: "all set",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Data: map[string][]byte{
					"auth_url":         []byte("here"),
					"username":         []byte("this,that"),
					"user_domain_name": []byte("id"),
					"user_domain_id":   []byte("secret"),
					"user_id":          []byte("there"),
					"password":         []byte("cred"),
					"domain_id":        []byte("text"),
					"domain_name":      []byte("where"),
					"container_name":   []byte("then"),
				},
			},
		},
	}
	for _, tst := range table {
		tst := tst
		t.Run(tst.name, func(t *testing.T) {
			t.Parallel()

			opts, err := extractSecret(tst.secret, lokiv1.ObjectStorageSecretSwift)
			if tst.wantError == "" {
				require.NoError(t, err)
				require.NotEmpty(t, opts.SecretName)
				require.NotEmpty(t, opts.SecretSHA1)
				require.Equal(t, opts.SharedStore, lokiv1.ObjectStorageSecretSwift)
			} else {
				require.EqualError(t, err, tst.wantError)
			}
		})
	}
}
