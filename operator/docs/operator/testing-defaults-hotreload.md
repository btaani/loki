# Testing: Hot-Reloadable Global Default Limits

This guide walks through deploying and testing the `defaults:` feature, which allows global Loki limits to be changed at runtime without restarting any pods.

## Prerequisites

- OpenShift cluster
- Cluster Logging Operator configured with a ClusterLogForwarder instance
- `kubectl` / `oc` configured with cluster access

## 1. Deploy the Loki Operator

```bash
make olm-deploy REGISTRY_BASE=<quay.io/your-quay-username> VARIANT=openshift VERSION=0.0.1-$RANDOM
```
PS: the operator is configured to use a custom Loki image (`quay.io/btaani/loki:defaults-hotreload-loki-poc-with-metric2`) that implements the runtime-config `defaults:` feature.

Wait for the operator to be ready.

```bash
kubectl rollout status deployment/loki-operator-controller-manager -n openshift-operators-redhat
```

## 2. Deploy a Storage Secret

## 3. Deploy a LokiStack

Apply a LokiStack CR, for example for AWS S3:

```bash
kubectl apply -f hack/lokistack_gateway_ocp.yaml
```

## 4. Observe the runtime-config

Once the LokiStack is running, the operator generates a runtime-config ConfigMap that includes a `defaults:` block populated from `spec.limits.global`:

```bash
kubectl get configmap lokistack-dev-config -n openshift-logging \
  -o jsonpath='{.data.runtime-config\.yaml}'
```

You should see something like:

```yaml
defaults:
  ingestion_rate_mb: 4
  ingestion_burst_size_mb: 6
  ...
overrides:
  application:
    ruler_alertmanager_config:
      ...
```

## 5. Observe the per-tenant ingestion rate metric

The distributor exposes a new metric showing the effective ingestion rate limit per tenant:

```
loki_distributor_tenant_ingestion_rate_limit_bytes_per_second{tenant="<tenant>"}
```

This metric is populated once log traffic is flowing. Query it in your OCP console:
```
loki_distributor_tenant_ingestion_rate_limit_bytes_per_second{tenant="application"}
```
At this stage, the ingestion rate for `application` should be the default, which is 4

## 6. Change global limits and observe the effect

Edit `spec.limits.global` in the LokiStack CR:

```bash
kubectl patch lokistack lokistack-dev -n openshift-logging --type=merge -p '{
  "spec": {
    "limits": {
      "global": {
        "ingestion": {
          "ingestionRate": 8
        }
      }
    }
  }
}'
```

Within ~30 seconds (no pod restarts), the runtime-config updates and the metric reflects the new limit:

```bash
kubectl get configmap lokistack-dev-config -n openshift-logging \
  -o jsonpath='{.data.runtime-config\.yaml}' | grep ingestion_rate_mb
```

Expected output:
```
  ingestion_rate_mb: 8
```
Now query the metric for ingestion rate for `application` again, the value should change to reflect the new default, which is 8.

## 7. Add a per-tenant override

Per-tenant limits override `defaults:` for that specific tenant:

```bash
kubectl patch lokistack lokistack-dev -n openshift-logging --type=merge -p '{
  "spec": {
    "limits": {
      "tenants": {
        "application": {
          "ingestion": {
            "ingestionRate": 16
          }
        }
      }
    }
  }
}'
```

After ~30 seconds, verify in the metric:

```
loki_distributor_tenant_ingestion_rate_limit_bytes_per_second{tenant="application"}
loki_distributor_tenant_ingestion_rate_limit_bytes_per_second{tenant="infrastructure"}
```
`application` should get the new override value of 16, which infrastructure gets the global default of 8

## Key observations

- **No pod restarts** occur when changing `spec.limits.global` or `spec.limits.tenants`
- All tenants automatically inherit from `defaults:` for any field not explicitly set in their `overrides:` entry
- The metric updates within the Loki runtime-config poll interval (~10s) plus Kubernetes ConfigMap propagation time (~60s)
