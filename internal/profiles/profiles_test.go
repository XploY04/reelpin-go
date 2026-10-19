package profiles

import (
	"errors"
	"strings"
	"testing"
)

func ptr(s string) *string { return &s }

func TestNormalizeTrimsAndDropsBlankFields(t *testing.T) {
	got, err := Normalize(Params{Email: ptr("  me@example.com "), FullName: ptr("   "), AvatarURL: nil})
	if err != nil {
		t.Fatal(err)
	}
	if got.Email == nil || *got.Email != "me@example.com" {
		t.Errorf("email = %v", got.Email)
	}
	if got.FullName != nil || got.AvatarURL != nil {
		t.Errorf("blank fields must stay unset: %+v", got)
	}
}

func TestNormalizeRejectsWhatCannotBeStored(t *testing.T) {
	tests := []struct {
		name   string
		params Params
		field  string
	}{
		{"email", Params{Email: ptr("not-an-email")}, "email"},
		{"long email", Params{Email: ptr(strings.Repeat("a", 250) + "@example.com")}, "email"},
		{"long name", Params{FullName: ptr(strings.Repeat("é", 201))}, "full_name"},
		{"ftp avatar", Params{AvatarURL: ptr("ftp://example.com/a.png")}, "avatar_url"},
		{"hostless avatar", Params{AvatarURL: ptr("https:///a.png")}, "avatar_url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Normalize(tt.params)
			var invalid *ValidationError
			if !errors.As(err, &invalid) || invalid.Field != tt.field {
				t.Fatalf("err = %v, want a validation error for %s", err, tt.field)
			}
		})
	}
}
