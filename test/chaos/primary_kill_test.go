package chaos

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/howl-cloud/redis-operator/test/chaos/faults"
	"github.com/howl-cloud/redis-operator/test/chaos/invariants"
)

var _ = Describe("Primary kill during writes", Label("pod-kill", "failover"), func() {
	It("fails over, keeps data, and resumes writes", func() {
		ctx, cancel := scenarioContext()
		defer cancel()

		const prefix = "ac1"
		Expect(prepareBaseline(ctx, prefix, 1000)).To(Succeed())

		primaryPod, err := getPrimaryPod(ctx)
		Expect(err).NotTo(HaveOccurred())

		Expect(invariants.AssertReplicationConverged(ctx, testNamespace, primaryPod.Name, redisPassword, 1, 5000)).To(Succeed())
		offsetBefore, err := faults.ReplicationOffset(ctx, testNamespace, primaryPod.Name, redisPassword)
		Expect(err).NotTo(HaveOccurred())

		clientPod, err := ensureWorkloadClientPod(ctx)
		Expect(err).NotTo(HaveOccurred())

		workloadScript := fmt.Sprintf(`i=1
while [ $i -le 50000 ]; do
  redis-cli --no-auth-warning -h %s-leader SET inflight-%s:$i inflight-$i >/dev/null 2>&1 || true
  i=$((i+1))
done
`, clusterName, prefix)

		writer, err := startKubectlBackgroundCommand(
			ctx,
			"-n", testNamespace, "exec", clientPod.Name, "--",
			"env", "REDISCLI_AUTH="+redisPassword,
			"sh", "-ceu", workloadScript,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(writer.ensureRunning(1 * time.Second)).To(Succeed())

		Expect(faults.KillPod(ctx, k8sClient, testNamespace, primaryPod.Name)).To(Succeed())

		_, err = writer.wait()
		Expect(err).NotTo(HaveOccurred())

		newPrimaryName, err := faults.WaitForPrimaryChange(ctx, k8sClient, testNamespace, clusterName, primaryPod.Name, 3*time.Minute)
		Expect(err).NotTo(HaveOccurred())

		Expect(waitForClusterHealthy(ctx)).To(Succeed())
		Expect(faults.WaitForPodReady(ctx, k8sClient, testNamespace, primaryPod.Name, defaultPodReadyTimeout)).To(Succeed())
		Expect(faults.WaitForPodRole(ctx, k8sClient, testNamespace, primaryPod.Name, redisPassword, "slave", 3*time.Minute)).To(Succeed())

		Expect(invariants.AssertDataIntegrity(ctx, k8sClient, testNamespace, clusterName, redisPassword, prefix, 1000)).To(Succeed())

		offsetAfter, err := faults.ReplicationOffset(ctx, testNamespace, newPrimaryName, redisPassword)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertOffsetNotRegressed(offsetBefore, offsetAfter)).To(Succeed())

		writeResult, err := faults.ExecRedisCLI(ctx, testNamespace, newPrimaryName, redisPassword, "SET", prefix+":post-failover", "ok")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(writeResult)).To(Equal("OK"))

		Expect(invariants.AssertNoSplitBrain(ctx, k8sClient, testNamespace, clusterName, redisPassword)).To(Succeed())
	})
})
