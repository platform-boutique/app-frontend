# Kubernetes

**Workload:** stateless `Deployment`; scale freely (sessions are in a cookie, carts in `cart-service`; no sticky sessions required). This is the one workload that needs external exposure.

**Ports to declare**

| Name | Port | Protocol | Expose via Service? |
|---|---|---|---|
| `http` | `8080` | TCP (HTTP/1.1) | Yes — Service port `80` → `8080`; front it with an Ingress / Gateway / LoadBalancer |
| `metrics` | `9090` | TCP (HTTP/1.1) | Prometheus `/metrics` (was `9464`; a chart that still declares or probes `9464` must change). Also pushed over OTLP |

**Exposure:** serve over **HTTPS** (see cookie gotcha). Ingress `Service` target is `frontend:80` → pod `8080`. If you use a sub-path ingress, also set `BASE_URL` to match. Set the NetworkPolicy so that only the ingress controller can reach `8080`.

**Probes**

```yaml
startupProbe:
  httpGet: { path: /_healthz, port: 8080 }
  periodSeconds: 2
  failureThreshold: 15
readinessProbe:
  httpGet: { path: /_healthz, port: 8080 }
  periodSeconds: 5
livenessProbe:
  httpGet: { path: /_healthz, port: 8080 }
  periodSeconds: 10
  failureThreshold: 3
```

Health is static, so readiness doesn't reflect backend health. Pod startup is near-instant.

**Security context**

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 100
  runAsGroup: 101
  readOnlyRootFilesystem: true       # nothing is written at runtime
  allowPrivilegeEscalation: false
  capabilities: { drop: ["ALL"] }
```

**Resources** — requests sized from measured idle usage in the test stack (see the chart's `values.yaml` for limits):

```yaml
resources:
  requests: { cpu: 100m, memory: 48Mi }
  limits:   { cpu: "500m", memory: 192Mi }
```

**Config / secrets:** no secrets. Service addresses, observability endpoints and branding vars belong in values → env.

**Telemetry:** OTLP/gRPC to Alloy (`global.monitoring.otlpEndpoint`), profiles to Alloy `:4040`. Metrics carry `job="frontend"` and `instance=<pod>`; the same series are also available by scraping the `metrics` port (`9090`); whether the Helm chart declares a scrape annotation or `ServiceMonitor` for it was not checked.

**Helm chart:** [`platform-demo-helm/frontend`](https://github.com/platform-boutique/platform-demo-helm/tree/main/frontend) (built on the shared `platform-common` library chart). It ships with HA defaults — 2 replicas,
HPA, PodDisruptionBudget, topology spread, NetworkPolicy, restricted pod security context, probes and the full env contract (see Configuration) —
and was verified under the `restricted` Pod Security Standard on the earlier local 3-node kind cluster (since retired). The platform now targets AWS EKS, where this chart has not been deployed or verified yet.

**Deployment and admission policies:** Argo CD deploys the chart from `platform-demo-helm` (`envs/<env>/values/frontend.yaml` pins the image digest; promotion is a pull request). In the cluster Kyverno enforces, for this service: pods named `app.kubernetes.io/name: frontend` that run under the ServiceAccount of the same name (Cilium derives the network identity from that label), an image from this repository pinned by digest and signed by our release pipeline, a read-only root filesystem, resource requests and a memory limit, readiness and liveness probes and no API token. The Helm CI evaluates the rendered chart against these policies before merge; see [`platform-demo-helm/policies`](https://github.com/platform-boutique/platform-demo-helm/tree/main/policies).
