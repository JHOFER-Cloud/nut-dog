package control

import (
	"encoding/json"
	"os"
	"testing"
)

func srcName(s SourceState) string {
	return map[SourceState]string{SourceHealthy: "healthy", SourceHold: "hold", SourceUnknown: "unknown", SourceShed: "shed"}[s]
}

func desName(d Desired) string {
	return map[Desired]string{DesiredHold: "hold", DesiredOff: "off", DesiredOn: "on"}[d]
}

// TestGenerateNutDogTables dumps the full precedence and reconcile tables as JSON, for the
// decision reference page. Skipped unless OUT names a file. See energy-watchdog's
// DEVELOPMENT.md, "Regenerating the decision reference".
func TestGenerateNutDogTables(t *testing.T) {
	out := os.Getenv("OUT")
	if out == "" {
		t.Skip("set OUT=<path> to regenerate the decision tables")
	}
	srcs := []SourceState{SourceHealthy, SourceHold, SourceUnknown, SourceShed}
	reqs := []Request{NoRequest, RequestHold, RequestOff, RequestOn}

	type dRow struct{ A, B, Ext, Out string }
	var d []dRow
	for i, a := range srcs {
		for j, b := range srcs {
			if j < i {
				continue
			}
			for _, e := range reqs {
				d = append(d, dRow{srcName(a), srcName(b), e.String(), desName(DesiredForLoad([]SourceState{a, b}, e))})
			}
		}
	}

	type rRow struct {
		Desired, Actual, Shed string
		Actions               []string
	}
	var r []rRow
	actuals := []struct {
		s ActualState
		n string
	}{{ActualUp, "up"}, {ActualDown, "down"}, {ActualUnknown, "unknown"}}
	sheds := []struct {
		s ShedState
		n string
	}{{ShedAsserted, "asserted"}, {ShedReleased, "released"}, {ShedUnknown, "unknown"}}
	for _, dd := range []Desired{DesiredHold, DesiredOff, DesiredOn} {
		for _, a := range actuals {
			for _, s := range sheds {
				var names []string
				for _, act := range ReconcileLoad("p1", NutServer, dd, a.s, s.s) {
					names = append(names, act.Kind.String())
				}
				r = append(r, rRow{desName(dd), a.n, s.n, names})
			}
		}
	}
	b, _ := json.Marshal(map[string]any{"desired": d, "reconcile": r})
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("desired rows: %d, reconcile rows: %d", len(d), len(r))
}
