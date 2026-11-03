package config

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Environment override prefixes. SPACEMAN_AGENT_ variables belong to the node agent and are ignored by central.
const (
	EnvPrefix      = "SPACEMAN_"
	AgentEnvPrefix = "SPACEMAN_AGENT_"
)

// Environment variables with a prefix that are not configuration overrides.
var (
	// The remote CLI (internal/cli) and `spaceman-agent enroll` read their own SPACEMAN_ variables; an operator's shell
	// may export them on the central host, so central must not refuse to start (or run `spaceman backup`) because of them.
	nonConfigEnv = map[string]bool{
		"SPACEMAN_KEK":                 true, // base64 KEK when kek.source = env
		"SPACEMAN_CONFIG":              true, // config file path
		"SPACEMAN_REAL":                true, // opt-in real-dependency tests
		"SPACEMAN_URL":                 true, // reserved (earlier CLI endpoint name)
		"SPACEMAN_SERVER":              true, // CLI: central URL
		"SPACEMAN_TOKEN":               true, // CLI: bearer token
		"SPACEMAN_TOKEN_FILE":          true, // CLI: bearer token file
		"SPACEMAN_OUTPUT":              true, // CLI: output format
		"SPACEMAN_SPACE":               true, // CLI: default space
		"SPACEMAN_ALLOW_INSECURE_HTTP": true, // CLI: plain http to loopback
		"SPACEMAN_ALLOW_NETWORK_FS":    true, // development mode only: allow data_dir on a network filesystem
		"SPACEMAN_ENROLL_TOKEN":        true, // spaceman-agent enroll
		"SPACEMAN_ENDPOINT":            true, // Terraform provider: central URL
	}
	nonConfigAgentEnv = map[string]bool{
		"SPACEMAN_AGENT_CONFIG": true,
	}
)

// EnvKeys lists every supported central override variable, sorted. Used for documentation and tests.
func EnvKeys() []string { return envKeys(Default(), EnvPrefix) }

// AgentEnvKeys lists every supported node agent override variable, sorted.
func AgentEnvKeys() []string { return envKeys(DefaultAgent(), AgentEnvPrefix) }

func envKeys(cfg any, prefix string) []string {
	var keys []string
	walk(reflect.ValueOf(cfg).Elem(), nil, func(path []string, _ reflect.Value) {
		keys = append(keys, envName(prefix, path))
	})
	sort.Strings(keys)
	return keys
}

func envName(prefix string, path []string) string {
	return prefix + strings.ToUpper(strings.Join(path, "_"))
}

// walk visits every leaf field (non-struct) with its yaml path.
func walk(v reflect.Value, path []string, fn func([]string, reflect.Value)) {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		p := append(append([]string(nil), path...), tag)
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct && fv.Type() != reflect.TypeFor[Duration]() {
			walk(fv, p, fn)
			continue
		}
		fn(p, fv)
	}
}

func applyEnv(cfg any, environ []string, prefix string, skip map[string]bool) error {
	env := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, prefix) {
			continue
		}
		if prefix == EnvPrefix && strings.HasPrefix(k, AgentEnvPrefix) {
			continue // the node agent's variables, possibly set on the same host
		}
		env[k] = v
	}
	known := map[string]bool{}
	var firstErr error
	walk(reflect.ValueOf(cfg).Elem(), nil, func(path []string, fv reflect.Value) {
		name := envName(prefix, path)
		known[name] = true
		raw, ok := env[name]
		if !ok || firstErr != nil {
			return
		}
		if err := setFromString(fv, raw); err != nil {
			firstErr = fmt.Errorf("environment %s: %w", name, err)
		}
	})
	if firstErr != nil {
		return firstErr
	}
	var unknown []string
	for k := range env {
		if !known[k] && !skip[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unknown environment override(s): %s", strings.Join(unknown, ", "))
	}
	return nil
}

func setFromString(fv reflect.Value, raw string) error {
	switch {
	case fv.Type() == reflect.TypeFor[Duration]():
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		fv.SetInt(int64(d))
		return nil
	case fv.Kind() == reflect.String:
		fv.SetString(raw)
		return nil
	case fv.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		fv.SetBool(b)
		return nil
	case fv.Kind() == reflect.Int || fv.Kind() == reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		fv.SetInt(n)
		return nil
	case fv.Kind() == reflect.Slice:
		parts := splitList(raw)
		s := reflect.MakeSlice(fv.Type(), len(parts), len(parts))
		for i, p := range parts {
			if err := setFromString(s.Index(i), p); err != nil {
				return err
			}
		}
		fv.Set(s)
		return nil
	}
	return fmt.Errorf("unsupported field type %s", fv.Type())
}

func splitList(raw string) []string {
	var out []string
	for p := range strings.SplitSeq(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
