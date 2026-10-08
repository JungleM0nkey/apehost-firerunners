package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func fireactionsMetadata(t *testing.T, md map[string]interface{}) map[string]interface{} {
	t.Helper()
	return md["latest"].(map[string]interface{})["meta-data"].(map[string]interface{})["fireactions"].(map[string]interface{})
}

func TestRunnerMetadata_PoolEnvReachesAgentKey(t *testing.T) {
	shutdown := true
	cfg := &PoolConfig{
		ShutdownOnExit: &shutdown,
		Firecracker:    &FirecrackerConfig{Metadata: map[string]interface{}{"custom": "kept"}},
		Env:            map[string]string{"TURBO_API": "http://10.200.0.1:8787", "TURBO_TOKEN": "tok"},
	}

	md := runnerMetadata(cfg, "fa-abc", "jit")

	fa := fireactionsMetadata(t, md)
	assert.Equal(t, map[string]interface{}{"TURBO_API": "http://10.200.0.1:8787", "TURBO_TOKEN": "tok"}, fa["env"])
	assert.Equal(t, "jit", fa["runner_jit_config"])
	assert.Equal(t, "fa-abc", fa["hostname"])
	assert.Equal(t, "kept", md["latest"].(map[string]interface{})["meta-data"].(map[string]interface{})["custom"])
}

func TestRunnerMetadata_NoEnvIsUnchanged(t *testing.T) {
	shutdown := false
	cfg := &PoolConfig{ShutdownOnExit: &shutdown, Firecracker: &FirecrackerConfig{}}

	fa := fireactionsMetadata(t, runnerMetadata(cfg, "fa-abc", "jit"))

	assert.Equal(t, map[string]interface{}{
		"runner_id":         "fa-abc",
		"runner_jit_config": "jit",
		"hostname":          "fa-abc",
		"shutdown_on_exit":  false,
	}, fa)
}
