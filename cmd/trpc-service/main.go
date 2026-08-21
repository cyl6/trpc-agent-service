package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/cyl6/trpc-agent-service/trpcservice"
	agentruntime "github.com/cyl6/trpc-agent-service/trpcservice/agent"
	"github.com/cyl6/trpc-agent-service/trpcservice/channels"
	"github.com/cyl6/trpc-agent-service/trpcservice/config"
	"github.com/cyl6/trpc-agent-service/trpcservice/coordination"
	auditlog "github.com/cyl6/trpc-agent-service/trpcservice/log"
	"github.com/cyl6/trpc-agent-service/trpcservice/metrics"
	"github.com/cyl6/trpc-agent-service/trpcservice/queue"
	"github.com/cyl6/trpc-agent-service/trpcservice/tenant"
	"github.com/cyl6/trpc-agent-service/trpcservice/tenant/governance"
	"github.com/cyl6/trpc-agent-service/trpcservice/web"
	"github.com/cyl6/trpc-agent-service/trpcservice/worker"

	ametric "trpc.group/trpc-go/trpc-agent-go/telemetry/metric"
	atrace "trpc.group/trpc-go/trpc-agent-go/telemetry/trace"
)

func main() {
	configPath := flag.String("config", "config/example.yaml", "path to service config YAML")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("trpc-agent-service %s (commit %s)\n", trpcservice.Version, trpcservice.GitCommit)
		return
	}
	if err := run(*configPath); err != nil {
		// Startup errors may originate in third-party clients that echo an
		// endpoint or provider response. Keep the ordinary process log stable;
		// operators diagnose details through controlled startup probes.
		log.Printf("trpc-agent-service startup failed")
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	metricCleanup := func() error { return nil }
	traceCleanup := func() error { return nil }
	if cfg.Telemetry.OTLPEndpoint != "" {
		meterProvider, metricErr := ametric.NewMeterProvider(
			context.Background(),
			ametric.WithEndpoint(cfg.Telemetry.OTLPEndpoint),
			ametric.WithProtocol(cfg.Telemetry.OTLPProtocol),
			ametric.WithServiceName(cfg.Telemetry.ServiceName),
		)
		if metricErr != nil {
			return errors.New("initialize telemetry metrics")
		}
		if metricErr = ametric.InitMeterProvider(meterProvider); metricErr != nil {
			_ = meterProvider.Shutdown(context.Background())
			return errors.New("initialize framework metric instruments")
		}
		otel.SetMeterProvider(meterProvider)
		metricCleanup = func() error {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return meterProvider.Shutdown(shutdownCtx)
		}
		opts := []atrace.Option{
			atrace.WithEndpoint(cfg.Telemetry.OTLPEndpoint),
			atrace.WithProtocol(cfg.Telemetry.OTLPProtocol),
			atrace.WithServiceName(cfg.Telemetry.ServiceName),
			sensitiveSpanPolicy(),
		}
		traceCleanup, err = atrace.Start(context.Background(), opts...)
		if err != nil {
			_ = metricCleanup()
			return err
		}
	}
	defer func() { _ = traceCleanup() }()
	defer func() { _ = metricCleanup() }()

	tenantRegistry, err := tenant.NewRegistry(cfg)
	if err != nil {
		return err
	}
	httpClient := &http.Client{Timeout: 15 * time.Second}
	channelRegistry := channels.NewRegistry(channels.NewTelegram(httpClient), channels.NewSlack(httpClient))
	coordinator, err := buildCoordinator(cfg.Coordination)
	if err != nil {
		return err
	}
	defer coordinator.Close()
	runtimeManager := agentruntime.NewManager()
	defer runtimeManager.Close()
	metricsExporter := metrics.NewMetrics()
	secretValues := configuredSecrets(cfg)
	auditRouter := auditlog.NewRouter(tenantRegistry, os.Stdout, secretValues)
	defer auditRouter.Close()
	workerService := worker.NewService(
		runtimeManager, coordinator, channelRegistry, governance.NewFilter(), auditRouter, metricsExporter,
		worker.Options{LockTTL: cfg.Coordination.LockTTL, DedupTTL: cfg.Coordination.DedupTTL},
	)
	workQueue := queue.NewInProcess(workerService, cfg.Server.QueueSize, cfg.Server.WorkerCount, func(error) {
		// Provider errors can echo input or a newly rotated secret. Ordinary
		// process logs therefore expose only a stable category.
		log.Printf("worker task failed: category=processing_failed")
	})
	defer workQueue.Close()
	gatewayServer := web.NewServer(
		tenantRegistry, channelRegistry, workQueue, workerService, metricsExporter,
		cfg.Server.AdminTokenEnv, configPath, cfg,
	)
	httpServer := &http.Server{
		Addr: cfg.Server.Address, Handler: gatewayServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("trpc-agent-service listening on %s", cfg.Server.Address)
		errCh <- httpServer.ListenAndServe()
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case serveErr := <-errCh:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	}
}

// sensitiveSpanPolicy drops payload-bearing attributes at their framework
// source. The Collector repeats these deletions as defense in depth, but a
// source-side rule also prevents sensitive values from being marshaled into a
// span when the application exports to a differently configured Collector.
func sensitiveSpanPolicy() atrace.Option {
	rules := []atrace.AttributePolicyOption{
		atrace.WithAttributeRule(atrace.OperationChat, atrace.AttrLLMRequest, atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationChat, atrace.AttrLLMResponse, atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationChat, atrace.AttrInputMessages, atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationChat, atrace.AttrInputMessagesOTel, atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationChat, atrace.AttrOutputMessages, atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationChat, atrace.AttrOutputMessagesOTel, atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationChat, atrace.AttributeKey("gen_ai.request.tool.definitions"), atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationInvokeAgent, atrace.AttrInputMessages, atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationInvokeAgent, atrace.AttrInputMessagesOTel, atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationInvokeAgent, atrace.AttrOutputMessages, atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationInvokeAgent, atrace.AttrOutputMessagesOTel, atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationInvokeAgent, atrace.AttributeKey("gen_ai.system_instructions"), atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationExecuteTool, atrace.AttributeKey("gen_ai.tool.call.arguments"), atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationExecuteTool, atrace.AttributeKey("gen_ai.tool.call.result"), atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationWorkflow, atrace.AttributeKey("gen_ai.workflow.request"), atrace.Drop()),
		atrace.WithAttributeRule(atrace.OperationWorkflow, atrace.AttributeKey("gen_ai.workflow.response"), atrace.Drop()),
	}
	return atrace.WithSpanAttributePolicy(rules...)
}

func buildCoordinator(cfg config.CoordinationConfig) (coordination.Coordinator, error) {
	switch cfg.Backend {
	case "inmemory":
		return coordination.NewInMemory(), nil
	case "redis":
		rawURL, err := config.Secret(cfg.RedisURLEnv)
		if err != nil {
			return nil, err
		}
		return coordination.NewRedis(rawURL, cfg.KeyPrefix)
	default:
		return nil, errors.New("unsupported coordination backend")
	}
}

func configuredSecrets(cfg *config.Config) []string {
	values := make([]string, 0)
	for _, name := range cfg.SecretEnvNames() {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			values = append(values, value)
		}
	}
	return values
}
