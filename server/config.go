package server

import (
	"fmt"
	"os"

	"github.com/go-playground/validator/v10"
	"github.com/hostinger/fireactions/helper/envvar"
	"gopkg.in/yaml.v3"
)

// Config is the configuration for the Client.
type Config struct {
	BindAddress      string            `yaml:"bind_address" validate:"required,hostname_port"`
	Containerd       *ContainerdConfig `yaml:"containerd" validate:"required"`
	Metrics          *MetricsConfig    `yaml:"metrics"`
	BasicAuthEnabled bool              `yaml:"basic_auth_enabled" validate:""`
	BasicAuthUsers   map[string]string `yaml:"basic_auth_users" validate:"required_if=basic_auth_enabled true"`
	GitHub           *GitHubConfig     `yaml:"github" validate:"required"`
	Pools            []*PoolConfig     `yaml:"pools" validate:"required,min=1"`
	LogLevel         string            `yaml:"log_level" validate:"required,oneof=debug info warn error fatal panic trace"`

	path string
}

type ContainerdConfig struct {
	Address   string `yaml:"address" validate:"required"`
	Namespace string `yaml:"namespace" validate:"required"`
}

type MetricsConfig struct {
	Enabled bool   `yaml:"enabled" validate:""`
	Address string `yaml:"address" validate:"required_if=enabled true,hostname_port"`
}

type GitHubConfig struct {
	AppPrivateKey string `yaml:"app_private_key" validate:"required_without=AppPrivateKeyFile"`
	// AppPrivateKeyFile is read at startup when AppPrivateKey is empty, so the
	// config file itself can hold no secrets.
	AppPrivateKeyFile string `yaml:"app_private_key_file"`
	AppID             int64  `yaml:"app_id" validate:"required"`
}

type RunnerConfig struct {
	Name            string `yaml:"name" validate:"required"`
	ImagePullPolicy string `yaml:"image_pull_policy" validate:"required,oneof=Always Never IfNotPresent"`
	Image           string `yaml:"image" validate:"required"`
	Organization    string `yaml:"organization" validate:"required"`
	// Repository, when set, registers repo-level runners for Organization/Repository
	// instead of org-level ones (needed for personal accounts).
	Repository string   `yaml:"repository"`
	GroupID    int64    `yaml:"group_id" validate:"required"`
	Labels     []string `yaml:"labels" validate:"required"`
}

type FirecrackerConfig struct {
	BinaryPath      string                   `yaml:"binary_path" `
	KernelImagePath string                   `yaml:"kernel_image_path"`
	KernelArgs      string                   `yaml:"kernel_args"`
	MachineConfig   FirecrackerMachineConfig `yaml:"machine_config"`
	Metadata        map[string]interface{}   `yaml:"metadata"`
}

type FirecrackerMachineConfig struct {
	VcpuCount  int64 `yaml:"vcpu_count"`
	MemSizeMib int64 `yaml:"mem_size_mib"`
}

// DefaultConfig creates a new Config with default values.
func DefaultConfig() *Config {
	c := &Config{
		BindAddress:      ":8080",
		Containerd:       &ContainerdConfig{Address: "/run/containerd/containerd.sock", Namespace: "fireactions"},
		Metrics:          &MetricsConfig{Enabled: true, Address: ":8081"},
		BasicAuthEnabled: false,
		BasicAuthUsers:   map[string]string{},
		GitHub:           &GitHubConfig{AppPrivateKey: "", AppID: 0},
		Pools:            []*PoolConfig{},
		LogLevel:         "debug",
	}

	return c
}

// NewConfigFromFile creates a new Config from a file.
func NewConfig(path string) (*Config, error) {
	c := DefaultConfig()
	c.path = path

	err := c.Load()
	if err != nil {
		return nil, err
	}

	err = c.Validate()
	if err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}

	return c, nil
}

// LoadFromFile loads the configuration from a file.
func (c *Config) Load() error {
	file, err := os.OpenFile(c.path, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}

	defer func() {
		_ = file.Close()
	}()

	if err := yaml.NewDecoder(file).Decode(c); err != nil {
		return err
	}

	if c.GitHub != nil && c.GitHub.AppPrivateKey == "" && c.GitHub.AppPrivateKeyFile != "" {
		key, err := os.ReadFile(c.GitHub.AppPrivateKeyFile)
		if err != nil {
			return fmt.Errorf("github app_private_key_file: %w", err)
		}
		c.GitHub.AppPrivateKey = string(key)
	}

	return nil
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	if err := validator.New().Struct(c); err != nil {
		return err
	}

	for _, pool := range c.Pools {
		if err := envvar.Validate(pool.Env); err != nil {
			return fmt.Errorf("pool %s: %w", pool.Name, err)
		}

		if pool.Min != nil && pool.Max != nil && *pool.Min > *pool.Max {
			return fmt.Errorf("pool %s: min (%d) is greater than max (%d)", pool.Name, *pool.Min, *pool.Max)
		}

		if pool.ScaleSet != nil {
			if pool.Max == nil {
				return fmt.Errorf("pool %s: scale_set requires max (the capacity advertised to GitHub)", pool.Name)
			}
			if pool.Replicas != 0 {
				return fmt.Errorf("pool %s: replicas and scale_set are mutually exclusive; use min for warm VMs", pool.Name)
			}
		}
	}

	return nil
}
