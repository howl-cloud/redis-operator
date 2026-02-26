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

var _ = Describe("Network partition", Label("network", "failover"), func() {
	It("fences isolated primary and converges to a single primary", func() {
		ctx, cancel := scenarioContext()
		defer cancel()

		const prefix = "ac2"
		Expect(prepareBaseline(ctx, prefix, 1000)).To(Succeed())

		primaryPod, err := getPrimaryPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(faults.WaitForPodRole(ctx, k8sClient, testNamespace, primaryPod.Name, redisPassword, "master", 2*time.Minute)).To(Succeed())

		Expect(invariants.AssertReplicationConverged(ctx, testNamespace, primaryPod.Name, redisPassword, 1, 5000)).To(Succeed())
		offsetBefore, err := faults.ReplicationOffset(ctx, testNamespace, primaryPod.Name, redisPassword)
		Expect(err).NotTo(HaveOccurred())
		Expect(primaryPod.Status.PodIP).NotTo(BeEmpty())

		operatorPod, err := faults.GetOperatorPod(ctx, k8sClient, operatorNamespace, releaseName)
		Expect(err).NotTo(HaveOccurred())
		Expect(operatorPod.Status.PodIP).NotTo(BeEmpty())
		Expect(operatorPod.Spec.NodeName).NotTo(BeEmpty())

		blockerName, err := faults.ApplyNetworkBlock(ctx, k8sClient, faults.NetworkBlockConfig{
			Namespace: testNamespace,
			PodName:   fmt.Sprintf("%s-netblock", clusterName),
			HostNode:  operatorPod.Spec.NodeName,
			SourceIP:  operatorPod.Status.PodIP,
			TargetIP:  primaryPod.Status.PodIP,
			Image:     networkBlockerImage,
		})
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			Expect(faults.RemoveNetworkBlocker(context.Background(), k8sClient, testNamespace, blockerName)).To(Succeed())
		}()

		newPrimaryName, err := faults.WaitForPrimaryChange(ctx, k8sClient, testNamespace, clusterName, primaryPod.Name, 4*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		Expect(faults.WaitForPhase(ctx, k8sClient, testNamespace, clusterName, "Degraded", 2*time.Minute)).To(Succeed())

		Expect(ensureLeaderEndpointExcludes(ctx, primaryPod.Name)).To(Succeed())
		Expect(invariants.AssertPodNotWritable(ctx, testNamespace, primaryPod.Name, redisPassword, prefix+":isolated-write")).To(Succeed())

		Expect(invariants.AssertDataIntegrity(ctx, k8sClient, testNamespace, clusterName, redisPassword, prefix, 1000)).To(Succeed())
		offsetAfter, err := faults.ReplicationOffset(ctx, testNamespace, newPrimaryName, redisPassword)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertOffsetNotRegressed(offsetBefore, offsetAfter)).To(Succeed())

		Expect(faults.RemoveNetworkBlocker(ctx, k8sClient, testNamespace, blockerName)).To(Succeed())
		Expect(faults.WaitForPodReady(ctx, k8sClient, testNamespace, primaryPod.Name, defaultPodReadyTimeout)).To(Succeed())
		Expect(faults.WaitForPodRole(ctx, k8sClient, testNamespace, primaryPod.Name, redisPassword, "slave", 3*time.Minute)).To(Succeed())
		Expect(faults.WaitForPhase(ctx, k8sClient, testNamespace, clusterName, "Healthy", defaultClusterReadyTimeout)).To(Succeed())
		Expect(invariants.AssertNoSplitBrain(ctx, k8sClient, testNamespace, clusterName, redisPassword)).To(Succeed())
	})
})
