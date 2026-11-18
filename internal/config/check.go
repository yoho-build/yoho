package config

import (
	"fmt"
	"time"

	"github.com/yoho-build/yoho/internal/probe"
)

// Service check timeouts.
const (
	DefaultCheckTimeout = 30 * time.Second
	MaxCheckTimeout     = 10 * time.Minute
)

// Probe is the probe the check runs.
func (c ServiceCheck) Probe() probe.Check {
	return probe.Check{Type: c.Type, Port: c.Port, Path: c.Path, Host: c.Host}
}

// Validate reports a check that cannot run.
func (c ServiceCheck) Validate() error {
	if err := c.Probe().Validate(); err != nil {
		return err
	}
	if c.Timeout == "" {
		return nil
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d <= 0 {
		return fmt.Errorf("check timeout %q is not a positive Go duration such as 30s or 2m", c.Timeout)
	}
	if d > MaxCheckTimeout {
		return fmt.Errorf("check timeout %s is above the maximum of %s", c.Timeout, MaxCheckTimeout)
	}
	return nil
}

// TimeoutDuration is how long a deploy retries the check: Timeout, or
// DefaultCheckTimeout when unset or invalid.
func (c ServiceCheck) TimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(c.Timeout); err == nil && d > 0 {
		return d
	}
	return DefaultCheckTimeout
}

// WithDefaults returns c with the default path, Host header and timeout
// filled, as a Release records it.
func (c ServiceCheck) WithDefaults() ServiceCheck {
	if c.Type != probe.TCP {
		if c.Path == "" {
			c.Path = "/"
		}
		if c.Host == "" {
			c.Host = probe.DefaultHost
		}
	}
	c.Timeout = c.TimeoutDuration().String()
	return c
}

// String names the check for output, e.g. "websocket /sync on port 8080".
func (c ServiceCheck) String() string {
	return fmt.Sprintf("%s on port %d", c.Probe(), c.Port)
}
