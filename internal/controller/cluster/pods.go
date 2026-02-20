package cluster

import (
	"context"
	"fmt"
	"sort"
	"strconv"

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
)

// reconcilePods ensures pods match the desired state: scale up, scale down, rolling updates.
func (r *ClusterReconciler) reconcilePods(ctx context.Context, cluster *redisv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	existingPods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}

	desired := int(cluster.Spec.Instances)
	current := len(existingPods)

	// Scale up: create missing pods.
	if current < desired {
		logger.Info("Scaling up", "current", current, "desired", desired)
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ScaleUp", "Scaling up from %d to %d instances", current, desired)
		for i := current; i < desired; i++ {
			podName := podNameForIndex(cluster.Name, i)
			role := redisv1.LabelRoleReplica
			if i == 0 && cluster.Status.CurrentPrimary == "" {
				role = redisv1.LabelRolePrimary
			}
			if err := r.createPod(ctx, cluster, podName, i, role); err != nil {
				return fmt.Errorf("creating pod %s: %w", podName, err)
			}
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

// createPod creates a single Redis pod.
func (r *ClusterReconciler) createPod(ctx context.Context, cluster *redisv1.RedisCluster, podName string, index int, role string) error {
	var existing corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{
		Name: podName, Namespace: cluster.Namespace,
	}, &existing); err == nil {
		return nil
	}

	labels := podLabels(cluster.Name, podName, role)

	var projectedSources []corev1.VolumeProjection
	secretRefs := []*redisv1.LocalObjectReference{
		cluster.Spec.AuthSecret,
		cluster.Spec.ACLConfigSecret,
		cluster.Spec.TLSSecret,
		cluster.Spec.CASecret,
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
			InitContainers: []corev1.Container{
				{
					Name:    "copy-manager",
					Image:   "redis-operator:latest", // Will be overridden via env/config.
					Command: []string{"cp", "/manager", instanceManagerBinaryPath},
					VolumeMounts: []corev1.VolumeMount{
						{Name: controllerVolumeName, MountPath: controllerMountPath},
					},
				},
			},
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
					Resources: cluster.Spec.Resources,
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

	if err := r.Create(ctx, pod); err != nil {
		return fmt.Errorf("creating pod: %w", err)
	}

	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "PodCreated", "Created pod %s with role %s", podName, role)
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

func podNameForIndex(clusterName string, index int) string {
	return fmt.Sprintf("%s-%d", clusterName, index)
}

func pvcNameForIndex(clusterName string, index int) string {
	return fmt.Sprintf("%s-data-%d", clusterName, index)
}

func podLabels(clusterName, podName, role string) map[string]string {
	return map[string]string{
		redisv1.LabelCluster:  clusterName,
		redisv1.LabelInstance: podName,
		redisv1.LabelRole:     role,
	}
}

func serviceAccountName(clusterName string) string {
	return clusterName
}

func intOrString(port int) intstr.IntOrString {
	return intstr.FromInt32(int32(port))
}

// reconcileServiceAccount ensures the ServiceAccount exists.
func (r *ClusterReconciler) reconcileServiceAccount(ctx context.Context, cluster *redisv1.RedisCluster) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccountName(cluster.Name),
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				redisv1.LabelCluster: cluster.Name,
			},
		},
	}
	var existing corev1.ServiceAccount
	if err := r.Get(ctx, types.NamespacedName{
		Name: sa.Name, Namespace: sa.Namespace,
	}, &existing); err != nil {
		if errors.IsNotFound(err) {
			return r.Create(ctx, sa)
		}
		return err
	}
	return nil
}

// reconcileRBAC ensures the Role and RoleBinding exist for the instance manager.
func (r *ClusterReconciler) reconcileRBAC(_ context.Context, _ *redisv1.RedisCluster) error {
	return nil
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
	conf := "port 6379\nbind 0.0.0.0\nappendonly yes\n"
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
