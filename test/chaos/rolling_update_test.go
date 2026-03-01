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

var _ = Describe("Rolling update under load", Label("rolling-update"), func() {
	It("keeps writes healthy while replicas and primary restart", func() {
		ctx, cancel := scenarioContext()
		defer cancel()

		const prefix = "ac6"
		Expect(prepareBaseline(ctx, prefix, 250)).To(Succeed())

		primaryPod, err := getPrimaryPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(invariants.AssertReplicationConverged(ctx, testNamespace, primaryPod.Name, redisPassword, 1, 5000)).To(Succeed())

		clientPod, err := ensureWorkloadClientPod(ctx)
		Expect(err).NotTo(HaveOccurred())

		workloadScript := fmt.Sprintf(`i=1
while true; do
  redis-cli --no-auth-warning -h %s-leader SET rolling:ac6:$i value-$i >/dev/null 2>&1 || true
  i=$((i+1))
done
`, clusterName)

		workloadCtx, stopWorkload := context.WithCancel(ctx)
		workload, err := startKubectlBackgroundCommand(
			workloadCtx,
			"-n", testNamespace, "exec", clientPod.Name, "--",
			"env", "REDISCLI_AUTH="+redisPassword,
			"sh", "-ceu", workloadScript,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(workload.ensureRunning(1 * time.Second)).To(Succeed())

		_, err = faults.RunKubectl(
			ctx,
			"-n", testNamespace,
			"patch", "rediscluster/"+clusterName,
			"--type", "merge",
			"-p", `{"spec":{"resources":{"requests":{"cpu":"150m","memory":"128Mi"},"limits":{"cpu":"600m","memory":"256Mi"}}}}`,
		)
		Expect(err).NotTo(HaveOccurred())

		Expect(performManualRollingRestart(ctx, workload)).To(Succeed())
		stopWorkload()

		select {
		case <-workload.done:
		case <-time.After(10 * time.Second):
			Fail("timed out waiting for rolling-update workload command to stop")
		}

		Expect(waitForClusterHealthy(ctx)).To(Succeed())

		Expect(invariants.AssertDataIntegrity(ctx, k8sClient, testNamespace, clusterName, redisPassword, prefix, 250)).To(Succeed())
		Expect(invariants.AssertNoSplitBrain(ctx, k8sClient, testNamespace, clusterName, redisPassword)).To(Succeed())
	})
})
