package metrics

import (
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Registry struct {
	registry           *prometheus.Registry
	uploads            *prometheus.CounterVec
	uploadBytes        *prometheus.CounterVec
	uploadDuration     *prometheus.HistogramVec
	downloads          *prometheus.CounterVec
	resolves           *prometheus.CounterVec
	blobDedup          *prometheus.CounterVec
	publishes          *prometheus.CounterVec
	promotions         *prometheus.CounterVec
	signingFailures    *prometheus.CounterVec
	stagingOldestAge   prometheus.Gauge
	jobBacklog         *prometheus.GaugeVec
	dependencyRequests *prometheus.CounterVec
	dependencyDuration *prometheus.HistogramVec
}

func NewRegistry() *Registry {
	registry := prometheus.NewRegistry()
	metrics := &Registry{
		registry: registry,
		uploads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "artifact_repository", Name: "uploads_total", Help: "Artifact upload attempts.",
		}, []string{"result"}),
		uploadBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "artifact_repository", Name: "upload_bytes_total", Help: "Artifact upload bytes.",
		}, []string{"result"}),
		uploadDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "artifact_repository", Name: "upload_duration_seconds", Help: "Artifact upload duration.",
			Buckets: prometheus.DefBuckets,
		}, []string{"result"}),
		downloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "artifact_repository", Name: "downloads_total", Help: "Artifact download attempts.",
		}, []string{"result"}),
		resolves: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "artifact_repository", Name: "resolves_total", Help: "Channel resolve attempts.",
		}, []string{"result"}),
		blobDedup: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "artifact_repository", Name: "blob_dedup_total", Help: "Blob deduplication outcomes.",
		}, []string{"result"}),
		publishes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "artifact_repository", Name: "publishes_total", Help: "Release publish attempts.",
		}, []string{"result"}),
		promotions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "artifact_repository", Name: "promotions_total", Help: "Channel promotion attempts.",
		}, []string{"result"}),
		signingFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "artifact_repository", Name: "signing_failures_total", Help: "Manifest signing failures.",
		}, []string{"code"}),
		stagingOldestAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "artifact_repository", Name: "staging_oldest_age_seconds", Help: "Age of the oldest pending staging object.",
		}),
		jobBacklog: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "artifact_repository", Name: "job_backlog", Help: "Pending or running background jobs.",
		}, []string{"kind"}),
		dependencyRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "artifact_repository", Name: "dependency_requests_total", Help: "Dependency request outcomes.",
		}, []string{"dependency", "result"}),
		dependencyDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "artifact_repository", Name: "dependency_duration_seconds", Help: "Dependency request duration.",
			Buckets: prometheus.DefBuckets,
		}, []string{"dependency", "result"}),
	}
	registry.MustRegister(
		metrics.uploads,
		metrics.uploadBytes,
		metrics.uploadDuration,
		metrics.downloads,
		metrics.resolves,
		metrics.blobDedup,
		metrics.publishes,
		metrics.promotions,
		metrics.signingFailures,
		metrics.stagingOldestAge,
		metrics.jobBacklog,
		metrics.dependencyRequests,
		metrics.dependencyDuration,
	)
	return metrics
}

func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.registry, promhttp.HandlerOpts{})
}

func (r *Registry) InstrumentHTTP(next http.Handler) http.Handler {
	if next == nil {
		return http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		operation := metricOperation(request)
		if operation == "" {
			next.ServeHTTP(w, request)
			return
		}
		started := time.Now()
		response := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(response, request)
		result := metricResult(response.status)
		switch operation {
		case "upload":
			r.ObserveUpload(result, request.ContentLength, time.Since(started))
		case "download":
			r.ObserveDownload(result)
		case "resolve":
			r.ObserveResolve(result)
		case "publish":
			r.ObservePublish(result)
		case "promotion":
			r.ObservePromotion(result)
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func metricOperation(request *http.Request) string {
	path := request.URL.Path
	switch {
	case request.Method == http.MethodPut && strings.Contains(path, "/artifacts/"):
		return "upload"
	case (request.Method == http.MethodGet || request.Method == http.MethodHead) && strings.Contains(path, "/artifacts/"):
		return "download"
	case request.Method == http.MethodGet && strings.HasSuffix(path, "/resolve"):
		return "resolve"
	case request.Method == http.MethodPost && strings.HasSuffix(path, "/publish"):
		return "publish"
	case request.Method == http.MethodPost && strings.HasSuffix(path, "/promotions"):
		return "promotion"
	default:
		return ""
	}
}

func metricResult(status int) string {
	switch {
	case status >= 200 && status < 400:
		return "success"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "denied"
	case status == http.StatusNotFound:
		return "not_found"
	case status == http.StatusConflict:
		return "conflict"
	default:
		return "failed"
	}
}

func (r *Registry) ObserveUpload(result string, size int64, duration time.Duration) {
	result = boundedResult(result)
	r.uploads.WithLabelValues(result).Inc()
	if size > 0 {
		r.uploadBytes.WithLabelValues(result).Add(float64(size))
	}
	r.uploadDuration.WithLabelValues(result).Observe(duration.Seconds())
}

func (r *Registry) ObserveDownload(result string) {
	r.downloads.WithLabelValues(boundedResult(result)).Inc()
}

func (r *Registry) ObserveResolve(result string) {
	r.resolves.WithLabelValues(boundedResult(result)).Inc()
}

func (r *Registry) ObserveBlobDedup(hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	r.blobDedup.WithLabelValues(result).Inc()
}

func (r *Registry) ObservePublish(result string) {
	r.publishes.WithLabelValues(boundedResult(result)).Inc()
}

func (r *Registry) ObservePromotion(result string) {
	r.promotions.WithLabelValues(boundedResult(result)).Inc()
}

func (r *Registry) ObserveSigningFailure(code string) {
	r.signingFailures.WithLabelValues(boundedSigningCode(code)).Inc()
}

func (r *Registry) SetStagingOldestAge(age time.Duration) {
	seconds := age.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	r.stagingOldestAge.Set(seconds)
}

func (r *Registry) SetJobBacklog(kind string, count int) {
	if count < 0 {
		count = 0
	}
	r.jobBacklog.WithLabelValues(boundedJobKind(kind)).Set(float64(count))
}

func (r *Registry) ObserveDependency(dependency, result string, duration time.Duration) {
	dependency = boundedDependency(dependency)
	result = boundedDependencyResult(result)
	r.dependencyRequests.WithLabelValues(dependency, result).Inc()
	r.dependencyDuration.WithLabelValues(dependency, result).Observe(duration.Seconds())
}

func boundedResult(value string) string {
	switch value {
	case "success", "failed", "denied", "not_found", "conflict", "canceled":
		return value
	default:
		return "unknown"
	}
}

func boundedSigningCode(value string) string {
	switch value {
	case "invalid_key", "sign_failed", "verify_failed":
		return value
	default:
		return "unknown"
	}
}

func boundedJobKind(value string) string {
	switch value {
	case "cleanup_blob", "cleanup_upload", "cleanup_idempotency", "recover_publish":
		return value
	default:
		return "unknown"
	}
}

func boundedDependency(value string) string {
	switch value {
	case "postgres", "filesystem":
		return value
	default:
		return "unknown"
	}
}

func boundedDependencyResult(value string) string {
	switch value {
	case "success", "failed", "timeout":
		return value
	default:
		return "unknown"
	}
}
