# Testing: Ruler Alertmanager Hot-Reload

## Prerequisites
- Kubernetes/OpenShift cluster
- Loki Operator that uses the custom Loki image built from this PR for its Loki components
- LokiStack `lokistack-dev` deployed in namespace `openshift-logging`
- Log collectors shipping logs to Loki

## Steps

### 1. Deploy the RulerConfig and AlertingRule

```bash
kubectl apply -f ruler-hotreload/rulerconfig.yaml
kubectl apply -f ruler-hotreload/alertingrule.yaml
```

### 2. Wait for the ruler to start firing

Watch the ruler logs until you see `Error sending alerts` to `alertmanager-initial.example.com`.
This confirms the notifier is built and cached with the initial URL.

```bash
kubectl logs lokistack-dev-ruler-0 -n openshift-logging -f | grep "Error sending"
```

Expected:
```
msg="Error sending alerts" alertmanager=https://alertmanager-initial.example.com/api/v2/alerts ...
```

### 3. Record baseline pod start times

```bash
kubectl get pods -n openshift-logging -l app.kubernetes.io/instance=lokistack-dev \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.startTime}{"\n"}{end}'
```

### 4. Change the alertmanager endpoint

```bash
kubectl patch rulerconfig lokistack-dev -n openshift-logging --type='json' \
  -p='[{"op": "replace", "path": "/spec/overrides/infrastructure/alertmanager/endpoints/0", "value": "https://alertmanager-new.example.com"}]'
```

### 5. Wait ~2 minutes then verify

**Pods must not have restarted** (start times identical to baseline):
```bash
kubectl get pods -n openshift-logging -l app.kubernetes.io/instance=lokistack-dev \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.startTime}{"\n"}{end}'
```

**Ruler logs must show the notifier was updated and the new URL is being used:**
```bash
kubectl logs lokistack-dev-ruler-0 -n openshift-logging --since=3m | \
  grep -E "updating notifier|Error sending"
```

## Expected Output

```
level=info ... msg="ruler alertmanager config changed, updating notifier" user=infrastructure
level=error ... msg="Error sending alerts" alertmanager=https://alertmanager-new.example.com/api/v2/alerts ...
```

Pod start times should be identical to the baseline, no restarts occurred.
