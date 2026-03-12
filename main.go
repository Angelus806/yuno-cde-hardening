package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	version        = getEnv("SERVICE_VERSION", "unknown")
	failRate       = getEnvFloat("FAIL_RATE", 0.0) // 0.0 = healthy, 1.0 = always fail
	startupDelay   = getEnvInt("STARTUP_DELAY_SECONDS", 5)
	isReady        int32 // atomic: 0 = not ready, 1 = ready
	isShuttingDown int32 // atomic: 0 = running, 1 = shutting down

	// Metrics counters
	totalRequests  int64
	failedRequests int64
	activeConns    int64
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f
		}
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		i, err := strconv.Atoi(v)
		if err == nil {
			return i
		}
	}
	return fallback
}

// healthHandler - liveness probe
func healthHandler(w http.ResponseWriter, r *http.Request) {
	if atomic.LoadInt32(&isShuttingDown) == 1 {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "shutting_down", "version": version})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": version})
}

// readyHandler - readiness probe (used by load balancer)
func readyHandler(w http.ResponseWriter, r *http.Request) {
	if atomic.LoadInt32(&isReady) == 0 || atomic.LoadInt32(&isShuttingDown) == 1 {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not_ready", "version": version})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ready", "version": version})
}

// authorizeHandler - main payment authorization endpoint
func authorizeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	atomic.AddInt64(&activeConns, 1)
	defer atomic.AddInt64(&activeConns, -1)
	atomic.AddInt64(&totalRequests, 1)

	// Simulate processing latency (50-200ms)
	latency := time.Duration(50+rand.Intn(150)) * time.Millisecond
	time.Sleep(latency)

	// Simulate failures based on FAIL_RATE env var
	if rand.Float64() < failRate {
		atomic.AddInt64(&failedRequests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"authorized": false,
			"error":      "upstream_provider_error",
			"version":    version,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"authorized":     true,
		"transaction_id": fmt.Sprintf("txn_%d", time.Now().UnixNano()),
		"latency_ms":     latency.Milliseconds(),
		"version":        version,
	})
}

// metricsHandler - Prometheus-compatible metrics
func metricsHandler(w http.ResponseWriter, r *http.Request) {
	total := atomic.LoadInt64(&totalRequests)
	failed := atomic.LoadInt64(&failedRequests)
	active := atomic.LoadInt64(&activeConns)

	var successRate float64 = 1.0
	if total > 0 {
		successRate = float64(total-failed) / float64(total)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP authorizer_requests_total Total authorization requests\n")
	fmt.Fprintf(w, "# TYPE authorizer_requests_total counter\n")
	fmt.Fprintf(w, "authorizer_requests_total{version=%q} %d\n", version, total)

	fmt.Fprintf(w, "# HELP authorizer_requests_failed_total Failed authorization requests\n")
	fmt.Fprintf(w, "# TYPE authorizer_requests_failed_total counter\n")
	fmt.Fprintf(w, "authorizer_requests_failed_total{version=%q} %d\n", version, failed)

	fmt.Fprintf(w, "# HELP authorizer_success_rate Current success rate (0-1)\n")
	fmt.Fprintf(w, "# TYPE authorizer_success_rate gauge\n")
	fmt.Fprintf(w, "authorizer_success_rate{version=%q} %.4f\n", version, successRate)

	fmt.Fprintf(w, "# HELP authorizer_active_connections Current active connections\n")
	fmt.Fprintf(w, "# TYPE authorizer_active_connections gauge\n")
	fmt.Fprintf(w, "authorizer_active_connections{version=%q} %d\n", version, active)

	fmt.Fprintf(w, "# HELP authorizer_info Service version info\n")
	fmt.Fprintf(w, "# TYPE authorizer_info gauge\n")
	fmt.Fprintf(w, "authorizer_info{version=%q} 1\n", version)
}

func main() {
	log.Printf("[%s] Starting transaction-authorizer service (FAIL_RATE=%.2f)", version, failRate)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/ready", readyHandler)
	mux.HandleFunc("/authorize", authorizeHandler)
	mux.HandleFunc("/metrics", metricsHandler)

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	// Graceful startup: simulate initialization delay
	go func() {
		log.Printf("[%s] Initializing... waiting %ds before accepting traffic", version, startupDelay)
		time.Sleep(time.Duration(startupDelay) * time.Second)
		atomic.StoreInt32(&isReady, 1)
		log.Printf("[%s] Ready to serve traffic", version)
	}()

	// Graceful shutdown on SIGTERM/SIGINT
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-quit
		log.Printf("[%s] Received shutdown signal - draining connections...", version)
		atomic.StoreInt32(&isShuttingDown, 1)
		atomic.StoreInt32(&isReady, 0)

		// Connection draining period: let load balancer stop sending traffic
		time.Sleep(10 * time.Second)
		log.Printf("[%s] Shutdown complete", version)
		os.Exit(0)
	}()

	log.Printf("[%s] Listening on :8080", version)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
