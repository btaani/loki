package loki

import (
	"bytes"
	"fmt"
	"io"

	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/runtimeconfig"
	"go.yaml.in/yaml/v4"

	"github.com/grafana/loki/v3/pkg/runtime"
	util_log "github.com/grafana/loki/v3/pkg/util/log"
	"github.com/grafana/loki/v3/pkg/validation"
)

// runtimeConfigValues are values that can be reloaded from configuration file while Loki is running.
// Reloading is done by runtimeconfig.Manager, which also keeps the currently loaded config.
// These values are then pushed to the components that are interested in them.
type runtimeConfigValues struct {
	DefaultLimits *validation.Limits            `yaml:"defaults"`
	TenantLimits  map[string]*validation.Limits `yaml:"overrides"`
	TenantConfig  map[string]*runtime.Config    `yaml:"configs"`

	Multi kv.MultiRuntimeConfig `yaml:"multi_kv_config"`
}

func (r runtimeConfigValues) validate() error {
	if r.DefaultLimits != nil {
		if err := r.DefaultLimits.Validate(); err != nil {
			return fmt.Errorf("invalid defaults: %w", err)
		}
	}
	for t, c := range r.TenantLimits {
		if c == nil {
			level.Warn(util_log.Logger).Log("msg", "skipping empty tenant limit definition", "tenant", t)
			continue
		}

		if err := c.Validate(); err != nil {
			return fmt.Errorf("invalid override for tenant %s: %w", t, err)
		}
	}
	return nil
}

// newRuntimeConfigLoader returns a Loader that captures the startup limits so
// it can use them as the base when merging with the runtime defaults: block.
// When defaults: is present, per-tenant overrides: blocks inherit from defaults:
// for any field not explicitly set, rather than falling back to limits_config.
func newRuntimeConfigLoader(startupLimits validation.Limits) runtimeconfig.Loader {
	return func(r io.Reader) (interface{}, error) {
		return loadRuntimeConfig(r, startupLimits)
	}
}

func loadRuntimeConfig(r io.Reader, startupLimits validation.Limits) (interface{}, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	// First pass: parse only the defaults: block with startup limits as the seed.
	// The resulting struct has startupLimits values with defaults: fields applied on top.
	var firstPass struct {
		DefaultLimits *validation.Limits `yaml:"defaults"`
	}
	if err := yaml.Unmarshal(raw, &firstPass); err != nil {
		return nil, err
	}

	// If defaults: is set, update the YAML unmarshalling seed so that per-tenant
	// overrides: blocks inherit from defaults: for any field they don't explicitly set.
	// We restore the startup seed after parsing to ensure correctness on future reloads.
	if firstPass.DefaultLimits != nil {
		validation.SetDefaultLimitsForYAMLUnmarshalling(*firstPass.DefaultLimits)
		defer validation.SetDefaultLimitsForYAMLUnmarshalling(startupLimits)
	}

	// Second pass: parse the full config. Per-tenant overrides: blocks are now
	// seeded from the updated default (either defaults: or startupLimits).
	overrides := &runtimeConfigValues{}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&overrides); err != nil {
		return nil, err
	}
	if err := overrides.validate(); err != nil {
		return nil, err
	}
	return overrides, nil
}

// tenantLimitsFromRuntimeConfig implements validation.TenantLimits
type tenantLimitsFromRuntimeConfig struct {
	c *runtimeconfig.Manager
}

func (t *tenantLimitsFromRuntimeConfig) AllByUserID() map[string]*validation.Limits {
	if t.c == nil {
		return nil
	}

	cfg, ok := t.c.GetConfig().(*runtimeConfigValues)
	if cfg != nil && ok {
		return cfg.TenantLimits
	}

	return nil
}

func (t *tenantLimitsFromRuntimeConfig) TenantLimits(userID string) *validation.Limits {
	allByUserID := t.AllByUserID()
	if allByUserID == nil {
		return nil
	}

	return allByUserID[userID]
}

func (t *tenantLimitsFromRuntimeConfig) Defaults() *validation.Limits {
	if t.c == nil {
		return nil
	}

	cfg, ok := t.c.GetConfig().(*runtimeConfigValues)
	if cfg != nil && ok {
		return cfg.DefaultLimits
	}

	return nil
}

func newtenantLimitsFromRuntimeConfig(c *runtimeconfig.Manager) validation.TenantLimits {
	return &tenantLimitsFromRuntimeConfig{c: c}
}

type tenantConfigProvider struct {
	c *runtimeconfig.Manager
}

func newTenantConfigProvider(c *runtimeconfig.Manager) runtime.TenantConfigProvider {
	return &tenantConfigProvider{c: c}
}

// TenantConfig returns the user config or default config if none was defined.
func (t *tenantConfigProvider) TenantConfig(userID string) *runtime.Config {
	if t.c == nil {
		return nil
	}

	cfg, ok := t.c.GetConfig().(*runtimeConfigValues)
	if !ok || cfg == nil {
		return nil
	}
	if tenantCfg, ok := cfg.TenantConfig[userID]; ok {
		return tenantCfg
	}
	return nil
}

func multiClientRuntimeConfigChannel(manager *runtimeconfig.Manager) func() <-chan kv.MultiRuntimeConfig {
	if manager == nil {
		return nil
	}
	// returns function that can be used in MultiConfig.ConfigProvider
	return func() <-chan kv.MultiRuntimeConfig {
		outCh := make(chan kv.MultiRuntimeConfig, 1)

		// push initial config to the channel
		val := manager.GetConfig()
		if cfg, ok := val.(*runtimeConfigValues); ok && cfg != nil {
			outCh <- cfg.Multi
		}

		ch := manager.CreateListenerChannel(1)
		go func() {
			for val := range ch {
				if cfg, ok := val.(*runtimeConfigValues); ok && cfg != nil {
					outCh <- cfg.Multi
				}
			}
		}()

		return outCh
	}
}
