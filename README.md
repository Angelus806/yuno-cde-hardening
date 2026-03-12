# Transaction Authorizer — Zero-Downtime Deployment Solution

> **Challenge:** Redesign deployment infrastructure after the Diwali Incident — 47,000 failed transactions, $2.3M GMV lost due to a simultaneous rollout of a bad version.

## Solution Overview

This solution implements a **canary deployment strategy** with automated health verification, SLO-based alerting, and chaos engineering tooling to prevent recurrence.

```
┌─────────────────────────────────────────────────────────────┐
│                    Load Balancer (nginx)                     │
│              75% stable ◄────────► 25% canary               │
└──────────────┬───────────────────────────┬──────────────────┘
               │                           │
    ┌──────────▼──────────┐    ┌───────────▼──────────┐
    │   Stable Pool (v1)  │    │   Canary (v2)        │
    │  • authorizer-v1-1  │    │  • authorizer-v2     │
    │  • authorizer-v1-2  │    │  FAIL_RATE=0.02      │
    │  • authorizer-v1-3  │    │  (set to 0.85 for    │
    │  FAIL_RATE=0.01     │    │   chaos test)        │
    └─────────────────────┘    └──────────────────────┘
               │                           │
               └─────────┬─────────────────┘
                         │
              ┌──────────▼──────────┐
              │   Prometheus        │ :9090
              │   + Alert Rules     │
              └──────────┬──────────┘
                         │
              ┌──────────▼──────────┐
              │   Grafana           │ :3000
              │   Dashboard         │
              └─────────────────────┘
```

## Quick Start

### Prerequisites
- Docker Desktop (Windows/Mac) or Docker Engine (Linux)
- Git

### 1. Clone & setup secrets (demo values)

```bash
git clone https://github.com/YOUR_USERNAME/transaction-authorizer
cd transaction-authorizer

# Create demo secret files (in production, these come from Key Vault)
echo "sk_live_DEMO_KEY" > secrets/stripe_api_key.txt
echo "postgresql://user:pass@localhost/db" > secrets/db_connection_string.txt
echo "demo_encryption_key_32_bytes_here" > secrets/encryption_key.txt
```

### 2. Start the full stack

```bash
docker compose up -d
```

This starts:
| Service | URL | Description |
|---|---|---|
| Load Balancer | http://localhost:8080 | Main entry point (75/25 split) |
| Grafana | http://localhost:3000 | Dashboards (admin/admin) |
| Prometheus | http://localhost:9090 | Metrics & alerts |

### 3. Send test traffic

```bash
# Single authorization request
curl -X POST http://localhost:8080/authorize \
  -H "Content-Type: application/json" \
  -d '{"amount": 100, "currency": "USD", "merchant_id": "test_merchant"}'

# Continuous load (Linux/Mac)
while true; do
  curl -s -X POST http://localhost:8080/authorize \
    -H "Content-Type: application/json" \
    -d '{"amount": 100, "currency": "USD", "merchant_id": "load_test"}' | jq .
  sleep 0.5
done
```

### 4. View the dashboard

Open Grafana at http://localhost:3000 (admin/admin)  
The **Transaction Authorizer - Deployment Health** dashboard is pre-provisioned.

### 5. Simulate the Diwali Incident

```bash
# Inject a bad canary (85% fail rate)
docker compose -f docker-compose.yml -f docker-compose.chaos.yml up -d authorizer-v2-canary

# Watch metrics degrade in Grafana (refresh every 10s)
# Then restore healthy canary:
docker compose up -d authorizer-v2-canary
```

Or use the automated chaos test script:

```bash
# Linux/Mac
chmod +x chaos/chaos-test.sh
./chaos/chaos-test.sh

# Windows (Git Bash)
bash chaos/chaos-test.sh
```

---

## Architecture Decisions

See [DESIGN.md](./DESIGN.md) for full reasoning. Summary:

| Decision | Choice | Rationale |
|---|---|---|
| Deployment strategy | Canary (25% initial) | Limits blast radius vs. blue-green's 100% flip |
| Traffic splitting | nginx `split_clients` | Simple, deterministic, no external dependency |
| Monitoring | Prometheus + Grafana | Pull model, reliable, excellent alerting |
| SLO target | 99.95% success rate | Allows ≤2.5 failed auths/sec at 5K req/s |
| Alert latency | 30s - 2 min | Fast enough to limit damage, stable enough to avoid noise |
| Secrets (local) | Docker secrets | Never in env vars, never in image, never in git |
| Secrets (prod) | Azure Key Vault + ESO | Rotation without restarts, full audit trail |

---

## CI/CD Pipeline

The GitHub Actions workflow in `.github/workflows/deploy.yml` implements:

```
Push to main
     │
     ▼
Build & Push Image ──► Security Scan (Trivy)
     │
     ▼
Deploy Canary (25% traffic)
     │
     ▼
Observe 2 minutes ──► Check success rate vs SLO
     │                        │
     │ healthy              degraded
     ▼                        ▼
Manual Approval        Automated Rollback
  Gate (GitHub           (pipeline fails,
  Environments)          on-call paged)
     │
     ▼
Rolling Update (1 instance at a time)
     │
     ▼
Decommission Canary ──► Post-deploy validation
```

---

## Endpoints

The mock `transaction-authorizer` service exposes:

| Endpoint | Method | Description |
|---|---|---|
| `/authorize` | POST | Payment authorization (main endpoint) |
| `/health` | GET | Liveness probe (always 200 if running) |
| `/ready` | GET | Readiness probe (200 only after startup delay) |
| `/metrics` | GET | Prometheus-format metrics |

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `SERVICE_VERSION` | `unknown` | Version label in metrics/logs |
| `STARTUP_DELAY_SECONDS` | `5` | Simulates initialization time before ready |
| `FAIL_RATE` | `0.0` | Fraction of requests that fail (0.0–1.0) |

---

## Production Notes

This is a 2-hour demonstration. In production I would additionally implement:

- **Argo Rollouts** for native K8s canary with Prometheus metric analysis
- **Istio** for mTLS between services and more sophisticated traffic splitting
- **PagerDuty** integration for alert routing to on-call
- **Azure Key Vault** with External Secrets Operator for secrets management
- **Proper Prometheus histograms** for p95/p99 latency SLOs
- **Multi-region** Terraform modules with per-region pipelines and manual approval gates

See [DESIGN.md](./DESIGN.md) for the full production vs. exercise trade-off analysis.
