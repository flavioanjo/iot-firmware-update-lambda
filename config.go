package main

import (
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Default values for the runtime configuration. They are deliberately small:
// a Lambda execution environment processes one request at a time, so a large
// pool only multiplies the number of SQL Server sessions per instance.
const (
	defaultMaxOpenConns    = 4
	defaultMaxIdleConns    = 2
	defaultConnMaxLifetime = 5 * time.Minute
	defaultConnMaxIdleTime = 1 * time.Minute
	defaultQueryTimeout    = 10 * time.Second
	defaultLocalPort       = "8080"
)

// config holds every environment-driven setting used by the service.
type config struct {
	// ConnString is the SQL Server connection string (DB_CONNECTION_STRING).
	// It is never logged.
	ConnString      string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	QueryTimeout    time.Duration
	MaxBlockSize    int
	LocalPort       string
}

// loadConfig reads the configuration from the environment. Invalid values are
// reported through the logger and replaced with the documented defaults, so a
// typo in an optional tuning variable never prevents the service from starting.
func loadConfig(logger *slog.Logger) config {
	return config{
		ConnString:      os.Getenv("DB_CONNECTION_STRING"),
		MaxOpenConns:    envInt(logger, "DB_MAX_OPEN_CONNS", defaultMaxOpenConns, 1, 100),
		MaxIdleConns:    envInt(logger, "DB_MAX_IDLE_CONNS", defaultMaxIdleConns, 0, 100),
		ConnMaxLifetime: envDuration(logger, "DB_CONN_MAX_LIFETIME", defaultConnMaxLifetime),
		ConnMaxIdleTime: envDuration(logger, "DB_CONN_MAX_IDLE_TIME", defaultConnMaxIdleTime),
		QueryTimeout:    envDuration(logger, "DB_QUERY_TIMEOUT", defaultQueryTimeout),
		MaxBlockSize:    envInt(logger, "MAX_BLOCK_SIZE", defaultMaxBlockSize, 1, hardMaxBlockSize),
		LocalPort:       envString("PORT", defaultLocalPort),
	}
}

func envString(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(logger *slog.Logger, name string, def, min, max int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		logger.Warn("ignoring invalid configuration value",
			slog.String("variable", name), slog.Int("default", def),
			slog.Int("min", min), slog.Int("max", max))
		return def
	}
	return v
}

func envDuration(logger *slog.Logger, name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		logger.Warn("ignoring invalid configuration value",
			slog.String("variable", name), slog.String("default", def.String()))
		return def
	}
	return v
}
