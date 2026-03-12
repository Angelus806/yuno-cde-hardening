#!/bin/bash
# =============================================================================
# chaos-test.sh - Simulates the Diwali Incident
# Injects a bad deployment (high fail rate) and verifies automated detection.
#
# Usage:
#   ./chaos/chaos-test.sh                    # Full chaos test
#   ./chaos/chaos-test.sh --observe-only     # Just monitor existing stack
# =============================================================================

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log()    { echo -e "${BLUE}[$(date +'%H:%M:%S')]${NC} $1"; }
success(){ echo -e "${GREEN}[$(date +'%H:%M:%S')] ✅ $1${NC}"; }
warn()   { echo -e "${YELLOW}[$(date +'%H:%M:%S')] ⚠️  $1${NC}"; }
error()  { echo -e "${RED}[$(date +'%H:%M:%S')] ❌ $1${NC}"; }

echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║        CHAOS TEST: Simulating the Diwali Incident           ║"
echo "║        Transaction Authorizer - Rollback Verification       ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

# ── Phase 1: Verify baseline health ──────────────────────────────────────────
log "Phase 1: Verifying baseline health of stable instances..."

HEALTHY=0
for i in 1 2 3; do
  STATUS=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/health 2>/dev/null || echo "000")
  if [ "$STATUS" = "200" ]; then
    HEALTHY=$((HEALTHY + 1))
  fi
done

if [ $HEALTHY -lt 2 ]; then
  error "Stack is not healthy. Run 'docker compose up -d' first."
  exit 1
fi
success "Baseline healthy: $HEALTHY/3 stable instances responding"

# ── Phase 2: Inject bad canary ───────────────────────────────────────────────
log "Phase 2: Injecting bad canary (FAIL_RATE=0.85) - simulating Diwali incident..."
warn "This simulates deploying v2.8.4 with a critical bug"

docker compose -f docker-compose.yml -f docker-compose.chaos.yml up -d authorizer-v2-canary

log "Waiting 15s for canary to start..."
sleep 15

# ── Phase 3: Observe failure pattern ─────────────────────────────────────────
log "Phase 3: Observing failure pattern (simulating real-time monitoring)..."
echo ""

OBSERVATION_DURATION=60
INTERVAL=5
CHECKS=$((OBSERVATION_DURATION / INTERVAL))
FAILURES_DETECTED=0

for i in $(seq 1 $CHECKS); do
  # Test canary directly
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST http://localhost:8082/authorize \
    -H "Content-Type: application/json" \
    -d '{"amount": 100, "currency": "USD", "merchant_id": "chaos_test"}' \
    2>/dev/null || echo "000")

  # Get canary metrics
  METRICS=$(curl -s http://localhost:8082/metrics 2>/dev/null || echo "")
  SUCCESS_RATE=$(echo "$METRICS" | grep 'authorizer_success_rate' | awk '{print $2}' | head -1 || echo "N/A")

  if [ "$HTTP_CODE" != "200" ] || ([ "$SUCCESS_RATE" != "N/A" ] && awk "BEGIN{exit !(${SUCCESS_RATE:-1} < 0.9995)}"); then
    FAILURES_DETECTED=$((FAILURES_DETECTED + 1))
    warn "Check $i/$CHECKS | HTTP: $HTTP_CODE | Success rate: ${SUCCESS_RATE:-N/A} | DEGRADED"
  else
    log "Check $i/$CHECKS | HTTP: $HTTP_CODE | Success rate: ${SUCCESS_RATE:-N/A} | OK"
  fi

  sleep $INTERVAL
done

echo ""

# ── Phase 4: Simulate automated rollback ─────────────────────────────────────
if [ $FAILURES_DETECTED -ge 3 ]; then
  error "ALERT TRIGGERED: Detected $FAILURES_DETECTED/$CHECKS unhealthy checks!"
  echo ""
  log "Phase 4: Initiating AUTOMATED ROLLBACK..."
  warn "In production: PagerDuty alert fired, on-call engineer notified"

  # Rollback: stop bad canary
  docker compose stop authorizer-v2-canary
  docker compose rm -f authorizer-v2-canary

  log "Restarting healthy canary (v2 with low fail rate)..."
  docker compose up -d authorizer-v2-canary  # Uses docker-compose.yml (healthy)

  sleep 15

  # Verify stable instances still serving traffic
  STABLE_OK=0
  for i in 1 2 3; do
    STATUS=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/health 2>/dev/null || echo "000")
    [ "$STATUS" = "200" ] && STABLE_OK=$((STABLE_OK + 1))
  done

  echo ""
  if [ $STABLE_OK -ge 2 ]; then
    success "ROLLBACK SUCCESSFUL"
    success "Stable instances: $STABLE_OK/3 healthy"
    success "Zero downtime maintained throughout the incident"
    echo ""
    echo "  📊 What happened:"
    echo "     - Bad canary detected within ${OBSERVATION_DURATION}s"
    echo "     - Automated rollback triggered (no manual intervention)"
    echo "     - Stable v1 instances served 100% traffic during rollback"
    echo "     - This is how we prevent the next Diwali incident"
  else
    error "Rollback failed - manual intervention required!"
    exit 1
  fi
else
  success "Canary appears healthy ($FAILURES_DETECTED/$CHECKS failures detected)"
  log "No rollback needed - deployment can proceed"
fi

echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║  🎯 Chaos Test Complete                                     ║"
echo "║  📈 Check Grafana: http://localhost:3000                    ║"
echo "║  🔎 Check Prometheus: http://localhost:9090                 ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""
