package proto

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestWelcomeControlPlanePorts(t *testing.T) {
	b, err := json.Marshal(Welcome{HostID: "h1", ControlPlanePorts: []int{8090, 9000}})
	if err != nil {
		t.Fatal(err)
	}
	var w Welcome
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.ControlPlanePorts, []int{8090, 9000}) {
		t.Errorf("round trip: %v", w.ControlPlanePorts)
	}
	b, err = json.Marshal(Welcome{HostID: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "controlPlanePorts") {
		t.Errorf("empty list sent: %s", b)
	}
	// An older luxd's Welcome has no field: no ports.
	var old Welcome
	if err := json.Unmarshal([]byte(`{"hostId":"h1","leaseSeconds":30,"live":[]}`), &old); err != nil || old.ControlPlanePorts != nil {
		t.Errorf("older Welcome: %v %v", old.ControlPlanePorts, err)
	}
}
