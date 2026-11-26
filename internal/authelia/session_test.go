package authelia

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

// A sign-in lasts a week, and an hour of activity ends it sooner: a short absolute
// lifetime is a sign-in a person repeats all day, while an idle browser is signed
// out at the same hour it always was.
func TestASessionLastsAWeekAndEndsAfterAnIdleHour(t *testing.T) {
	raw, err := RenderConfiguration(testConfiguration())
	if err != nil {
		t.Fatalf("RenderConfiguration: %v", err)
	}
	var doc struct {
		Session struct {
			Cookies []struct {
				Expiration string `yaml:"expiration"`
				Inactivity string `yaml:"inactivity"`
			} `yaml:"cookies"`
		} `yaml:"session"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the rendered configuration does not parse: %v\n%s", err, raw)
	}
	if len(doc.Session.Cookies) == 0 {
		t.Fatalf("the configuration has no session cookie:\n%s", raw)
	}
	for _, c := range doc.Session.Cookies {
		if c.Expiration != "1 week" {
			t.Errorf("session expiration = %q, want 1 week", c.Expiration)
		}
		if c.Inactivity != "1 hour" {
			t.Errorf("session inactivity = %q, want 1 hour", c.Inactivity)
		}
	}
}
