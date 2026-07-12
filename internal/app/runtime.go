package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/api"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/artifact"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/blob"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/channel"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/config"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/jobs"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/metrics"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/ratelimit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/release"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/repository"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/signing"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
)

type serviceRuntime struct {
	pool        *pgxpool.Pool
	store       *storage.MinIO
	signer      *signing.Ed25519
	clock       clock.System
	ids         id.UUIDGenerator
	audit       *audit.Service
	idempotency *idempotency.Service
	blobs       *blob.Service
	publisher   *release.PublishService
	metrics     *metrics.Registry
}

func openCheckedDatabase(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := database.Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := database.CheckSchemaVersion(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("check database schema before startup: %w", err)
	}
	return pool, nil
}

func newServiceRuntime(ctx context.Context, cfg config.Config, random io.Reader) (_ *serviceRuntime, err error) {
	pool, err := openCheckedDatabase(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			pool.Close()
		}
	}()
	store, err := storage.NewMinIO(storage.MinIOOptions{
		Endpoint: cfg.MinIOEndpoint, PublicEndpoint: cfg.MinIOPublicEndpoint,
		AccessKey: cfg.MinIOAccessKey, SecretKey: cfg.MinIOSecretKey,
		Bucket: cfg.MinIOBucket, Region: cfg.MinIORegion, UseTLS: cfg.MinIOUseTLS,
	})
	if err != nil {
		return nil, err
	}
	signer, err := signing.LoadEd25519(cfg.SigningPrivateKeyFile, cfg.SigningPublicKeyFile)
	if err != nil {
		return nil, err
	}
	sealer, err := idempotency.NewSealer(cfg.IdempotencyResponseKey, random)
	if err != nil {
		return nil, err
	}
	systemClock := clock.System{}
	ids := id.UUIDGenerator{}
	auditService := audit.NewService(pool)
	idempotencyService := idempotency.NewService(pool, sealer, systemClock.Now)
	metricRegistry := metrics.NewRegistry()
	blobService, err := blob.NewService(blob.Options{Pool: pool, Clock: systemClock, Lease: cfg.UploadLease})
	if err != nil {
		return nil, err
	}
	publisher, err := release.NewPublishService(release.PublishServiceOptions{
		Pool: pool, Blobs: blobService, Store: store, Signer: signer,
		Audit: auditService, Idempotency: idempotencyService,
		Clock: systemClock, IDs: ids, LeaseDuration: cfg.PublishLease,
		Heartbeat: cfg.PublishHeartbeat, IdempotencyTTL: cfg.IdempotencyTTL, Metrics: metricRegistry,
	})
	if err != nil {
		return nil, err
	}
	if err := registerSigningKey(ctx, pool, signer); err != nil {
		return nil, err
	}
	return &serviceRuntime{
		pool: pool, store: store, signer: signer, clock: systemClock, ids: ids,
		audit: auditService, idempotency: idempotencyService, blobs: blobService,
		publisher: publisher, metrics: metricRegistry,
	}, nil
}

func (runtime *serviceRuntime) Close() {
	runtime.pool.Close()
}

func registerSigningKey(ctx context.Context, pool *pgxpool.Pool, signer *signing.Ed25519) error {
	publicKey := signer.PublicKey()
	fingerprint := sha256.Sum256(publicKey)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin signing key registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "UPDATE signing_keys SET active = false WHERE active AND key_id <> $1", signer.KeyID()); err != nil {
		return fmt.Errorf("deactivate prior signing key: %w", err)
	}
	if _, err := db.New(tx).UpsertSigningKey(ctx, db.UpsertSigningKeyParams{
		KeyID: signer.KeyID(), PublicKey: publicKey,
		Fingerprint: hex.EncodeToString(fingerprint[:]), Active: true,
	}); err != nil {
		return fmt.Errorf("register active signing key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit signing key registration: %w", err)
	}
	return nil
}

func runAPI(ctx context.Context, cfg config.Config, random io.Reader) error {
	runtime, err := newServiceRuntime(ctx, cfg, random)
	if err != nil {
		return err
	}
	defer runtime.Close()
	requestLimiter, err := newRateLimiter(cfg)
	if err != nil {
		return err
	}
	authService, err := auth.NewService(auth.ServiceOptions{
		Pool: runtime.pool, Idempotency: runtime.idempotency, Audit: runtime.audit,
		Pepper: cfg.TokenPepper, Random: random, IDs: runtime.ids, Clock: runtime.clock,
		IdempotencyTTL: cfg.IdempotencyTTL,
	})
	if err != nil {
		return err
	}
	repositoryService, err := repository.NewService(repository.Options{
		Pool: runtime.pool, Idempotency: runtime.idempotency, Audit: runtime.audit,
		IdempotencyTTL: cfg.IdempotencyTTL,
	})
	if err != nil {
		return err
	}
	artifactService, err := artifact.NewService(artifact.Options{
		Pool: runtime.pool, Blobs: runtime.blobs, Store: runtime.store, Audit: runtime.audit,
		Clock: runtime.clock, IDs: runtime.ids, MaxUploadBytes: cfg.MaxUploadBytes,
		UploadIdleTimeout: cfg.UploadIdleTimeout, UploadLease: cfg.UploadLease,
		UploadHeartbeat: cfg.UploadHeartbeat, UploadMaxDuration: cfg.UploadMaxDuration,
		PresignTTL: cfg.PresignTTL, Metrics: runtime.metrics,
	})
	if err != nil {
		return err
	}
	packageService, err := release.NewPackageService(release.PackageServiceOptions{
		Pool: runtime.pool, Idempotency: runtime.idempotency, Audit: runtime.audit,
		IdempotencyTTL: cfg.IdempotencyTTL,
	})
	if err != nil {
		return err
	}
	draftService, err := release.NewDraftService(release.DraftServiceOptions{
		Pool: runtime.pool, Idempotency: runtime.idempotency, Audit: runtime.audit,
		IdempotencyTTL: cfg.IdempotencyTTL,
	})
	if err != nil {
		return err
	}
	channelService, err := channel.NewService(channel.Options{
		Pool: runtime.pool, Idempotency: runtime.idempotency, Audit: runtime.audit,
		Store: runtime.store, IdempotencyTTL: cfg.IdempotencyTTL, PresignTTL: cfg.PresignTTL,
	})
	if err != nil {
		return err
	}
	handler := runtime.metrics.InstrumentHTTP(api.NewServer(api.Dependencies{
		Readiness:        &runtimeReadiness{pool: runtime.pool, store: runtime.store, metrics: runtime.metrics},
		ReadinessTimeout: cfg.ReadinessTimeout, Metrics: runtime.metrics.Handler(),
		Authenticator: authService, RateLimiter: requestLimiter, Identity: authService, Audit: runtime.audit,
		Repositories: repositoryService, Artifacts: artifactService, Packages: packageService,
		Drafts: draftService, Publisher: runtime.publisher, Channels: channelService,
	}))
	return serveHTTP(ctx, cfg.HTTPAddr, handler)
}

func newRateLimiter(cfg config.Config) (*ratelimit.Limiter, error) {
	return ratelimit.New(ratelimit.Options{
		Read:              ratelimit.Rate{PerSecond: cfg.RateLimitReadRPS, Burst: cfg.RateLimitReadBurst},
		Mutation:          ratelimit.Rate{PerSecond: cfg.RateLimitMutationRPS, Burst: cfg.RateLimitMutationBurst},
		Upload:            ratelimit.Rate{PerSecond: cfg.RateLimitUploadRPS, Burst: cfg.RateLimitUploadBurst},
		UploadConcurrency: cfg.RateLimitUploadConcurrency,
		IdleTTL:           cfg.RateLimitIdleTTL,
	})
}

func runWorker(ctx context.Context, cfg config.Config, random io.Reader) error {
	runtime, err := newServiceRuntime(ctx, cfg, random)
	if err != nil {
		return err
	}
	defer runtime.Close()
	cleaner, err := jobs.NewCleaner(jobs.CleanupOptions{
		Pool: runtime.pool, Blobs: runtime.blobs, Store: runtime.store,
		Clock: runtime.clock, IDs: runtime.ids, OrphanRetention: cfg.OrphanRetention,
		DeleteLease: cfg.UploadLease, DeleteHeartbeat: cfg.UploadHeartbeat,
		DeleteTimeout: cfg.UploadIdleTimeout, DeleteQuarantine: cfg.UploadLease, BatchSize: 100,
	})
	if err != nil {
		return err
	}
	recovery, err := jobs.NewPublishRecovery(jobs.PublishRecoveryOptions{
		Pool: runtime.pool, Publisher: runtime.publisher, Clock: runtime.clock, BatchSize: 50,
	})
	if err != nil {
		return err
	}
	runner, err := jobs.NewRunner(jobs.RunnerOptions{
		Pool: runtime.pool, Clock: runtime.clock, IDs: runtime.ids,
		Definitions: []jobs.Definition{
			{Kind: jobs.KindCleanupBlob, Interval: 5 * time.Minute, Handler: observeWorkerMetrics(runtime, cleaner.CleanupBlobsOnce)},
			{Kind: jobs.KindCleanupUpload, Interval: 5 * time.Minute, Handler: observeWorkerMetrics(runtime, cleaner.CleanupUploadsOnce)},
			{Kind: jobs.KindCleanupIdempotency, Interval: time.Hour, Handler: observeWorkerMetrics(runtime, cleaner.CleanupIdempotencyOnce)},
			{Kind: jobs.KindRecoverPublish, Interval: 10 * time.Second, Handler: observeWorkerMetrics(runtime, recovery.RecoverPublishingOnce)},
		},
		Lease: 15 * time.Minute, PollInterval: time.Second,
		RetryBase: 5 * time.Second, RetryMax: 5 * time.Minute, MaxAttempts: 10,
	})
	if err != nil {
		return err
	}
	handler := api.NewServer(api.Dependencies{
		Readiness:        &runtimeReadiness{pool: runtime.pool, store: runtime.store, metrics: runtime.metrics},
		ReadinessTimeout: cfg.ReadinessTimeout, Metrics: runtime.metrics.Handler(),
	})
	return runTogether(ctx,
		func(ctx context.Context) error { return serveHTTP(ctx, cfg.HTTPAddr, handler) },
		runner.Run,
	)
}

func observeWorkerMetrics(runtime *serviceRuntime, handler jobs.Handler) jobs.Handler {
	return func(ctx context.Context) error {
		_ = refreshWorkerMetrics(ctx, runtime.pool, runtime.metrics, runtime.clock.Now())
		err := handler(ctx)
		_ = refreshWorkerMetrics(ctx, runtime.pool, runtime.metrics, runtime.clock.Now())
		return err
	}
}

func refreshWorkerMetrics(ctx context.Context, pool *pgxpool.Pool, registry *metrics.Registry, now time.Time) error {
	if pool == nil || registry == nil {
		return fmt.Errorf("refresh worker metrics: database and registry are required")
	}
	counts := map[string]int{
		"cleanup_blob": 0, "cleanup_upload": 0, "cleanup_idempotency": 0, "recover_publish": 0,
	}
	rows, err := pool.Query(ctx, `
		SELECT kind, count(*)
		FROM jobs
		WHERE state IN ('pending', 'running')
		GROUP BY kind`)
	if err != nil {
		return fmt.Errorf("query worker job backlog: %w", err)
	}
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			rows.Close()
			return fmt.Errorf("scan worker job backlog: %w", err)
		}
		counts[kind] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate worker job backlog: %w", err)
	}
	rows.Close()
	for kind, count := range counts {
		registry.SetJobBacklog(kind, count)
	}
	var oldest *time.Time
	if err := pool.QueryRow(ctx,
		"SELECT min(created_at) FROM upload_sessions WHERE cleanup_completed_at IS NULL",
	).Scan(&oldest); err != nil {
		return fmt.Errorf("query oldest staging session: %w", err)
	}
	age := time.Duration(0)
	if oldest != nil {
		age = now.UTC().Sub(oldest.UTC())
	}
	registry.SetStagingOldestAge(age)
	return nil
}

type runtimeReadiness struct {
	pool    *pgxpool.Pool
	store   storage.Store
	metrics *metrics.Registry
}

func (readiness *runtimeReadiness) Ready(ctx context.Context) error {
	started := time.Now()
	err := readiness.pool.Ping(ctx)
	readiness.observeDependency("postgres", err, started)
	if err != nil {
		return fmt.Errorf("PostgreSQL readiness: %w", err)
	}
	started = time.Now()
	err = readiness.store.Ready(ctx)
	readiness.observeDependency("minio", err, started)
	if err != nil {
		return err
	}
	return nil
}

func (readiness *runtimeReadiness) observeDependency(name string, err error, started time.Time) {
	if readiness.metrics == nil {
		return
	}
	result := "success"
	if err != nil {
		result = "failed"
		if errors.Is(err, context.DeadlineExceeded) {
			result = "timeout"
		}
	}
	readiness.metrics.ObserveDependency(name, result, time.Since(started))
}

func runTogether(ctx context.Context, processes ...func(context.Context) error) error {
	processContext, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(processes))
	for _, process := range processes {
		go func(process func(context.Context) error) {
			results <- process(processContext)
		}(process)
	}
	var processErrors []error
	for index := range processes {
		err := <-results
		if index == 0 {
			cancel()
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			processErrors = append(processErrors, err)
		}
	}
	return errors.Join(processErrors...)
}

func serveHTTP(ctx context.Context, address string, handler http.Handler) error {
	server := &http.Server{
		Addr: address, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20,
	}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP on %s: %w", address, err)
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownContext)
		serveErr := <-result
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(shutdownErr, serveErr)
	}
}
