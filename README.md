# Yuno CDE Kubernetes Cluster Hardening
## Deployment Guide & Testing Instructions

> **Context:** This guide implements defense-in-depth security hardening for Yuno's  
> Cardholder Data Environment (CDE) Kubernetes cluster following a container escape  
> vulnerability identified during penetration testing. All controls target PCI-DSS 4.0 compliance.

---

## Prerequisites

| Requirement | Version | Verification |
|---|---|---|
| kubectl | >= 1.28 | `kubectl version --client` |
| Helm | >= 3.12 | `helm version` |
| cluster-admin access | — | `kubectl auth can-i '*' '*' --all-namespaces` |
| Kyverno CLI (optional, for testing) | >= 1.11 | `kyverno version` |
| cosign (for supply chain) | >= 2.0 | `cosign version` |
| Vault CLI (for secrets setup) | >= 1.15 | `vault version` |

### Local Development (Docker Desktop / kind)

If using Docker Desktop with Kubernetes enabled:
```bash
# Verify cluster is running
kubectl cluster-info

# For kind (alternative):
kind create cluster --name yuno-cde-test
kubectl cluster-info --context kind-yuno-cde-test
```

---

## Repository Structure

```
yuno-cde-hardening/
├── admission-control/
│   └── kyverno-policies.yaml       # Pod security admission policies
├── network-policies/
│   └── network-policies.yaml       # Zero-trust NetworkPolicy manifests
├── secrets-management/
│   └── secrets-management.yaml     # ESO + Vault configuration
├── runtime-security/
│   └── falco-rules.yaml            # Falco detection rules
├── supply-chain/
│   └── image-supply-chain.yaml     # Cosign + Kyverno image verification
└── docs/
    └── threat-analysis.md          # Threat model & design decisions
```

---

## Step 1: Install Kyverno

```bash
# Add the Kyverno Helm repository
helm repo add kyverno https://kyverno.github.io/kyverno/
helm repo update

# Install Kyverno in its own namespace
# IMPORTANT: Set failurePolicy=Fail to prevent webhook bypass if Kyverno is unavailable
helm install kyverno kyverno/kyverno \
  --namespace kyverno \
  --create-namespace \
  --set admissionController.replicas=3 \
  --set backgroundController.enabled=true \
  --set reportsController.enabled=true \
  --wait

# Verify Kyverno is running
kubectl get pods -n kyverno
# Expected: 3x kyverno-admission-controller pods Running
```

---

## Step 2: Create the CDE Namespace

```bash
# Create namespace with Pod Security Admission labels
kubectl apply -f network-policies/network-policies.yaml \
  --dry-run=client   # Validate first

kubectl apply -f network-policies/network-policies.yaml \
  -l 'kind=Namespace'   # Apply only the Namespace resource first

# Verify labels
kubectl get namespace cde --show-labels
# Expected: pci-scope=cde, environment=production, pod-security.kubernetes.io/enforce=restricted
```

---

## Step 3: Apply Admission Control Policies

```bash
# Apply all Kyverno policies
kubectl apply -f admission-control/kyverno-policies.yaml

# Verify policies were created
kubectl get clusterpolicy
# Expected output:
# NAME                           ADMISSION   BACKGROUND   READY   AGE
# block-privileged-containers    true        true         True    10s
# block-host-namespaces          true        true         True    10s
# block-hostpath-volumes         true        true         True    10s
# require-drop-all-capabilities  true        true         True    10s
# require-non-root-user          true        true         True    10s
# require-readonly-root-fs       true        true         True    10s
# require-resource-limits        true        true         True    10s

# Check for any existing non-compliant pods in the CDE namespace
kubectl get policyreport -n cde
```

### ✅ Test 1: Privileged Pod is Blocked

```bash
# Attempt to deploy a privileged pod — should be REJECTED
cat <<EOF | kubectl apply -f - 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: test-privileged
  namespace: cde
spec:
  containers:
  - name: test
    image: nginx:latest
    securityContext:
      privileged: true
EOF

# Expected output:
# Error from server: error when creating "STDIN": admission webhook
# "validate.kyverno.svc-fail" denied the request:
# policy block-privileged-containers/block-privileged-containers:
# Privileged containers are prohibited in the CDE namespace.
```

### ✅ Test 2: hostPath Volume is Blocked

```bash
cat <<EOF | kubectl apply -f - 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: test-hostpath
  namespace: cde
spec:
  containers:
  - name: test
    image: nginx:latest
  volumes:
  - name: host-vol
    hostPath:
      path: /etc
EOF

# Expected: Admission webhook denies with "hostPath volume mounts are prohibited"
```

### ✅ Test 3: Compliant Pod is Allowed

```bash
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: test-compliant
  namespace: cde
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
  - name: test
    image: nginx:1.25.3   # Specific tag, not latest
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: [ALL]
      runAsNonRoot: true
      runAsUser: 1000
    resources:
      requests:
        memory: "64Mi"
        cpu: "50m"
      limits:
        memory: "128Mi"
        cpu: "100m"
    volumeMounts:
    - name: tmp
      mountPath: /tmp
  volumes:
  - name: tmp
    emptyDir: {}
EOF

# Expected: pod/test-compliant created
kubectl delete pod test-compliant -n cde   # Cleanup
```

---

## Step 4: Apply NetworkPolicies

```bash
# Apply all NetworkPolicies
kubectl apply -f network-policies/network-policies.yaml

# Verify NetworkPolicies were created
kubectl get networkpolicies -n cde
# Expected:
# NAME                               POD-SELECTOR                 AGE
# default-deny-all                   <none>                       10s
# api-gateway-policy                 app=api-gateway              10s
# tokenization-service-policy        app=tokenization-service     10s
# vault-db-policy                    app=vault-db                 10s
# payment-gateway-egress-external    app=payment-gateway          10s
# allow-prometheus-metrics-scraping  <none>                       10s
# allow-dns-egress                   <none>                       10s
```

### ✅ Test 4: Lateral Movement is Blocked

```bash
# Deploy test pods simulating a compromised payment-gateway trying to reach vault-db
# (This reproduces the pentest finding)

# 1. Deploy a "compromised" payment-gateway pod
kubectl run attacker \
  --image=curlimages/curl:latest \
  --namespace=cde \
  --labels="app=payment-gateway" \
  --command -- sleep 3600

# Wait for pod to be running
kubectl wait --for=condition=Ready pod/attacker -n cde --timeout=60s

# 2. Attempt to connect to vault-db (should time out / be blocked)
kubectl exec -n cde attacker -- \
  curl --connect-timeout 5 http://vault-db:5432 2>&1

# Expected: curl: (28) Connection timed out after 5000 milliseconds
# (NetworkPolicy blocks the connection — vault-db only accepts from tokenization-service)

# Cleanup
kubectl delete pod attacker -n cde
```

### ✅ Test 5: Legitimate Traffic is Allowed

```bash
# Deploy a tokenization-service pod and verify it CAN reach vault-db
kubectl run tokenization-test \
  --image=curlimages/curl:latest \
  --namespace=cde \
  --labels="app=tokenization-service" \
  --command -- sleep 3600

kubectl exec -n cde tokenization-test -- \
  curl --connect-timeout 5 http://vault-db:5432 2>&1

# Expected: Connection attempted (vault-db will reject at app level, 
# but the NetworkPolicy ALLOWS the connection — no timeout)
# (You may see a connection refused from postgres, not a timeout)

kubectl delete pod tokenization-test -n cde
```

---

## Step 5: Set Up External Secrets Operator + Vault

### 5a. Install External Secrets Operator

```bash
helm repo add external-secrets https://charts.external-secrets.io
helm repo update

helm install external-secrets external-secrets/external-secrets \
  --namespace external-secrets \
  --create-namespace \
  --wait

kubectl get pods -n external-secrets
# Expected: external-secrets-* pods Running
```

### 5b. Configure HashiCorp Vault (run on Vault server or local dev)

```bash
# For local testing, start Vault in dev mode:
# vault server -dev -dev-root-token-id="root"
# export VAULT_ADDR='http://127.0.0.1:8200'
# export VAULT_TOKEN="root"

# Enable Kubernetes auth in Vault
vault auth enable kubernetes

# Configure Kubernetes auth
vault write auth/kubernetes/config \
  kubernetes_host="https://$(kubectl get svc kubernetes -o jsonpath='{.spec.clusterIP}'):443"

# Create the CDE policy
vault policy write cde-payment-services - <<'EOF'
path "secret/data/cde/*" {
  capabilities = ["read"]
}
EOF

# Create the Kubernetes auth role
vault write auth/kubernetes/role/cde-payment-services \
  bound_service_account_names=external-secrets-vault-auth \
  bound_service_account_namespaces=cde \
  policies=cde-payment-services \
  ttl=1h

# Seed test secrets
vault kv put secret/cde/stripe \
  api_key="sk_test_REPLACE_WITH_REAL_KEY" \
  webhook_secret="whsec_REPLACE_WITH_REAL_SECRET"

vault kv put secret/cde/database \
  username="cde_app_user" \
  password="REPLACE_WITH_REAL_PASSWORD" \
  host="vault-db.cde.svc.cluster.local"
```

### 5c. Apply ESO Configuration

```bash
kubectl apply -f secrets-management/secrets-management.yaml

# Verify ExternalSecrets are syncing
kubectl get externalsecret -n cde
# Expected:
# NAME                           STORE          REFRESH INTERVAL   STATUS   READY
# stripe-credentials             vault-backend  1h                 SecretSynced  True
# vault-db-credentials           vault-backend  1h                 SecretSynced  True
# payment-provider-credentials   vault-backend  1h                 SecretSynced  True
```

### ✅ Test 6: Secrets Are Not Plaintext in Pod Spec

```bash
# Verify the Kubernetes Secret was created by ESO
kubectl get secret stripe-credentials -n cde

# Verify the values are NOT visible in any pod's spec YAML
# (They appear as secretRef references, not literal values)
kubectl get pod payment-gateway-* -n cde -o yaml | grep -i stripe
# Expected: "stripe-credentials" (the secret name), NOT the actual key value

# Verify Vault audit log captured the access (if Vault audit is enabled)
vault audit enable file file_path=/vault/logs/audit.log
vault audit list
```

---

## Step 6: Install Falco (Runtime Security)

```bash
helm repo add falcosecurity https://falcosecurity.github.io/charts
helm repo update

# Install Falco with the CDE custom rules
helm install falco falcosecurity/falco \
  --namespace falco \
  --create-namespace \
  --set falco.jsonOutput=true \
  --set falco.jsonIncludeOutputProperty=true \
  --set-file falco.rulesFile[0]=/dev/null \
  --wait

# Apply the custom CDE rules ConfigMap
kubectl apply -f runtime-security/falco-rules.yaml

# Verify Falco is running on all nodes
kubectl get pods -n falco -o wide
# Expected: One falco-* pod per node (DaemonSet)
```

### ✅ Test 7: Shell Spawn Detection

```bash
# Deploy a test pod simulating a compromised Java service
kubectl run java-service-test \
  --image=openjdk:17-slim \
  --namespace=cde \
  --labels="app=payment-gateway" \
  --command -- sleep 3600

# Trigger the Falco rule by spawning a shell
kubectl exec -n cde java-service-test -- /bin/bash -c "whoami"

# Check Falco logs for the alert
kubectl logs -n falco -l app.kubernetes.io/name=falco --tail=20 | grep "Shell spawned"
# Expected: CRITICAL Shell spawned in CDE container (...)

kubectl delete pod java-service-test -n cde
```

---

## Step 7: Apply Supply Chain Policies

### 7a. Generate Cosign Key Pair

```bash
# Generate signing key pair (do this ONCE, store private key securely in Vault)
cosign generate-key-pair

# This creates: cosign.key (private) and cosign.pub (public)
# Store private key in Vault:
vault kv put secret/cosign/private key=@cosign.key password="$COSIGN_PASSWORD"

# NEVER commit cosign.key to git
echo "cosign.key" >> .gitignore
```

### 7b. Apply Image Verification Policies

```bash
# Replace the placeholder public key in supply-chain/image-supply-chain.yaml
# with the contents of cosign.pub, then apply:

kubectl apply -f supply-chain/image-supply-chain.yaml

kubectl get clusterpolicy | grep -E "verify|restrict|block-latest|sbom"
```

### ✅ Test 8: Unsigned Image is Blocked

```bash
cat <<EOF | kubectl apply -f - 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: test-unsigned
  namespace: cde
spec:
  containers:
  - name: test
    image: nginx:1.25.3   # Unsigned image from Docker Hub
EOF

# Expected: Admission webhook denies — image not signed by Yuno's CI key
```

---

## Validation Summary Checklist

Run through this checklist before the QSA audit:

```bash
# 1. All Kyverno policies are enforcing
kubectl get clusterpolicy -o custom-columns=NAME:.metadata.name,ACTION:.spec.validationFailureAction,READY:.status.conditions[-1].status

# 2. Default-deny NetworkPolicy exists in CDE namespace
kubectl get networkpolicy default-deny-all -n cde

# 3. No pods in CDE namespace have privileged: true
kubectl get pods -n cde -o json | jq '.items[].spec.containers[].securityContext.privileged' | grep -v null | grep true
# Expected: no output

# 4. No pods have plaintext secrets as env vars
kubectl get pods -n cde -o json | jq '.items[].spec.containers[].env[] | select(.value != null) | select(.name | test("KEY|SECRET|PASSWORD|TOKEN"))' 
# Expected: no output (all secrets use secretRef, not literal .value)

# 5. Falco is running on all nodes
kubectl get daemonset falco -n falco
# Expected: DESIRED == READY

# 6. ExternalSecrets are all synced
kubectl get externalsecret -n cde -o custom-columns=NAME:.metadata.name,STATUS:.status.conditions[-1].reason
# Expected: all show SecretSynced

# 7. etcd encryption at rest (run on control plane)
# ETCDCTL_API=3 etcdctl get /registry/secrets/cde/stripe-credentials \
#   --endpoints=https://127.0.0.1:2379 \
#   --cert=/etc/kubernetes/pki/etcd/server.crt \
#   --key=/etc/kubernetes/pki/etcd/server.key \
#   --cacert=/etc/kubernetes/pki/etcd/ca.crt | head -c 50
# Expected: starts with /registry/secrets.io (encrypted), NOT "apiVersion"
```

---

## Rollback Procedure

If a policy breaks a legitimate workload:

```bash
# 1. Identify the blocking policy
kubectl get events -n cde --field-selector reason=PolicyViolation

# 2. Switch policy to Audit mode temporarily (non-breaking)
kubectl patch clusterpolicy <policy-name> \
  --type merge \
  -p '{"spec":{"validationFailureAction":"Audit"}}'

# 3. Apply the exception policy or fix the workload
# 4. Switch back to Enforce
kubectl patch clusterpolicy <policy-name> \
  --type merge \
  -p '{"spec":{"validationFailureAction":"Enforce"}}'
```

---

## Troubleshooting

| Symptom | Likely Cause | Resolution |
|---|---|---|
| Pods stuck in Pending | Kyverno webhook timing out | Check `kubectl get pods -n kyverno`; restart if needed |
| `Connection timed out` between services | NetworkPolicy too restrictive | Check `kubectl describe networkpolicy -n cde`; add missing rule |
| ExternalSecret shows `SecretSyncedError` | Vault auth failed | Verify ServiceAccount token and Vault role binding |
| Falco not producing alerts | Rules not loaded | `kubectl logs -n falco <pod> | grep "Loading rules"` |
| Image rejected as unsigned | Cosign public key mismatch | Verify public key in Kyverno policy matches `cosign.pub` |
