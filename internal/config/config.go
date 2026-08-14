// Package config loads runtime configuration from the process environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	App       App
	HTTP      HTTP
	Database  Database
	Auth      Auth
	GreenAPI  GreenAPI
	Media     Media
	Scheduler Scheduler
}

type App struct {
	Env             string
	DefaultTimezone string
	LogLevel        string
	LogFormat       string
}

type HTTP struct {
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	TrustedProxies  []string
	AllowedOrigins  []string
	WebDir          string
}

type Database struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	AutoMigrate     bool
}

type Auth struct {
	SessionSecret     string
	SessionTTL        time.Duration
	SessionCookieName string
	CSRFCookieName    string
	SecureCookies     bool
	CookieDomain      string
	LoginMaxAttempts  int
	LoginLockWindow   time.Duration
	LoginLockDuration time.Duration
	BootstrapEmail    string
	BootstrapPassword string
}

type GreenAPI struct {
	InstanceID   string
	Token        string
	BaseURL      string
	MediaURL     string
	WebhookToken string
	Timeout      time.Duration
	// RateInterval is the minimum spacing between two outbound API calls.
	RateInterval time.Duration
}

type Media struct {
	StoragePath  string
	PublicURL    string
	MaxUploadMB  int64
	FFmpegPath   string
	FFprobePath  string
	VoiceBitrate string
}

type Scheduler struct {
	Enabled          bool
	Workers          int
	PollInterval     time.Duration
	BatchSize        int
	MaxAttempts      int
	RetryBaseDelay   time.Duration
	RetryMaxDelay    time.Duration
	LockTimeout      time.Duration
	StaleJobTTL      time.Duration
	WebhookWorkers   int
	WebhookBatchSize int
}

// Load reads configuration from the environment, applying defaults and
// validating anything the process cannot start without.
func Load() (*Config, error) {
	cfg := &Config{
		App: App{
			Env:             envStr("APP_ENV", "development"),
			DefaultTimezone: envStr("TIMEZONE", "Asia/Almaty"),
			LogLevel:        envStr("LOG_LEVEL", "info"),
			LogFormat:       envStr("LOG_FORMAT", "json"),
		},
		HTTP: HTTP{
			Port:            envInt("PORT", 8080),
			ReadTimeout:     envDuration("HTTP_READ_TIMEOUT", 30*time.Second),
			WriteTimeout:    envDuration("HTTP_WRITE_TIMEOUT", 60*time.Second),
			IdleTimeout:     envDuration("HTTP_IDLE_TIMEOUT", 120*time.Second),
			ShutdownTimeout: envDuration("HTTP_SHUTDOWN_TIMEOUT", 25*time.Second),
			TrustedProxies:  envList("TRUSTED_PROXIES", nil),
			AllowedOrigins:  envList("ALLOWED_ORIGINS", nil),
			WebDir:          envStr("WEB_DIR", "./web"),
		},
		Database: Database{
			URL:             envStr("DATABASE_URL", ""),
			MaxConns:        int32(envInt("DATABASE_MAX_CONNS", 20)),
			MinConns:        int32(envInt("DATABASE_MIN_CONNS", 2)),
			MaxConnLifetime: envDuration("DATABASE_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime: envDuration("DATABASE_CONN_IDLE", 30*time.Minute),
			AutoMigrate:     envBool("DATABASE_AUTO_MIGRATE", true),
		},
		Auth: Auth{
			SessionSecret:     envStr("SESSION_SECRET", ""),
			SessionTTL:        envDuration("SESSION_TTL", 12*time.Hour),
			SessionCookieName: envStr("SESSION_COOKIE_NAME", "wa_session"),
			CSRFCookieName:    envStr("CSRF_COOKIE_NAME", "wa_csrf"),
			SecureCookies:     envBool("SECURE_COOKIES", false),
			CookieDomain:      envStr("COOKIE_DOMAIN", ""),
			LoginMaxAttempts:  envInt("LOGIN_MAX_ATTEMPTS", 8),
			LoginLockWindow:   envDuration("LOGIN_LOCK_WINDOW", 15*time.Minute),
			LoginLockDuration: envDuration("LOGIN_LOCK_DURATION", 15*time.Minute),
			BootstrapEmail:    envStr("ADMIN_EMAIL", ""),
			BootstrapPassword: envStr("ADMIN_PASSWORD", ""),
		},
		GreenAPI: GreenAPI{
			InstanceID:   envStr("GREEN_API_INSTANCE_ID", ""),
			Token:        envStr("GREEN_API_TOKEN", ""),
			BaseURL:      strings.TrimRight(envStr("GREEN_API_URL", "https://api.green-api.com"), "/"),
			MediaURL:     strings.TrimRight(envStr("GREEN_API_MEDIA_URL", "https://media.green-api.com"), "/"),
			WebhookToken: envStr("GREEN_API_WEBHOOK_TOKEN", ""),
			Timeout:      envDuration("GREEN_API_TIMEOUT", 45*time.Second),
			RateInterval: envDuration("GREEN_API_RATE_INTERVAL", 1100*time.Millisecond),
		},
		Media: Media{
			StoragePath:  envStr("MEDIA_STORAGE_PATH", "./storage/media"),
			PublicURL:    strings.TrimRight(envStr("MEDIA_PUBLIC_URL", ""), "/"),
			MaxUploadMB:  int64(envInt("MEDIA_MAX_UPLOAD_MB", 64)),
			FFmpegPath:   envStr("FFMPEG_PATH", "ffmpeg"),
			FFprobePath:  envStr("FFPROBE_PATH", "ffprobe"),
			VoiceBitrate: envStr("VOICE_BITRATE", "32k"),
		},
		Scheduler: Scheduler{
			Enabled:          envBool("SCHEDULER_ENABLED", true),
			Workers:          envInt("SCHEDULER_WORKERS", 4),
			PollInterval:     envDuration("SCHEDULER_POLL_INTERVAL", 5*time.Second),
			BatchSize:        envInt("SCHEDULER_BATCH_SIZE", 25),
			MaxAttempts:      envInt("SCHEDULER_MAX_ATTEMPTS", 5),
			RetryBaseDelay:   envDuration("SCHEDULER_RETRY_BASE_DELAY", 30*time.Second),
			RetryMaxDelay:    envDuration("SCHEDULER_RETRY_MAX_DELAY", 30*time.Minute),
			LockTimeout:      envDuration("SCHEDULER_LOCK_TIMEOUT", 5*time.Minute),
			StaleJobTTL:      envDuration("SCHEDULER_STALE_JOB_TTL", 2*time.Hour),
			WebhookWorkers:   envInt("WEBHOOK_WORKERS", 2),
			WebhookBatchSize: envInt("WEBHOOK_BATCH_SIZE", 50),
		},
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	var problems []string

	if c.Database.URL == "" {
		problems = append(problems, "DATABASE_URL is required")
	}
	if len(c.Auth.SessionSecret) < 32 {
		problems = append(problems, "SESSION_SECRET is required and must be at least 32 characters")
	}
	if _, err := time.LoadLocation(c.App.DefaultTimezone); err != nil {
		problems = append(problems, fmt.Sprintf("TIMEZONE %q is not a valid IANA location", c.App.DefaultTimezone))
	}
	if c.HTTP.Port < 1 || c.HTTP.Port > 65535 {
		problems = append(problems, "PORT must be between 1 and 65535")
	}
	if c.Scheduler.Workers < 1 {
		problems = append(problems, "SCHEDULER_WORKERS must be at least 1")
	}
	if c.Scheduler.MaxAttempts < 1 {
		problems = append(problems, "SCHEDULER_MAX_ATTEMPTS must be at least 1")
	}
	if c.Media.MaxUploadMB < 1 {
		problems = append(problems, "MEDIA_MAX_UPLOAD_MB must be at least 1")
	}
	if c.IsProduction() && !c.Auth.SecureCookies {
		problems = append(problems, "SECURE_COOKIES must be true when APP_ENV=production")
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

func (c *Config) IsProduction() bool { return c.App.Env == "production" }

// WhatsAppConfigured reports whether outbound Green API calls can be attempted.
// The platform still boots without credentials so the admin panel stays usable.
func (c *Config) WhatsAppConfigured() bool {
	return c.GreenAPI.InstanceID != "" && c.GreenAPI.Token != ""
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}

func envList(key string, def []string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
