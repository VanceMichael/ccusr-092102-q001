package mission_test

import (
	"encoding/json"
	"os"
	"testing"
)

func TestMissionFixtureHasIdentity(t *testing.T) {
	raw, err := os.ReadFile("fixtures/mission.json")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if value["satellite_id"] == "" || value["station_id"] == "" {
		t.Fatal("卫星与测控站标识不能为空")
	}
}
