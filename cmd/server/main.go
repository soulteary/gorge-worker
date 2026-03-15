package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/soulteary/gorge-worker/internal/config"
	"github.com/soulteary/gorge-worker/internal/handlers"
	"github.com/soulteary/gorge-worker/internal/httpapi"
	"github.com/soulteary/gorge-worker/internal/taskqueue"
	"github.com/soulteary/gorge-worker/internal/worker"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

func main() {
	cfg := config.LoadFromEnv()

	client := taskqueue.NewClient(cfg.TaskQueueURL, cfg.TaskQueueToken)

	registry := worker.NewRegistry()
	handlers.RegisterAll(registry, cfg.ConduitURL, cfg.ConduitToken)

	consumer := worker.NewConsumer(
		client,
		registry,
		cfg.LeaseLimit,
		cfg.PollIntervalMs,
		cfg.MaxWorkers,
		cfg.IdleTimeoutSec,
		cfg.TaskClassFilter,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go consumer.Run(ctx)

	e := echo.New()
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true, LogURI: true, LogMethod: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			c.Logger().Infof("%s %s %d", v.Method, v.URI, v.Status)
			return nil
		},
	}))
	e.Use(middleware.Recover())

	httpapi.RegisterRoutes(e, &httpapi.Deps{
		Consumer: consumer,
		Token:    cfg.ServiceToken,
	})

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		cancel()
		_ = e.Shutdown(context.Background())
	}()

	e.Logger.Fatal(e.Start(cfg.ListenAddr))
}
