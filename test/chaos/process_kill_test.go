package chaos

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/howl-cloud/redis-operator/test/chaos/faults"
	"github.com/howl-cloud/redis-operator/test/chaos/invariants"
)

var _ = Describe("Redis process kill", Label("pod-kill", "oom"), func() {
	It("recovers from redis-server crash without losing consistency", func() {
		ctx, cancel := scenarioContext()
		defer cancel()

		const prefix = "ac5"
		Expect(prepareBaseline(ctx, prefix, 300)).To(Succeed())

		primaryPod, err := getPrimaryPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertReplicationConverged(ctx, testNamespace, primaryPod.Name, redisPassword, 1, 5000)).To(Succeed())

		offsetBefore, err := faults.ReplicationOffset(ctx, testNamespace, primaryPod.Name, redisPassword)
		Expect(err).NotTo(HaveOccurred())
		restartBefore, err := faults.PodRestartCount(ctx, k8sClient, testNamespace, primaryPod.Name)
		Expect(err).NotTo(HaveOccurred())

		_, err = faults.ExecInPod(ctx, testNamespace, primaryPod.Name, "sh", "-ceu", `kill -9 "$(pgrep redis-server)"`)
		Expect(err).NotTo(HaveOccurred())

		Expect(faults.WaitForRestartCountIncrease(ctx, k8sClient, testNamespace, primaryPod.Name, restartBefore, 3*time.Minute)).To(Succeed())
		Expect(faults.WaitForPodReady(ctx, k8sClient, testNamespace, primaryPod.Name, defaultPodReadyTimeout)).To(Succeed())
		Expect(faults.WaitForPhase(ctx, k8sClient, testNamespace, clusterName, "Healthy", defaultClusterReadyTimeout)).To(Succeed())

		currentPrimary, err := getPrimaryPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		offsetAfter, err := faults.ReplicationOffset(ctx, testNamespace, currentPrimary.Name, redisPassword)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertOffsetNotRegressed(offsetBefore, offsetAfter)).To(Succeed())

		Expect(invariants.AssertDataIntegrity(ctx, k8sClient, testNamespace, clusterName, redisPassword, prefix, 300)).To(Succeed())
		Expect(invariants.AssertNoSplitBrain(ctx, k8sClient, testNamespace, clusterName, redisPassword)).To(Succeed())
	})
})
