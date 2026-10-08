package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvFromMetadata(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		env, err := EnvFromMetadata(map[string]interface{}{"hostname": "x"})
		require.NoError(t, err)
		assert.Nil(t, env)
	})

	t.Run("present", func(t *testing.T) {
		env, err := EnvFromMetadata(map[string]interface{}{"env": map[string]interface{}{"TURBO_CACHE": "remote:r"}})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"TURBO_CACHE": "remote:r"}, env)
	})

	t.Run("non-string value", func(t *testing.T) {
		_, err := EnvFromMetadata(map[string]interface{}{"env": map[string]interface{}{"N": 1.0}})
		assert.ErrorContains(t, err, `env "N": expected a string`)
	})

	t.Run("reserved name, value not leaked", func(t *testing.T) {
		_, err := EnvFromMetadata(map[string]interface{}{"env": map[string]interface{}{"HOME": "s3cret"}})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "s3cret")
	})
}
