package fleet

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite golden files")

var testEnv = map[string]string{
	EnvCacheUpstreamToken:     "test-upstream-token",
	EnvCacheTokenRW:           "test-rw-token",
	EnvCacheTokenRO:           "test-ro-token",
	"FLEET_TEST_SENTRY_TOKEN": "test-sentry-token",
}

func getenv(env map[string]string) Getenv {
	return func(k string) string { return env[k] }
}

func loadFixture(t *testing.T) *File {
	t.Helper()
	b, err := os.ReadFile("testdata/fleet.yaml")
	require.NoError(t, err)
	f, err := ParseBytes(b)
	require.NoError(t, err)
	return f
}

func TestRender_Golden(t *testing.T) {
	hosts, err := Render(loadFixture(t), getenv(testEnv))
	require.NoError(t, err)
	require.Len(t, hosts, 2)

	for host, files := range hosts {
		dir := filepath.Join("testdata", "golden", host)
		if *update {
			require.NoError(t, os.RemoveAll(dir))
			require.NoError(t, WriteHost(dir, files))
		}

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Len(t, files, len(entries), "%s: golden dir has extra or missing files", host)

		for _, f := range files {
			want, err := os.ReadFile(filepath.Join(dir, f.Name))
			require.NoError(t, err, "%s/%s: missing golden file (run go test ./fleet -update)", host, f.Name)
			assert.Equal(t, string(want), string(f.Content), "%s/%s differs from golden file", host, f.Name)
		}
	}
}

func TestRender_Deterministic(t *testing.T) {
	a, err := Render(loadFixture(t), getenv(testEnv))
	require.NoError(t, err)
	b, err := Render(loadFixture(t), getenv(testEnv))
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

func TestRender_SecretFilesAre0600(t *testing.T) {
	hosts, err := Render(loadFixture(t), getenv(testEnv))
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, WriteHost(dir, hosts["podbox"]))

	for _, f := range hosts["podbox"] {
		info, err := os.Stat(filepath.Join(dir, f.Name))
		require.NoError(t, err)
		hasSecret := strings.Contains(string(f.Content), "test-")
		if hasSecret {
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "%s holds secrets", f.Name)
		} else {
			assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), f.Name)
		}
	}
}

func TestRender_SecretsOnlyInSecretFiles(t *testing.T) {
	hosts, err := Render(loadFixture(t), getenv(testEnv))
	require.NoError(t, err)

	for host, files := range hosts {
		for _, f := range files {
			if f.Mode == 0o600 {
				continue
			}
			for _, v := range testEnv {
				assert.NotContains(t, string(f.Content), v, "%s/%s is world-readable but contains a secret", host, f.Name)
			}
		}
	}
}

func TestParse_UnknownKeyIsAnError(t *testing.T) {
	_, err := ParseBytes([]byte("github: {app_id: 1}\npools: []\nhosts: {}\ndefautls: {}\n"))
	assert.ErrorContains(t, err, "field defautls not found")

	_, err = ParseBytes([]byte("github: {app_id: 1}\nhosts: {podbox: {cache: {max_sise: 1GiB}}}\n"))
	assert.ErrorContains(t, err, "field max_sise not found")
}

func TestRender_MissingSecretIsAnError(t *testing.T) {
	for _, missing := range []string{EnvCacheUpstreamToken, EnvCacheTokenRW, EnvCacheTokenRO, "FLEET_TEST_SENTRY_TOKEN"} {
		t.Run(missing, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range testEnv {
				if k != missing {
					env[k] = v
				}
			}

			_, err := Render(loadFixture(t), getenv(env))
			assert.ErrorContains(t, err, missing)
		})
	}
}

func TestRender_APIMustBeLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:18080", ":18080", "10.200.0.1:18080", "[::]:18080", "[::1]:18080"} {
		t.Run(addr, func(t *testing.T) {
			f := loadFixture(t)
			h := f.Hosts["podbox"]
			h.API = addr
			f.Hosts["podbox"] = h

			_, err := Render(f, getenv(testEnv))
			assert.ErrorContains(t, err, "must bind an IPv4 loopback address")
		})
	}

	f := loadFixture(t)
	f.Defaults.API = "127.0.0.2:18080"
	_, err := Render(f, getenv(testEnv))
	assert.NoError(t, err, "any 127/8 address is loopback")
}

func TestRender_Validation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(f *File)
		wantErr string
	}{
		{"secret-looking env literal", func(f *File) { f.Pools[0].Env["NPM_TOKEN"] = "abc" }, `env "NPM_TOKEN" looks like a secret`},
		{"TURBO_ env with cache", func(f *File) { f.Pools[0].Env["TURBO_API"] = "x" }, "TURBO_* is set by the pool's cache mode"},
		{"bad repository", func(f *File) { f.Pools[0].Repository = "nope" }, "must be OWNER/REPO"},
		{"scale set without max", func(f *File) { f.Pools[0].Max = nil }, "scale_set requires max"},
		{"replicas missing", func(f *File) { f.Pools[1].Replicas = nil }, "replicas is required"},
		{"override unknown pool", func(f *File) {
			h := f.Hosts["podbox"]
			h.Pools = map[string]PoolOverride{"nope": {}}
			f.Hosts["podbox"] = h
		}, `override for unknown pool "nope"`},
		{"min over max on a host", func(f *File) {
			h := f.Hosts["box-b"]
			h.Pools["fireactions-4vcpu-8gb"] = PoolOverride{Min: intp(5), Max: intp(4)}
			f.Hosts["box-b"] = h
		}, "min (5) is greater than max (4)"},
		{"cache pool on cache-less host", func(f *File) {
			h := f.Hosts["box-b"]
			off := false
			h.Cache.Enabled = &off
			f.Hosts["box-b"] = h
		}, "uses the cache (ro) but the cache is disabled"},
		{"bad memory", func(f *File) { f.Pools[0].Memory = "8GB" }, `memory "8GB"`},
		{"bad upstream", func(f *File) { f.Defaults.Cache.Upstream = `https://x/"evil` }, "cache.upstream"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := loadFixture(t)
			tt.mutate(f)
			_, err := Render(f, getenv(testEnv))
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestSystemdEscapePath(t *testing.T) {
	assert.Equal(t, "var-lib-fireactions-turbocache", systemdEscapePath("/var/lib/fireactions/turbocache"))
	assert.Equal(t, `mnt-ramdisk-turbo\x2dcache`, systemdEscapePath("/mnt/ramdisk/turbo-cache/"))
}

func intp(n int) *int { return &n }
