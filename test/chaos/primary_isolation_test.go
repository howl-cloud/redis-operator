package chaos

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/howl-cloud/redis-operator/test/chaos/faults"
	"github.com/howl-cloud/redis-operator/test/chaos/invariants"
)

var _ = Describe("Primary isolation runtime guard", Label("network", "isolation"), func() {
	It("forces primary restart and converges without split-brain", func() {
		ctx, cancel := scenarioContext()
		defer cancel()

		const prefix = "ac-primary-isolation"
		Expect(prepareBaseline(ctx, prefix, 1000)).To(Succeed())

		primaryPod, err := getPrimaryPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(faults.WaitForPodRole(ctx, k8sClient, testNamespace, primaryPod.Name, redisPassword, "master", 2*time.Minute)).To(Succeed())
		Expect(primaryPod.Status.PodIP).NotTo(BeEmpty())
		Expect(primaryPod.Spec.NodeName).NotTo(BeEmpty())

		Expect(invariants.AssertReplicationConverged(ctx, testNamespace, primaryPod.Name, redisPassword, 1, 5000)).To(Succeed())
		offsetBefore, err := faults.ReplicationOffset(ctx, testNamespace, primaryPod.Name, redisPassword)
		Expect(err).NotTo(HaveOccurred())
		restartBefore, err := faults.PodRestartCount(ctx, k8sClient, testNamespace, primaryPod.Name)
		Expect(err).NotTo(HaveOccurred())

		apiServerIP, err := getKubernetesServiceIP(ctx)
		Expect(err).NotTo(HaveOccurred())
		peerIPs, err := getPeerIPsExcluding(ctx, primaryPod.Name)
		Expect(err).NotTo(HaveOccurred())
		Expect(peerIPs).NotTo(BeEmpty())

		blockerName, err := faults.ApplyPrimaryIsolation(ctx, k8sClient, faults.PrimaryIsolationConfig{
			Namespace:   testNamespace,
			PodName:     fmt.Sprintf("%s-primary-isolation-netblock", clusterName),
			HostNode:    primaryPod.Spec.NodeName,
			SourceIP:    primaryPod.Status.PodIP,
			APIServerIP: apiServerIP,
			PeerIPs:     peerIPs,
			Image:       networkBlockerImage,
		})
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			Expect(faults.RemoveNetworkBlocker(context.Background(), k8sClient, testNamespace, blockerName)).To(Succeed())
		}()

		Expect(faults.WaitForRestartCountIncrease(ctx, k8sClient, testNamespace, primaryPod.Name, restartBefore, 4*time.Minute)).To(Succeed())

		newPrimaryName, err := faults.WaitForPrimaryChange(ctx, k8sClient, testNamespace, clusterName, primaryPod.Name, 4*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		Expect(faults.WaitForPhase(ctx, k8sClient, testNamespace, clusterName, "Healthy", defaultClusterReadyTimeout)).To(Succeed())

		Expect(faults.RemoveNetworkBlocker(ctx, k8sClient, testNamespace, blockerName)).To(Succeed())
		Expect(faults.WaitForPodReady(ctx, k8sClient, testNamespace, primaryPod.Name, defaultPodReadyTimeout)).To(Succeed())
		Expect(faults.WaitForPodRole(ctx, k8sClient, testNamespace, primaryPod.Name, redisPassword, "slave", 3*time.Minute)).To(Succeed())

		Expect(invariants.AssertDataIntegrity(ctx, k8sClient, testNamespace, clusterName, redisPassword, prefix, 1000)).To(Succeed())
		offsetAfter, err := faults.ReplicationOffset(ctx, testNamespace, newPrimaryName, redisPassword)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertOffsetNotRegressed(offsetBefore, offsetAfter)).To(Succeed())
		Expect(invariants.AssertNoSplitBrain(ctx, k8sClient, testNamespace, clusterName, redisPassword)).To(Succeed())
	})
})
