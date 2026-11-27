package main

import "testing"

func TestDetectTrials(t *testing.T) {
	cases := []struct {
		name      string
		runs      int
		runsSet   bool
		catchRate float64
		want      int
		wantErr   bool
	}{
		{"default", 60, false, 0, 60, false},
		{"runs set", 135, true, 0, 135, false},
		{"catch rate", 60, false, 0.02, 149, false},
		{"both", 135, true, 0.02, 0, true},
		{"bad rate", 60, false, 1.5, 0, true},
		{"zero runs", 0, true, 0, 0, true},
	}
	for _, c := range cases {
		got, err := detectTrials(c.runs, c.runsSet, c.catchRate)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: got %d, %v", c.name, got, err)
		}
	}
}

func TestServiceLatencies(t *testing.T) {
	def := []int{0, 50, 150}
	if got, err := serviceLatencies(def, false, true); err != nil || len(got) != 3 {
		t.Errorf("with a service: got %v, %v", got, err)
	}
	// the default levels shouldn't stop a suite with no service from running
	if got, err := serviceLatencies(def, false, false); err != nil || got != nil {
		t.Errorf("default, no service: got %v, %v", got, err)
	}
	if _, err := serviceLatencies([]int{150}, true, false); err == nil {
		t.Error("asked for latency with no service: want an error")
	}
	if got, err := serviceLatencies([]int{0}, true, false); err != nil || got != nil {
		t.Errorf("asked for 0 with no service: got %v, %v", got, err)
	}
}
