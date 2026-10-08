// Package fleet renders one fleet.yaml into each host's fireactions config,
// cache-proxy (workerd) config and systemd units.
//
// fleet.yaml holds no secrets. Secret values come from environment variables
// at render time and only land in files written with mode 0600.
package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Secret environment variables read at render time.
const (
	EnvCacheUpstreamToken = "FLEET_CACHE_UPSTREAM_TOKEN" // token the proxy presents to the upstream Worker
	EnvCacheTokenRW       = "FLEET_CACHE_TOKEN_RW"       // proxy token for read-write pools
	EnvCacheTokenRO       = "FLEET_CACHE_TOKEN_RO"       // proxy token for read-only pools
)

// File is fleet.yaml.
type File struct {
	GitHub   GitHub          `yaml:"github"`
	Defaults Host            `yaml:"defaults"`
	Pools    []Pool          `yaml:"pools"`
	Hosts    map[string]Host `yaml:"hosts"`
}

// GitHub is the fleet's GitHub App. The key stays on each host as a file.
type GitHub struct {
	AppID             int64  `yaml:"app_id"`
	AppPrivateKeyFile string `yaml:"app_private_key_file"`
}

// Host holds per-host settings. Defaults are overlaid by each host's entry;
// unset fields inherit.
type Host struct {
	API         string                  `yaml:"api"`
	Metrics     string                  `yaml:"metrics"`
	LogLevel    string                  `yaml:"log_level"`
	BridgeIP    string                  `yaml:"bridge_ip"`
	Containerd  Containerd              `yaml:"containerd"`
	Firecracker Firecracker             `yaml:"firecracker"`
	Cache       Cache                   `yaml:"cache"`
	Pools       map[string]PoolOverride `yaml:"pools"`
}

type Containerd struct {
	Address   string `yaml:"address"`
	Namespace string `yaml:"namespace"`
}

type Firecracker struct {
	BinaryPath      string `yaml:"binary_path"`
	KernelImagePath string `yaml:"kernel_image_path"`
	KernelArgs      string `yaml:"kernel_args"`
}

// Cache configures the per-host Turborepo cache proxy.
type Cache struct {
	Enabled     *bool  `yaml:"enabled"`
	Upstream    string `yaml:"upstream"`
	Team        string `yaml:"team"`
	MaxSize     string `yaml:"max_size"`
	Port        int    `yaml:"port"`
	MetricsPort int    `yaml:"metrics_port"`
	Dir         string `yaml:"dir"`
}

// Pool is a runner pool, present on every host unless a host disables it.
type Pool struct {
	Name            string            `yaml:"name"`
	RunnerName      string            `yaml:"runner_name"`
	Repository      string            `yaml:"repository"`
	Labels          []string          `yaml:"labels"`
	Image           string            `yaml:"image"`
	ImagePullPolicy string            `yaml:"image_pull_policy"`
	VCPU            int64             `yaml:"vcpu"`
	Memory          string            `yaml:"memory"`
	Replicas        *int              `yaml:"replicas"`
	Min             *int              `yaml:"min"`
	Max             *int              `yaml:"max"`
	ScaleSet        bool              `yaml:"scale_set"`
	Cache           string            `yaml:"cache"`
	Env             map[string]string `yaml:"env"`
}

// PoolOverride changes a pool on one host.
type PoolOverride struct {
	Disabled bool `yaml:"disabled"`
	Replicas *int `yaml:"replicas"`
	Min      *int `yaml:"min"`
	Max      *int `yaml:"max"`
}

// Parse decodes fleet.yaml strictly: unknown or misspelled keys are errors.
func Parse(r io.Reader) (*File, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var f File
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("fleet.yaml is empty")
		}
		return nil, fmt.Errorf("fleet.yaml: %w", err)
	}

	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("fleet.yaml: expected a single YAML document")
	}

	return &f, nil
}

// ParseBytes is Parse on a byte slice.
func ParseBytes(b []byte) (*File, error) { return Parse(bytes.NewReader(b)) }

// resolved is one host's effective configuration.
type resolved struct {
	name  string
	host  Host
	pools []Pool
}

var builtinDefaults = Host{
	API:      "127.0.0.1:18080",
	Metrics:  "127.0.0.1:18081",
	LogLevel: "info",
	BridgeIP: "10.200.0.1",
	Containerd: Containerd{
		Address:   "/run/fireactions-containerd/containerd.sock",
		Namespace: "fireactions",
	},
	Firecracker: Firecracker{
		BinaryPath:      "/usr/local/bin/firecracker",
		KernelImagePath: "/var/lib/fireactions/kernels/6.18/vmlinux",
		KernelArgs:      "console=ttyS0 reboot=k panic=1 pci=off nomodules rw",
	},
	Cache: Cache{
		Upstream:    "https://turbo.apehost.net",
		MaxSize:     "4GiB",
		Port:        8787,
		MetricsPort: 9787,
		Dir:         "/var/lib/fireactions/turbocache",
	},
}

func overlay(base, over Host) Host {
	str := func(b *string, o string) {
		if o != "" {
			*b = o
		}
	}
	num := func(b *int, o int) {
		if o != 0 {
			*b = o
		}
	}

	str(&base.API, over.API)
	str(&base.Metrics, over.Metrics)
	str(&base.LogLevel, over.LogLevel)
	str(&base.BridgeIP, over.BridgeIP)
	str(&base.Containerd.Address, over.Containerd.Address)
	str(&base.Containerd.Namespace, over.Containerd.Namespace)
	str(&base.Firecracker.BinaryPath, over.Firecracker.BinaryPath)
	str(&base.Firecracker.KernelImagePath, over.Firecracker.KernelImagePath)
	str(&base.Firecracker.KernelArgs, over.Firecracker.KernelArgs)
	if over.Cache.Enabled != nil {
		base.Cache.Enabled = over.Cache.Enabled
	}
	str(&base.Cache.Upstream, over.Cache.Upstream)
	str(&base.Cache.Team, over.Cache.Team)
	str(&base.Cache.MaxSize, over.Cache.MaxSize)
	num(&base.Cache.Port, over.Cache.Port)
	num(&base.Cache.MetricsPort, over.Cache.MetricsPort)
	str(&base.Cache.Dir, over.Cache.Dir)

	pools := map[string]PoolOverride{}
	for k, v := range base.Pools {
		pools[k] = v
	}
	for k, v := range over.Pools {
		pools[k] = v
	}
	base.Pools = pools

	return base
}

// resolve validates the file and returns every host's effective config, sorted by host name.
func (f *File) resolve() ([]resolved, error) {
	if f.GitHub.AppID == 0 {
		return nil, errors.New("github.app_id is required")
	}
	if len(f.Hosts) == 0 {
		return nil, errors.New("hosts: at least one host is required")
	}
	if len(f.Pools) == 0 {
		return nil, errors.New("pools: at least one pool is required")
	}

	poolNames := map[string]bool{}
	for i := range f.Pools {
		p := &f.Pools[i]
		if err := p.validate(); err != nil {
			return nil, fmt.Errorf("pool %q: %w", p.Name, err)
		}
		if poolNames[p.Name] {
			return nil, fmt.Errorf("pool %q: duplicate name", p.Name)
		}
		poolNames[p.Name] = true
	}

	names := make([]string, 0, len(f.Hosts))
	for name := range f.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]resolved, 0, len(names))
	for _, name := range names {
		if !hostName.MatchString(name) {
			return nil, fmt.Errorf("host %q: invalid name", name)
		}

		h := overlay(overlay(builtinDefaults, f.Defaults), f.Hosts[name])
		if err := h.validate(); err != nil {
			return nil, fmt.Errorf("host %s: %w", name, err)
		}

		for poolName := range h.Pools {
			if !poolNames[poolName] {
				return nil, fmt.Errorf("host %s: override for unknown pool %q", name, poolName)
			}
		}

		var pools []Pool
		for _, p := range f.Pools {
			o := h.Pools[p.Name]
			if o.Disabled {
				continue
			}
			if o.Replicas != nil {
				p.Replicas = o.Replicas
			}
			if o.Min != nil {
				p.Min = o.Min
			}
			if o.Max != nil {
				p.Max = o.Max
			}
			if err := p.validateSizing(); err != nil {
				return nil, fmt.Errorf("host %s: pool %q: %w", name, p.Name, err)
			}
			if p.Cache != "off" && !h.cacheEnabled() {
				return nil, fmt.Errorf("host %s: pool %q uses the cache (%s) but the cache is disabled", name, p.Name, p.Cache)
			}
			pools = append(pools, p)
		}
		if len(pools) == 0 {
			return nil, fmt.Errorf("host %s: no pools enabled", name)
		}

		out = append(out, resolved{name: name, host: h, pools: pools})
	}

	return out, nil
}

var (
	hostName   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretName = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PASSWD|KEY|CREDENTIAL)`)
	envRef     = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
	// Plain enough to embed in a capnp string literal without escaping.
	upstreamURL = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$`)
)

func (h *Host) cacheEnabled() bool { return h.Cache.Enabled == nil || *h.Cache.Enabled }

func (h *Host) validate() error {
	// The fireactions gRPC API is unauthenticated: VMs on the bridge must never reach it.
	if err := requireLoopback("api", h.API); err != nil {
		return err
	}
	if err := requireLoopback("metrics", h.Metrics); err != nil {
		return err
	}
	switch h.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level %q: must be debug, info, warn or error", h.LogLevel)
	}
	if ip := net.ParseIP(h.BridgeIP); ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return fmt.Errorf("bridge_ip %q: must be the VM bridge's IPv4 gateway", h.BridgeIP)
	}

	if h.cacheEnabled() {
		c := h.Cache
		if !upstreamURL.MatchString(c.Upstream) {
			return fmt.Errorf("cache.upstream %q: must be a plain http(s) URL without /v8", c.Upstream)
		}
		if !identifier.MatchString(c.Team) {
			return fmt.Errorf("cache.team %q: required, the TURBO_TEAM slug", c.Team)
		}
		if _, err := parseSize(c.MaxSize); err != nil {
			return fmt.Errorf("cache.max_size: %w", err)
		}
		if c.Port <= 0 || c.Port > 65535 || c.MetricsPort <= 0 || c.MetricsPort > 65535 {
			return errors.New("cache.port and cache.metrics_port must be valid ports")
		}
		if !strings.HasPrefix(c.Dir, "/") || strings.Contains(c.Dir, "..") || strings.ContainsAny(c.Dir, " \t\n\"'\\") {
			return fmt.Errorf("cache.dir %q: must be a plain absolute path", c.Dir)
		}
	}

	return nil
}

func requireLoopback(field, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s %q: %w", field, addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || ip.To4() == nil || !ip.IsLoopback() {
		return fmt.Errorf("%s %q: must bind an IPv4 loopback address (the API is unauthenticated)", field, addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
		return fmt.Errorf("%s %q: invalid port", field, addr)
	}
	return nil
}

func (p *Pool) validate() error {
	if !identifier.MatchString(p.Name) {
		return errors.New("name is required (letters, digits, . _ -)")
	}
	if p.RunnerName == "" {
		p.RunnerName = p.Name
	}
	if !identifier.MatchString(p.RunnerName) {
		return fmt.Errorf("runner_name %q: invalid", p.RunnerName)
	}
	owner, repo, ok := strings.Cut(p.Repository, "/")
	if !ok || !identifier.MatchString(owner) || !identifier.MatchString(repo) {
		return fmt.Errorf("repository %q: must be OWNER/REPO (a private repository)", p.Repository)
	}
	if len(p.Labels) == 0 {
		return errors.New("labels are required")
	}
	if p.Image == "" {
		return errors.New("image is required")
	}
	if p.ImagePullPolicy == "" {
		p.ImagePullPolicy = "IfNotPresent"
	}
	switch p.ImagePullPolicy {
	case "Always", "Never", "IfNotPresent":
	default:
		return fmt.Errorf("image_pull_policy %q: must be Always, Never or IfNotPresent", p.ImagePullPolicy)
	}
	if p.VCPU <= 0 {
		return errors.New("vcpu must be positive")
	}
	if b, err := parseSize(p.Memory); err != nil || b%(1<<20) != 0 {
		return fmt.Errorf("memory %q: must be a whole number of MiB, e.g. 8GiB", p.Memory)
	}
	if p.Cache == "" {
		p.Cache = "off"
	}
	switch p.Cache {
	case "off", "ro", "rw":
	default:
		return fmt.Errorf("cache %q: must be off, ro or rw", p.Cache)
	}

	for _, name := range sortedKeys(p.Env) {
		value := p.Env[name]
		if !envName.MatchString(name) {
			return fmt.Errorf("env %q: invalid name", name)
		}
		if p.Cache != "off" && strings.HasPrefix(name, "TURBO_") {
			return fmt.Errorf("env %q: TURBO_* is set by the pool's cache mode", name)
		}
		if secretName.MatchString(name) && !envRef.MatchString(value) {
			return fmt.Errorf("env %q looks like a secret: set it to $SOME_ENV_VAR so the value comes from the environment, not fleet.yaml", name)
		}
	}

	return p.validateSizing()
}

func (p *Pool) validateSizing() error {
	for name, v := range map[string]*int{"replicas": p.Replicas, "min": p.Min, "max": p.Max} {
		if v != nil && *v < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	if p.Min != nil && p.Max != nil && *p.Min > *p.Max {
		return fmt.Errorf("min (%d) is greater than max (%d)", *p.Min, *p.Max)
	}
	if p.ScaleSet {
		if p.Max == nil || *p.Max < 1 {
			return errors.New("scale_set requires max >= 1 (the capacity advertised to GitHub)")
		}
		if p.Replicas != nil {
			return errors.New("replicas and scale_set are mutually exclusive; use min for warm VMs")
		}
	} else if p.Replicas == nil {
		return errors.New("replicas is required unless scale_set is true")
	}
	return nil
}

// parseSize parses sizes like 512MiB, 8GiB or 1TiB into bytes.
func parseSize(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}}
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseInt(n, 10, 64)
			if err != nil || v <= 0 {
				return 0, fmt.Errorf("%q: expected a positive whole number before %s", s, u.suffix)
			}
			return v * u.mult, nil
		}
	}
	return 0, fmt.Errorf("%q: expected a size like 512MiB, 8GiB or 1TiB", s)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
