package envvar

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "nil", env: nil},
		{name: "ok", env: map[string]string{"TURBO_API": "http://10.200.0.1:8787", "TURBO_TOKEN": "s3cret", "_X1": ""}},
		{name: "bad name", env: map[string]string{"1ABC": "x"}, wantErr: `env "1ABC": invalid variable name`},
		{name: "dash", env: map[string]string{"A-B": "x"}, wantErr: "invalid variable name"},
		{name: "reserved", env: map[string]string{"PATH": "/tmp"}, wantErr: "reserved variable"},
		{name: "reserved prefix", env: map[string]string{"GITHUB_TOKEN": "x"}, wantErr: "GITHUB_* variables are reserved"},
		{name: "nul", env: map[string]string{"A": "s3cret\x00"}, wantErr: "NUL byte"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.env)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}

			assert.ErrorContains(t, err, tt.wantErr)
			assert.NotContains(t, err.Error(), "s3cret", "errors must not leak values")
		})
	}
}

func TestPairsSorted(t *testing.T) {
	assert.Equal(t, []string{"A=1", "B=x=y"}, Pairs(map[string]string{"B": "x=y", "A": "1"}))
	assert.Empty(t, Pairs(nil))
}
