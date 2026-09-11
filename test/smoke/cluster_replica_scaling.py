#!/usr/bin/env python3
"""Cluster replica scaling. Requires --kubeconfig to a kind cluster; creates its own namespace."""

import argparse
import json
import subprocess
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kubeconfig", required=True)
    parser.add_argument("--operator-namespace", default="redis-operator-system")
    parser.add_argument("--operator-deployment", default="redis-operator")
    parser.add_argument("--handover-only", action="store_true", help="run only planned and emergency fencing scenarios")
    args = parser.parse_args()
    namespace = f"replica-scaling-{int(time.time())}"
    base = ["kubectl", "--kubeconfig", args.kubeconfig]
    name = "scale"
    planned_fences = set()

    def kubectl(*cmd, data=None):
        result = subprocess.run(base + list(cmd), input=data, text=True,
                                capture_output=True, timeout=60, check=True)
        return result.stdout.strip()

    def obj(kind, object_name):
        return json.loads(kubectl("-n", namespace, "get", kind, object_name, "-o", "json"))

    def redis(pod, *command):
        return kubectl("-n", namespace, "exec", f"{name}-{pod}", "-c", "redis", "--",
                       "sh", "-c", 'exec redis-cli --no-auth-warning -a "$(cat /projected/scale-auth/password)" "$@"',
                       "sh", *command)

    def wait(description, check, timeout=240):
        deadline = time.monotonic() + timeout
        last = ""
        while time.monotonic() < deadline:
            try:
                if check():
                    print(description, flush=True)
                    return
            except (AssertionError, KeyError, subprocess.CalledProcessError) as exc:
                last = str(exc)
            time.sleep(2)
        raise AssertionError(f"Timed out: {description}. {last}")

    def topology(replicas):
        cluster = obj("rediscluster", name)
        status = cluster.get("status", {})
        marker = cluster["metadata"].get("annotations", {}).get("redis.io/cluster-handover")
        if marker:
            owner, _ = marker.split("/")
            pod = obj("pod", owner)
            ready = any(c["type"] == "Ready" and c["status"] == "True" for c in pod.get("status", {}).get("conditions", []))
            if not ready:
                assert redis(owner.rsplit("-", 1)[1], "PING") == "PONG"
                assert all(c["restartCount"] == 0 for c in pod["status"].get("containerStatuses", []))
                if owner not in planned_fences:
                    print(f"planned fence: {owner} unready, Redis alive without restarts", flush=True)
                    planned_fences.add(owner)

        expected = 3 * (replicas + 1)
        pods = json.loads(kubectl("-n", namespace, "get", "pods", "-l", "redis.io/cluster=scale", "-o", "json"))["items"]
        assert {p["metadata"]["name"] for p in pods} == {f"{name}-{i}" for i in range(expected)}
        assert all(any(c["type"] == "Ready" and c["status"] == "True" for c in p.get("status", {}).get("conditions", [])) for p in pods)
        assert status["clusterState"] == "ok" and status["slotsAssigned"] == 16384
        assert len(status["shards"]) == 3
        assert not cluster["metadata"].get("annotations", {}).get("redis.io/fencedInstances")
        assert not cluster["metadata"].get("annotations", {}).get("redis.io/cluster-handover")
        nodes = {line.split()[0]: line.split() for line in redis(0, "CLUSTER", "NODES").splitlines()}
        pod_map = {p["metadata"]["name"]: p for p in pods}
        for shard, members in status["shards"].items():
            primary = members["primaryPod"]
            assert len(members.get("replicaPods", [])) == replicas
            primary_id = status["instancesStatus"][primary]["nodeID"]
            assert "master" in nodes[primary_id][2].split(",")
            for pod, role in [(primary, "primary")] + [(p, "replica") for p in members.get("replicaPods", [])]:
                labels = pod_map[pod]["metadata"]["labels"]
                assert labels["redis.io/shard"] == shard and labels["redis.io/shard-role"] == role
                assert labels["redis.io/role"] == role
                state = status["instancesStatus"][pod]
                assert state["connected"]
                if role == "replica":
                    assert state["primaryNodeID"] == primary_id and state["masterLinkStatus"] == "up"
                    assert nodes[state["nodeID"]][3] == primary_id
        return True

    def keys(write=False):
        operation = 'set "check:$i" "value-$i"' if write else 'get "check:$i"'
        output = kubectl("-n", namespace, "exec", "scale-0", "-c", "redis", "--", "sh", "-c",
                         'for i in $(seq 1 30); do redis-cli -c --no-auth-warning -a "$(cat /projected/scale-auth/password)" '
                         + operation + '; done')
        expected = ["OK"] * 30 if write else [f"value-{i}" for i in range(1, 31)]
        assert output.splitlines() == expected, output

    def scale(replicas):
        kubectl("-n", namespace, "patch", "rediscluster", name, "--type=merge", "-p",
                json.dumps({"spec": {"replicasPerShard": replicas}}))
        wait(f"{replicas} replicas per shard: topology, labels, readiness agree", lambda: topology(replicas))
        keys()

    def operator(replicas):
        kubectl("-n", args.operator_namespace, "scale", "deployment", args.operator_deployment, f"--replicas={replicas}")
        if replicas == 0:
            wait("operator paused for legacy topology setup", lambda: not json.loads(kubectl("-n", args.operator_namespace, "get", "pods", "-l", "app.kubernetes.io/name=redis-operator", "-o", "json"))["items"])
        else:
            kubectl("-n", args.operator_namespace, "rollout", "status", "deployment/" + args.operator_deployment, "--timeout=55s")

    def promote(pod):
        assert redis(pod, "CLUSTER", "FAILOVER") == "OK"
        wait(f"pod {pod} promoted by Redis", lambda: redis(pod, "ROLE").splitlines()[0] == "master")

    def follow(pod, primary):
        node_id = redis(primary, "CLUSTER", "MYID")
        assert redis(pod, "CLUSTER", "REPLICATE", node_id) == "OK"
        wait(f"pod {pod} follows pod {primary}", lambda: "master_link_status:up" in redis(pod, "INFO", "replication"))

    if not kubectl("config", "current-context").startswith("kind-"):
        raise ValueError("This test requires a kind kubeconfig")
    kubectl("create", "namespace", namespace)
    try:
        manifest = {"apiVersion": "redis.io/v1", "kind": "RedisCluster", "metadata": {"name": name, "namespace": namespace},
                    "spec": {"mode": "cluster", "shards": 3, "replicasPerShard": 0,
                             "storage": {"size": "128Mi"},
                             "redis": {"cluster-allow-replica-migration": "no"}}}
        kubectl("apply", "-f", "-", data=json.dumps(manifest))
        wait("three primaries bootstrapped", lambda: topology(0))
        keys(write=True)
        if args.handover_only:
            scale(1)
            promote(3)
            wait("operator follows Redis failover", lambda: topology(1))
            scale(0)
        else:
            for replicas in [1, 2, 3, 2]:
                scale(replicas)
            promote(6)
            wait("operator follows Redis failover", lambda: topology(2))
            scale(1)
            scale(0)
            scale(2)
            operator(0)
            # Restore primaries 0,1,2 before creating the pre-PR contiguous layout.
            for pod in [0, 1, 2]:
                if redis(pod, "ROLE").splitlines()[0] != "master":
                    promote(pod)
            follow(3, 1)
            promote(3)
            follow(6, 2)
            promote(6)
            for pod, primary in [(1, 0), (2, 0), (4, 3), (5, 3), (7, 6), (8, 6)]:
                follow(pod, primary)
            operator(1)
            wait("legacy contiguous topology observed", lambda: topology(2))
            scale(1)
            scale(0)
        assert planned_fences, "No planned handover fence was observed"
        scale(1)
        prior = sum(c["restartCount"] for c in obj("pod", "scale-3")["status"]["containerStatuses"])
        kubectl("-n", namespace, "patch", "rediscluster", name, "--type=merge", "-p",
                json.dumps({"metadata": {"annotations": {"redis.io/fencedInstances": '["scale-3"]'}}}))
        wait("emergency fence stops Redis", lambda: sum(c["restartCount"] for c in obj("pod", "scale-3")["status"]["containerStatuses"]) > prior)
        kubectl("-n", namespace, "patch", "rediscluster", name, "--type=merge", "-p",
                json.dumps({"metadata": {"annotations": {"redis.io/fencedInstances": None}}}))
        wait("replica recovers after emergency fence removal", lambda: topology(1))
        keys()
        print("PASS: scaling, planned handover, and emergency fencing preserved all 30 keys", flush=True)
    except Exception:
        for command in [("-n", namespace, "get", "rediscluster", name, "-o", "json"),
                        ("-n", args.operator_namespace, "logs", "deployment/" + args.operator_deployment, "--tail=60")]:
            try:
                print(kubectl(*command), flush=True)
            except subprocess.CalledProcessError:
                pass
        raise
    finally:
        try:
            operator(1)
        finally:
            kubectl("delete", "namespace", namespace, "--wait=false")


if __name__ == "__main__":
    main()
