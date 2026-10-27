// Package config loads the instance environment (.env) and the capture
// capture configuration model (sources).
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Env holds instance settings. Secrets live only here, never in the archive.
type Env struct {
	// Database is the PostgreSQL URL of the application data and the index.
	Database       string
	DataDir        string
	Storage        string
	StorageReplica string
	// Storages maps storage names to URLs; "default" is HOARFROST_STORAGE.
	Storages map[string]string
	// Replicas maps storage names to replica URLs.
	Replicas        map[string]string
	Secret          string
	Listen          string
	BaseURL         string
	Coordinator     string
	WorkerToken     string
	WorkerName      string
	Workers         int
	EmbeddedWorker  bool
	UserAgent       string
	ChromePath      string
	ChromeNoSandbox bool
	SealGrace       time.Duration
	Retention       time.Duration
	LeaseDuration   time.Duration
	ConcatBySource  bool
	// VolumeSize bounds the capture bytes of one WACZ volume of a week.
	VolumeSize     int64
	InsecureCookie bool
	LogLevel       string
	LogFormat      string
}

// LoadDotEnv sets variables from a .env file without overriding variables
// already present in the environment. A missing file is not an error.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	vars, err := ParseDotEnv(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range vars {
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return nil
}

// ParseDotEnv parses KEY=VALUE lines with optional quotes, "export"
// prefixes and # comments.
func ParseDotEnv(r interface{ Read([]byte) (int, error) }) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", n)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			uq, err := strconv.Unquote(v)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n, err)
			}
			v = uq
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
		}
		out[k] = v
	}
	return out, sc.Err()
}

// FromEnvironment reads HOARFROST_* variables with defaults.
func FromEnvironment() (*Env, error) {
	e := &Env{
		Database:       os.Getenv("HOARFROST_DATABASE"),
		DataDir:        get("HOARFROST_DATA_DIR", "./data"),
		Storage:        os.Getenv("HOARFROST_STORAGE"),
		StorageReplica: os.Getenv("HOARFROST_STORAGE_REPLICA"),
		Secret:         os.Getenv("HOARFROST_SECRET"),
		Listen:         get("HOARFROST_LISTEN", ":8080"),
		BaseURL:        os.Getenv("HOARFROST_BASE_URL"),
		Coordinator:    os.Getenv("HOARFROST_COORDINATOR"),
		WorkerToken:    os.Getenv("HOARFROST_WORKER_TOKEN"),
		WorkerName:     os.Getenv("HOARFROST_WORKER_NAME"),
		UserAgent:      os.Getenv("HOARFROST_USER_AGENT"),
		ChromePath:     os.Getenv("HOARFROST_CHROME"),
		LogLevel:       get("HOARFROST_LOG_LEVEL", "info"),
		LogFormat:      get("HOARFROST_LOG_FORMAT", "text"),
	}
	var err error
	if e.Workers, err = intVar("HOARFROST_WORKERS", 2); err != nil {
		return nil, err
	}
	if e.EmbeddedWorker, err = boolVar("HOARFROST_EMBEDDED_WORKER", true); err != nil {
		return nil, err
	}
	if e.ChromeNoSandbox, err = boolVar("HOARFROST_CHROME_NO_SANDBOX", false); err != nil {
		return nil, err
	}
	if e.ConcatBySource, err = boolVar("HOARFROST_CONCAT_BY_SOURCE", false); err != nil {
		return nil, err
	}
	// A volume must fit one S3 PUT request (5 GiB) with its indexes.
	if e.VolumeSize, err = ParseByteSize(get("HOARFROST_VOLUME_SIZE", "4GiB")); err != nil || e.VolumeSize <= 0 || e.VolumeSize > 4608<<20 {
		return nil, fmt.Errorf("HOARFROST_VOLUME_SIZE: %q is not a size up to 4.5GiB", os.Getenv("HOARFROST_VOLUME_SIZE"))
	}
	if e.InsecureCookie, err = boolVar("HOARFROST_INSECURE_COOKIE", false); err != nil {
		return nil, err
	}
	if e.SealGrace, err = durationVar("HOARFROST_SEAL_GRACE", 6*time.Hour); err != nil {
		return nil, err
	}
	if e.Retention, err = durationVar("HOARFROST_INCOMING_RETENTION", 30*24*time.Hour); err != nil {
		return nil, err
	}
	if e.LeaseDuration, err = durationVar("HOARFROST_LEASE", 10*time.Minute); err != nil {
		return nil, err
	}
	if e.Storage == "" {
		e.Storage = "file://" + filepath.Join(absOr(e.DataDir), "archive")
	}
	if e.Storages, err = namedURLs("HOARFROST_STORAGES"); err != nil {
		return nil, err
	}
	if _, ok := e.Storages["default"]; ok {
		return nil, errors.New("HOARFROST_STORAGES: use HOARFROST_STORAGE for the default storage")
	}
	e.Storages["default"] = e.Storage
	if e.Replicas, err = namedURLs("HOARFROST_REPLICAS"); err != nil {
		return nil, err
	}
	if e.StorageReplica != "" {
		e.Replicas["default"] = e.StorageReplica
	}
	for name := range e.Replicas {
		if _, ok := e.Storages[name]; !ok {
			return nil, fmt.Errorf("HOARFROST_REPLICAS: unknown storage %q", name)
		}
	}
	if e.WorkerName == "" {
		host, _ := os.Hostname()
		e.WorkerName = host
	}
	return e, nil
}

// namedURLs parses "name=url,name2=url2".
func namedURLs(key string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(os.Getenv(key), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, u, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if !ok || !ValidStorageName(name) || strings.TrimSpace(u) == "" {
			return nil, fmt.Errorf("%s: expected name=url entries, got %q", key, part)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%s: duplicate storage %q", key, name)
		}
		out[name] = strings.TrimSpace(u)
	}
	return out, nil
}

// StorageNames returns configured storage names, default first.
func (e *Env) StorageNames() []string {
	names := []string{"default"}
	for n := range e.Storages {
		if n != "default" {
			names = append(names, n)
		}
	}
	sort.Strings(names[1:])
	return names
}

// RequireSecret validates the secret used as password pepper and for
// session signing.
func (e *Env) RequireSecret() error {
	if len(e.Secret) < 32 {
		return errors.New("HOARFROST_SECRET must be set to at least 32 random characters")
	}
	return nil
}

func absOr(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func get(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func intVar(k string, def int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

func boolVar(k string, def bool) (bool, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", k, err)
	}
	return b, nil
}

func durationVar(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	d, err := ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}

// ParseDuration extends time.ParseDuration with a "d" (day) unit.
func ParseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}
