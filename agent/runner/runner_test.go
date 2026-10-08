package runner

import (
	"bytes"
	"os/user"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestProcessEnv_IncludesPoolEnvAfterBase(t *testing.T) {
	r := New("cfg", WithEnv(map[string]string{"TURBO_TOKEN": "s3cret", "TURBO_API": "http://10.200.0.1:8787"}))

	env := r.processEnv(&user.User{Username: "runner", HomeDir: "/home/runner"}, 1001, 121)

	assert.Equal(t, []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LOGNAME=runner",
		"HOME=/home/runner",
		"USER=runner",
		"UID=1001",
		"GID=121",
		"TURBO_API=http://10.200.0.1:8787",
		"TURBO_TOKEN=s3cret",
	}, env)
}

func TestProcessEnv_NoPoolEnv(t *testing.T) {
	env := New("cfg").processEnv(&user.User{Username: "runner", HomeDir: "/home/runner"}, 1001, 121)
	assert.Len(t, env, 6)
}

func TestWithEnv_ValuesNotLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	r := New("cfg", WithLogger(&logger), WithOwner("root"), WithGroup("root"),
		WithDirectory(t.TempDir()), WithEnv(map[string]string{"TURBO_TOKEN": "s3cret"}))

	// Run gets as far as building the env, then fails to start (no run.sh).
	assert.Error(t, r.Run(t.Context()))

	assert.Contains(t, buf.String(), "TURBO_TOKEN", "variable names are logged")
	assert.NotContains(t, buf.String(), "s3cret", "values are not")
}
