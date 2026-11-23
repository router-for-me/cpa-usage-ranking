package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cpa-usage-ranking/internal/admin"
	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/avatars"
	"cpa-usage-ranking/internal/cache"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/config"
	"cpa-usage-ranking/internal/httpserver"
	"cpa-usage-ranking/internal/leaderboard"
	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/readiness"
	"cpa-usage-ranking/internal/registration"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/statistics"
	"cpa-usage-ranking/internal/storage"
	"github.com/gin-gonic/gin"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("service stopped", "error", err.Error())
		os.Exit(1)
	}
	logger.Info("service stopped")
}

func run(logger *slog.Logger) (err error) {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.AdminConfigInsecurePerm {
		logger.Warn("administrator configuration file has broad permissions; 0600 or 0640 recommended")
	}
	modelCatalog, err := models.Load(cfg.ModelsPath)
	if err != nil {
		return err
	}
	gin.SetMode(gin.ReleaseMode)
	avatarFiles := avatars.BuiltinFS()
	catalog, err := avatars.Load(avatarFiles)
	if err != nil {
		return err
	}
	var frontend *httpserver.Frontend
	if cfg.WebDir != "" {
		frontend, err = httpserver.OpenFrontend(cfg.WebDir)
		if err != nil {
			return err
		}
		defer frontend.Close()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := storage.Open(ctx, cfg.SQLitePath)
	if err != nil {
		return err
	}
	// Serve 返回前已停止接入并排空请求，再释放数据库连接。
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := storage.InitSchema(ctx, db.Writer); err != nil {
		return err
	}
	if err := reports.AdvanceWindow(ctx, db.Writer, time.Now()); err != nil {
		return fmt.Errorf("advance report window: %w", err)
	}
	modelRegistry, err := models.Register(ctx, db.Writer, modelCatalog)
	if err != nil {
		return err
	}
	logger.Info("sqlite initialized")
	repository := clients.NewRepository(db)
	reportAdmission, err := reports.NewAdmission(cfg.ReportIngestion, nil)
	if err != nil {
		return err
	}
	reportWriter, err := reports.NewWriter(reports.NewStore(db.Writer, modelRegistry, nil), cfg.ReportWriter)
	if err != nil {
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		err = errors.Join(err, reportWriter.Close(shutdown))
	}()
	windowCtx, stopWindow := context.WithCancel(context.Background())
	windowDone := make(chan struct{})
	go func() { defer close(windowDone); watchReportWindow(windowCtx, db.Writer, logger) }()
	defer func() { stopWindow(); <-windowDone }()
	maintenanceOptions := reports.DefaultMaintenanceOptions()
	maintenanceOptions.DailyRetentionDays = cfg.DailyRetentionDays
	maintenance, err := reports.NewMaintenance(db.Reader, db.Writer, reportWriter, logger, maintenanceOptions)
	if err != nil {
		return err
	}
	maintenanceCtx, stopMaintenance := context.WithCancel(context.Background())
	maintenanceDone := make(chan struct{})
	go func() { defer close(maintenanceDone); maintenance.Run(maintenanceCtx) }()
	// 先停止生产者，之后关闭 writer 并等待事务释放，最后才关闭数据库。
	defer func() { stopMaintenance(); <-maintenanceDone }()
	reader, err := statistics.NewReader(db.Reader, statistics.DefaultMaxCandidates, statistics.DefaultReadTimeout)
	if err != nil {
		return err
	}
	cacheOptions := cache.DefaultOptions()
	cacheOptions.Interval = cfg.CacheRefreshInterval
	cacheService, err := cache.New(reader, leaderboard.Items(), cacheOptions, logger)
	if err != nil {
		return err
	}
	cacheCtx, stopCache := context.WithCancel(context.Background())
	cacheDone := make(chan struct{})
	go func() {
		defer close(cacheDone)
		if err := cacheService.Run(cacheCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("cache worker stopped", "error", err.Error())
		}
	}()
	defer func() { stopCache(); <-cacheDone }()
	ready := readiness.New(db, modelRegistry, catalog, cacheService)
	router := httpserver.NewRouterWithReadiness(logger, ready.Ready)
	httpserver.MountLeaderboards(router, cacheService)
	if err := router.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		return fmt.Errorf("configure trusted proxies: %w", err)
	}
	if err := httpserver.MountAvatars(router, catalog, avatarFiles); err != nil {
		return err
	}
	if frontend != nil {
		frontend.Mount(router)
	}
	var adminService *admin.Service
	if cfg.AdminOwner != nil {
		adminService, err = admin.New(*cfg.AdminOwner, db)
		if err != nil {
			return err
		}
	}
	if err := httpserver.MountAdmin(router, adminService, frontend, logger, httpserver.AdminHTTPOptions{
		AllowLocalHTTP: cfg.AdminAllowLocalHTTP, TrustedProxies: cfg.TrustedProxies, GroupIPv6By64: cfg.RegistrationIPv6By64,
		RequestRefresh: cacheService.RequestRefresh,
	}); err != nil {
		return err
	}
	if err := httpserver.MountReports(router, auth.NewAuthenticator(repository), reportAdmission, reportWriter, int64(cfg.ReportIngestion.MaxBodyBytes)); err != nil {
		return err
	}
	httpserver.MountWithdraw(router, repository, cacheService.RequestRefresh)
	var github *registration.GitHub
	if cfg.GitHub != nil {
		github, err = registration.NewGitHub(*cfg.GitHub, nil)
		if err != nil {
			return err
		}
	}
	registrationOptions := registration.DefaultOptions()
	registrationOptions.Enabled = cfg.RegistrationEnabled && github != nil
	registrationService, err := registration.NewService(db, catalog, registrationOptions)
	if err != nil {
		return err
	}
	if err := registrationService.PruneExpired(ctx); err != nil {
		return fmt.Errorf("prune expired registrations: %w", err)
	}
	registrationCtx, stopRegistrations := context.WithCancel(context.Background())
	registrationDone := make(chan struct{})
	go func() {
		defer close(registrationDone)
		watchRegistrationExpiry(registrationCtx, registrationService, logger)
	}()
	defer func() { stopRegistrations(); <-registrationDone }()
	requestLimiter, err := registration.NewRequestLimiter(cfg.RegistrationRequests, nil)
	if err != nil {
		return err
	}
	if err := httpserver.MountRegistration(router, registrationService, github, requestLimiter, httpserver.RegistrationHTTPOptions{
		GroupIPv6By64: cfg.RegistrationIPv6By64, MaxBodyBytes: int64(cfg.RegistrationMaxBodyBytes),
	}); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen HTTP: %w", err)
	}
	logger.Info("http listener opened", "address", listener.Addr().String())
	return httpserver.Serve(ctx, listener, router, cfg.ShutdownTimeout)
}

func watchRegistrationExpiry(ctx context.Context, registrationService *registration.Service, logger *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := registrationService.PruneExpired(ctx); err != nil && ctx.Err() == nil {
				logger.Error("registration expiry cleanup failed")
			}
		}
	}
}

// Observe midnight even without incoming reports. A one-minute maximum wait
// also notices forward clock corrections; every report independently advances
// and rechecks the durable watermark before it can commit.
func watchReportWindow(ctx context.Context, writer *sql.DB, logger *slog.Logger) {
	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		delay := next.Sub(now)
		if delay > time.Minute {
			delay = time.Minute
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if err := reports.AdvanceWindow(ctx, writer, time.Now()); err != nil && ctx.Err() == nil {
				logger.Error("report window checkpoint failed")
			}
		}
	}
}
