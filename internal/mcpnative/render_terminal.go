package mcpnative

import (
	"fmt"
	"strings"

	"github.com/shaul/mesh/internal/agentapi"
)

func renderTerminalAndServiceData(b *strings.Builder, data any) {
	switch v := data.(type) {
	case TerminalScreenData:
		renderTerminalScreen(b, v)
	case TerminalSizeData:
		fmt.Fprintf(b, "job %s terminal size %dx%d", v.Job, v.Size.Cols, v.Size.Rows)
	case agentapi.ServiceChange:
		renderServiceRow(b, v.Service)
		fmt.Fprintf(b, "\nstate: %s", v.State)
		for _, warning := range v.Warnings {
			fmt.Fprintf(b, "\nwarning: %s", escapeControlText(warning))
		}
	case agentapi.ServiceRemoval:
		fmt.Fprintf(b, "removed route %s on %s", v.Name, v.HostID)
	case agentapi.AppsData:
		renderApps(b, v)
	case agentapi.SessionHibernation:
		fmt.Fprintf(b, "hibernated session %s on %s; the next mesh attach resumes it", v.SessionID, v.HostID)
	case agentapi.SessionRecovery:
		fmt.Fprintf(b, "session %s on %s replaces %s (%s); attach with mesh attach %s", v.SessionID, v.HostID, v.RecoveredFrom, v.Action, v.SessionID)
	}
}

func renderTerminalScreen(b *strings.Builder, v TerminalScreenData) {
	state := "no one attached"
	if v.Screen.Attached {
		state = "a person is attached; input and resize wait until they detach"
	}
	fmt.Fprintf(b, "job %s screen %dx%d (%s)\n", v.Job, v.Screen.Cols, v.Screen.Rows, state)
	for _, line := range v.Screen.Lines {
		b.WriteString(escapeControlText(line))
		b.WriteByte('\n')
	}
}

func renderApps(b *strings.Builder, v agentapi.AppsData) {
	fmt.Fprintf(b, "%d apps on %s", len(v.Apps), v.HostID)
	for _, app := range v.Apps {
		fmt.Fprintf(b, "\n%s %s expires %s %s", app.ID, app.Status, app.ExpiresAt.Format("2006-01-02 15:04 MST"), app.URL)
	}
	if v.Truncated {
		fmt.Fprintf(b, "\n(first %d apps shown)", agentapi.MaxAppRows)
	}
}
