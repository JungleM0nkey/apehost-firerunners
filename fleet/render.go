package fleet

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/hostinger/fireactions/server"
	"gopkg.in/yaml.v3"
)

//go:embed templates/*
var templates embed.FS

// Rendered file names, per host.
const (
	FileFireactions = "fireactions.yaml"                // -> /etc/fireactions/config.yaml
	FileCapnp       = "cache-proxy.capnp"               // -> /opt/fireactions/cache-proxy/config.capnp
	FileCacheEnv    = "cache-proxy.env"                 // -> /etc/fireactions/cache-proxy.env
	FileCacheUnit   = "fireactions-cache-proxy.service" // -> /etc/systemd/system/
)

const header = "Rendered by `fireactions fleet render` from fleet.yaml. Do not edit; edit fleet.yaml and re-render."

// File is one rendered file.
type RenderedFile struct {
	Name    string
	Mode    fs.FileMode // 0600 when the file holds secrets
	Content []byte
}

// Getenv looks up secret environment variables; os.Getenv in production.
type Getenv func(string) string

// Render renders every host. Output is deterministic: same input and
// environment, byte-identical files.
func Render(f *File, getenv Getenv) (map[string][]RenderedFile, error) {
	hosts, err := f.resolve()
	if err != nil {
		return nil, err
	}

	out := make(map[string][]RenderedFile, len(hosts))
	for _, h := range hosts {
		files, err := renderHost(f, h, getenv)
		if err != nil {
			return nil, fmt.Errorf("host %s: %w", h.name, err)
		}
		out[h.name] = files
	}

	return out, nil
}

func renderHost(f *File, h resolved, getenv Getenv) ([]RenderedFile, error) {
	secret := func(name string) (string, error) {
		v := getenv(name)
		if v == "" {
			return "", fmt.Errorf("secret %s is not set in the environment", name)
		}
		if !tokenChars.MatchString(v) {
			return "", fmt.Errorf("secret %s contains characters other than [A-Za-z0-9._~+/=-]", name)
		}
		return v, nil
	}

	usesCache := map[string]bool{}
	for _, p := range h.pools {
		usesCache[p.Cache] = true
	}
	tokens := map[string]string{}
	for mode, env := range map[string]string{"rw": EnvCacheTokenRW, "ro": EnvCacheTokenRO} {
		if usesCache[mode] {
			v, err := secret(env)
			if err != nil {
				return nil, err
			}
			tokens[mode] = v
		}
	}

	fa, err := renderFireactions(f, h, tokens, getenv)
	if err != nil {
		return nil, err
	}
	files := []RenderedFile{{Name: FileFireactions, Mode: 0o600, Content: fa}}

	if !h.host.cacheEnabled() {
		return files, nil
	}

	upstreamToken, err := secret(EnvCacheUpstreamToken)
	if err != nil {
		return nil, err
	}

	cacheFiles, err := renderCache(h, upstreamToken, tokens)
	if err != nil {
		return nil, err
	}

	return append(files, cacheFiles...), nil
}

var tokenChars = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)

// fireactions config output types: the subset of server.Config we emit, in a fixed order.
type faConfig struct {
	BindAddress string       `yaml:"bind_address"`
	Metrics     faMetrics    `yaml:"metrics"`
	Containerd  faContainerd `yaml:"containerd"`
	GitHub      faGitHub     `yaml:"github"`
	Pools       []faPool     `yaml:"pools"`
	LogLevel    string       `yaml:"log_level"`
}

type faMetrics struct {
	Enabled bool   `yaml:"enabled"`
	Address string `yaml:"address"`
}

type faContainerd struct {
	Address   string `yaml:"address"`
	Namespace string `yaml:"namespace"`
}

type faGitHub struct {
	AppID             int64  `yaml:"app_id"`
	AppPrivateKeyFile string `yaml:"app_private_key_file"`
}

type faPool struct {
	Name        string            `yaml:"name"`
	Replicas    *int              `yaml:"replicas,omitempty"`
	Min         *int              `yaml:"min,omitempty"`
	Max         *int              `yaml:"max,omitempty"`
	ScaleSet    *faScaleSet       `yaml:"scale_set,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"`
	Runner      faRunner          `yaml:"runner"`
	Firecracker faFirecracker     `yaml:"firecracker"`
}

type faScaleSet struct {
	Name   string   `yaml:"name"`
	Labels []string `yaml:"labels"`
}

type faRunner struct {
	Name            string   `yaml:"name"`
	Image           string   `yaml:"image"`
	ImagePullPolicy string   `yaml:"image_pull_policy"`
	GroupID         int64    `yaml:"group_id"`
	Organization    string   `yaml:"organization"`
	Repository      string   `yaml:"repository"`
	Labels          []string `yaml:"labels"`
}

type faFirecracker struct {
	BinaryPath      string          `yaml:"binary_path"`
	KernelImagePath string          `yaml:"kernel_image_path"`
	KernelArgs      string          `yaml:"kernel_args"`
	MachineConfig   faMachineConfig `yaml:"machine_config"`
}

type faMachineConfig struct {
	VcpuCount  int64 `yaml:"vcpu_count"`
	MemSizeMib int64 `yaml:"mem_size_mib"`
}

func renderFireactions(f *File, h resolved, tokens map[string]string, getenv Getenv) ([]byte, error) {
	keyFile := f.GitHub.AppPrivateKeyFile
	if keyFile == "" {
		keyFile = "/etc/fireactions/app.pem"
	}

	cfg := faConfig{
		BindAddress: h.host.API,
		Metrics:     faMetrics{Enabled: true, Address: h.host.Metrics},
		Containerd:  faContainerd(h.host.Containerd),
		GitHub:      faGitHub{AppID: f.GitHub.AppID, AppPrivateKeyFile: keyFile},
		LogLevel:    h.host.LogLevel,
	}

	for _, p := range h.pools {
		owner, repo, _ := strings.Cut(p.Repository, "/")
		mem, _ := parseSize(p.Memory)

		env := map[string]string{}
		for _, name := range sortedKeys(p.Env) {
			value := p.Env[name]
			if m := envRef.FindStringSubmatch(value); m != nil {
				if value = getenv(m[1]); value == "" {
					return nil, fmt.Errorf("pool %q: env %s refers to $%s, which is not set in the environment", p.Name, name, m[1])
				}
			}
			env[name] = value
		}
		if p.Cache != "off" {
			env["TURBO_API"] = fmt.Sprintf("http://%s:%d", h.host.BridgeIP, h.host.Cache.Port)
			env["TURBO_TEAM"] = h.host.Cache.Team
			env["TURBO_TOKEN"] = tokens[p.Cache]
			env["TURBO_CACHE"] = map[string]string{"ro": "remote:r", "rw": "remote:rw"}[p.Cache]
		}
		if len(env) == 0 {
			env = nil
		}

		fp := faPool{
			Name: p.Name, Replicas: p.Replicas, Min: p.Min, Max: p.Max, Env: env,
			Runner: faRunner{
				Name: p.RunnerName, Image: p.Image, ImagePullPolicy: p.ImagePullPolicy,
				GroupID: 1, Organization: owner, Repository: repo, Labels: p.Labels,
			},
			Firecracker: faFirecracker{
				BinaryPath:      h.host.Firecracker.BinaryPath,
				KernelImagePath: h.host.Firecracker.KernelImagePath,
				KernelArgs:      h.host.Firecracker.KernelArgs,
				MachineConfig:   faMachineConfig{VcpuCount: p.VCPU, MemSizeMib: mem >> 20},
			},
		}
		if p.ScaleSet {
			// One scale set per host per pool; GitHub matches jobs across hosts.
			fp.ScaleSet = &faScaleSet{Name: p.Name + "-" + h.name, Labels: p.Labels}
		}
		cfg.Pools = append(cfg.Pools, fp)
	}

	var buf bytes.Buffer
	buf.WriteString("# " + header + "\n# Contains secrets (pool env tokens): keep it 0600, never commit it.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}

	if err := checkFireactions(buf.Bytes()); err != nil {
		return nil, fmt.Errorf("rendered fireactions config is invalid: %w", err)
	}

	return buf.Bytes(), nil
}

// checkFireactions validates rendered output with fireactions' own config validation.
func checkFireactions(b []byte) error {
	c := server.DefaultConfig()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return err
	}
	return c.Validate()
}

type cacheData struct {
	Header        string
	BridgeIP      string
	Port          int
	MetricsPort   int
	Dir           string
	SettingsJSON  string
	MountUnit     string
	TmpfsSize     string
	UpstreamToken string
	TokensRW      string
	TokensRO      string
}

func renderCache(h resolved, upstreamToken string, tokens map[string]string) ([]RenderedFile, error) {
	c := h.host.Cache
	maxBytes, _ := parseSize(c.MaxSize)

	settings, err := json.Marshal(struct {
		Upstream                   string `json:"upstream"`
		MaxBytes                   int64  `json:"maxBytes"`
		MaintenanceIntervalSeconds int    `json:"maintenanceIntervalSeconds"`
	}{strings.TrimRight(c.Upstream, "/"), maxBytes, 60})
	if err != nil {
		return nil, err
	}

	// The tmpfs also holds the index and in-flight writes: cap + 25%, at least +512MiB.
	tmpfsMiB := (maxBytes >> 20) + max((maxBytes>>20)/4, 512)

	data := cacheData{
		Header:        header,
		BridgeIP:      h.host.BridgeIP,
		Port:          c.Port,
		MetricsPort:   c.MetricsPort,
		Dir:           strings.TrimRight(c.Dir, "/"),
		SettingsJSON:  strings.ReplaceAll(string(settings), `"`, `\"`),
		MountUnit:     systemdEscapePath(c.Dir) + ".mount",
		TmpfsSize:     fmt.Sprintf("%dM", tmpfsMiB),
		UpstreamToken: upstreamToken,
		TokensRW:      tokens["rw"],
		TokensRO:      tokens["ro"],
	}

	files := []RenderedFile{
		{Name: FileCapnp, Mode: 0o644},
		{Name: FileCacheEnv, Mode: 0o600},
		{Name: FileCacheUnit, Mode: 0o644},
		{Name: data.MountUnit, Mode: 0o644},
	}
	tmpls := []string{"cache-proxy.capnp.tmpl", "cache-proxy.env.tmpl", "cache-proxy.service.tmpl", "cache-proxy.mount.tmpl"}

	for i, name := range tmpls {
		t, err := template.ParseFS(templates, "templates/"+name)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data); err != nil {
			return nil, err
		}
		files[i].Content = buf.Bytes()
	}

	return files, nil
}

// systemdEscapePath is `systemd-escape --path` for plain absolute paths.
func systemdEscapePath(p string) string {
	p = strings.Trim(p, "/")
	var b strings.Builder
	for i, r := range p {
		switch {
		case r == '/':
			b.WriteByte('-')
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.' && i > 0:
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, `\x%02x`, r)
		}
	}
	return b.String()
}

// WriteHost writes one host's rendered files into dir with their modes.
func WriteHost(dir string, files []RenderedFile) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for _, f := range files {
		path := filepath.Join(dir, f.Name)
		// Remove first so a stricter mode always applies, even over an existing file.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.WriteFile(path, f.Content, f.Mode); err != nil {
			return err
		}
		if err := os.Chmod(path, f.Mode); err != nil { // umask-proof
			return err
		}
	}
	return nil
}
