# Threat Analysis & Design Decisions
## Yuno CDE Kubernetes Cluster Hardening

**Author:** Platform Security / DevOps Engineering  
**Date:** 2025-05-14  
**Scope:** Production CDE Cluster — Payment Tokenization Services  
**PCI-DSS Version:** 4.0  

---

## 1. Threat Model

### 1.1 Attack Vectors Addressed

The pentest identified a 23-minute end-to-end compromise. The following table maps each attack step to the control that now blocks it:

| Attack Step | Root Cause | Control Implemented |
|---|---|---|
| Initial container escape | Container running with `CAP_SYS_ADMIN` | Kyverno: block-privileged-containers + require-drop-all-capabilities |
| Node access after escape | `hostPID`/`hostIPC` enabled | Kyverno: block-host-namespaces |
| Lateral movement to tokenization DB | Flat network, no NetworkPolicy | NetworkPolicy: default-deny-all + allowlist per service |
| Credential exfiltration via ENV vars | Secrets as plaintext env vars in pod spec | External Secrets Operator: secrets sourced from Vault, not manifest literals |
| Persistence via malicious binary | No runtime monitoring | Falco: detects unexpected process execution |
| Compromised base image | No image provenance verification | Cosign + Kyverno: blocks unsigned images |

### 1.2 Blast Radius Analysis

**If one control fails — what does the attacker gain?**

```
┌────────────────────────────────────────────────────────────────────┐
│  Layer 1: Admission Control (Kyverno)                              │
│  FAIL: Attacker deploys a privileged pod                           │
│  → Contained by Layer 2 (NetworkPolicy blocks lateral movement)    │
├────────────────────────────────────────────────────────────────────┤
│  Layer 2: NetworkPolicy (Zero-Trust Segmentation)                  │
│  FAIL: Attacker pivots from compromised pod                        │
│  → Cannot reach tokenization DB (only tokenization-service can)    │
│  → Cannot exfil data on non-:443 ports                            │
│  → Layer 3 (Falco) alerts on unexpected connections               │
├────────────────────────────────────────────────────────────────────┤
│  Layer 3: Runtime Security (Falco)                                 │
│  FAIL: Attacker evades Falco rules                                 │
│  → Cannot read plaintext credentials (Layer 4 protects these)     │
│  → Secret rotation limits credential useful lifetime              │
├────────────────────────────────────────────────────────────────────┤
│  Layer 4: Secrets Management (ESO + Vault)                         │
│  FAIL: Attacker reads a K8s Secret from etcd                       │
│  → 1h rotation window limits exposure                             │
│  → Vault audit log creates forensic trail for PCI incident report │
│  → Secret is scoped to specific service SA (minimal blast radius) │
└────────────────────────────────────────────────────────────────────┘
```

**Worst-case single-control failure:**  
If NetworkPolicy is bypassed (e.g., a CNI plugin vulnerability), the attacker can reach other pods. However, they still cannot:  
- Read secrets from Vault without a valid ServiceAccount token for the `cde-payment-services` Vault role  
- Deploy persistent backdoors (Falco detects unexpected binaries)  
- Exfiltrate bulk data silently (Falco alerts on unexpected outbound connections)

---

## 2. Design Decisions

### 2.1 Policy Engine: Kyverno vs. OPA Gatekeeper

**Decision: Kyverno**

| Criterion | Kyverno | OPA Gatekeeper |
|---|---|---|
| Configuration language | YAML (native K8s) | Rego (custom DSL) |
| Learning curve | Low — DevOps team already knows YAML | High — requires Rego expertise |
| Mutation support | Native (auto-inject securityContexts) | Requires separate Mutating Webhook |
| Policy exceptions | Native PolicyException CRD | Manual exclusion via ConstraintTemplate |
| Audit mode | Built-in background scanning | Requires separate audit controller |
| Ecosystem maturity | CNCF Incubating | CNCF Graduated |

**Rationale:** Kyverno's YAML-native policies lower the barrier for the DevOps team to understand and contribute to security policies. OPA/Rego expertise is a specialized skill that creates a single-point-of-knowledge risk. For PCI-DSS purposes, Kyverno's built-in audit mode allows continuous compliance reporting without additional tooling.

**Trade-off accepted:** OPA Gatekeeper is more flexible for complex, multi-resource policy logic. If Yuno later requires cross-resource validation (e.g., "a Service with label X must have a NetworkPolicy"), migrating to OPA Gatekeeper would be justified.

---

### 2.2 Network Policy Strategy: Allowlist vs. Label-Based

**Decision: Allowlist with pod label selectors + namespace selectors**

The `default-deny-all` policy combined with service-specific allowlist policies creates a zero-trust network. Each service only permits the exact traffic flows required:

```
[External] → api-gateway:8080 → tokenization-service:8443 → vault-db:5432
                ↑
       [ingress-nginx namespace]
       
[monitoring namespace / prometheus] → *:9090 (all CDE pods, metrics only)
[payment-gateway] → :443 egress only (external payment providers)
```

**Why not IP-based CIDR restrictions for payment providers?**  
Stripe, Adyen, and Mercado Pago use Anycast CDN infrastructure with dynamic IPs that are not published in stable CIDR ranges. Maintaining IP allowlists would require constant updates and create operational risk (payment outage if a provider IP changes). The mitigation for this gap is: (1) port restriction to `:443` only, (2) Falco runtime alerts for unexpected destinations.

**Future improvement:** Deploy an egress proxy (Envoy or Squid) with an FQDN-based allowlist (`api.stripe.com`, `api.adyen.com`). This provides domain-level restriction without IP fragility.

---

### 2.3 Secrets Management: ESO + Vault vs. Vault Agent Injector

**Decision: External Secrets Operator + HashiCorp Vault**

| Criterion | ESO + Vault | Vault Agent Injector |
|---|---|---|
| App code changes required | None | None |
| Secret delivery mechanism | Kubernetes Secret → envFrom | Sidecar writes files to shared volume |
| Rotation | Auto-refresh via `refreshInterval` + rolling restart | Sidecar handles live rotation (no restart needed) |
| Operational complexity | Medium | High (sidecar per pod, Vault annotations) |
| Kubernetes Secret visibility | Secrets exist in K8s API | Secrets bypass K8s Secret store entirely |

**Rationale:** ESO provides a simpler operational model. The key constraint — app code reads `os.Getenv()` — is satisfied by `envFrom: secretRef` which sources env vars from the ESO-managed Kubernetes Secret. Critically, the **values never appear in the pod spec manifest**, which is the artifact committed to git and stored in etcd. This satisfies the PCI-DSS requirement to eliminate plaintext credentials from configuration artifacts.

**The envFrom approach vs. file-based injection:**  
File-based injection (mounting secrets as files, then using an env wrapper) would technically eliminate them from the K8s Secret store entirely. However, this requires either app code changes (to read files instead of env vars) or an LD_PRELOAD shim — both adding complexity. The ESO + envFrom approach meets PCI-DSS requirements when combined with **etcd encryption at rest** (which must be enabled separately on the control plane).

**Critical dependency:** etcd encryption at rest must be enabled. Without it, Kubernetes Secrets are base64-encoded (not encrypted) in etcd, and an attacker with etcd access can read them. This is documented in the deployment guide.

---

### 2.4 Runtime Security: Falco vs. Tetragon

**Decision: Falco**

Falco is selected over Cilium Tetragon because:
- Falco is the CNCF-graduated standard for runtime security; QSA auditors recognize it
- Falco has a mature ecosystem with pre-built rule sets for PCI-DSS
- Tetragon requires Cilium CNI (we are not mandating a specific CNI)
- Falco's rule language (YAML + condition expressions) is accessible to the DevOps team

---

### 2.5 Handling the Legacy Network Monitoring Sidecar

**The constraint:** A network monitoring sidecar requires `CAP_NET_RAW` for packet capture. We cannot break this service. We also cannot allow broad privilege escalation.

**Decision: Narrow Kyverno exception with compensating controls**

The exception policy (`exception-netmon-cap-net-raw`) is scoped with:
- Label selector: only pods with `security.yuno.com/exception: cap-net-raw-netmon`
- Container name restriction: only the `netmon-sidecar` container
- All other security controls remain enforced (non-root, no privilege escalation, read-only FS)
- Exception is time-bounded with an expiry date in the annotation
- Exception ownership is documented (platform-security@yuno.com)

**Exit strategy:** The sidecar should be refactored to use a DaemonSet running with minimal privileges on the node level, rather than per-pod. This eliminates the need for `CAP_NET_RAW` inside containers. Target: Q4 2025.

---

## 3. Trade-offs: Security vs. Developer Velocity

### 3.1 kubectl exec in Production

**Problem:** Overly restrictive egress policies could break `kubectl exec` used for debugging.

**Resolution:** `kubectl exec` tunnels through the Kubernetes API Server using SPDY/WebSocket. It does NOT use pod-to-pod networking and is NOT affected by NetworkPolicy. Developers retain `kubectl exec` access to CDE pods subject to RBAC controls. Production CDE exec is gated by a separate elevated RBAC role that generates an audit log entry (PCI-DSS Requirement 10.2 compliance).

### 3.2 Secret Rotation Window

**Problem:** `refreshInterval: 1h` means a rotated secret takes up to 1 hour to propagate, plus a rolling restart.

**Resolution:** Forced refresh via annotation + rolling restart completes in under 5 minutes for a 2-replica deployment. The 1-hour window is acceptable for planned rotation. For emergency rotation (compromise scenario), the procedure is documented with a target RTO of 10 minutes.

### 3.3 Image Signing in Development Workflows

**Problem:** Requiring signed images blocks developers deploying feature branches to staging.

**Resolution:** The `verify-image-signatures` policy is scoped to `cde` and `payment-services` namespaces only. Developers deploy freely to `dev`, `integration`, and `qa` namespaces. The CI/CD pipeline signs images automatically — developers don't need to run Cosign manually.

---

## 4. Residual Risks

| Risk | Likelihood | Impact | Mitigation Status |
|---|---|---|---|
| etcd not encrypted at rest | Medium | Critical | **Prerequisite** — must be verified before go-live |
| Payment provider egress not IP-restricted | High | Medium | Compensating: Falco alerts + port restriction |
| Vault HA not configured | Unknown | High | Out of scope — Vault team responsibility |
| Node-level kernel exploits (unpatchable CVEs) | Low | Critical | Seccomp RuntimeDefault profile applied; node patching SLA needed |
| Kyverno webhook failure (degraded mode) | Low | High | Set `failurePolicy: Fail` on webhook to prevent bypass |
| Service mesh (mTLS) not implemented | Medium | Medium | Future: Istio/Linkerd for pod-to-pod mTLS |

**What would be added with another week:**
1. **Istio service mesh** — mTLS between all CDE pods; even if NetworkPolicy is bypassed, traffic is encrypted and authenticated
2. **Egress proxy** — Envoy proxy with FQDN-based allowlist for payment provider APIs
3. **Kyverno audit reporting** — Daily compliance reports to S3/Blob storage for QSA evidence collection
4. **OPA-based RBAC audit** — Continuous scanning of RBAC bindings for privilege creep

---

## 5. PCI-DSS Requirement Mapping

| PCI-DSS 4.0 Requirement | Control Implemented |
|---|---|
| **1.2.1** — Restrict traffic between CDE and non-CDE | NetworkPolicy: default-deny-all + service allowlists |
| **1.2.5** — All services, protocols, and ports are identified and approved | NetworkPolicy YAML manifests serve as approved traffic documentation |
| **2.2.1** — System components use vendor-supported software | Image supply chain: Trivy scan blocks HIGH/CRITICAL CVEs |
| **2.2.7** — All non-console administrative access encrypted | kubectl access via TLS (K8s API server); exec audit logs |
| **3.4** — Render CHD unreadable | Secrets from Vault via ESO; never plaintext in pod specs or git |
| **3.6** — Key management procedures | Vault manages secret lifecycle; cosign key rotation documented |
| **6.3.3** — Security patches applied | Trivy in CI pipeline blocks images with HIGH/CRITICAL CVEs |
| **7.1** — Limit access to system components | RBAC: secretRef scoped to specific Secret names; no wildcard get/list |
| **7.2** — Access control systems with default-deny | NetworkPolicy default-deny-all; Kyverno Enforce mode |
| **10.2.1** — Audit log for all CHD access | Vault audit log; Falco events; Kubernetes audit log |
| **10.2.4** — Invalid access attempts logged | Falco: privilege escalation, unexpected connections, shell spawns |
| **10.2.5** — Changes to identification mechanisms logged | Falco: capset, setuid, setgid syscall rules |
| **10.6** — Synchronize time | NTP configuration on nodes (prerequisite, not in scope) |
| **11.3** — External/internal penetration testing | This hardening directly addresses the pentest findings |
| **11.5** — Intrusion detection mechanisms | Falco DaemonSet; alerts to Slack + PagerDuty |
