# config/webhook

Webhook configuration manifests.

## Files

| File | Description |
|------|-------------|
| `manifests.yaml` | `MutatingWebhookConfiguration` (defaulter) and `ValidatingWebhookConfiguration` (validator) |
| `service.yaml` | `Service` routing webhook traffic to the operator pod on port 9443 |

## TLS

The operator self-manages its webhook CA and certificate — no dependency on cert-manager.

On startup, `RunController` ensures:
1. A CA keypair exists in `redis-operator-ca-secret` (creates it if absent, rotates before expiry).
2. A webhook TLS cert signed by that CA exists in `redis-operator-webhook-cert`.
3. The CA bundle in `MutatingWebhookConfiguration` and `ValidatingWebhookConfiguration` is kept up to date via a controller loop.
