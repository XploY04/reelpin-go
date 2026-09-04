package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	for _, key := range []string{"ENVIRONMENT", "PORT", "DATABASE_URL", "APP_VERSION"} {
		t.Setenv(key, "")
	}

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
			env:  map[string]string{"ENVIRONMENT": "production", "DATABASE_URL": "postgres://user:pass@db:5432/reelpin"},
			check: func(t *testing.T, c Config) {
				if c.DatabaseURL != "postgres://user:pass@db:5432/reelpin" {
					t.Errorf("DatabaseURL = %q", c.DatabaseURL)
				}
			},
		},
		{
			name:    "invalid environment",
			env:     map[string]string{"ENVIRONMENT": "staging"},
			wantErr: true,
		},
		{
			name:    "non numeric port",
			env:     map[string]string{"PORT": "eight-thousand"},
			wantErr: true,
		},
		{
			name:    "port too low",
			env:     map[string]string{"PORT": "0"},
			wantErr: true,
		},
		{
			name:    "port too high",
			env:     map[string]string{"PORT": "70000"},
			wantErr: true,
		},
		{
			name:    "production without database url",
			env:     map[string]string{"ENVIRONMENT": "production"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{"ENVIRONMENT", "PORT", "DATABASE_URL", "APP_VERSION"} {
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
