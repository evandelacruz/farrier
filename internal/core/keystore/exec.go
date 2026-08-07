package keystore

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/evandelacruz/farrier/internal/core/driver"
)

// execDriver satisfies Driver for an out-of-tree keystore driver reached
// through the CORE-003 exec protocol: one process per Resolve call,
// method "resolve", params {"key": keyName}, result {"secret": <base64>}.
// Base64 lets an executable return arbitrary binary key material (a
// certificate, a host key) through JSON, which is text-only.
type execDriver struct {
	invoker driver.Invoker
}

type execResolveParams struct {
	Key string `json:"key"`
}

type execResolveResult struct {
	Secret string `json:"secret"`
}

func (d execDriver) Resolve(ctx context.Context, keyName string) ([]byte, error) {
	var result execResolveResult
	if err := d.invoker.Invoke(ctx, "resolve", execResolveParams{Key: keyName}, &result); err != nil {
		return nil, fmt.Errorf("keystore: exec: resolve key %q: %w", keyName, err)
	}
	secret, err := base64.StdEncoding.DecodeString(result.Secret)
	if err != nil {
		return nil, fmt.Errorf("keystore: exec: resolve key %q: decode secret: %w", keyName, err)
	}
	if len(secret) == 0 {
		return nil, fmt.Errorf("keystore: exec: key %q resolved to empty secret", keyName)
	}
	return secret, nil
}

func newExecDriver(driverName string, config map[string]any) (Driver, error) {
	path, err := stringConfig(config, "path")
	if err != nil {
		return nil, fmt.Errorf("keystore: %s: %w (unrecognized driver name, treated as an out-of-tree exec driver)", driverName, err)
	}

	var args []string
	if raw, ok := config["args"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("keystore: %s: config.args must be a list of strings", driverName)
		}
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("keystore: %s: config.args must be a list of strings", driverName)
			}
			args = append(args, s)
		}
	}

	return execDriver{invoker: driver.Exec{Path: path, Args: args}}, nil
}
