package blob

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/driver"
)

// New builds the Adapter named by driverName from its non-secret config, as
// carried by a bundle manifest's blob DriverRef (BKUP-006, the first real
// caller: it resolves a bundle's configured blob target into the
// state.BlobExporter backup.Run captures from). "local" and "s3" are the
// shipped in-tree adapters (BLOB-001, BLOB-002); any other name is treated
// as an out-of-tree adapter executable reached through the CORE-003 exec
// protocol, configured the same way keystore.New and dns's exec driver
// are: config.path is the executable, config.args its fixed arguments —
// the same plugin posture across every driver-type package (CLAUDE.md
// "house patterns").
//
// s3's credentials are read from the AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY environment variables, never from config — the
// same posture backup.OpenDestination's own "s3://" resolution already
// takes (BKUP-005) for exactly the same reason: a secret access key must
// never appear in a bundle manifest, in `ps` output, or in shell history.
func New(driverName string, config map[string]any) (Adapter, error) {
	switch driverName {
	case "":
		return nil, errors.New("blob: driver name is required")
	case "local":
		return newLocalFromConfig(config)
	case "s3":
		return newS3FromConfig(config)
	default:
		return newExecFromConfig(driverName, config)
	}
}

func newLocalFromConfig(config map[string]any) (Adapter, error) {
	path, err := stringConfig(config, "path")
	if err != nil {
		return nil, fmt.Errorf("blob: local: %w", err)
	}
	return NewLocal(path)
}

func newS3FromConfig(config map[string]any) (Adapter, error) {
	bucket, err := stringConfig(config, "bucket")
	if err != nil {
		return nil, fmt.Errorf("blob: s3: %w", err)
	}
	endpoint, err := stringConfig(config, "endpoint")
	if err != nil {
		return nil, fmt.Errorf("blob: s3: %w", err)
	}
	region, _ := config["region"].(string)

	useSSL := true
	if v, ok := config["ssl"]; ok {
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("blob: s3: config.ssl must be a boolean")
		}
		useSSL = b
	}
	var pathStyle bool
	if v, ok := config["pathStyle"]; ok {
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("blob: s3: config.pathStyle must be a boolean")
		}
		pathStyle = b
	}

	accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if accessKeyID == "" || secretAccessKey == "" {
		return nil, errors.New("blob: s3: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set")
	}

	return NewS3(S3Config{
		Endpoint:        endpoint,
		Region:          region,
		Bucket:          bucket,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		UseSSL:          useSSL,
		PathStyle:       pathStyle,
	})
}

func newExecFromConfig(driverName string, config map[string]any) (Adapter, error) {
	path, err := stringConfig(config, "path")
	if err != nil {
		return nil, fmt.Errorf("blob: %s: %w (unrecognized driver name, treated as an out-of-tree exec driver)", driverName, err)
	}

	var args []string
	if raw, ok := config["args"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("blob: %s: config.args must be a list of strings", driverName)
		}
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("blob: %s: config.args must be a list of strings", driverName)
			}
			args = append(args, s)
		}
	}

	return NewExec(driver.Exec{Path: path, Args: args}), nil
}

// stringConfig reads a required, non-empty string field out of a driver's
// config map, naming the field in any error so a bad manifest is
// diagnosable without a debugger — the same helper keystore.New's own
// drivers use.
func stringConfig(config map[string]any, field string) (string, error) {
	raw, ok := config[field]
	if !ok {
		return "", fmt.Errorf("config.%s is required", field)
	}
	s, ok := raw.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("config.%s must be a non-empty string", field)
	}
	return s, nil
}
