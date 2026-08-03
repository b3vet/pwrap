package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	HTTPAddr    string
	DatabaseURL string

	// TenantHost/Port/Database control what pwrapd hands to SDKs in the minted DSN.
	// They default to DatabaseURL's host/port/db so local dev Just Works, but can be
	// overridden when pwrapd sits behind a different endpoint than clients see
	// (e.g. internal Postgres vs. public pooler, or Neon vs. local).
	TenantHost     string
	TenantPort     string
	TenantDatabase string
	TenantSSLMode  string

	BootstrapToken string
	Environment    string

	// PostgREST integration (Track A / M7).
	// AuthenticatorRole is the shared Postgres role PostgREST connects as. pwrapd
	// GRANTs each tenant role into it so PostgREST can SET ROLE per JWT.
	// JWTSecret signs REST tokens (HS256). Must match PGRST_JWT_SECRET on the
	// PostgREST side.
	// RestExternalURL is what the SDK tells clients to hit (default: localhost:3000).
	// REST-related config is optional — if AuthenticatorPassword or JWTSecret is
	// empty, pwrapd logs a warning and the /rest/token endpoint returns 503.
	AuthenticatorRole     string
	AuthenticatorPassword string
	AnonRole              string
	JWTSecret             string
	RestExternalURL       string
	RestTokenTTLSeconds   int

	// EncryptionKey is a base64-encoded 32-byte AES-256 key. When set, pwrapd
	// encrypts the project pg_password at rest using envelope AES-GCM and
	// re-encrypts any legacy plaintext rows at startup. When unset, pwrapd logs
	// a warning and stores credentials in cleartext. Operators are expected to
	// set this in production. See internal/crypto.
	EncryptionKey string

	// Telemetry — OpenTelemetry. OTLPEndpoint enables OTLP/HTTP export for both
	// traces and metrics (e.g. "http://localhost:4318"). ServiceName tags every
	// span/metric (defaults to "pwrapd"). MetricsAddr exposes a Prometheus
	// /metrics endpoint when set (e.g. ":9464"); useful for in-cluster scraping
	// without an OTLP collector. TraceSampleRate accepts [0,1]; defaults to 1.0
	// in development and should be lowered in production.
	OTLPEndpoint    string
	OTLPInsecure    bool
	OTelServiceName string
	MetricsAddr     string
	TraceSampleRate float64
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:       getenv("PWRAP_HTTP_ADDR", ":8080"),
		DatabaseURL:    os.Getenv("PWRAP_DATABASE_URL"),
		BootstrapToken: os.Getenv("PWRAP_BOOTSTRAP_TOKEN"),
		Environment:    getenv("PWRAP_ENV", "development"),
		TenantSSLMode:  getenv("PWRAP_TENANT_SSLMODE", "disable"),

		AuthenticatorRole:     getenv("PWRAP_AUTHENTICATOR_ROLE", "pwrap_authenticator"),
		AuthenticatorPassword: os.Getenv("PWRAP_AUTHENTICATOR_PASSWORD"),
		AnonRole:              getenv("PWRAP_ANON_ROLE", "pwrap_anon"),
		JWTSecret:             os.Getenv("PWRAP_JWT_SECRET"),
		RestExternalURL:       getenv("PWRAP_REST_URL", "http://localhost:3000"),
		RestTokenTTLSeconds:   intEnv("PWRAP_REST_TOKEN_TTL_SECONDS", 3600),

		EncryptionKey:   os.Getenv("PWRAP_ENCRYPTION_KEY"),
		OTLPEndpoint:    os.Getenv("PWRAP_OTLP_ENDPOINT"),
		OTLPInsecure:    boolEnv("PWRAP_OTLP_INSECURE", true),
		OTelServiceName: getenv("PWRAP_OTEL_SERVICE_NAME", "pwrapd"),
		MetricsAddr:     os.Getenv("PWRAP_METRICS_ADDR"),
		TraceSampleRate: floatEnv("PWRAP_TRACE_SAMPLE_RATE", 1.0),
	}
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return cfg, errors.New("PWRAP_DATABASE_URL is required")
	}
	host, port, db, err := parseHostPortDB(cfg.DatabaseURL)
	if err != nil {
		return cfg, fmt.Errorf("parse PWRAP_DATABASE_URL: %w", err)
	}
	cfg.TenantHost = getenv("PWRAP_TENANT_HOST", host)
	cfg.TenantPort = getenv("PWRAP_TENANT_PORT", port)
	cfg.TenantDatabase = getenv("PWRAP_TENANT_DATABASE", db)
	return cfg, nil
}

// RestEnabled reports whether the REST/JWT path has enough config to function.
func (c Config) RestEnabled() bool {
	return c.AuthenticatorPassword != "" && c.JWTSecret != ""
}

func parseHostPortDB(dsn string) (host, port, db string, err error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", "", "", err
	}
	host = u.Hostname()
	port = u.Port()
	if port == "" {
		port = "5432"
	}
	db = strings.TrimPrefix(u.Path, "/")
	return host, port, db, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func intEnv(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}

func floatEnv(k string, def float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%f", &f); err != nil {
		return def
	}
	return f
}

func boolEnv(k string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}
