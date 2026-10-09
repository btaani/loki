package manifests

import (
	"fmt"
	"path"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/grafana/loki/operator/internal/manifests/internal/config"
)

// BuildQueryScheduler returns a list of k8s objects for the Loki QueryScheduler
func BuildQueryScheduler(opts Options) ([]client.Object, error) {
	deployment := NewQuerySchedulerDeployment(opts)
	if opts.Gates.HTTPEncryption {
		if err := configureQuerySchedulerHTTPServicePKI(deployment, opts); err != nil {
			return nil, err
		}
	}

	if opts.Gates.GRPCEncryption {
		if err := configureQuerySchedulerGRPCServicePKI(deployment, opts); err != nil {
			return nil, err
		}
	}

	if opts.Gates.HTTPEncryption || opts.Gates.GRPCEncryption {
		caBundleName := signingCABundleName(opts.Name)
		if err := configureServiceCA(&deployment.Spec.Template.Spec, caBundleName); err != nil {
			return nil, err
		}
	}

	if opts.Gates.RestrictedPodSecurityStandard {
		if err := configurePodSpecForRestrictedStandard(&deployment.Spec.Template.Spec); err != nil {
			return nil, err
		}
	}

	if err := configureHashRingEnv(&deployment.Spec.Template.Spec, opts); err != nil {
		return nil, err
	}

	if err := configureProxyEnv(&deployment.Spec.Template.Spec, opts); err != nil {
		return nil, err
	}

	if err := configureReplication(&deployment.Spec.Template, opts.Stack.Replication, LabelQuerySchedulerComponent, opts.Name); err != nil {
		return nil, err
	}

	return []client.Object{
		deployment,
		NewQuerySchedulerGRPCService(opts),
		NewQuerySchedulerHTTPService(opts),
		NewQuerySchedulerPodDisruptionBudget(opts),
	}, nil
}

// NewQuerySchedulerDeployment creates a deployment object for the query-scheduler
func NewQuerySchedulerDeployment(opts Options) *appsv1.Deployment {
	l := ComponentLabels(LabelQuerySchedulerComponent, opts.Name)
	a := commonAnnotations(opts)
	podSpec := corev1.PodSpec{
		ServiceAccountName: opts.Name,
		Affinity:           configureAffinity(LabelQuerySchedulerComponent, opts.Name, opts.Gates.DefaultNodeAffinity, opts.Stack.Template.QueryScheduler),
		Volumes: []corev1.Volume{
			{
				Name: configVolumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						DefaultMode: &defaultConfigMapMode,
						LocalObjectReference: corev1.LocalObjectReference{
							Name: lokiConfigMapName(opts.Name),
						},
					},
				},
			},
		},
		Containers: []corev1.Container{
			{
				Image: opts.Image,
				Name:  "loki-query-scheduler",
				Resources: corev1.ResourceRequirements{
					Limits:   opts.ResourceRequirements.QueryScheduler.Limits,
					Requests: opts.ResourceRequirements.QueryScheduler.Requests,
				},
				Args: []string{
					"-target=query-scheduler",
					fmt.Sprintf("-config.file=%s", path.Join(config.LokiConfigMountDir, config.LokiConfigFileName)),
					fmt.Sprintf("-runtime-config.file=%s", path.Join(config.LokiConfigMountDir, config.LokiRuntimeConfigFileName)),
					"-config.expand-env=true",
				},
				ReadinessProbe: lokiReadinessProbe(),
				LivenessProbe:  lokiLivenessProbe(),
				Ports: []corev1.ContainerPort{
					{
						Name:          lokiHTTPPortName,
						ContainerPort: httpPort,
						Protocol:      protocolTCP,
					},
					{
						Name:          lokiGRPCPortName,
						ContainerPort: grpcPort,
						Protocol:      protocolTCP,
					},
					{
						Name:          lokiGossipPortName,
						ContainerPort: gossipPort,
						Protocol:      protocolTCP,
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      configVolumeName,
						ReadOnly:  false,
						MountPath: config.LokiConfigMountDir,
					},
				},
				TerminationMessagePath:   "/dev/termination-log",
				TerminationMessagePolicy: "File",
				ImagePullPolicy:          "IfNotPresent",
			},
		},
	}

	if opts.Stack.Template != nil && opts.Stack.Template.QueryScheduler != nil {
		podSpec.Tolerations = opts.Stack.Template.QueryScheduler.Tolerations
		podSpec.NodeSelector = opts.Stack.Template.QueryScheduler.NodeSelector
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: appsv1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   QuerySchedulerName(opts.Name),
			Labels: l,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(opts.Stack.Template.QueryScheduler.Replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels.Merge(l, GossipLabels()),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:        fmt.Sprintf("loki-query-scheduler-%s", opts.Name),
					Labels:      labels.Merge(l, GossipLabels()),
					Annotations: a,
				},
				Spec: podSpec,
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
			},
		},
	}
}

// NewQuerySchedulerGRPCService creates a k8s headless service for the query-scheduler gRPC endpoint.
// The service is headless so that queriers and query-frontends can connect to individual pods
// via DNS-based discovery.
func NewQuerySchedulerGRPCService(opts Options) *corev1.Service {
	serviceName := serviceNameQuerySchedulerGRPC(opts.Name)
	l := ComponentLabels(LabelQuerySchedulerComponent, opts.Name)

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   serviceName,
			Labels: l,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Ports: []corev1.ServicePort{
				{
					Name:       lokiGRPCPortName,
					Port:       grpcPort,
					Protocol:   protocolTCP,
					TargetPort: intstr.IntOrString{IntVal: grpcPort},
				},
			},
			Selector: l,
		},
	}
}

// NewQuerySchedulerHTTPService creates a k8s service for the query-scheduler HTTP endpoint
func NewQuerySchedulerHTTPService(opts Options) *corev1.Service {
	serviceName := serviceNameQuerySchedulerHTTP(opts.Name)
	l := ComponentLabels(LabelQuerySchedulerComponent, opts.Name)

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   serviceName,
			Labels: l,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       lokiHTTPPortName,
					Port:       httpPort,
					Protocol:   protocolTCP,
					TargetPort: intstr.IntOrString{IntVal: httpPort},
				},
			},
			Selector: l,
		},
	}
}

// NewQuerySchedulerPodDisruptionBudget returns a PodDisruptionBudget for the LokiStack
// query-scheduler pods.
func NewQuerySchedulerPodDisruptionBudget(opts Options) *policyv1.PodDisruptionBudget {
	l := ComponentLabels(LabelQuerySchedulerComponent, opts.Name)
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PodDisruptionBudget",
			APIVersion: policyv1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Labels:    l,
			Name:      QuerySchedulerName(opts.Name),
			Namespace: opts.Namespace,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: l,
			},
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
		},
	}
}

func configureQuerySchedulerHTTPServicePKI(deployment *appsv1.Deployment, opts Options) error {
	serviceName := serviceNameQuerySchedulerHTTP(opts.Name)
	return configureHTTPServicePKI(&deployment.Spec.Template.Spec, serviceName)
}

func configureQuerySchedulerGRPCServicePKI(deployment *appsv1.Deployment, opts Options) error {
	serviceName := serviceNameQuerySchedulerGRPC(opts.Name)
	return configureGRPCServicePKI(&deployment.Spec.Template.Spec, serviceName)
}
