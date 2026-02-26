package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const (
	instanceManagerBinaryPath = "/controller/manager"
	redisDataVolumeName       = "data"
	redisDataMountPath        = "/data"
	controllerVolumeName      = "controller"
	controllerMountPath       = "/controller"
	projectedVolumeName       = "projected-secrets"
	projectedMountPath        = "/projected"
	tlsVolumeName             = "tls-certs"
	tlsMountPath              = "/tls"
	restoreDataInitName       = "restore-data"
	backupCredsVolumeName     = "backup-credentials"
	backupCredsMountPath      = "/backup-credentials"
	specHashAnnotation        = "redis.io/spec-hash"
	defaultIsolationTimeout   = 5 * time.Second
	livenessTimeoutBuffer     = 2 * time.Second
)

// reconcilePods ensures pods match the desired state: scale up, scale down, rolling updates.
func (r *ClusterReconciler) reconcilePods(ctx context.Context, cluster *redisv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	existingPods, err := r.listDataPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}

	desired := int(cluster.Spec.Instances)
	current := len(existingPods)

	// Ensure desired ordinals exist.
	if current < desired {
		logger.Info("Scaling up", "current", current, "desired", desired)
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ScaleUp", "Scaling up from %d to %d instances", current, desired)
	}
	for i := 0; i < desired; i++ {
		podName := podNameForIndex(cluster.Name, i)
		role := redisv1.LabelRoleReplica
		if podName == cluster.Status.CurrentPrimary || (cluster.Status.CurrentPrimary == "" && i == 0) {
			role = redisv1.LabelRolePrimary
		}
		if err := r.createPod(ctx, cluster, podName, i, role); err != nil {
			return fmt.Errorf("creating pod %s: %w", podName, err)
		}
	}

	// Scale down: delete excess pods (highest ordinal first, never the primary).
	if current > desired {
		logger.Info("Scaling down", "current", current, "desired", desired)
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ScaleDown", "Scaling down from %d to %d instances", current, desired)
		sort.Slice(existingPods, func(i, j int) bool {
			return existingPods[i].Name > existingPods[j].Name // Descending.
		})
		for i := 0; i < current-desired; i++ {
			pod := existingPods[i]
			if pod.Name == cluster.Status.CurrentPrimary {
				logger.Info("Skipping primary during scale-down", "pod", pod.Name)
				continue
			}
			if err := r.Delete(ctx, &pod); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("deleting pod %s: %w", pod.Name, err)
			}
			r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "PodDeleted", "Deleted pod %s during scale-down", pod.Name)
		}
	}

	// Set initial primary if not yet set.
	if cluster.Status.CurrentPrimary == "" && desired > 0 {
		primaryName := podNameForIndex(cluster.Name, 0)
		patch := client.MergeFrom(cluster.DeepCopy())
		cluster.Status.CurrentPrimary = primaryName
		cluster.Status.Phase = redisv1.ClusterPhaseCreating
		if err := r.Status().Patch(ctx, cluster, patch); err != nil {
			return fmt.Errorf("setting initial primary: %w", err)
		}
	}

	return nil
}

// reconcileSentinelPods ensures sentinel pods match the fixed desired count.
func (r *ClusterReconciler) reconcileSentinelPods(ctx context.Context, cluster *redisv1.RedisCluster) error {
	if cluster.Spec.Mode != redisv1.ClusterModeSentinel {
		return nil
	}
	if cluster.Status.CurrentPrimary == "" {
		return nil
	}

	logger := log.FromContext(ctx)
	existingPods, err := r.listSentinelPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing sentinel pods: %w", err)
	}

	desired := redisv1.SentinelInstances
	current := len(existingPods)

	if current < desired {
		logger.Info("Scaling up sentinel pods", "current", current, "desired", desired)
	}
	for i := 0; i < desired; i++ {
		podName := sentinelPodNameForIndex(cluster.Name, i)
		if err := r.createSentinelPod(ctx, cluster, podName); err != nil {
			return fmt.Errorf("creating sentinel pod %s: %w", podName, err)
		}
	}

	if current > desired {
		logger.Info("Scaling down sentinel pods", "current", current, "desired", desired)
		sort.Slice(existingPods, func(i, j int) bool {
			return existingPods[i].Name > existingPods[j].Name
		})
		for i := 0; i < current-desired; i++ {
			pod := existingPods[i]
			if err := r.Delete(ctx, &pod); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("deleting sentinel pod %s: %w", pod.Name, err)
			}
		}
	}

	return nil
}

// createPod creates a single Redis pod.
func (r *ClusterReconciler) createPod(ctx context.Context, cluster *redisv1.RedisCluster, podName string, index int, role string) error {
	desiredHash := r.computeSpecHash(cluster)
	labels := podLabels(cluster.Name, podName, role)
	logger := log.FromContext(ctx)

	var existing corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{
		Name: podName, Namespace: cluster.Namespace,
	}, &existing); err == nil {
		currentHash := getPodSpecHash(&existing)
		if currentHash != desiredHash {
			logger.V(1).Info(
				"Pod spec hash differs from desired state; waiting for rolling update",
				"pod", podName,
				"currentHash", currentHash,
				"desiredHash", desiredHash,
			)
		}

		patch := client.MergeFrom(existing.DeepCopy())
		changed := false
		if existing.Labels == nil {
			existing.Labels = make(map[string]string)
			changed = true
		}
		for key, value := range labels {
			if existing.Labels[key] != value {
				existing.Labels[key] = value
				changed = true
			}
		}
		if changed {
			if err := r.Patch(ctx, &existing, patch); err != nil {
				return fmt.Errorf("patching pod %s labels: %w", podName, err)
			}
		}
		return nil
	}

	var projectedSources []corev1.VolumeProjection
	secretRefs := []*redisv1.LocalObjectReference{
		cluster.Spec.AuthSecret,
		cluster.Spec.ACLConfigSecret,
	}
	for _, ref := range secretRefs {
		if ref != nil {
			projectedSources = append(projectedSources, corev1.VolumeProjection{
				Secret: &corev1.SecretProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
				},
			})
		}
	}

	initContainers := []corev1.Container{
		{
			Name:            "copy-manager",
			Image:           r.OperatorImage,
			Command:         []string{"/manager", "copy-binary", instanceManagerBinaryPath},
			SecurityContext: redisContainerSecurityContext(false),
			VolumeMounts: []corev1.VolumeMount{
				{Name: controllerVolumeName, MountPath: controllerMountPath},
			},
		},
	}

	backupCredsSecretName := ""
	if cluster.Spec.BackupCredentialsSecret != nil {
		backupCredsSecretName = cluster.Spec.BackupCredentialsSecret.Name
	}

	if shouldRestoreFromBackup(cluster, podName, index) {
		backup, err := r.getBootstrapBackup(ctx, cluster)
		if err != nil {
			return fmt.Errorf("getting bootstrap backup %q: %w", cluster.Spec.Bootstrap.BackupName, err)
		}

		if backup.Status.Phase != redisv1.BackupPhaseCompleted {
			return fmt.Errorf("bootstrap backup %s/%s is not completed (phase=%s)", backup.Namespace, backup.Name, backup.Status.Phase)
		}
		if backup.Spec.Destination == nil || backup.Spec.Destination.S3 == nil {
			return fmt.Errorf("bootstrap backup %s/%s has no S3 destination", backup.Namespace, backup.Name)
		}
		if backup.Status.BackupPath == "" {
			return fmt.Errorf("bootstrap backup %s/%s has empty status.backupPath", backup.Namespace, backup.Name)
		}
		backupMethod := backup.Spec.Method
		if backupMethod == "" {
			backupMethod = redisv1.BackupMethodRDB
		}
		if backupMethod != redisv1.BackupMethodRDB && backupMethod != redisv1.BackupMethodAOF {
			return fmt.Errorf("bootstrap backup %s/%s uses unsupported method %q",
				backup.Namespace,
				backup.Name,
				backupMethod,
			)
		}

		restoreInit := corev1.Container{
			Name:            restoreDataInitName,
			Image:           r.OperatorImage,
			Command:         []string{"/manager", "restore"},
			SecurityContext: redisContainerSecurityContext(false),
			Args: []string{
				fmt.Sprintf("--cluster-name=%s", cluster.Name),
				fmt.Sprintf("--backup-name=%s", backup.Name),
				fmt.Sprintf("--backup-namespace=%s", backup.Namespace),
				fmt.Sprintf("--data-dir=%s", redisDataMountPath),
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: redisDataVolumeName, MountPath: redisDataMountPath},
			},
		}

		if backupCredsSecretName != "" {
			restoreInit.VolumeMounts = append(restoreInit.VolumeMounts, corev1.VolumeMount{
				Name:      backupCredsVolumeName,
				MountPath: backupCredsMountPath,
				ReadOnly:  true,
			})
		}

		initContainers = append(initContainers, restoreInit)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: cluster.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				specHashAnnotation: desiredHash,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:        serviceAccountName(cluster.Name),
			NodeSelector:              cluster.Spec.NodeSelector,
			Affinity:                  cluster.Spec.Affinity,
			Tolerations:               cluster.Spec.Tolerations,
			TopologySpreadConstraints: cluster.Spec.TopologySpreadConstraints,
			SecurityContext:           redisPodSecurityContext(),
			InitContainers:            initContainers,
			Containers: []corev1.Container{
				{
					Name:    "redis",
					Image:   cluster.Spec.ImageName,
					Command: []string{instanceManagerBinaryPath, "instance"},
					Args: []string{
						fmt.Sprintf("--cluster-name=%s", cluster.Name),
						fmt.Sprintf("--pod-name=%s", podName),
						fmt.Sprintf("--pod-namespace=%s", cluster.Namespace),
					},
					Ports: []corev1.ContainerPort{
						{Name: "redis", ContainerPort: 6379, Protocol: corev1.ProtocolTCP},
						{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
					},
					Resources:       cluster.Spec.Resources,
					SecurityContext: redisContainerSecurityContext(false),
					VolumeMounts: []corev1.VolumeMount{
						{Name: redisDataVolumeName, MountPath: redisDataMountPath},
						{Name: controllerVolumeName, MountPath: controllerMountPath},
					},
					Env: []corev1.EnvVar{
						{Name: "POD_NAME", Value: podName},
						{Name: "CLUSTER_NAME", Value: cluster.Name},
						{Name: "POD_NAMESPACE", Value: cluster.Namespace},
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/healthz",
								Port: intOrString(8080),
							},
						},
						InitialDelaySeconds: 10,
						PeriodSeconds:       10,
						FailureThreshold:    3,
						TimeoutSeconds:      livenessTimeoutSeconds(cluster),
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/readyz",
								Port: intOrString(8080),
							},
						},
						InitialDelaySeconds: 5,
						PeriodSeconds:       5,
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: redisDataVolumeName,
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcNameForIndex(cluster.Name, index),
						},
					},
				},
				{
					Name: controllerVolumeName,
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}

	if len(projectedSources) > 0 {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: projectedVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: projectedSources,
				},
			},
		})
		pod.Spec.Containers[0].VolumeMounts = append(
			pod.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: projectedVolumeName, MountPath: projectedMountPath},
		)
	}

	if isTLSEnabled(cluster) {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{
							Secret: &corev1.SecretProjection{
								LocalObjectReference: corev1.LocalObjectReference{Name: cluster.Spec.TLSSecret.Name},
								Items: []corev1.KeyToPath{
									{Key: "tls.crt", Path: "tls.crt"},
									{Key: "tls.key", Path: "tls.key"},
								},
							},
						},
						{
							Secret: &corev1.SecretProjection{
								LocalObjectReference: corev1.LocalObjectReference{Name: cluster.Spec.CASecret.Name},
								Items: []corev1.KeyToPath{
									{Key: "ca.crt", Path: "ca.crt"},
								},
							},
						},
					},
				},
			},
		})
		pod.Spec.Containers[0].VolumeMounts = append(
			pod.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: tlsVolumeName, MountPath: tlsMountPath, ReadOnly: true},
		)
	}

	if backupCredsSecretName != "" {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: backupCredsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{
							Secret: &corev1.SecretProjection{
								LocalObjectReference: corev1.LocalObjectReference{Name: backupCredsSecretName},
							},
						},
					},
				},
			},
		})
		pod.Spec.Containers[0].VolumeMounts = append(
			pod.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: backupCredsVolumeName, MountPath: backupCredsMountPath, ReadOnly: true},
		)
	}

	if err := r.Create(ctx, pod); err != nil {
		return fmt.Errorf("creating pod: %w", err)
	}

	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "PodCreated", "Created pod %s with role %s", podName, role)
	return nil
}

func shouldRestoreFromBackup(cluster *redisv1.RedisCluster, podName string, index int) bool {
	if cluster.Spec.Bootstrap == nil || cluster.Spec.Bootstrap.BackupName == "" {
		return false
	}
	// Restore is a one-time bootstrap action for the very first primary only.
	return cluster.Status.CurrentPrimary == "" && index == 0 && podName == podNameForIndex(cluster.Name, 0)
}

func (r *ClusterReconciler) getBootstrapBackup(ctx context.Context, cluster *redisv1.RedisCluster) (*redisv1.RedisBackup, error) {
	var backup redisv1.RedisBackup
	if cluster.Spec.Bootstrap == nil || cluster.Spec.Bootstrap.BackupName == "" {
		return nil, fmt.Errorf("spec.bootstrap.backupName is required")
	}

	if err := r.Get(ctx, types.NamespacedName{
		Name:      cluster.Spec.Bootstrap.BackupName,
		Namespace: cluster.Namespace,
	}, &backup); err != nil {
		return nil, err
	}

	return &backup, nil
}

// createSentinelPod creates a single sentinel pod.
func (r *ClusterReconciler) createSentinelPod(ctx context.Context, cluster *redisv1.RedisCluster, podName string) error {
	labels := podLabels(cluster.Name, podName, redisv1.LabelRoleSentinel)

	var existing corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{
		Name: podName, Namespace: cluster.Namespace,
	}, &existing); err == nil {
		if cluster.Spec.AuthSecret != nil && !sentinelPodHasAuthProjection(existing, cluster.Spec.AuthSecret.Name) {
			if err := r.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("deleting sentinel pod %s for auth projection update: %w", podName, err)
			}
			return nil
		}

		patch := client.MergeFrom(existing.DeepCopy())
		changed := false
		if existing.Labels == nil {
			existing.Labels = make(map[string]string)
			changed = true
		}
		for key, value := range labels {
			if existing.Labels[key] != value {
				existing.Labels[key] = value
				changed = true
			}
		}
		if changed {
			if err := r.Patch(ctx, &existing, patch); err != nil {
				return fmt.Errorf("patching sentinel pod %s labels: %w", podName, err)
			}
		}
		return nil
	}
	var projectedSources []corev1.VolumeProjection
	if cluster.Spec.AuthSecret != nil {
		projectedSources = append(projectedSources, corev1.VolumeProjection{
			Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: cluster.Spec.AuthSecret.Name},
			},
		})
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:        serviceAccountName(cluster.Name),
			NodeSelector:              cluster.Spec.NodeSelector,
			Affinity:                  cluster.Spec.Affinity,
			Tolerations:               cluster.Spec.Tolerations,
			TopologySpreadConstraints: cluster.Spec.TopologySpreadConstraints,
			SecurityContext:           redisPodSecurityContext(),
			InitContainers: []corev1.Container{
				{
					Name:            "copy-manager",
					Image:           r.OperatorImage,
					Command:         []string{"/manager", "copy-binary", instanceManagerBinaryPath},
					SecurityContext: redisContainerSecurityContext(false),
					VolumeMounts: []corev1.VolumeMount{
						{Name: controllerVolumeName, MountPath: controllerMountPath},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:    "sentinel",
					Image:   cluster.Spec.ImageName,
					Command: []string{instanceManagerBinaryPath, "instance"},
					Args: []string{
						fmt.Sprintf("--cluster-name=%s", cluster.Name),
						fmt.Sprintf("--pod-name=%s", podName),
						fmt.Sprintf("--pod-namespace=%s", cluster.Namespace),
						"--role=sentinel",
					},
					Ports: []corev1.ContainerPort{
						{Name: "sentinel", ContainerPort: redisv1.SentinelPort, Protocol: corev1.ProtocolTCP},
						{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
					},
					Resources:       cluster.Spec.Resources,
					SecurityContext: redisContainerSecurityContext(false),
					VolumeMounts: []corev1.VolumeMount{
						{Name: redisDataVolumeName, MountPath: redisDataMountPath},
						{Name: controllerVolumeName, MountPath: controllerMountPath},
					},
					Env: []corev1.EnvVar{
						{Name: "POD_NAME", Value: podName},
						{Name: "CLUSTER_NAME", Value: cluster.Name},
						{Name: "POD_NAMESPACE", Value: cluster.Namespace},
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/healthz",
								Port: intOrString(8080),
							},
						},
						InitialDelaySeconds: 10,
						PeriodSeconds:       10,
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/readyz",
								Port: intOrString(8080),
							},
						},
						InitialDelaySeconds: 5,
						PeriodSeconds:       5,
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: redisDataVolumeName,
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: controllerVolumeName,
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}

	if len(projectedSources) > 0 {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: projectedVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: projectedSources,
				},
			},
		})
		pod.Spec.Containers[0].VolumeMounts = append(
			pod.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: projectedVolumeName, MountPath: projectedMountPath},
		)
	}

	if err := r.Create(ctx, pod); err != nil {
		return fmt.Errorf("creating sentinel pod: %w", err)
	}

	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "SentinelPodCreated", "Created sentinel pod %s", podName)
	return nil
}

func (r *ClusterReconciler) listClusterPods(ctx context.Context, cluster *redisv1.RedisCluster) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		redisv1.LabelCluster: cluster.Name,
	}); err != nil {
		return nil, err
	}
	return podList.Items, nil
}

func (r *ClusterReconciler) listDataPods(ctx context.Context, cluster *redisv1.RedisCluster) ([]corev1.Pod, error) {
	allPods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return nil, err
	}

	var dataPods []corev1.Pod
	for _, pod := range allPods {
		if pod.Labels[redisv1.LabelRole] == redisv1.LabelRoleSentinel {
			continue
		}
		dataPods = append(dataPods, pod)
	}
	return dataPods, nil
}

func (r *ClusterReconciler) listSentinelPods(ctx context.Context, cluster *redisv1.RedisCluster) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		redisv1.LabelCluster: cluster.Name,
		redisv1.LabelRole:    redisv1.LabelRoleSentinel,
	}); err != nil {
		return nil, err
	}
	return podList.Items, nil
}

func sentinelPodHasAuthProjection(pod corev1.Pod, secretName string) bool {
	for _, vol := range pod.Spec.Volumes {
		if vol.Name != projectedVolumeName || vol.Projected == nil {
			continue
		}
		for _, source := range vol.Projected.Sources {
			if source.Secret != nil && source.Secret.Name == secretName {
				return true
			}
		}
	}
	return false
}

func podNameForIndex(clusterName string, index int) string {
	return fmt.Sprintf("%s-%d", clusterName, index)
}

func sentinelPodNameForIndex(clusterName string, index int) string {
	return fmt.Sprintf("%s-sentinel-%d", clusterName, index)
}

func pvcNameForIndex(clusterName string, index int) string {
	return fmt.Sprintf("%s-data-%d", clusterName, index)
}

func getPodSpecHash(pod *corev1.Pod) string {
	if pod.Annotations != nil {
		if hash, ok := pod.Annotations[specHashAnnotation]; ok {
			return hash
		}
	}
	// Backward compatibility for any pre-fix pods that stored the hash as a label.
	if pod.Labels != nil {
		if hash, ok := pod.Labels[specHashAnnotation]; ok {
			return hash
		}
	}
	return ""
}

func (r *ClusterReconciler) computeSpecHash(cluster *redisv1.RedisCluster) string {
	var builder strings.Builder

	builder.WriteString("redisImage=")
	builder.WriteString(cluster.Spec.ImageName)
	builder.WriteString("\n")

	builder.WriteString("operatorImage=")
	builder.WriteString(r.OperatorImage)
	builder.WriteString("\n")

	appendResourceListHash(&builder, "resources.limits", cluster.Spec.Resources.Limits)
	appendResourceListHash(&builder, "resources.requests", cluster.Spec.Resources.Requests)

	if cluster.Spec.AuthSecret != nil {
		builder.WriteString("authSecret=")
		builder.WriteString(cluster.Spec.AuthSecret.Name)
		builder.WriteString("\n")
	}
	if cluster.Spec.ACLConfigSecret != nil {
		builder.WriteString("aclConfigSecret=")
		builder.WriteString(cluster.Spec.ACLConfigSecret.Name)
		builder.WriteString("\n")
	}
	if cluster.Spec.TLSSecret != nil {
		builder.WriteString("tlsSecret=")
		builder.WriteString(cluster.Spec.TLSSecret.Name)
		builder.WriteString("\n")
	}
	if cluster.Spec.CASecret != nil {
		builder.WriteString("caSecret=")
		builder.WriteString(cluster.Spec.CASecret.Name)
		builder.WriteString("\n")
	}

	redisConfigKeys := make([]string, 0, len(cluster.Spec.Redis))
	for key := range cluster.Spec.Redis {
		redisConfigKeys = append(redisConfigKeys, key)
	}
	sort.Strings(redisConfigKeys)
	for _, key := range redisConfigKeys {
		builder.WriteString("redis.")
		builder.WriteString(key)
		builder.WriteString("=")
		builder.WriteString(cluster.Spec.Redis[key])
		builder.WriteString("\n")
	}

	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func appendResourceListHash(builder *strings.Builder, prefix string, resources corev1.ResourceList) {
	keys := make([]string, 0, len(resources))
	for key := range resources {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)

	for _, key := range keys {
		resourceName := corev1.ResourceName(key)
		quantity := resources[resourceName]
		builder.WriteString(prefix)
		builder.WriteString(".")
		builder.WriteString(key)
		builder.WriteString("=")
		builder.WriteString(quantity.String())
		builder.WriteString("\n")
	}
}

func podLabels(clusterName, podName, role string) map[string]string {
	workload := redisv1.LabelWorkloadData
	if role == redisv1.LabelRoleSentinel {
		workload = redisv1.LabelWorkloadSentinel
	}

	return map[string]string{
		redisv1.LabelCluster:  clusterName,
		redisv1.LabelInstance: podName,
		redisv1.LabelRole:     role,
		redisv1.LabelWorkload: workload,
	}
}

func serviceAccountName(clusterName string) string {
	return clusterName
}

func isTLSEnabled(cluster *redisv1.RedisCluster) bool {
	return cluster.Spec.TLSSecret != nil && cluster.Spec.CASecret != nil
}

func intOrString(port int) intstr.IntOrString {
	return intstr.FromInt32(int32(port))
}

func livenessTimeoutSeconds(cluster *redisv1.RedisCluster) int32 {
	apiServerTimeout := defaultIsolationTimeout
	peerTimeout := defaultIsolationTimeout

	if cluster.Spec.PrimaryIsolation != nil {
		if cfg := cluster.Spec.PrimaryIsolation.APIServerTimeout; cfg != nil && cfg.Duration > 0 {
			apiServerTimeout = cfg.Duration
		}
		if cfg := cluster.Spec.PrimaryIsolation.PeerTimeout; cfg != nil && cfg.Duration > 0 {
			peerTimeout = cfg.Duration
		}
	}

	total := apiServerTimeout + peerTimeout + livenessTimeoutBuffer
	seconds := int32(math.Ceil(total.Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}

func redisPodSecurityContext() *corev1.PodSecurityContext {
	runAsNonRoot := true
	redisUID := int64(999)
	redisGID := int64(999)
	return &corev1.PodSecurityContext{
		RunAsNonRoot: &runAsNonRoot,
		RunAsUser:    &redisUID,
		RunAsGroup:   &redisGID,
		FSGroup:      &redisGID,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func redisContainerSecurityContext(readOnlyRootFilesystem bool) *corev1.SecurityContext {
	runAsNonRoot := true
	allowPrivilegeEscalation := false
	return &corev1.SecurityContext{
		RunAsNonRoot:             &runAsNonRoot,
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
		ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// reconcileConfigMap ensures the ConfigMap with redis.conf template exists.
func (r *ClusterReconciler) reconcileConfigMap(ctx context.Context, cluster *redisv1.RedisCluster) error {
	cmName := fmt.Sprintf("%s-config", cluster.Name)
	var existing corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{
		Name: cmName, Namespace: cluster.Namespace,
	}, &existing); err == nil {
		return nil
	}

	// Build redis.conf content from spec.
	conf := "port 6379\nbind 0.0.0.0\nappendonly yes\naof-use-rdb-preamble yes\nappenddirname appendonlydir\n"
	for key, val := range cluster.Spec.Redis {
		conf += fmt.Sprintf("%s %s\n", key, val)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				redisv1.LabelCluster: cluster.Name,
			},
		},
		Data: map[string]string{
			"redis.conf": conf,
		},
	}

	return r.Create(ctx, cm)
}

func podIndex(clusterName, podName string) int {
	suffix := podName[len(clusterName)+1:]
	idx, _ := strconv.Atoi(suffix)
	return idx
}
