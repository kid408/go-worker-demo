package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
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

	"go-worker-demo/internal/sessionrpc"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
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

	sessionEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "go_worker_session_events_total",
			Help: "Total session events handled by workers.",
		},
		[]string{"action", "result"},
	)

	sessionEventDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "go_worker_session_event_duration_seconds",
			Help:    "Duration of session event handling on the worker in seconds.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.3, 0.5, 1, 2, 5},
		},
		[]string{"action", "result"},
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
			Help: "Current queue depth on the worker.",
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
			Help: "Current worker temperature in celsius.",
		},
	)

	minioUploadsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "go_worker_minio_upload_total",
			Help: "Total MinIO upload attempts from worker.",
		},
		[]string{"bucket", "result"},
	)
)

type config struct {
	serviceName                string
	targetServiceName          string
	targetDiscoveryServiceName string
	staticGatewayAddrs         []string
	instanceID                 string
	appPort                    string
	grpcPort                   string
	metricsPort                string
	consulHTTPAddr             string
	logPath                    string
	peerRefreshInterval        time.Duration
	reportInterval             time.Duration
	grpcRequestTimeout         time.Duration
	minioEndpoint              string
	minioAccessKey             string
	minioSecretKey             string
	minioBucket                string
	minioUseSSL                bool
}

type app struct {
	sessionrpc.UnimplementedWorkerServiceServer

	config     config
	startedAt  time.Time
	logger     *log.Logger
	httpClient *http.Client
	random     *rand.Rand
	randMu     sync.Mutex

	gatewaysMu sync.RWMutex
	gateways   []peer

	sessionsMu sync.RWMutex
	sessions   map[string]sessionState

	minioClient *minio.Client

	requestCount atomic.Uint64
	queueDepth   atomic.Int64
	activeJobs   atomic.Int64
	temperature  atomic.Uint64
}

type sessionState struct {
	SnapshotObjectKey string
	LoginAt           time.Time
	LastAction        string
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
	Action             string  `json:"action,omitempty"`
	EventID            string  `json:"event_id,omitempty"`
	SessionID          string  `json:"session_id,omitempty"`
	ClientID           string  `json:"client_id,omitempty"`
	Detail             string  `json:"detail,omitempty"`
	QueueDepth         int64   `json:"queue_depth,omitempty"`
	ActiveJobs         int64   `json:"active_jobs,omitempty"`
	TemperatureCelsius float64 `json:"temperature_celsius,omitempty"`
	GatewayCount       int     `json:"gateway_count,omitempty"`
	SnapshotObjectKey  string  `json:"snapshot_object_key,omitempty"`
	Timestamp          string  `json:"ts"`
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

func main() {
	prometheus.MustRegister(
		httpRequestsTotal,
		httpRequestDurationSeconds,
		processUp,
		discoveredGateways,
		sessionEventsTotal,
		sessionEventDurationSeconds,
		reportsSentTotal,
		queueDepthGauge,
		activeJobsGauge,
		temperatureGauge,
		minioUploadsTotal,
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
			Timeout: 3 * time.Second,
		},
		random:   rand.New(rand.NewSource(time.Now().UnixNano())),
		sessions: make(map[string]sessionState),
	}

	if cfg.minioEndpoint != "" {
		client, clientErr := minio.New(cfg.minioEndpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(cfg.minioAccessKey, cfg.minioSecretKey, ""),
			Secure: cfg.minioUseSSL,
		})
		if clientErr != nil {
			log.Fatalf("init minio client failed: %v", clientErr)
		}
		application.minioClient = client
		if err := application.ensureMinioBucket(context.Background()); err != nil {
			log.Fatalf("ensure minio bucket failed: %v", err)
		}
	}

	processUp.Set(1)
	application.queueDepth.Store(0)
	application.activeJobs.Store(0)
	application.setTemperature(41.2)
	queueDepthGauge.Set(0)
	activeJobsGauge.Set(0)
	temperatureGauge.Set(41.2)
	discoveredGateways.WithLabelValues(cfg.serviceName, cfg.targetServiceName).Set(0)

	appMux := http.NewServeMux()
	appMux.HandleFunc("/", application.handleRoot)
	appMux.HandleFunc("/healthz", application.handleHealth)
	appMux.HandleFunc("/health", application.handleHealth)
	appMux.HandleFunc("/gateways", application.handleGateways)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())

	httpServer := &http.Server{
		Addr:              ":" + cfg.appPort,
		Handler:           application.withMetrics(appMux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	metricsServer := &http.Server{
		Addr:              ":" + cfg.metricsPort,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	grpcListener, err := net.Listen("tcp", ":"+cfg.grpcPort)
	if err != nil {
		log.Fatalf("listen grpc failed: %v", err)
	}

	grpcServer := grpc.NewServer(sessionrpc.DefaultServerOptions()...)
	sessionrpc.RegisterWorkerServiceServer(grpcServer, application)

	go application.refreshGatewaysLoop()
	go application.reportLoop()

	go func() {
		application.writeLog(logEntry{Level: "info", Event: "http_server_starting"})
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server failed: %v", err)
		}
	}()

	go func() {
		application.writeLog(logEntry{Level: "info", Event: "metrics_server_starting"})
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("metrics server failed: %v", err)
		}
	}()

	go func() {
		application.writeLog(logEntry{Level: "info", Event: "grpc_server_starting"})
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Fatalf("grpc server failed: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	processUp.Set(0)
	application.writeLog(logEntry{Level: "info", Event: "shutdown_signal_received"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	grpcServer.GracefulStop()
	_ = httpServer.Shutdown(ctx)
	_ = metricsServer.Shutdown(ctx)
}

func (a *app) ProcessSessionEvent(ctx context.Context, event *sessionrpc.SessionEvent) (*sessionrpc.SessionResult, error) {
	startedAt := time.Now()
	result := "success"

	a.queueDepth.Add(1)
	queueDepthGauge.Set(float64(a.queueDepth.Load()))
	a.activeJobs.Add(1)
	activeJobsGauge.Set(float64(a.activeJobs.Load()))
	a.randomizeTemperature(0.2, 1.8)

	defer func() {
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
		a.randomizeTemperature(-0.3, 0.9)
		sessionEventsTotal.WithLabelValues(event.Action, result).Inc()
		sessionEventDurationSeconds.WithLabelValues(event.Action, result).Observe(time.Since(startedAt).Seconds())
	}()

	if !isValidAction(event.Action) {
		result = "error"
		return &sessionrpc.SessionResult{
			EventId:            event.EventId,
			SessionId:          event.SessionId,
			Action:             event.Action,
			Result:             "error",
			Message:            "invalid action",
			GatewayId:          event.GatewayId,
			WorkerId:           a.config.instanceID,
			QueueDepth:         a.queueDepth.Load(),
			ActiveJobs:         a.activeJobs.Load(),
			TemperatureCelsius: a.temperatureValue(),
			ProcessedAt:        time.Now().Format(time.RFC3339),
		}, nil
	}

	a.writeLog(logEntry{
		Level:      "info",
		Event:      "session_event_received",
		Action:     event.Action,
		EventID:    event.EventId,
		SessionID:  event.SessionId,
		ClientID:   event.ClientId,
		QueueDepth: a.queueDepth.Load(),
		ActiveJobs: a.activeJobs.Load(),
	})

	time.Sleep(a.processingDelay(event.Action))

	snapshotKey := a.handleSessionState(ctx, event)

	a.writeLog(logEntry{
		Level:              "info",
		Event:              "session_event_processed",
		Action:             event.Action,
		EventID:            event.EventId,
		SessionID:          event.SessionId,
		ClientID:           event.ClientId,
		QueueDepth:         a.queueDepth.Load(),
		ActiveJobs:         a.activeJobs.Load(),
		TemperatureCelsius: a.temperatureValue(),
		SnapshotObjectKey:  snapshotKey,
	})

	return &sessionrpc.SessionResult{
		EventId:            event.EventId,
		SessionId:          event.SessionId,
		Action:             event.Action,
		Result:             "success",
		Message:            "worker handled session event",
		GatewayId:          event.GatewayId,
		WorkerId:           a.config.instanceID,
		SnapshotObjectKey:  snapshotKey,
		QueueDepth:         a.queueDepth.Load(),
		ActiveJobs:         a.activeJobs.Load(),
		TemperatureCelsius: a.temperatureValue(),
		ProcessedAt:        time.Now().Format(time.RFC3339),
	}, nil
}

func (a *app) handleSessionState(ctx context.Context, event *sessionrpc.SessionEvent) string {
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()

	state := a.sessions[event.SessionId]

	switch event.Action {
	case sessionrpc.ActionLogin:
		snapshotKey := a.buildSnapshotObjectKey(event.SessionId)
		if a.minioClient != nil {
			if err := a.uploadLoginSnapshot(ctx, snapshotKey, event); err != nil {
				a.writeLog(logEntry{
					Level:     "error",
					Event:     "minio_upload_failed",
					Action:    event.Action,
					EventID:   event.EventId,
					SessionID: event.SessionId,
					ClientID:  event.ClientId,
					Detail:    err.Error(),
				})
			}
		}
		state = sessionState{
			SnapshotObjectKey: snapshotKey,
			LoginAt:           time.Now(),
			LastAction:        event.Action,
		}
		a.sessions[event.SessionId] = state
		return snapshotKey
	case sessionrpc.ActionHeartbeat:
		state.LastAction = event.Action
		if state.LoginAt.IsZero() {
			state.LoginAt = time.Now()
		}
		a.sessions[event.SessionId] = state
		return state.SnapshotObjectKey
	case sessionrpc.ActionLogout:
		delete(a.sessions, event.SessionId)
		state.LastAction = event.Action
		return state.SnapshotObjectKey
	default:
		return state.SnapshotObjectKey
	}
}

func (a *app) uploadLoginSnapshot(ctx context.Context, objectKey string, event *sessionrpc.SessionEvent) error {
	if a.minioClient == nil {
		return nil
	}

	payload := map[string]any{
		"event_id":     event.EventId,
		"session_id":   event.SessionId,
		"client_id":    event.ClientId,
		"user_id":      event.UserId,
		"device_id":    event.DeviceId,
		"action":       event.Action,
		"payload":      event.Payload,
		"gateway_id":   event.GatewayId,
		"worker_id":    a.config.instanceID,
		"generated_at": time.Now().Format(time.RFC3339),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		minioUploadsTotal.WithLabelValues(a.config.minioBucket, "marshal_error").Inc()
		return err
	}

	reader := bytes.NewReader(body)
	_, err = a.minioClient.PutObject(ctx, a.config.minioBucket, objectKey, reader, int64(len(body)), minio.PutObjectOptions{
		ContentType: "application/json",
	})
	if err != nil {
		minioUploadsTotal.WithLabelValues(a.config.minioBucket, "error").Inc()
		return err
	}

	minioUploadsTotal.WithLabelValues(a.config.minioBucket, "success").Inc()
	return nil
}

func (a *app) ensureMinioBucket(ctx context.Context) error {
	exists, err := a.minioClient.BucketExists(ctx, a.config.minioBucket)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return a.minioClient.MakeBucket(ctx, a.config.minioBucket, minio.MakeBucketOptions{})
}

func (a *app) handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":                a.config.serviceName,
		"targetService":          a.config.targetServiceName,
		"targetDiscoveryService": a.config.targetDiscoveryServiceName,
		"instanceId":             a.config.instanceID,
		"appPort":                a.config.appPort,
		"grpcPort":               a.config.grpcPort,
		"metricsPort":            a.config.metricsPort,
		"gatewayCount":           len(a.snapshotGateways()),
		"queueDepth":             a.queueDepth.Load(),
		"activeJobs":             a.activeJobs.Load(),
		"temperatureCelsius":     a.temperatureValue(),
		"trackedSessions":        a.sessionCount(),
		"requestCount":           a.requestCount.Add(1),
		"uptimeSec":              int64(time.Since(a.startedAt).Seconds()),
		"time":                   time.Now().Format(time.RFC3339),
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
		"trackedSessions":        a.sessionCount(),
		"time":                   time.Now().Format(time.RFC3339),
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

func (a *app) withMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(recorder, r)

		codeLabel := strconv.Itoa(recorder.statusCode)
		httpRequestsTotal.WithLabelValues(r.URL.Path, r.Method, codeLabel).Inc()
		httpRequestDurationSeconds.WithLabelValues(r.URL.Path, r.Method, codeLabel).Observe(time.Since(startedAt).Seconds())
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
	var (
		gateways []peer
		err      error
	)

	if len(a.config.staticGatewayAddrs) > 0 {
		gateways = buildStaticPeers(a.config.targetServiceName, a.config.staticGatewayAddrs)
	} else {
		gateways, err = a.fetchPeersFromConsul()
		if err != nil {
			a.writeLog(logEntry{
				Level:         "error",
				Event:         "gateway_refresh_failed",
				TargetService: a.config.targetServiceName,
				Detail:        err.Error(),
			})
			return
		}
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
	target := fmt.Sprintf("%s:%d", gateway.Address, gateway.Port)
	conn, err := sessionrpc.DialContext(context.Background(), target)
	if err != nil {
		reportsSentTotal.WithLabelValues(a.config.targetServiceName, "error").Inc()
		return err
	}
	defer conn.Close()

	client := sessionrpc.NewGatewayServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), a.config.grpcRequestTimeout)
	defer cancel()

	_, err = client.ReportWorkerStatus(ctx, &sessionrpc.WorkerStatusReport{
		WorkerId:           a.config.instanceID,
		GatewayId:          gateway.ID,
		QueueDepth:         a.queueDepth.Load(),
		ActiveJobs:         a.activeJobs.Load(),
		TemperatureCelsius: a.temperatureValue(),
		OnlineSessions:     int64(a.sessionCount()),
		Message:            "worker grpc status report",
		SentAt:             time.Now().Format(time.RFC3339),
	})
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

	reportsSentTotal.WithLabelValues(a.config.targetServiceName, "success").Inc()
	a.writeLog(logEntry{
		Level:              "info",
		Event:              "report_sent",
		TargetService:      a.config.targetServiceName,
		PeerID:             gateway.ID,
		PeerAddress:        peerAddress(gateway),
		QueueDepth:         a.queueDepth.Load(),
		ActiveJobs:         a.activeJobs.Load(),
		TemperatureCelsius: a.temperatureValue(),
	})
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

	peers := make([]peer, 0, len(entries))
	for _, entry := range entries {
		address := strings.TrimSpace(entry.Service.Address)
		if address == "" {
			address = strings.TrimSpace(entry.Node.Address)
		}
		if address == "" || entry.Service.Port == 0 {
			continue
		}
		peers = append(peers, peer{
			ID:      entry.Service.ID,
			Service: entry.Service.Service,
			Address: address,
			Port:    entry.Service.Port,
		})
	}

	slices.SortFunc(peers, func(a, b peer) int {
		return strings.Compare(peerAddress(a), peerAddress(b))
	})
	return peers, nil
}

func (a *app) pickRandomGateway() (peer, bool) {
	gateways := a.snapshotGateways()
	if len(gateways) == 0 {
		return peer{}, false
	}
	if len(gateways) == 1 {
		return gateways[0], true
	}
	return gateways[a.randomInt(len(gateways))], true
}

func buildStaticPeers(serviceName string, addresses []string) []peer {
	peers := make([]peer, 0, len(addresses))
	for idx, raw := range addresses {
		address, port, ok := parseStaticAddress(raw)
		if !ok {
			continue
		}
		peers = append(peers, peer{
			ID:      fmt.Sprintf("static-%s-%02d", serviceName, idx+1),
			Service: serviceName,
			Address: address,
			Port:    port,
		})
	}
	return peers
}

func parseStaticAddress(raw string) (string, int, bool) {
	host, portText, ok := strings.Cut(strings.TrimSpace(raw), ":")
	if !ok || strings.TrimSpace(host) == "" || strings.TrimSpace(portText) == "" {
		return "", 0, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(portText))
	if err != nil || port <= 0 {
		return "", 0, false
	}
	return strings.TrimSpace(host), port, true
}

func (a *app) snapshotGateways() []peer {
	a.gatewaysMu.RLock()
	defer a.gatewaysMu.RUnlock()
	result := make([]peer, len(a.gateways))
	copy(result, a.gateways)
	return result
}

func (a *app) randomInt(max int) int {
	a.randMu.Lock()
	defer a.randMu.Unlock()
	return a.random.Intn(max)
}

func (a *app) setTemperature(value float64) {
	a.temperature.Store(math.Float64bits(value))
}

func (a *app) temperatureValue() float64 {
	return math.Float64frombits(a.temperature.Load())
}

func (a *app) randomizeTemperature(minDelta, maxDelta float64) {
	a.randMu.Lock()
	defer a.randMu.Unlock()

	current := a.temperatureValue()
	delta := minDelta + a.random.Float64()*(maxDelta-minDelta)
	next := current + delta
	if next < 36 {
		next = 36
	}
	if next > 78 {
		next = 78
	}
	next = math.Round(next*10) / 10
	a.setTemperature(next)
	temperatureGauge.Set(next)
}

func (a *app) processingDelay(action string) time.Duration {
	base := 50
	switch action {
	case sessionrpc.ActionLogin:
		base = 160
	case sessionrpc.ActionHeartbeat:
		base = 60
	case sessionrpc.ActionLogout:
		base = 110
	}
	return time.Duration(base+a.randomInt(120)) * time.Millisecond
}

func (a *app) sessionCount() int {
	a.sessionsMu.RLock()
	defer a.sessionsMu.RUnlock()
	return len(a.sessions)
}

func (a *app) buildSnapshotObjectKey(sessionID string) string {
	now := time.Now()
	return fmt.Sprintf("login-snapshots/%04d/%02d/%02d/%s.json", now.Year(), now.Month(), now.Day(), sessionID)
}

func loadConfig() config {
	serviceName := envOrDefault("SERVICE_NAME", "worker")
	targetServiceName := envOrDefault("TARGET_SERVICE_NAME", "gateway")
	targetDiscoveryServiceName := envOrDefault("TARGET_DISCOVERY_SERVICE_NAME", "gateway-grpc")
	instanceID := envOrDefault("INSTANCE_ID", envOrDefault("NOMAD_ALLOC_ID", hostnameOrDefault()))

	return config{
		serviceName:                serviceName,
		targetServiceName:          targetServiceName,
		targetDiscoveryServiceName: targetDiscoveryServiceName,
		staticGatewayAddrs:         envCSV("STATIC_GATEWAY_ADDRS"),
		instanceID:                 instanceID,
		appPort:                    envOrDefault("APP_PORT", "18081"),
		grpcPort:                   envOrDefault("GRPC_PORT", "19081"),
		metricsPort:                envOrDefault("METRICS_PORT", "12113"),
		consulHTTPAddr:             ensureHTTPPrefix(envOrDefault("CONSUL_HTTP_ADDR", "127.0.0.1:8500")),
		logPath:                    envOrDefault("APP_LOG_PATH", "/app/logs/go-worker-demo.log"),
		peerRefreshInterval:        envDurationMillisOrDefault("PEER_REFRESH_INTERVAL_MS", 5000),
		reportInterval:             envDurationMillisOrDefault("REPORT_INTERVAL_MS", 4000),
		grpcRequestTimeout:         envDurationMillisOrDefault("GRPC_REQUEST_TIMEOUT_MS", 3000),
		minioEndpoint:              envOrDefault("MINIO_ENDPOINT", ""),
		minioAccessKey:             envOrDefault("MINIO_ACCESS_KEY", ""),
		minioSecretKey:             envOrDefault("MINIO_SECRET_KEY", ""),
		minioBucket:                envOrDefault("MINIO_BUCKET", "login-snapshots"),
		minioUseSSL:                envBoolOrDefault("MINIO_USE_SSL", false),
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

func (a *app) writeLog(entry logEntry) {
	entry.Service = a.config.serviceName
	entry.InstanceID = a.config.instanceID
	entry.Timestamp = time.Now().Format(time.RFC3339)
	body, err := json.Marshal(entry)
	if err != nil {
		a.logger.Printf(`{"level":"error","event":"log_marshal_failed","service":"%s","instance_id":"%s","detail":%q,"ts":"%s"}`,
			a.config.serviceName,
			a.config.instanceID,
			err.Error(),
			time.Now().Format(time.RFC3339),
		)
		return
	}
	a.logger.Println(string(body))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envDurationMillisOrDefault(key string, fallback int) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return time.Duration(fallback) * time.Millisecond
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return time.Duration(fallback) * time.Millisecond
	}
	return time.Duration(value) * time.Millisecond
}

func envBoolOrDefault(key string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envCSV(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func ensureHTTPPrefix(value string) string {
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	return "http://" + value
}

func hostnameOrDefault() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "unknown-host"
	}
	return name
}

func peerAddress(item peer) string {
	return fmt.Sprintf("%s:%d", item.Address, item.Port)
}

func isValidAction(action string) bool {
	switch action {
	case sessionrpc.ActionLogin, sessionrpc.ActionHeartbeat, sessionrpc.ActionLogout:
		return true
	default:
		return false
	}
}
