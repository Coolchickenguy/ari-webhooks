package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	DatabaseUrl         string
	TokenEncKey         []byte
	Port                int
	ServeHttp           bool
	RunOutbox           bool
	RunJobs             bool
	HackatimeAdminKey   string
	HackatimeBaseUrl    string
	LapseApiKey         string
	LapseBaseUrl        string
	VmApiToken          string
	VmApiBase           string
	InternalApiToken    string
	GitCloneConcurrency int
	GitCloneTimeoutSecs int
	GhProxyUrl          string
	GhProxyApiKey       string
	JobWorkers          int
	WorkerId            string
}

var defaults = map[string]string{}

// call from an init function, before Load
func SetDefault(name, value string) {
	defaults[name] = value
}

func Env(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return defaults[name]
}

func Load() (*Config, error) {
	// Dev convenience: pull in ./.env when present. Real environment variables
	// always win (godotenv never overrides), so production is unaffected.
	_ = godotenv.Load()

	var missing []string
	databaseUrl := os.Getenv("DATABASE_URL")
	if databaseUrl == "" {
		missing = append(missing, "DATABASE_URL")
	}
	rawKey := os.Getenv("TOKEN_ENC_KEY")
	if rawKey == "" {
		missing = append(missing, "TOKEN_ENC_KEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	key, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil || len(key) != 32 { // every secret at rest depends on this key; refuse to boot with a bad one
		return nil, fmt.Errorf("TOKEN_ENC_KEY must be a base64-encoded 32-byte key (openssl rand -base64 32)")
	}

	hostname, _ := os.Hostname()
	c := &Config{
		DatabaseUrl:         databaseUrl,
		TokenEncKey:         key,
		Port:                envInt("PORT", 8080),
		ServeHttp:           envBool("SERVE_HTTP", true),
		RunOutbox:           envBool("RUN_OUTBOX", true),
		RunJobs:             envBool("RUN_JOBS", true),
		HackatimeAdminKey:   os.Getenv("HACKATIME_ADMIN_KEY"),
		HackatimeBaseUrl:    Env("HACKATIME_BASE_URL"),
		LapseApiKey:         os.Getenv("LAPSE_API_KEY"),
		LapseBaseUrl:        Env("LAPSE_BASE_URL"),
		VmApiToken:          os.Getenv("VM_API_TOKEN"),
		VmApiBase:           Env("VM_API_BASE"),
		InternalApiToken:    strings.TrimSpace(os.Getenv("INTERNAL_API_TOKEN")), // ari trims its copy of the secret; a pasted trailing newline must not defeat the exact match
		GitCloneConcurrency: envInt("GIT_CLONE_CONCURRENCY", 1),                 // git is the box's dominant memory cost; raise only with RAM to spare
		GitCloneTimeoutSecs: envInt("GIT_CLONE_TIMEOUT_SECONDS", 300),
		GhProxyUrl:          strings.TrimSpace(Env("GH_PROXY_URL")),
		GhProxyApiKey:       strings.TrimSpace(os.Getenv("GH_PROXY_API_KEY")),
		JobWorkers:          envInt("JOB_WORKERS", 2),
		WorkerId:            hostname,
	}
	if workerId := os.Getenv("WORKER_ID"); workerId != "" {
		c.WorkerId = workerId
	}
	return c, nil
}

func envInt(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(name string, fallback bool) bool {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	return v != "false" && v != "0"
}
