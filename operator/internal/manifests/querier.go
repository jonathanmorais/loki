package manifests

import (
	"fmt"
	"path"

	"github.com/ViaQ/logerr/v2/kverrors"
	"github.com/grafana/loki/operator/internal/manifests/internal/config"
	"github.com/grafana/loki/operator/internal/manifests/storage"
	"github.com/imdario/mergo"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BuildQuerier returns a list of k8s objects for Loki Querier
func BuildQuerier(opts Options) ([]client.Object, error) {
	deployment := NewQuerierDeployment(opts)
	if opts.Gates.HTTPEncryption {
		if err := configureQuerierHTTPServicePKI(deployment, opts.Name); err != nil {
			return nil, err
		}
	}

	if err := storage.ConfigureDeployment(deployment, opts.ObjectStorage); err != nil {
		return nil, err
	}

	if opts.Gates.GRPCEncryption {
		if err := configureQuerierGRPCServicePKI(deployment, opts.Name, opts.Namespace); err != nil {
			return nil, err
		}
	}

	return []client.Object{
		deployment,
		NewQuerierGRPCService(opts),
		NewQuerierHTTPService(opts),
	}, nil
}

// NewQuerierDeployment creates a deployment object for a querier
func NewQuerierDeployment(opts Options) *appsv1.Deployment {
	podSpec := corev1.PodSpec{
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
				Name:  "loki-querier",
				Resources: corev1.ResourceRequirements{
					Limits:   opts.ResourceRequirements.Querier.Limits,
					Requests: opts.ResourceRequirements.Querier.Requests,
				},
				Args: []string{
					"-target=querier",
					fmt.Sprintf("-config.file=%s", path.Join(config.LokiConfigMountDir, config.LokiConfigFileName)),
					fmt.Sprintf("-runtime-config.file=%s", path.Join(config.LokiConfigMountDir, config.LokiRuntimeConfigFileName)),
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
				SecurityContext:          containerSecurityContext(),
			},
		},
		SecurityContext: podSecurityContext(opts.Gates.RuntimeSeccompProfile),
	}

	if opts.Stack.Template != nil && opts.Stack.Template.Querier != nil {
		podSpec.Tolerations = opts.Stack.Template.Querier.Tolerations
		podSpec.NodeSelector = opts.Stack.Template.Querier.NodeSelector
	}

	l := ComponentLabels(LabelQuerierComponent, opts.Name)
	a := commonAnnotations(opts.ConfigSHA1)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: appsv1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   QuerierName(opts.Name),
			Labels: l,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: pointer.Int32Ptr(opts.Stack.Template.Querier.Replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels.Merge(l, GossipLabels()),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:        fmt.Sprintf("loki-querier-%s", opts.Name),
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

// NewQuerierGRPCService creates a k8s service for the querier GRPC endpoint
func NewQuerierGRPCService(opts Options) *corev1.Service {
	serviceName := serviceNameQuerierGRPC(opts.Name)
	labels := ComponentLabels(LabelQuerierComponent, opts.Name)

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceName,
			Labels:      labels,
			Annotations: serviceAnnotations(serviceName, opts.Gates.OpenShift.ServingCertsService),
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
			Selector: labels,
		},
	}
}

// NewQuerierHTTPService creates a k8s service for the querier HTTP endpoint
func NewQuerierHTTPService(opts Options) *corev1.Service {
	serviceName := serviceNameQuerierHTTP(opts.Name)
	labels := ComponentLabels(LabelQuerierComponent, opts.Name)

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceName,
			Labels:      labels,
			Annotations: serviceAnnotations(serviceName, opts.Gates.OpenShift.ServingCertsService),
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
			Selector: labels,
		},
	}
}

func configureQuerierHTTPServicePKI(deployment *appsv1.Deployment, stackName string) error {
	serviceName := serviceNameQuerierHTTP(stackName)
	return configureHTTPServicePKI(&deployment.Spec.Template.Spec, serviceName)
}

func configureQuerierGRPCServicePKI(deployment *appsv1.Deployment, stackName, stackNS string) error {
	caBundleName := signingCABundleName(stackName)
	secretVolumeSpec := corev1.PodSpec{
		Volumes: []corev1.Volume{
			{
				Name: caBundleName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: caBundleName,
						},
					},
				},
			},
		},
	}

	secretContainerSpec := corev1.Container{
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      caBundleName,
				ReadOnly:  false,
				MountPath: caBundleDir,
			},
		},
		Args: []string{
			// Enable GRPC over TLS for ingester client
			"-ingester.client.tls-enabled=true",
			fmt.Sprintf("-ingester.client.tls-ca-path=%s", signingCAPath()),
			fmt.Sprintf("-ingester.client.tls-server-name=%s", fqdn(serviceNameIngesterGRPC(stackName), stackNS)),
			// Enable GRPC over TLS for query frontend client
			"-querier.frontend-client.tls-enabled=true",
			fmt.Sprintf("-querier.frontend-client.tls-ca-path=%s", signingCAPath()),
			fmt.Sprintf("-querier.frontend-client.tls-server-name=%s", fqdn(serviceNameQueryFrontendGRPC(stackName), stackNS)),
			// Enable GRPC over TLS for boltb-shipper index-gateway client
			"-boltdb.shipper.index-gateway-client.grpc.tls-enabled=true",
			fmt.Sprintf("-boltdb.shipper.index-gateway-client.grpc.tls-ca-path=%s", signingCAPath()),
			fmt.Sprintf("-boltdb.shipper.index-gateway-client.grpc.tls-server-name=%s", fqdn(serviceNameIndexGatewayGRPC(stackName), stackNS)),
		},
	}

	if err := mergo.Merge(&deployment.Spec.Template.Spec, secretVolumeSpec, mergo.WithAppendSlice); err != nil {
		return kverrors.Wrap(err, "failed to merge volumes")
	}

	if err := mergo.Merge(&deployment.Spec.Template.Spec.Containers[0], secretContainerSpec, mergo.WithAppendSlice); err != nil {
		return kverrors.Wrap(err, "failed to merge container")
	}

	serviceName := serviceNameQuerierGRPC(stackName)
	return configureGRPCServicePKI(&deployment.Spec.Template.Spec, serviceName)
}
