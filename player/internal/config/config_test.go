package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyMusikEnvVars(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{"no legacy vars", []string{"HOME=/root", "MUSIC_HIVE_PASSWORD=y", "MUSIC_HIVE_FFMPEG=ffmpeg"}, nil},
		{"legacy reported, new format not", []string{"MUSIK_PASSWORD=x", "MUSIC_HIVE_PASSWORD=y"}, []string{"MUSIK_PASSWORD"}},
		{"sorted output", []string{"MUSIK_B=1", "MUSIK_A=2", "MUSIC_HIVE_C=3"}, []string{"MUSIK_A", "MUSIK_B"}},
		{"empty value still reported", []string{"MUSIK_PASSWORD="}, []string{"MUSIK_PASSWORD"}},
		{"bare prefix reported", []string{"MUSIK_=1"}, []string{"MUSIK_"}},
		{"empty environ", nil, nil},
	}
	for _, tt := range tests {
		if got := legacyMusikEnvVars(tt.env); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: legacyMusikEnvVars(%v) = %v, want %v", tt.name, tt.env, got, tt.want)
		}
	}
}

// Real os.Environ path: t.Setenv mutates the process environment.
func TestLegacyMusikEnvVarsFromOsEnviron(t *testing.T) {
	t.Setenv("MUSIK_PASSWORD", "x")
	t.Setenv("MUSIC_HIVE_PASSWORD", "y")
	got := legacyMusikEnvVars(os.Environ())
	found := false
	for _, name := range got {
		if strings.HasPrefix(name, "MUSIC_HIVE_") {
			t.Fatalf("new-format var leaked into legacy list: %s", name)
		}
		if name == "MUSIK_PASSWORD" {
			found = true
		}
	}
	if !found {
		t.Errorf("MUSIK_PASSWORD missing from %v", got)
	}
}

// TestValidateCORSWildcard: wildcard в CORS_ORIGINS запрещён вместе с
// always-on Access-Control-Allow-Credentials (issue #46) — fail-fast.
func TestValidateCORSWildcard(t *testing.T) {
	for _, ok := range []struct {
		origins []string
	}{
		{nil},
		{[]string{"https://hive.example"}},
		{[]string{"https://hive.example", "http://localhost:5173"}},
	} {
		if err := (Config{CORSOrigins: ok.origins}).Validate(); err != nil {
			t.Errorf("Validate(%v) = %v, want nil", ok.origins, err)
		}
	}
	err := (Config{CORSOrigins: []string{"https://hive.example", "*"}}).Validate()
	if err == nil {
		t.Fatal("wildcard origin must fail Validate, got nil")
	}
	if !strings.Contains(err.Error(), "MUSIC_HIVE_CORS_ORIGINS") {
		t.Fatalf("error must name the env var, got: %v", err)
	}
}
