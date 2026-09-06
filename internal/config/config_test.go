package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	for _, key := range configKeys {
		t.Setenv(key, "")
	}
	t.Setenv("SUPABASE_URL", "https://project.supabase.co")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Environment != "development" {
		t.Errorf("Environment = %q, want development", cfg.Environment)
	}
	if cfg.Port != 8000 {
		t.Errorf("Port = %d, want 8000", cfg.Port)
	}
	if cfg.DatabaseURL != localDatabaseURL {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, localDatabaseURL)
	}
	if cfg.Version != "dev" {
		t.Errorf("Version = %q, want dev", cfg.Version)
	}
	if cfg.SupabaseJWTAudience != "authenticated" {
		t.Errorf("SupabaseJWTAudience = %q, want authenticated", cfg.SupabaseJWTAudience)
	}
}

var configKeys = []string{
	"ENVIRONMENT", "PORT", "DATABASE_URL", "APP_VERSION",
	"SUPABASE_URL", "SUPABASE_JWT_AUDIENCE",
	"REDIS_URL", "REDIS_KEY_PREFIX", "RATE_LIMIT_SALT",
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(*testing.T, Config)
	}{
		{
			name: "test environment",
			env:  map[string]string{"ENVIRONMENT": "test", "PORT": "9090", "APP_VERSION": "1.2.3"},
			check: func(t *testing.T, c Config) {
				if c.Environment != "test" || c.Port != 9090 || c.Version != "1.2.3" {
					t.Errorf("unexpected config: %+v", c)
				}
			},
		},
		{
			name: "production with database url",
			env: map[string]string{
				"ENVIRONMENT":     "production",
				"DATABASE_URL":    "postgres://user:pass@db:5432/reelpin",
				"SUPABASE_URL":    "https://project.supabase.co",
				"REDIS_URL":       "redis://redis:6379/0",
				"RATE_LIMIT_SALT": "secret",
			},
			check: func(t *testing.T, c Config) {
				if c.DatabaseURL != "postgres://user:pass@db:5432/reelpin" {
					t.Errorf("DatabaseURL = %q", c.DatabaseURL)
				}
			},
		},
		{
			name:    "development without supabase url",
			env:     map[string]string{"ENVIRONMENT": "development"},
			wantErr: true,
		},
		{
			name:    "invalid environment",
			env:     map[string]string{"ENVIRONMENT": "staging"},
			wantErr: true,
		},
		{
			name:    "non numeric port",
			env:     map[string]string{"SUPABASE_URL": "https://project.supabase.co", "PORT": "eight-thousand"},
			wantErr: true,
		},
		{
			name:    "port too low",
			env:     map[string]string{"SUPABASE_URL": "https://project.supabase.co", "PORT": "0"},
			wantErr: true,
		},
		{
			name:    "port too high",
			env:     map[string]string{"SUPABASE_URL": "https://project.supabase.co", "PORT": "70000"},
			wantErr: true,
		},
		{
			name:    "production without database url",
			env:     map[string]string{"ENVIRONMENT": "production", "SUPABASE_URL": "https://project.supabase.co"},
			wantErr: true,
		},
		{
			name: "production needs redis",
			env: map[string]string{
				"ENVIRONMENT":     "production",
				"DATABASE_URL":    "postgres://db:5432/reelpin",
				"SUPABASE_URL":    "https://project.supabase.co",
				"RATE_LIMIT_SALT": "secret",
			},
			wantErr: true,
		},
		{
			name: "production needs a rate limit salt",
			env: map[string]string{
				"ENVIRONMENT":  "production",
				"DATABASE_URL": "postgres://db:5432/reelpin",
				"SUPABASE_URL": "https://project.supabase.co",
				"REDIS_URL":    "redis://redis:6379/0",
			},
			wantErr: true,
		},
		{
			name: "a malformed redis url fails at startup",
			env: map[string]string{
				"SUPABASE_URL": "https://project.supabase.co",
				"REDIS_URL":    "not-a-url",
			},
			wantErr: true,
		},
		{
			name: "production with everything",
			env: map[string]string{
				"ENVIRONMENT":     "production",
				"DATABASE_URL":    "postgres://db:5432/reelpin",
				"SUPABASE_URL":    "https://project.supabase.co",
				"REDIS_URL":       "redis://redis:6379/0",
				"RATE_LIMIT_SALT": "secret",
			},
			check: func(t *testing.T, c Config) {
				if c.RedisURL != "redis://redis:6379/0" || c.RateLimitSalt != "secret" {
					t.Errorf("unexpected config: %+v", c)
				}
				options, err := c.RedisOptions()
				if err != nil {
					t.Fatalf("RedisOptions: %v", err)
				}
				if options.DialTimeout <= 0 || options.ReadTimeout <= 0 || options.WriteTimeout <= 0 {
					t.Errorf("redis timeouts are unset: %+v", options)
				}
			},
		},
		{
			name: "development generates a salt",
			env:  map[string]string{"SUPABASE_URL": "https://project.supabase.co"},
			check: func(t *testing.T, c Config) {
				if c.RateLimitSalt == "" {
					t.Error("no salt was generated outside production")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range configKeys {
				t.Setenv(key, tt.env[key])
			}

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() = %+v, want error", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestTheCostGateIsAllFourVariablesOrNone(t *testing.T) {
	all := map[string]string{
		"COST_GATE_WARN_USD":   "12.00",
		"COST_GATE_STOP_USD":   "20.00",
		"COST_GATE_STOP_ORDER": "media,all",
		"COST_GATE_PRICES":     "gemini:*:call=0.001",
	}

	t.Run("none is off", func(t *testing.T) {
		cfg, err := loadWith(t, nil)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.CostGateConfigured() {
			t.Error("the gate reports configured with nothing set")
		}
	})

	t.Run("all four is on", func(t *testing.T) {
		cfg, err := loadWith(t, all)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.CostGateConfigured() {
			t.Error("the gate reports unconfigured with all four set")
		}
	})

	// Half a decision would run a limit assembled from defaults nobody approved.
	for missing := range all {
		t.Run("without "+missing, func(t *testing.T) {
			partial := map[string]string{}
			for name, value := range all {
				if name != missing {
					partial[name] = value
				}
			}
			if _, err := loadWith(t, partial); err == nil {
				t.Errorf("Load accepted a cost gate with no %s", missing)
			}
		})
	}
}

// loadWith sets the given variables for one test and clears the rest.
func loadWith(t *testing.T, values map[string]string) (Config, error) {
	t.Helper()
	for _, name := range []string{
		"COST_GATE_WARN_USD", "COST_GATE_STOP_USD",
		"COST_GATE_STOP_ORDER", "COST_GATE_PRICES",
	} {
		t.Setenv(name, values[name])
	}
	t.Setenv("SUPABASE_URL", "https://project.supabase.co")
	return Load()
}
