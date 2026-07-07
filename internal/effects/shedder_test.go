package effects

import (
	"errors"
	"testing"

	"github.com/JHOFER-Cloud/nut-dog/internal/control"
)

func TestNUTShedderSignals(t *testing.T) {
	type call struct{ ups, name, value string }
	var got []call
	s := NUTShedder{
		ShedUps: map[string]string{"p1": "shed-p1"},
		Set: func(ups, name, value string) error {
			got = append(got, call{ups, name, value})
			return nil
		},
	}

	if err := s.Assert("p1"); err != nil {
		t.Fatalf("Assert: %v", err)
	}
	if err := s.Release("p1"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	want := []call{
		{"shed-p1", "ups.status", "OB LB FSD"},
		{"shed-p1", "ups.status", "OL"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// ParseShedStatus must invert the exact strings Assert/Release write, so the
// controller reads back the state it drove.
func TestParseShedStatus(t *testing.T) {
	tests := []struct {
		status string
		want   control.ShedState
	}{
		{statusShed, control.ShedAsserted}, // "OB LB FSD"
		{statusOK, control.ShedReleased},   // "OL"
		{"OL CHRG", control.ShedReleased},
		{"", control.ShedUnknown},
		{"OB DISCHRG", control.ShedUnknown}, // on battery but not forced-shed
	}
	for _, tt := range tests {
		if got := ParseShedStatus(tt.status); got != tt.want {
			t.Errorf("ParseShedStatus(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestNUTShedderUnknownLoad(t *testing.T) {
	s := NUTShedder{ShedUps: map[string]string{}, Set: func(_, _, _ string) error { return nil }}
	if err := s.Assert("nope"); err == nil {
		t.Error("expected error for load without a shed ups")
	}
}

func TestNUTShedderPropagatesSetError(t *testing.T) {
	s := NUTShedder{
		ShedUps: map[string]string{"p1": "shed-p1"},
		Set:     func(_, _, _ string) error { return errors.New("upsd down") },
	}
	if err := s.Assert("p1"); err == nil {
		t.Error("expected set error to propagate")
	}
}
