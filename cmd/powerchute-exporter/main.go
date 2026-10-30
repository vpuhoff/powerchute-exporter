package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"powerchute-exporter/internal/config"
	"powerchute-exporter/internal/daemon"
	"powerchute-exporter/internal/exporter"
	"powerchute-exporter/internal/powerchute"
)

var version = "dev"

func main() { os.Exit(run()) }

func run() int {
	path := flag.String("config", "config.yaml", "YAML configuration file")
	check := flag.Bool("check-config", false, "validate configuration without connecting or reading credentials")
	showVersion := flag.Bool("version", false, "print version")
	serviceAction := flag.String("service", "", "Windows service: install, uninstall, start, stop or status")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if *serviceAction != "" {
		if err := daemon.Control(*serviceAction, *path); err != nil {
			if errors.Is(err, daemon.ErrNotRunning) {
				return 3
			}
			logger.Error("service command failed", "error", err.Error())
			return 1
		}
		return 0
	}
	if *check {
		if _, err := loadConfig(*path); err != nil {
			logger.Error("configuration error", "error", err.Error())
			return 1
		}
		fmt.Println("configuration valid")
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return daemon.Run(ctx, func(ctx context.Context, ready func(), logger *slog.Logger) int {
		return serve(ctx, *path, ready, logger)
	})
}

func loadConfig(path string) (config.Config, error) {
	c, err := config.Load(path)
	if err == nil {
		err = powerchute.ValidateFormats(c)
	}
	return c, err
}

func serve(ctx context.Context, path string, ready func(), logger *slog.Logger) int {
	c, err := loadConfig(path)
	if err != nil {
		logger.Error("configuration error", "error", err.Error())
		return 1
	}
	creds, err := c.PowerChute.Credentials()
	if err != nil {
		logger.Error(err.Error())
		return 1
	}
	client, err := powerchute.New(c, creds)
	if err != nil {
		logger.Error("client initialization failed", "error", err.Error())
		return 1
	}
	defer client.Close()
	if !c.PowerChute.TLSVerify {
		logger.Warn("TLS certificate verification is disabled explicitly")
	}
	if len(c.PowerChute.BaseURL) >= 5 && c.PowerChute.BaseURL[:5] == "http:" {
		logger.Warn("PowerChute connection uses unencrypted HTTP")
	}
	e := exporter.New(c, client, version, logger)
	address := net.JoinHostPort(c.Exporter.ListenAddress, strconv.Itoa(c.Exporter.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		logger.Error("cannot bind exporter listener")
		return 1
	}
	srv := &http.Server{Handler: e.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn)}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- srv.Serve(listener) }()
	logger.Info("exporter started", "listen", address, "version", version)
	ready()
	exitCode := 0
	select {
	case <-ctx.Done():
	case err := <-serveErrors:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("exporter HTTP server failed")
			exitCode = 1
		}
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	<-done
	logoutCtx, logoutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer logoutCancel()
	if err := client.Logout(logoutCtx); err != nil {
		logger.Warn("could not release PowerChute session", "reason", powerchute.ErrorReason(err))
	}
	return exitCode
}
