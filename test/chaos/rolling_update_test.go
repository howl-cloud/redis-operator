package chaos

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/howl-cloud/redis-operator/test/chaos/faults"
	"github.com/howl-cloud/redis-operator/test/chaos/invariants"
)

var _ = Describe("Rolling update under load", Label("rolling-update"), func() {
	It("keeps writes healthy while replicas and primary restart", func() {
		ctx, cancel := scenarioContext()
		defer cancel()

		const prefix = "ac6"
		Expect(prepareBaseline(ctx, prefix, 250)).To(Succeed())

		primaryPod, err := getPrimaryPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertReplicationConverged(ctx, testNamespace, primaryPod.Name, redisPassword, 1, 5000)).To(Succeed())
		offsetBefore, err := faults.ReplicationOffset(ctx, testNamespace, primaryPod.Name, redisPassword)
		Expect(err).NotTo(HaveOccurred())

		clientPod, err := ensureWorkloadClientPod(ctx)
		Expect(err).NotTo(HaveOccurred())

		benchmark, err := startKubectlBackgroundCommand(
			ctx,
			"-n", testNamespace, "exec", clientPod.Name, "--",
			"env", "REDISCLI_AUTH="+redisPassword,
			"redis-benchmark",
			"-h", clusterName+"-leader",
			"-t", "set",
			"-n", "500000",
			"-c", "10",
			"--csv",
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(benchmark.ensureRunning(1 * time.Second)).To(Succeed())

		_, err = faults.RunKubectl(
			ctx,
			"-n", testNamespace,
			"patch", "rediscluster/"+clusterName,
			"--type", "merge",
			"-p", `{"spec":{"resources":{"requests":{"cpu":"150m","memory":"128Mi"},"limits":{"cpu":"600m","memory":"256Mi"}}}}`,
		)
		Expect(err).NotTo(HaveOccurred())

		Expect(performManualRollingRestart(ctx, benchmark)).To(Succeed())

		benchmarkOutput, err := benchmark.wait()
		Expect(err).NotTo(HaveOccurred(), benchmarkOutput)
		Expect(benchmarkFailurePattern.MatchString(benchmarkOutput)).To(BeFalse(), benchmarkOutput)

		Expect(waitForClusterHealthy(ctx)).To(Succeed())

		currentPrimary, err := getPrimaryPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		offsetAfter, err := faults.ReplicationOffset(ctx, testNamespace, currentPrimary.Name, redisPassword)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertOffsetNotRegressed(offsetBefore, offsetAfter)).To(Succeed())

		Expect(invariants.AssertDataIntegrity(ctx, k8sClient, testNamespace, clusterName, redisPassword, prefix, 250)).To(Succeed())
		Expect(invariants.AssertNoSplitBrain(ctx, k8sClient, testNamespace, clusterName, redisPassword)).To(Succeed())
	})
})
