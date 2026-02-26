package chaos

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/chaos/faults"
	"github.com/howl-cloud/redis-operator/test/chaos/invariants"
)

var _ = Describe("Simultaneous replica failures", Label("pod-kill", "replica"), func() {
	It("keeps primary writable and rejoins replicas", func() {
		ctx, cancel := scenarioContext()
		defer cancel()

		const prefix = "ac4"
		Expect(prepareBaseline(ctx, prefix, 200)).To(Succeed())

		primaryPod, err := getPrimaryPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertReplicationConverged(ctx, testNamespace, primaryPod.Name, redisPassword, 1, 5000)).To(Succeed())

		offsetBefore, err := faults.ReplicationOffset(ctx, testNamespace, primaryPod.Name, redisPassword)
		Expect(err).NotTo(HaveOccurred())

		replicas, err := faults.GetReplicaPods(ctx, k8sClient, testNamespace, clusterName)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(replicas)).To(BeNumerically(">=", 2))
		for _, replica := range replicas {
			Expect(faults.KillPod(ctx, k8sClient, testNamespace, replica.Name)).To(Succeed())
		}

		setResult, err := faults.ExecRedisCLI(ctx, testNamespace, primaryPod.Name, redisPassword, "SET", prefix+":after-kill", "yes")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(setResult)).To(Equal("OK"))

		getResult, err := faults.ExecRedisCLI(ctx, testNamespace, primaryPod.Name, redisPassword, "GET", prefix+":after-kill")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(getResult)).To(Equal("yes"))

		Expect(faults.WaitForPhase(ctx, k8sClient, testNamespace, clusterName, string(redisv1.ClusterPhaseHealthy), defaultClusterReadyTimeout)).To(Succeed())
		Expect(waitForClusterHealthy(ctx)).To(Succeed())

		Expect(invariants.AssertDataIntegrity(ctx, k8sClient, testNamespace, clusterName, redisPassword, prefix, 200)).To(Succeed())

		currentPrimary, err := getPrimaryPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		offsetAfter, err := faults.ReplicationOffset(ctx, testNamespace, currentPrimary.Name, redisPassword)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertOffsetNotRegressed(offsetBefore, offsetAfter)).To(Succeed())
		Expect(invariants.AssertNoSplitBrain(ctx, k8sClient, testNamespace, clusterName, redisPassword)).To(Succeed())
	})
})
