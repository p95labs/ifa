# Kubernetes permissions

Every permission IFA requests, why it needs it, and what stops working without
it.

## The complete set

```yaml
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["get", "list", "watch"]

- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch"]

- apiGroups: ["autoscaling"]
  resources: ["horizontalpodautoscalers"]
  verbs: ["get", "list", "watch"]
```

That is all of it. No `create`, `update`, `patch`, `delete`, `deletecollection`,
`bind`, `escalate` or `impersonate`. No `pods/exec`, `pods/portforward`,
`pods/log`, or any other subresource. No secrets, no configmaps, no nodes, no
events, no CRDs.

## Why each one

| Resource | Used for | Without it |
|---|---|---|
| `deployments` | Desired and ready replica counts | `IFA-SCL-001` and `IFA-SCL-002` never fire; `/api/v1/workloads` is empty |
| `pods` | Container restart counts, GPU resource requests | Restart counts are absent from `/api/v1/workloads`; no rule is affected today |
| `horizontalpodautoscalers` | The replica ceiling | `IFA-SCL-001` cannot distinguish "the autoscaler will fix this" from "the autoscaler has nothing left" |

Discovery is entirely optional. With `kubernetes.enabled: false` IFA scrapes and
diagnoses normally; only the scaling rules and the workloads endpoint go quiet.
If the API is unreachable at startup, IFA logs a warning and carries on rather
than refusing to start — a missing kubeconfig should not turn into a total
outage.

## Namespace scope versus cluster scope

Default is a namespaced `Role`, and that is the recommendation:

```yaml
rbac:
  scope: namespace
config:
  kubernetes:
    namespace: inference
```

A `ClusterRole` is only needed to watch every namespace, which the chart selects
automatically when `config.kubernetes.namespace` is empty:

```yaml
rbac:
  scope: cluster
config:
  kubernetes:
    namespace: ""
```

Prefer namespace scope. A read-only cluster-wide `list pods` still lets the
holder read every pod spec in the cluster, and pod specs contain environment
variables.

To watch a handful of namespaces rather than all of them, install one release
per namespace. It costs a few tens of MB each and keeps the permission grant
tight.

## Verifying it yourself

```bash
kubectl get clusterrole ifa -o yaml     # or: kubectl -n inference get role ifa -o yaml

# Confirm the ServiceAccount cannot write:
SA=system:serviceaccount:inference:ifa
kubectl auth can-i --as="$SA" delete deployments -n inference   # no
kubectl auth can-i --as="$SA" create pods        -n inference   # no
kubectl auth can-i --as="$SA" get secrets        -n inference   # no
kubectl auth can-i --as="$SA" list deployments   -n inference   # yes
```

## Narrowing further

To grant nothing at all, set `rbac.create: false` and `config.kubernetes.enabled:
false`. IFA then runs with no Kubernetes access whatsoever and works entirely
from its configured scrape targets — a reasonable posture if you do not need the
scaling rules.

The permission set is checked in as a template rather than generated, so a
`helm template` diff shows exactly what an upgrade would change.
