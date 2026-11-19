package media

import (
	"encoding/json"
	"errors"
	"testing"

	"qushuiyin/service/internal/config"
)

func TestMediaVolumeHeadroom(t *testing.T) {
	r := Relay{Settings: config.Values{Data: map[string]json.RawMessage{"disk_high_watermark_percent": json.RawMessage("80")}}}
	r.volume = func(string) (uint64, uint64, error) { return 25, 100, nil }
	if got, err := r.volumeHeadroom(); got != 5 || err != nil {
		t.Fatalf("headroom = %d, %v", got, err)
	}
	r.volume = func(string) (uint64, uint64, error) { return 10, 100, nil }
	if _, err := r.reserve(t.Context(), 1); !errors.Is(err, ErrStorageCapacity) {
		t.Fatalf("expected capacity rejection, got %v", err)
	}
	r.Settings.Data["disk_high_watermark_percent"] = json.RawMessage("0")
	if _, err := r.reserve(t.Context(), 1); !errors.Is(err, ErrStorageCapacity) {
		t.Fatalf("a zero watermark must reject new files, got %v", err)
	}
}
