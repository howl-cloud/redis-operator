package chaos

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/howl-cloud/redis-operator/test/chaos/faults"
	"github.com/howl-cloud/redis-operator/test/chaos/invariants"
)

var _ = Describe("Operator restart during failover", Label("operator", "failover"), func() {
	It("reconciles to a healthy state after controller restart", func() {
		ctx, cancel := scenarioContext()
		defer cancel()

		const prefix = "ac3"
		Expect(prepareBaseline(ctx, prefix, 500)).To(Succeed())

		primaryPod, err := getPrimaryPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertReplicationConverged(ctx, testNamespace, primaryPod.Name, redisPassword, 1, 5000)).To(Succeed())

		offsetBefore, err := faults.ReplicationOffset(ctx, testNamespace, primaryPod.Name, redisPassword)
		Expect(err).NotTo(HaveOccurred())

		Expect(faults.KillPod(ctx, k8sClient, testNamespace, primaryPod.Name)).To(Succeed())
		Expect(faults.WaitForFailoverSignal(ctx, k8sClient, testNamespace, clusterName, primaryPod.Name, 3*time.Minute)).To(Succeed())

		Expect(faults.RestartOperator(ctx, k8sClient, operatorNamespace, releaseName, 4*time.Minute)).To(Succeed())

		newPrimaryName, err := faults.WaitForPrimaryChange(ctx, k8sClient, testNamespace, clusterName, primaryPod.Name, 4*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		Expect(waitForClusterHealthy(ctx)).To(Succeed())

		Expect(faults.WaitForPodReady(ctx, k8sClient, testNamespace, primaryPod.Name, defaultPodReadyTimeout)).To(Succeed())
		Expect(faults.WaitForPodRole(ctx, k8sClient, testNamespace, primaryPod.Name, redisPassword, "slave", 3*time.Minute)).To(Succeed())

		Expect(invariants.AssertDataIntegrity(ctx, k8sClient, testNamespace, clusterName, redisPassword, prefix, 500)).To(Succeed())
		offsetAfter, err := faults.ReplicationOffset(ctx, testNamespace, newPrimaryName, redisPassword)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertOffsetNotRegressed(offsetBefore, offsetAfter)).To(Succeed())
		Expect(invariants.AssertNoSplitBrain(ctx, k8sClient, testNamespace, clusterName, redisPassword)).To(Succeed())
	})
})
