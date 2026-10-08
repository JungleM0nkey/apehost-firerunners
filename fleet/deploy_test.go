package fleet

import (
	"os"
	"testing"

	"github.com/hostinger/fireactions/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// The committed fleet must always render, and podbox's rendered config must
// match the hand-written one it replaces (apart from the key now being a
// file, and the runner image, which the runner-image workflow bumps).
func TestDeployFleet_PodboxMatchesHandWrittenConfig(t *testing.T) {
	b, err := os.ReadFile("../deploy/fleet.yaml")
	require.NoError(t, err)
	f, err := ParseBytes(b)
	require.NoError(t, err)

	hosts, err := Render(f, getenv(nil))
	require.NoError(t, err)
	require.Contains(t, hosts, "podbox")

	var rendered, handWritten server.Config
	require.NoError(t, yaml.Unmarshal(hosts["podbox"][0].Content, &rendered))
	example, err := os.ReadFile("../deploy/podbox/config.example.yaml")
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(example, &handWritten))

	assert.Equal(t, handWritten.BindAddress, rendered.BindAddress)
	assert.Equal(t, handWritten.Metrics, rendered.Metrics)
	assert.Equal(t, handWritten.Containerd, rendered.Containerd)
	assert.Equal(t, handWritten.LogLevel, rendered.LogLevel)
	assert.Equal(t, handWritten.GitHub.AppID, rendered.GitHub.AppID)
	assert.Equal(t, "/etc/fireactions/app.pem", rendered.GitHub.AppPrivateKeyFile)
	assert.Empty(t, rendered.GitHub.AppPrivateKey, "the key stays in its own file")
	// The runner image is bumped by the runner-image workflow, so it may
	// differ from the hand-written snapshot; everything else must match.
	require.Len(t, rendered.Pools, len(handWritten.Pools))
	for i := range rendered.Pools {
		assert.NotEmpty(t, rendered.Pools[i].Runner.Image)
		rendered.Pools[i].Runner.Image = handWritten.Pools[i].Runner.Image
	}
	assert.Equal(t, handWritten.Pools, rendered.Pools)
}
