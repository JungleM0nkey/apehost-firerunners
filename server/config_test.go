package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewConfig(t *testing.T) {
	config, err := NewConfig("testdata/config1.yaml")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	assert.Equal(t, "testdata/config1.yaml", config.path)
}

func TestNewConfig_PoolEnv(t *testing.T) {
	base, err := os.ReadFile("testdata/config1.yaml")
	require.NoError(t, err)

	write := func(t *testing.T, env string) string {
		t.Helper()
		cfg := strings.Replace(string(base), "  runner:\n", env+"  runner:\n", 1)
		path := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(cfg), 0o600))
		return path
	}

	t.Run("valid env is loaded", func(t *testing.T) {
		config, err := NewConfig(write(t, "  env:\n    TURBO_API: http://10.200.0.1:8787\n"))
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"TURBO_API": "http://10.200.0.1:8787"}, config.Pools[0].Env)
	})

	t.Run("reserved name is rejected without leaking the value", func(t *testing.T) {
		_, err := NewConfig(write(t, "  env:\n    GITHUB_TOKEN: s3cret\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "GITHUB_TOKEN")
		assert.NotContains(t, err.Error(), "s3cret")
	})
}
