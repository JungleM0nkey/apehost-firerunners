package agent

import (
	"fmt"

	"github.com/hostinger/fireactions/helper/envvar"
)

// EnvFromMetadata reads the pool's runner environment from the "fireactions"
// MMDS document (key "env"). A missing key means no extra variables. Errors
// never include values.
func EnvFromMetadata(metadata map[string]interface{}) (map[string]string, error) {
	raw, ok := metadata["env"]
	if !ok || raw == nil {
		return nil, nil
	}

	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("env: expected an object, got %T", raw)
	}

	env := make(map[string]string, len(m))
	for name, value := range m {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("env %q: expected a string, got %T", name, value)
		}
		env[name] = s
	}

	if err := envvar.Validate(env); err != nil {
		return nil, err
	}

	return env, nil
}
