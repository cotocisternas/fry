//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/admintransport"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/telemetry"

	"github.com/jackc/pgx/v5/pgxpool"
)

type adminConfig struct {
	transportConfig
	MaxConnections               int    `json:"max_connections"`
	Listen                       string `json:"listen"`
	Origin                       string `json:"origin"`
	SessionMinutes               int    `json:"session_minutes"`
	BootstrapUsername            string `json:"bootstrap_username"`
	BootstrapPasswordEnvironment string `json:"bootstrap_password_environment"`
	BootstrapContentAccess       bool   `json:"bootstrap_content_access"`
	OIDC                         *struct {
		Issuer             string            `json:"issuer"`
		ClientID           string            `json:"client_id"`
		SecretEnvironment  string            `json:"client_secret_environment"`
		Scopes             []string          `json:"scopes"`
		SigningAlgorithms  []string          `json:"signing_algorithms"`
		Rules              []admin.ClaimRule `json:"rules"`
		LoginMinutes       int               `json:"login_minutes"`
		TokenMaxAgeMinutes int               `json:"token_max_age_minutes"`
		ClockSkewSeconds   int               `json:"clock_skew_seconds"`
		LoopbackHTTP       bool              `json:"loopback_http"`
	} `json:"oidc"`
}

func startAdmin(
	ctx context.Context,
	db *pgxpool.Pool,
	mail *receiptstore.Store,
	cfg *adminConfig,
	failures chan<- error,
) (func(), error) {
	if cfg == nil {
		return func() {}, nil
	}
	address, err := net.ResolveTCPAddr("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	service, err := admin.New(db, mail, time.Duration(cfg.SessionMinutes)*time.Minute)
	if err != nil {
		return nil, err
	}
	if err = service.CheckStorageQuotaSchema(ctx); err != nil {
		return nil, err
	}
	if err = service.RegisterConfiguredOrganizations(ctx); err != nil {
		return nil, err
	}
	if err = bootstrapAdmin(ctx, service, cfg); err != nil {
		return nil, err
	}

	handler, err := admintransport.NewHTTP(service, admintransport.HTTPOptions{Origin: cfg.Origin})
	if err != nil {
		return nil, err
	}
	if err = configureAdminOIDC(ctx, service, handler, cfg); err != nil {
		return nil, err
	}
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		return nil, err
	}
	stopHTTP := serveAdministration(ctx, listener, handler, cfg, failures)
	stopQuotas := startStorageQuotaEvaluator(ctx, service)
	return func() { stopQuotas(); stopHTTP() }, nil
}

func serveAdministration(
	ctx context.Context,
	listener net.Listener,
	handler http.Handler,
	cfg *adminConfig,
	failures chan<- error,
) func() {
	workerCtx, cancel := context.WithCancel(ctx)
	server := &http.Server{
		TLSConfig:         cfg.tls,
		Handler:           telemetry.FromContext(ctx).HTTPHandler("admin.http", handler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return workerCtx },
	}
	server.Protocols = new(http.Protocols)
	server.Protocols.SetHTTP1(true)
	server.Protocols.SetHTTP2(true)
	// Plaintext administration is already restricted to loopback by configuration
	// validation. Explicit h2c support lets native gRPC clients use that local mode.
	server.Protocols.SetUnencryptedHTTP2(cfg.Transport == "loopback")
	if cfg.Transport == "tls" {
		server.ReadHeaderTimeout = cfg.handshakeTimeout()
	}
	bounded := &admissionListener{
		Listener: listener,
		slots:    make(chan struct{}, cfg.MaxConnections),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var err error
		if cfg.Transport == "tls" {
			err = server.ServeTLS(bounded, "", "")
		} else {
			err = server.Serve(bounded)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			failures <- err
		}
	}()
	log.Printf("FRY administration listening on %s (origin %s)", listener.Addr(), cfg.Origin)
	return func() {
		cancel()
		shutdown, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if server.Shutdown(shutdown) != nil {
			if err := server.Close(); err != nil {
				log.Print("administration forced close incomplete")
			}
		}
		<-done
	}
}

func bootstrapAdmin(ctx context.Context, service *admin.Service, cfg *adminConfig) error {
	var err error
	if cfg.BootstrapUsername != "" {
		password := os.Getenv(cfg.BootstrapPasswordEnvironment)
		if password == "" {
			return fmt.Errorf("administrator bootstrap password environment variable is missing")
		}
		if _, err = service.Bootstrap(ctx, cfg.BootstrapUsername, password, cfg.BootstrapContentAccess); err != nil {
			return err
		}
	}
	return nil
}

func configureAdminOIDC(
	ctx context.Context,
	service *admin.Service,
	handler *admintransport.HTTP,
	cfg *adminConfig,
) error {
	var err error
	if cfg.OIDC != nil {
		oidc := cfg.OIDC
		secret := ""
		if oidc.SecretEnvironment != "" {
			secret = os.Getenv(oidc.SecretEnvironment)
			if secret == "" {
				return fmt.Errorf("OIDC client secret environment variable is missing")
			}
		}
		err = handler.ConfigureOIDC(
			ctx,
			admintransport.OIDCConfig{
				Issuer:            oidc.Issuer,
				ClientID:          oidc.ClientID,
				ClientSecret:      secret,
				Scopes:            oidc.Scopes,
				SigningAlgorithms: oidc.SigningAlgorithms,
				Rules:             oidc.Rules,
				LoginLifetime:     time.Duration(oidc.LoginMinutes) * time.Minute,
				TokenMaxAge:       time.Duration(oidc.TokenMaxAgeMinutes) * time.Minute,
				ClockSkew:         time.Duration(oidc.ClockSkewSeconds) * time.Second,
				LoopbackHTTP:      oidc.LoopbackHTTP,
			},
		)
		if err != nil {
			return err
		}
	} else if err = service.DisableOIDC(ctx); err != nil {
		return err
	}
	return nil
}
