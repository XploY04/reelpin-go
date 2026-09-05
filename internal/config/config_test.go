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
	if cfg.RedisKeyPrefix != "reelpin" {
		t.Errorf("RedisKeyPrefix = %q, want reelpin", cfg.RedisKeyPrefix)
	}
	if len(cfg.TrustedProxyCIDRs) != 0 {
		t.Errorf("TrustedProxyCIDRs = %v, want none by default", cfg.TrustedProxyCIDRs)
	}
}

var configKeys = []string{
	"ENVIRONMENT", "PORT", "DATABASE_URL", "APP_VERSION",
	"SUPABASE_URL", "SUPABASE_JWT_AUDIENCE",
	"REDIS_URL", "REDIS_KEY_PREFIX", "TRUSTED_PROXY_CIDRS",
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
				"ENVIRONMENT":  "production",
				"DATABASE_URL": "postgres://user:pass@db:5432/reelpin",
				"SUPABASE_URL": "https://project.supabase.co",
				"REDIS_URL":    "redis://cache:6379/0",
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
			name: "production without redis",
			env: map[string]string{
				"ENVIRONMENT":  "production",
				"DATABASE_URL": "postgres://user:pass@db:5432/reelpin",
				"SUPABASE_URL": "https://project.supabase.co",
			},
			wantErr: true,
		},
		{
			name: "trusted proxies are parsed",
			env: map[string]string{
				"SUPABASE_URL":        "https://project.supabase.co",
				"TRUSTED_PROXY_CIDRS": "10.0.0.0/8, 2001:db8::/32",
			},
			check: func(t *testing.T, c Config) {
				if len(c.TrustedProxyCIDRs) != 2 {
					t.Fatalf("proxies = %v, want two prefixes", c.TrustedProxyCIDRs)
				}
				if c.RateLimitPrefix() != "reelpin:ratelimit" || c.CachePrefix() != "reelpin:cache" {
					t.Errorf("key prefixes = %q / %q", c.RateLimitPrefix(), c.CachePrefix())
				}
			},
		},
		{
			name:    "malformed trusted proxy",
			env:     map[string]string{"SUPABASE_URL": "https://project.supabase.co", "TRUSTED_PROXY_CIDRS": "10.0.0.1"},
			wantErr: true,
		},
		{
			name:    "production without database url",
			env:     map[string]string{"ENVIRONMENT": "production", "SUPABASE_URL": "https://project.supabase.co"},
			wantErr: true,
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
