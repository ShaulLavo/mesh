package cli

import (
	"fmt"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

func TestDashboardLiveSessionsExcludeEndedBeforeBounding(t *testing.T) {
	var rows []protocol.SessionInfo
	for index := range 680 {
		state := "exited"
		if index%2 == 0 {
			state = "interrupted"
		}
		rows = append(rows, protocol.SessionInfo{ID: fmt.Sprint("ended-", index), State: state})
	}
	for index := range 9 {
		state := "running"
		if index%2 == 0 {
			state = "detached"
		}
		rows = append(rows, protocol.SessionInfo{ID: fmt.Sprint("live-", index), State: state, Command: []string{"shell"}})
	}
	rows = append(rows, protocol.SessionInfo{ID: "unknown", State: "unknown"})
	catalog := projectDashboardSessions(rows, ObservedSection{})
	if catalog.Total != 9 || len(catalog.Rows) != dashboardSessionLimit {
		t.Fatalf("ended history displaced live sessions or inflated totals: total=%d rows=%+v", catalog.Total, catalog.Rows)
	}
	for index, row := range catalog.Rows {
		if row.ID != fmt.Sprint("live-", index) {
			t.Fatalf("active row order changed: %+v", catalog.Rows)
		}
	}
}
