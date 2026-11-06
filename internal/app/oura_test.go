package app

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSubscriptionExpiryAcceptsProductionAndRFC3339Formats(t *testing.T) {
	expected := time.Date(2027, 2, 4, 1, 36, 47, 674307000, time.UTC)
	for _, value := range []string{
		"2027-02-04T01:36:47.674307",
		"2027-02-04T01:36:47.674307Z",
		"2027-02-04T03:36:47.674307+02:00",
	} {
		t.Run(value, func(t *testing.T) {
			var sub Subscription
			if err := json.Unmarshal([]byte(`{"expiration_time":"`+value+`"}`), &sub); err != nil {
				t.Fatal(err)
			}
			if !sub.Expiry.Equal(expected) {
				t.Fatalf("unexpected parsed expiry: %v", sub.Expiry.Time)
			}
		})
	}
	for _, value := range []string{`"invalid"`, `"2027-02-04"`, `null`, `123`} {
		var sub Subscription
		if err := json.Unmarshal([]byte(`{"expiration_time":`+value+`}`), &sub); err == nil {
			t.Fatalf("invalid expiry accepted: %s", value)
		}
	}
}
