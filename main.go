package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "go_worker_http_requests_total",
			Help: "Total HTTP requests received by the worker demo service.",
		},
		[]string{"path", "method", "code"},
	)

	httpRequestDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "go_worker_http_request_duration_seconds",
			Help:    "HTTP request duration of the worker demo service in seconds.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.3, 0.5, 1, 2, 5},
		},
		[]string{"path", "method", "code"},
	)

	processUp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "go_worker_process_up",
			Help: "Whether the worker demo process is considered up.",
		},
	)

	discoveredGateways = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "go_worker_discovered_gateways",
			Help: "Number of gateway instances currently discovered from Consul.",
		},
		[]string{"service", "target_service"},
	)

	workExecuteTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "go_worker_execute_total",
			Help: "Total work execution attempts handled by workers.",
		},
		[]string{"task_type", "result"},
	)

	workExecuteDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "go_worker_execute_duration_seconds",
			Help:    "Duration of work execution handled by workers in seconds.",
			Buckets: []float64{0.05, 0.1, 0.3, 0.5, 1, 2, 5},
		},
		[]string{"task_type", "result"},
	)

	reportsSentTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "go_worker_reports_sent_total",
			Help: "Total status reports sent from worker to gateway.",
		},
		[]string{"target_service", "result"},
	)

	queueDepthGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "go_worker_queue_depth",
			Help: "Simulated queue depth on the worker.",
		},
	)

	activeJobsGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "go_worker_active_jobs",
			Help: "Current number of active jobs on the worker.",
		},
	)

	temperatureGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "go_worker_temperature_celsius",
			Help: "Simulated worker temperature in celsius.",
		},
	)
)

type config struct {
	serviceName                string
	targetServiceName          string
	targetDiscoveryServiceName string
	instanceID                 string
	appPort                    string
	metricsPort                string
	consulHTTPAddr             string
	logPath                    string
	peerRefreshInterval        time.Duration
	reportInterval             time.Duration
	requestTimeout             time.Duration
}

type app struct {
	config       config
	startedAt    time.Time
	logger       *log.Logger
	httpClient   *http.Client
	random       *rand.Rand
	randMu       sync.Mutex
	gatewaysMu   sync.RWMutex
	gateways     []peer
	queueDepth   atomic.Int64
	activeJobs   atomic.Int64
	temperature  atomic.Uint64
	requestCount atomic.Uint64
}

type peer struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	Address string `json:"address"`
	Port    int    `json:"port"`
}

type consulServiceEntry struct {
	Node struct {
		Address string `json:"Address"`
	} `json:"Node"`
	Service struct {
		ID      string `json:"ID"`
		Service string `json:"Service"`
		Address string `json:"Address"`
		Port    int    `json:"Port"`
	} `json:"Service"`
}

type workRequest struct {
	RequestID    string `json:"request_id"`
	FromService  string `json:"from_service"`
	FromInstance string `json:"from_instance"`
	TaskType     string `json:"task_type"`
	DelayMs      int    `json:"delay_ms"`
	SentAt       string `json:"sent_at"`
}

type workerReport struct {
	FromService        string  `json:"from_service"`
	FromInstance       string  `json:"from_instance"`
	QueueDepth         int     `json:"queue_depth"`
	ActiveJobs         int     `json:"active_jobs"`
	TemperatureCelsius float64 `json:"temperature_celsius"`
	Message            string  `json:"message"`
	SentAt             string  `json:"sent_at"`
}

type logEntry struct {
	Level              string  `json:"level"`
	Event              string  `json:"event"`
	Service            string  `json:"service"`
	InstanceID         string  `json:"instance_id"`
	TargetService      string  `json:"target_service,omitempty"`
	PeerID             string  `json:"peer_id,omitempty"`
	PeerAddress        string  `json:"peer_address,omitempty"`
	Path               string  `json:"path,omitempty"`
	Method             string  `json:"method,omitempty"`
	Status             int     `json:"status,omitempty"`
	TaskType           string  `json:"task_type,omitempty"`
	Detail             string  `json:"detail,omitempty"`
	RequestID          string  `json:"request_id,omitempty"`
	QueueDepth         int64   `json:"queue_depth,omitempty"`
	ActiveJobs         int64   `json:"active_jobs,omitempty"`
	TemperatureCelsius float64 `json:"temperature_celsius,omitempty"`
	GatewayCount       int     `json:"gateway_count,omitempty"`
	Timestamp          string  `json:"ts"`
}

func main() {
	prometheus.MustRegister(
		httpRequestsTotal,
		httpRequestDurationSeconds,
		processUp,
		discoveredGateways,
		workExecuteTotal,
		workExecuteDurationSeconds,
		reportsSentTotal,
		queueDepthGauge,
		activeJobsGauge,
		temperatureGauge,
	)

	cfg := loadConfig()
	logger, logFile, err := newLogger(cfg.logPath)
	if err != nil {
		log.Fatalf("init logger failed: %v", err)
	}
	defer logFile.Close()

	application := &app{
		config:    cfg,
		startedAt: time.Now(),
		logger:    logger,
		httpClient: &http.Client{
			Timeout: cfg.requestTimeout,
		},
		random: rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	application.queueDepth.Store(0)
	application.activeJobs.Store(0)
	application.setTemperature(41.2)
	queueDepthGauge.Set(0)
	activeJobsGauge.Set(0)
	temperatureGauge.Set(41.2)
	discoveredGateways.WithLabelValues(cfg.serviceName, cfg.targetServiceName).Set(0)
	processUp.Set(1)

	appMux := http.NewServeMux()
	appMux.HandleFunc("/", application.handleRoot)
	appMux.HandleFunc("/healthz", application.handleHealth)
	appMux.HandleFunc("/health", application.handleHealth)
	appMux.HandleFunc("/gateways", application.handleGateways)
	appMux.HandleFunc("/work/execute", application.handleExecuteWork)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())

	appServer := &http.Server{
		Addr:              ":" + cfg.appPort,
		Handler:           application.withMetrics(appMux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	metricsServer := &http.Server{
		Addr:              ":" + cfg.metricsPort,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go application.refreshGatewaysLoop()
	go application.reportLoop()

	go func() {
		application.writeLog(logEntry{Level: "info", Event: "application_server_start"})
		if err := appServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("application server failed: %v", err)
		}
	}()

	go func() {
		application.writeLog(logEntry{Level: "info", Event: "metrics_server_start"})
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("metrics server failed: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	processUp.Set(0)
	application.writeLog(logEntry{Level: "info", Event: "shutdown_signal"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = appServer.Shutdown(ctx)
	_ = metricsServer.Shutdown(ctx)
}

func loadConfig() config {
	serviceName := envOrDefault("SERVICE_NAME", "worker")
	targetServiceName := envOrDefault("TARGET_SERVICE_NAME", "gateway")
	targetDiscoveryServiceName := envOrDefault("TARGET_DISCOVERY_SERVICE_NAME", "gateway-http")

	instanceID := envOrDefault("INSTANCE_ID", envOrDefault("NOMAD_ALLOC_ID", hostnameOrDefault("worker-demo")))

	return config{
		serviceName:                serviceName,
		targetServiceName:          targetServiceName,
		targetDiscoveryServiceName: targetDiscoveryServiceName,
		instanceID:                 instanceID,
		appPort:                    envOrDefault("APP_PORT", "18081"),
		metricsPort:                envOrDefault("METRICS_PORT", "12113"),
		consulHTTPAddr:             ensureHTTPPrefix(envOrDefault("CONSUL_HTTP_ADDR", "127.0.0.1:8500")),
		logPath:                    envOrDefault("APP_LOG_PATH", "/app/logs/go-worker-demo.log"),
		peerRefreshInterval:        envDurationMillisOrDefault("PEER_REFRESH_INTERVAL_MS", 5000),
		reportInterval:             envDurationMillisOrDefault("REPORT_INTERVAL_MS", 4000),
		requestTimeout:             envDurationMillisOrDefault("HTTP_REQUEST_TIMEOUT_MS", 3000),
	}
}

func newLogger(logPath string) (*log.Logger, *os.File, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, nil, err
	}

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}

	return log.New(io.MultiWriter(os.Stdout, file), "", 0), file, nil
}

func (a *app) handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":                a.config.serviceName,
		"targetService":          a.config.targetServiceName,
		"targetDiscoveryService": a.config.targetDiscoveryServiceName,
		"instanceId":             a.config.instanceID,
		"gatewayCount":           len(a.snapshotGateways()),
		"queueDepth":             a.queueDepth.Load(),
		"activeJobs":             a.activeJobs.Load(),
		"temperatureCelsius":     a.temperatureValue(),
		"time":                   time.Now().Format(time.RFC3339),
		"uptimeSec":              int64(time.Since(a.startedAt).Seconds()),
	})
}

func (a *app) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                 "ok",
		"service":                a.config.serviceName,
		"targetService":          a.config.targetServiceName,
		"targetDiscoveryService": a.config.targetDiscoveryServiceName,
		"instanceId":             a.config.instanceID,
		"gatewayCount":           len(a.snapshotGateways()),
		"queueDepth":             a.queueDepth.Load(),
		"activeJobs":             a.activeJobs.Load(),
		"temperatureCelsius":     a.temperatureValue(),
		"time":                   time.Now().Format(time.RFC3339),
		"uptimeSec":              int64(time.Since(a.startedAt).Seconds()),
	})
}

func (a *app) handleGateways(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":       a.config.serviceName,
		"targetService": a.config.targetServiceName,
		"instanceId":    a.config.instanceID,
		"gatewayCount":  len(a.snapshotGateways()),
		"gateways":      a.snapshotGateways(),
		"time":          time.Now().Format(time.RFC3339),
	})
}

func (a *app) handleExecuteWork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "method not allowed"})
		return
	}

	var payload workRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "invalid payload"})
		return
	}

	a.queueDepth.Add(1)
	queueDepthGauge.Set(float64(a.queueDepth.Load()))
	a.activeJobs.Add(1)
	activeJobsGauge.Set(float64(a.activeJobs.Load()))
	a.randomizeTemperature(0.2, 1.5)

	startedAt := time.Now()
	a.writeLog(logEntry{
		Level:      "info",
		Event:      "work_received",
		TaskType:   payload.TaskType,
		RequestID:  payload.RequestID,
		Detail:     fmt.Sprintf("delay_ms=%d from=%s", payload.DelayMs, payload.FromService),
		QueueDepth: a.queueDepth.Load(),
		ActiveJobs: a.activeJobs.Load(),
	})

	time.Sleep(time.Duration(payload.DelayMs) * time.Millisecond)

	a.activeJobs.Add(-1)
	if a.activeJobs.Load() < 0 {
		a.activeJobs.Store(0)
	}
	activeJobsGauge.Set(float64(a.activeJobs.Load()))

	if a.queueDepth.Load() > 0 {
		a.queueDepth.Add(-1)
	}
	if a.queueDepth.Load() < 0 {
		a.queueDepth.Store(0)
	}
	queueDepthGauge.Set(float64(a.queueDepth.Load()))
	a.randomizeTemperature(-0.4, 0.8)

	workExecuteTotal.WithLabelValues(payload.TaskType, "success").Inc()
	workExecuteDurationSeconds.WithLabelValues(payload.TaskType, "success").Observe(time.Since(startedAt).Seconds())

	a.writeLog(logEntry{
		Level:              "info",
		Event:              "work_finished",
		TaskType:           payload.TaskType,
		RequestID:          payload.RequestID,
		Status:             http.StatusOK,
		QueueDepth:         a.queueDepth.Load(),
		ActiveJobs:         a.activeJobs.Load(),
		TemperatureCelsius: a.temperatureValue(),
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"message":            "work finished",
		"service":            a.config.serviceName,
		"instanceId":         a.config.instanceID,
		"taskType":           payload.TaskType,
		"requestId":          payload.RequestID,
		"queueDepth":         a.queueDepth.Load(),
		"activeJobs":         a.activeJobs.Load(),
		"temperatureCelsius": a.temperatureValue(),
		"time":               time.Now().Format(time.RFC3339),
	})
}

func (a *app) withMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(recorder, r)

		codeLabel := strconv.Itoa(recorder.statusCode)
		httpRequestsTotal.WithLabelValues(r.URL.Path, r.Method, codeLabel).Inc()
		httpRequestDurationSeconds.WithLabelValues(r.URL.Path, r.Method, codeLabel).Observe(time.Since(startedAt).Seconds())
		a.requestCount.Add(1)
		a.writeLog(logEntry{
			Level:              "info",
			Event:              "http_request_processed",
			Path:               r.URL.Path,
			Method:             r.Method,
			Status:             recorder.statusCode,
			QueueDepth:         a.queueDepth.Load(),
			ActiveJobs:         a.activeJobs.Load(),
			TemperatureCelsius: a.temperatureValue(),
		})
	})
}

func (a *app) refreshGatewaysLoop() {
	ticker := time.NewTicker(a.config.peerRefreshInterval)
	defer ticker.Stop()

	a.refreshGateways()
	for range ticker.C {
		a.refreshGateways()
	}
}

func (a *app) refreshGateways() {
	gateways, err := a.fetchPeersFromConsul()
	if err != nil {
		a.writeLog(logEntry{
			Level:         "error",
			Event:         "gateway_refresh_failed",
			TargetService: a.config.targetServiceName,
			Detail:        err.Error(),
		})
		return
	}

	a.gatewaysMu.Lock()
	a.gateways = gateways
	a.gatewaysMu.Unlock()

	discoveredGateways.WithLabelValues(a.config.serviceName, a.config.targetServiceName).Set(float64(len(gateways)))
	a.writeLog(logEntry{
		Level:         "info",
		Event:         "gateway_list_refreshed",
		TargetService: a.config.targetServiceName,
		GatewayCount:  len(gateways),
	})
}

func (a *app) reportLoop() {
	ticker := time.NewTicker(a.config.reportInterval)
	defer ticker.Stop()

	a.writeLog(logEntry{
		Level:         "info",
		Event:         "report_loop_started",
		TargetService: a.config.targetServiceName,
	})

	for range ticker.C {
		gateway, ok := a.pickRandomGateway()
		if !ok {
			a.writeLog(logEntry{
				Level:         "info",
				Event:         "report_skipped_no_gateway",
				TargetService: a.config.targetServiceName,
			})
			continue
		}

		_ = a.sendReport(gateway)
	}
}

func (a *app) sendReport(gateway peer) error {
	payload := workerReport{
		FromService:        a.config.serviceName,
		FromInstance:       a.config.instanceID,
		QueueDepth:         int(a.queueDepth.Load()),
		ActiveJobs:         int(a.activeJobs.Load()),
		TemperatureCelsius: a.temperatureValue(),
		Message:            "worker status report",
		SentAt:             time.Now().Format(time.RFC3339),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("http://%s:%d/worker/report", gateway.Address, gateway.Port)
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := a.httpClient.Do(request)
	if err != nil {
		reportsSentTotal.WithLabelValues(a.config.targetServiceName, "error").Inc()
		a.writeLog(logEntry{
			Level:         "error",
			Event:         "report_send_failed",
			TargetService: a.config.targetServiceName,
			PeerID:        gateway.ID,
			PeerAddress:   peerAddress(gateway),
			Detail:        err.Error(),
		})
		return err
	}
	defer response.Body.Close()

	result := "success"
	if response.StatusCode >= http.StatusBadRequest {
		result = "error"
	}

	reportsSentTotal.WithLabelValues(a.config.targetServiceName, result).Inc()
	a.writeLog(logEntry{
		Level:              levelForResult(result),
		Event:              "report_sent",
		TargetService:      a.config.targetServiceName,
		PeerID:             gateway.ID,
		PeerAddress:        peerAddress(gateway),
		Status:             response.StatusCode,
		QueueDepth:         a.queueDepth.Load(),
		ActiveJobs:         a.activeJobs.Load(),
		TemperatureCelsius: a.temperatureValue(),
	})

	if result == "error" {
		return fmt.Errorf("gateway returned status %d", response.StatusCode)
	}
	return nil
}

func (a *app) fetchPeersFromConsul() ([]peer, error) {
	url := fmt.Sprintf("%s/v1/health/service/%s?passing=true", strings.TrimRight(a.config.consulHTTPAddr, "/"), a.config.targetDiscoveryServiceName)
	response, err := a.httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("consul returned status %d", response.StatusCode)
	}

	var entries []consulServiceEntry
	if err := json.NewDecoder(response.Body).Decode(&entries); err != nil {
		return nil, err
	}

	gateways := make([]peer, 0, len(entries))
	for _, entry := range entries {
		address := entry.Service.Address
		if address == "" {
			address = entry.Node.Address
		}
		if address == "" || entry.Service.Port == 0 {
			continue
		}
		gateways = append(gateways, peer{
			ID:      entry.Service.ID,
			Service: entry.Service.Service,
			Address: address,
			Port:    entry.Service.Port,
		})
	}

	slices.SortFunc(gateways, func(a peer, b peer) int {
		if a.Address == b.Address {
			return strings.Compare(a.ID, b.ID)
		}
		return strings.Compare(a.Address, b.Address)
	})

	return gateways, nil
}

func (a *app) snapshotGateways() []peer {
	a.gatewaysMu.RLock()
	defer a.gatewaysMu.RUnlock()

	gateways := make([]peer, len(a.gateways))
	copy(gateways, a.gateways)
	return gateways
}

func (a *app) pickRandomGateway() (peer, bool) {
	gateways := a.snapshotGateways()
	if len(gateways) == 0 {
		return peer{}, false
	}

	a.randMu.Lock()
	defer a.randMu.Unlock()
	return gateways[a.random.Intn(len(gateways))], true
}

func (a *app) randomizeTemperature(minDelta, maxDelta float64) {
	a.randMu.Lock()
	delta := minDelta + a.random.Float64()*(maxDelta-minDelta)
	a.randMu.Unlock()

	value := a.temperatureValue() + delta
	if value < 35 {
		value = 35
	}
	if value > 82 {
		value = 82
	}

	a.setTemperature(value)
	temperatureGauge.Set(value)
}

func (a *app) temperatureValue() float64 {
	return mathFromBits(a.temperature.Load())
}

func (a *app) setTemperature(value float64) {
	a.temperature.Store(bitsFromMath(value))
}

func (a *app) writeLog(entry logEntry) {
	entry.Service = a.config.serviceName
	entry.InstanceID = a.config.instanceID
	if entry.GatewayCount == 0 {
		entry.GatewayCount = len(a.snapshotGateways())
	}
	if entry.QueueDepth == 0 {
		entry.QueueDepth = a.queueDepth.Load()
	}
	if entry.ActiveJobs == 0 {
		entry.ActiveJobs = a.activeJobs.Load()
	}
	if entry.TemperatureCelsius == 0 {
		entry.TemperatureCelsius = a.temperatureValue()
	}
	entry.Timestamp = time.Now().Format(time.RFC3339)

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}

	a.logger.Println(string(data))
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envDurationMillisOrDefault(key string, fallbackMillis int) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return time.Duration(fallbackMillis) * time.Millisecond
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return time.Duration(fallbackMillis) * time.Millisecond
	}
	return time.Duration(parsed) * time.Millisecond
}

func ensureHTTPPrefix(value string) string {
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	return "http://" + value
}

func hostnameOrDefault(fallback string) string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return fallback
	}
	return hostname
}

func peerAddress(value peer) string {
	if value.Address == "" || value.Port == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", value.Address, value.Port)
}

func levelForResult(result string) string {
	if result == "error" {
		return "error"
	}
	return "info"
}

func bitsFromMath(value float64) uint64 {
	return math.Float64bits(value)
}

func mathFromBits(value uint64) float64 {
	return math.Float64frombits(value)
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}
