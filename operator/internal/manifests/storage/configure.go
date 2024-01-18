package storage

import (
	"fmt"
	"path"

	"github.com/ViaQ/logerr/v2/kverrors"
	"github.com/imdario/mergo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/pointer"

	lokiv1 "github.com/grafana/loki/operator/apis/loki/v1"
)

// ConfigureDeployment appends additional pod volumes and container env vars, args, volume mounts
// based on the object storage type. Currently supported amendments:
// - All: Ensure object storage secret mounted and auth projected as env vars.
// - GCS: Ensure env var GOOGLE_APPLICATION_CREDENTIALS in container
// - S3 & Swift: Ensure mounting custom CA configmap if any TLSConfig given
func ConfigureDeployment(d *appsv1.Deployment, opts Options) error {
	switch opts.SharedStore {
	case lokiv1.ObjectStorageSecretAlibabaCloud, lokiv1.ObjectStorageSecretAzure, lokiv1.ObjectStorageSecretGCS:
		return configureDeployment(d, opts)
	case lokiv1.ObjectStorageSecretS3:
		err := configureDeployment(d, opts)
		if err != nil {
			return err
		}
		return configureDeploymentCA(d, opts.TLS, lokiv1.ObjectStorageSecretS3)
	case lokiv1.ObjectStorageSecretSwift:
		err := configureDeployment(d, opts)
		if err != nil {
			return err
		}
		return configureDeploymentCA(d, opts.TLS, lokiv1.ObjectStorageSecretSwift)
	default:
		return nil
	}
}

// ConfigureStatefulSet appends additional pod volumes and container env vars, args, volume mounts
// based on the object storage type. Currently supported amendments:
// - All: Ensure object storage secret mounted and auth projected as env vars.
// - GCS: Ensure env var GOOGLE_APPLICATION_CREDENTIALS in container
// - S3 & Swift: Ensure mounting custom CA configmap if any TLSConfig given
func ConfigureStatefulSet(d *appsv1.StatefulSet, opts Options) error {
	switch opts.SharedStore {
	case lokiv1.ObjectStorageSecretAlibabaCloud, lokiv1.ObjectStorageSecretAzure, lokiv1.ObjectStorageSecretGCS:
		return configureStatefulSet(d, opts)
	case lokiv1.ObjectStorageSecretS3:
		if err := configureStatefulSet(d, opts); err != nil {
			return err
		}
		return configureStatefulSetCA(d, opts.TLS, lokiv1.ObjectStorageSecretS3)
	case lokiv1.ObjectStorageSecretSwift:
		if err := configureStatefulSet(d, opts); err != nil {
			return err
		}
		return configureStatefulSetCA(d, opts.TLS, lokiv1.ObjectStorageSecretSwift)
	default:
		return nil
	}
}

// ConfigureDeployment merges the object storage secret volume into the deployment spec.
// With this, the deployment will expose credentials specific environment variables.
func configureDeployment(d *appsv1.Deployment, opts Options) error {
	p := ensureObjectStoreCredentials(&d.Spec.Template.Spec, opts)

	if err := mergo.Merge(&d.Spec.Template.Spec, p, mergo.WithOverride); err != nil {
		return kverrors.Wrap(err, "failed to merge gcs object storage spec ")
	}

	return nil
}

// ConfigureDeploymentCA merges a S3 or Swift CA ConfigMap volume into the deployment spec.
func configureDeploymentCA(d *appsv1.Deployment, tls *TLSConfig, secretType lokiv1.ObjectStorageSecretType) error {
	if tls == nil {
		return nil
	}

	var p corev1.PodSpec
	switch secretType {
	case lokiv1.ObjectStorageSecretS3:
		p = ensureCAForObjectStorage(&d.Spec.Template.Spec, tls, lokiv1.ObjectStorageSecretS3)
	case lokiv1.ObjectStorageSecretSwift:
		p = ensureCAForObjectStorage(&d.Spec.Template.Spec, tls, lokiv1.ObjectStorageSecretSwift)
	}

	if err := mergo.Merge(&d.Spec.Template.Spec, p, mergo.WithOverride); err != nil {
		return kverrors.Wrap(err, "failed to merge object storage ca options ")
	}

	return nil
}

// ConfigureStatefulSet merges a the object storage secrect volume into the statefulset spec.
// With this, the statefulset will expose credentials specific environment variable.
func configureStatefulSet(s *appsv1.StatefulSet, opts Options) error {
	p := ensureObjectStoreCredentials(&s.Spec.Template.Spec, opts)

	if err := mergo.Merge(&s.Spec.Template.Spec, p, mergo.WithOverride); err != nil {
		return kverrors.Wrap(err, "failed to merge gcs object storage spec ")
	}

	return nil
}

// ConfigureStatefulSetCA merges a S3 or Swift CA ConfigMap volume into the statefulset spec.
func configureStatefulSetCA(s *appsv1.StatefulSet, tls *TLSConfig, secretType lokiv1.ObjectStorageSecretType) error {
	if tls == nil {
		return nil
	}
	var p corev1.PodSpec

	switch secretType {
	case lokiv1.ObjectStorageSecretS3:
		p = ensureCAForObjectStorage(&s.Spec.Template.Spec, tls, lokiv1.ObjectStorageSecretS3)
	case lokiv1.ObjectStorageSecretSwift:
		p = ensureCAForObjectStorage(&s.Spec.Template.Spec, tls, lokiv1.ObjectStorageSecretSwift)
	}

	if err := mergo.Merge(&s.Spec.Template.Spec, p, mergo.WithOverride); err != nil {
		return kverrors.Wrap(err, "failed to merge object storage ca options ")
	}

	return nil
}

func ensureObjectStoreCredentials(p *corev1.PodSpec, opts Options) corev1.PodSpec {
	container := p.Containers[0].DeepCopy()
	volumes := p.Volumes
	secretName := opts.SecretName

	volumes = append(volumes, corev1.Volume{
		Name: secretName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: secretName,
			},
		},
	})

	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      secretName,
		ReadOnly:  false,
		MountPath: secretDirectory,
	})

	if managedAuthEnabled(opts) {
		setSATokenPath(&opts)
		container.Env = append(container.Env, managedAuthCredentials(opts)...)
		volumes = append(volumes, saTokenVolume(opts))
		container.VolumeMounts = append(container.VolumeMounts, saTokenVolumeMount(opts))
	} else {
		container.Env = append(container.Env, staticAuthCredentials(opts)...)
	}
	container.Env = append(container.Env, serverSideEncryption(opts)...)

	return corev1.PodSpec{
		Containers: []corev1.Container{
			*container,
		},
		Volumes: volumes,
	}
}

func staticAuthCredentials(opts Options) []corev1.EnvVar {
	secretName := opts.SecretName
	switch opts.SharedStore {
	case lokiv1.ObjectStorageSecretAlibabaCloud:
		return []corev1.EnvVar{
			envVarFromSecret(EnvAlibabaCloudAccessKeyID, secretName, KeyAlibabaCloudAccessKeyID),
			envVarFromSecret(EnvAlibabaCloudAccessKeySecret, secretName, KeyAlibabaCloudSecretAccessKey),
		}
	case lokiv1.ObjectStorageSecretAzure:
		return []corev1.EnvVar{
			envVarFromSecret(EnvAzureStorageAccountName, secretName, KeyAzureStorageAccountName),
			envVarFromSecret(EnvAzureStorageAccountKey, secretName, KeyAzureStorageAccountKey),
		}
	case lokiv1.ObjectStorageSecretGCS:
		return []corev1.EnvVar{
			envVarFromValue(EnvGoogleApplicationCredentials, path.Join(secretDirectory, KeyGCPServiceAccountKeyFilename)),
		}
	case lokiv1.ObjectStorageSecretS3:
		return []corev1.EnvVar{
			envVarFromSecret(EnvAWSAccessKeyID, secretName, KeyAWSAccessKeyID),
			envVarFromSecret(EnvAWSAccessKeySecret, secretName, KeyAWSAccessKeySecret),
		}
	case lokiv1.ObjectStorageSecretSwift:
		return []corev1.EnvVar{
			envVarFromSecret(EnvSwiftUsername, secretName, KeySwiftUsername),
			envVarFromSecret(EnvSwiftPassword, secretName, KeySwiftPassword),
		}
	default:
		return []corev1.EnvVar{}
	}
}

func managedAuthCredentials(opts Options) []corev1.EnvVar {
	switch opts.SharedStore {
	case lokiv1.ObjectStorageSecretS3:
		return []corev1.EnvVar{
			envVarFromSecret(EnvAWSRoleArn, opts.SecretName, KeyAWSRoleArn),
			envVarFromValue(EnvAWSWebIdentityTokenFile, path.Join(opts.S3.WebIdentityTokenFile, "token")),
		}
	default:
		return []corev1.EnvVar{}
	}
}

func serverSideEncryption(opts Options) []corev1.EnvVar {
	secretName := opts.SecretName
	switch opts.SharedStore {
	case lokiv1.ObjectStorageSecretS3:
		if opts.S3 != nil && opts.S3.SSE.Type == SSEKMSType && opts.S3.SSE.KMSEncryptionContext != "" {
			return []corev1.EnvVar{
				envVarFromSecret(EnvAWSSseKmsEncryptionContext, secretName, KeyAWSSseKmsEncryptionContext),
			}
		}
		return []corev1.EnvVar{}
	default:
		return []corev1.EnvVar{}
	}
}

func ensureCAForObjectStorage(p *corev1.PodSpec, tls *TLSConfig, secretType lokiv1.ObjectStorageSecretType) corev1.PodSpec {
	container := p.Containers[0].DeepCopy()
	volumes := p.Volumes

	volumes = append(volumes, corev1.Volume{
		Name: storageTLSVolume,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: tls.CA,
				},
			},
		},
	})

	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      storageTLSVolume,
		ReadOnly:  false,
		MountPath: caDirectory,
	})

	switch secretType {
	case lokiv1.ObjectStorageSecretS3:
		container.Args = append(container.Args,
			fmt.Sprintf("-s3.http.ca-file=%s", path.Join(caDirectory, tls.Key)),
		)
	case lokiv1.ObjectStorageSecretSwift:
		container.Args = append(container.Args,
			fmt.Sprintf("-swift.http.ca-file=%s", path.Join(caDirectory, tls.Key)),
		)
	}

	return corev1.PodSpec{
		Containers: []corev1.Container{
			*container,
		},
		Volumes: volumes,
	}
}

func envVarFromSecret(name, secretName, secretKey string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: secretName,
				},
				Key: secretKey,
			},
		},
	}
}

func envVarFromValue(name, value string) corev1.EnvVar {
	return corev1.EnvVar{
		Name:  name,
		Value: value,
	}
}

func managedAuthEnabled(opts Options) bool {
	switch opts.SharedStore {
	case lokiv1.ObjectStorageSecretS3:
		return opts.S3 != nil && opts.S3.STS
	default:
		return false
	}
}

func setSATokenPath(opts *Options) {
	switch opts.SharedStore {
	case lokiv1.ObjectStorageSecretS3:
		opts.S3.WebIdentityTokenFile = saTokenVolumeK8sDirectory
		if opts.OpenShiftEnabled {
			opts.S3.WebIdentityTokenFile = saTokenVolumeOcpDirectory
		}
	}
}

func saTokenVolumeMount(opts Options) corev1.VolumeMount {
	var tokenPath string
	switch opts.SharedStore {
	case lokiv1.ObjectStorageSecretS3:
		tokenPath = opts.S3.WebIdentityTokenFile
	}
	return corev1.VolumeMount{
		Name:      saTokenVolumeName,
		MountPath: tokenPath,
	}
}

func saTokenVolume(opts Options) corev1.Volume {
	var audience string
	storeType := opts.SharedStore
	switch storeType {
	case lokiv1.ObjectStorageSecretS3:
		audience = awsDefaultAudience
		if opts.S3.Audience != "" {
			audience = opts.S3.Audience
		}
		if opts.OpenShiftEnabled {
			audience = awsOpenShiftAudience
		}
	}
	return corev1.Volume{
		Name: saTokenVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							ExpirationSeconds: pointer.Int64(saTokenExpiration),
							Path:              corev1.ServiceAccountTokenKey,
							Audience:          audience,
						},
					},
				},
			},
		},
	}
}
