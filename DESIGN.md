# Design Decisions: Zero-Downtime Deployments for Transaction Authorizer

> **Context:** Post-Diwali incident response. 47,000 failed transactions. $2.3M GMV lost. This never happens again.

---

## 1. Deployment Strategy: Canary over Blue-Green

### Decision
I chose **canary deployment** over blue-green.

### Reasoning

| Factor | Blue-Green | Canary | Winner |
|---|---|---|---|
| Rollback speed | Instant (flip LB) | ~2-5 min | Blue-Green |
| Infrastructure cost | 2x capacity always | ~1.25x during rollout | Canary |
| Blast radius on bad deploy | 100% if flip is wrong | 5-25% during observation | Canary |
| Real traffic testing | No (synthetic only) | Yes | Canary |
| Complexity | Low | Medium | Blue-Green |

The **Diwali incident's root cause** was that all instances were updated simultaneously — there was no gradual rollout. Blue-green would have helped only marginally: the bad version would have served 100% traffic the moment the load balancer flipped. Canary solves this directly: the bad version would have served only **25% of traffic**, and automated health checks would have rolled it back within 2 minutes rather than 12.

### Traffic Split Rationale
```
Stage 1: 0% → 25%  canary   (observe for 2 min, automated)
Stage 2: 25% → 100% stable   (manual approval gate required)
```

25% was chosen as the canary weight because:
- Small enough that a bad deploy affects < 1,250 req/s (vs. the 5,000 total)
- Large enough to generate statistically significant metrics within 2 minutes
- Matches industry standard for payment infrastructure canaries (Google SRE Book)

---

## 2. Observability: Metrics That Matter for Payment Authorization

### Chosen Monitoring Stack
- **Prometheus** for metrics collection (pull model, reliable, CNCF standard)
- **Grafana** for dashboards (integrates natively with Prometheus)
- **Alertmanager** (via Prometheus rules) for alert routing

### Key Metrics Selected

#### Tier 1 — Alerts Fire Immediately
| Metric | Threshold | Why |
|---|---|---|
| `authorizer_success_rate` | < 99.95% for 2 min | Core SLO. At 5K req/s, 0.05% error = 2.5 failed authorizations/second |
| `authorizer_success_rate{role="canary"}` | < 99.5% for 1 min | Canary-specific. More sensitive because we expect canary issues early |

#### Tier 2 — Investigation Triggers
| Metric | Threshold | Why |
|---|---|---|
| `authorizer_active_connections` | > 500 | Connection pool exhaustion pattern (common upstream issue) |
| Instance `up` | == 0 for 1 min | Container crash or OOM kill |

#### What I Did NOT Monitor (and why)
- **CPU/Memory**: Not directly correlated with payment failures. A service can be healthy at 90% CPU. I'd add these as secondary signals in production.
- **Throughput (req/s)**: Useful for capacity planning, not for SLO alerting.
- **Latency histograms**: The mock service doesn't expose proper histograms. In production I would use `histogram_quantile(0.99, rate(request_duration_seconds_bucket[5m]))` for p99 latency SLO.

### What Would Have Caught the Diwali Incident

The incident: bad deployment → all instances failed → 12-minute outage.

With this setup:
1. **T+0s**: Bad canary deployed (25% traffic only)
2. **T+30s**: `CanaryCompletelyFailing` alert fires (50% threshold, 30s window)
3. **T+90s**: `CanarySuccessRateDegraded` alert fires (99.5% threshold, 1 min window)
4. **T+120s**: Automated rollback triggered by CI/CD pipeline health check
5. **T+150s**: Canary removed, 100% traffic back to stable
6. **Total impact**: ~37.5 seconds × 1,250 req/s = ~46,875 failed requests **on canary only**

vs. the actual incident: 12 minutes × 5,000 req/s = **47,000+ failed requests** on all instances.

---

## 3. Secrets Management

### Approach: Docker Secrets (local) → AWS Secrets Manager / Azure Key Vault (production)

### Local Demo Implementation
Docker Compose `secrets:` mounts secret files from disk into containers at `/run/secrets/<name>`. The application reads them at runtime via `os.ReadFile("/run/secrets/stripe_api_key")`.

**What this demonstrates:**
- Secrets are never in environment variables (visible in `docker inspect`)
- Secrets are never in the image layers
- Secrets are never in docker-compose.yml values
- The `secrets/` directory is in `.gitignore`

### Production Implementation (What I'd Do With More Time)

**Azure (AKS):**
```yaml
# External Secrets Operator syncs from Azure Key Vault to K8s Secrets
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: authorizer-secrets
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: azure-keyvault
    kind: SecretStore
  target:
    name: authorizer-secrets
    creationPolicy: Owner
  data:
    - secretKey: stripe_api_key
      remoteRef:
        key: stripe-api-key
```

**Why External Secrets Operator over Sealed Secrets:**
- Supports credential rotation without pod restart (ESO watches for changes)
- Works across clouds (AWS SM, GCP SM, Azure KV, Vault)
- Audit trail in the cloud provider's secret manager

**Credential Rotation Without Downtime:**
1. Add new key version to Azure Key Vault
2. ESO detects change and updates K8s Secret (within `refreshInterval`)
3. Application watches `/run/secrets/` for changes using `inotify` (or polls every 60s)
4. No pod restart required — hot reload of credentials

**Security Considerations:**
- Secrets encrypted at rest in Key Vault (AES-256)
- Secrets encrypted in transit (TLS)
- RBAC: only the `authorizer` service account has read access to its secrets
- Secret values never appear in logs (app masks them)
- Rotation audit log in Key Vault for compliance

---

## 4. Failure Mode Analysis

### Scenario A: Canary fails health checks
1. Docker/K8s removes canary from load balancer pool immediately
2. All traffic falls back to stable instances (3 instances, 100% healthy)
3. `AuthorizerInstanceDown` alert fires within 1 minute
4. Pipeline's `deploy-canary` job detects failure and triggers rollback step
5. **User impact**: ~0 (stable was always serving 75%+)

### Scenario B: Canary metrics degrade (the Diwali pattern)
1. Canary stays up but returns errors
2. `CanarySuccessRateDegraded` fires at 1 minute
3. Pipeline observes success rate below SLO → runs rollback step
4. Rollback: `docker compose stop authorizer-v2-canary` + remove from nginx upstream
5. **User impact**: Only the 25% that hit canary during observation window

### Scenario C: Rollback itself fails
1. If `docker compose stop` fails (Docker daemon issue), stable instances continue serving
2. Alert: `AuthorizerInstanceDown` fires for canary (it's unreachable but not removed)
3. Nginx `proxy_next_upstream` configuration retries on upstream error — falls through to stable pool
4. On-call engineer paged via PagerDuty to manually intervene
5. **Mitigation in production**: Use K8s `kubectl rollout undo` which is idempotent and more reliable

### Scenario D: Monitoring stack goes down during deployment
1. Prometheus becomes unavailable
2. Pipeline cannot query success rate → `curl` to metrics endpoint fails
3. Pipeline treats missing metrics as "unknown" and **halts deployment** (fail-safe default)
4. Deployment pauses at canary stage, no promotion to stable
5. On-call engineer manually approves or rejects based on their own observation

### Scenario E: Network partition between canary and upstream payment provider
1. Canary gets 502s from Stripe/Adyen but stable does not
2. `CanarySuccessRateDegraded` fires within 1 minute
3. Rollback triggered — but this is actually a false positive (network issue, not code bug)
4. **Mitigation**: Add `upstream_error_code` label to metrics to distinguish provider errors from app errors

---

## 5. Multi-Region Design (Stretch Goal)

### Approach for LATAM / Southeast Asia / Europe

```
environments/
├── base/              # Shared Terraform modules (reused across regions)
│   ├── aks-cluster/
│   ├── monitoring/
│   └── networking/
├── staging/           # Deploy first, automated
├── latam/             # Manual approval gate
├── southeast-asia/    # Manual approval gate  
└── europe/            # Manual approval gate + GDPR config
```

**Pipeline flow:**
```
Build → Staging (auto) → LATAM (manual approval) → SEA (manual) → EU (manual)
```

**Data residency compliance:**
- Each region has its own secrets (different provider API keys per region)
- No cross-region data replication for card data (PCI-DSS requirement)
- EU region has additional GDPR configuration flag: `gdpr_mode: true`

**In this exercise:**  
I documented the approach in this design doc rather than implementing full Terraform modules, which would require actual cloud credentials and ~4 additional hours of work. The Docker Compose stack demonstrates the core concepts that would translate directly to Kubernetes manifests per region.

---

## 6. Production vs. Exercise Trade-offs

| Area | This Exercise | Production |
|---|---|---|
| Orchestration | Docker Compose | AKS / EKS with Argo Rollouts |
| Canary traffic split | nginx `split_clients` (hash-based) | Argo Rollouts with Prometheus analysis |
| Secrets | Docker secrets (files on disk) | Azure Key Vault + External Secrets Operator |
| Metrics | Custom `/metrics` endpoint | Proper Prometheus client library with histograms |
| Alerts | Prometheus rules only | Prometheus → Alertmanager → PagerDuty + Slack |
| RBAC | None (demo) | K8s RBAC, service accounts, network policies |
| TLS | None (demo) | mTLS via Istio service mesh |
| Multi-tenancy | Single compose stack | Namespaces per environment, resource quotas |
| Rollback speed | ~30s (docker compose stop) | ~10s (kubectl rollout undo, already in etcd) |

---

## 7. Repository Structure

```
transaction-authorizer/
├── mock-service/
│   ├── main.go                    # Go HTTP service: /authorize, /health, /ready, /metrics
│   └── Dockerfile                 # Multi-stage build, non-root user
├── docker/
│   └── nginx.conf                 # Load balancer: 75% stable, 25% canary
├── monitoring/
│   ├── prometheus/
│   │   ├── prometheus.yml         # Scrape config for all instances
│   │   └── alerts.yml             # SLO-based alerting rules
│   └── grafana/
│       └── provisioning/          # Auto-provisioned datasource + dashboard
├── secrets/
│   └── .gitkeep                   # Dir tracked, values NOT committed
├── chaos/
│   └── chaos-test.sh              # Injects bad canary, verifies rollback
├── .github/
│   └── workflows/
│       └── deploy.yml             # CI/CD: build → canary → observe → promote
├── docker-compose.yml             # Main stack
├── docker-compose.chaos.yml       # Override for Diwali incident simulation
├── DESIGN.md                      # This document
└── README.md                      # Quick start instructions
```
