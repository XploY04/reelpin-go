package notify

import "testing"

func TestNotificationFactoriesMatchTheAppRoutingContract(t *testing.T) {
	for _, test := range []struct {
		name         string
		notification Notification
		typeName     string
		target       string
	}{
		{"reel", ReelReady("user", "reel", "job", "Saved item", "x", "post"), "reel_ready", "reel_detail"},
		{"collection", CollectionUpdated("user", "collection", "A member", 1), "collection_update", "collection"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := test.notification.Data
			if data["schema_version"] != "1" || data["type"] != test.typeName || data["target"] != test.target {
				t.Fatalf("app routing payload=%v", data)
			}
		})
	}
}
