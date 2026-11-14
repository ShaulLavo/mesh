package updatebootstrap

import "testing"

func TestCurrentDaemonServiceRejectsOrdinaryCommand(t *testing.T) {
	if err := InspectCurrentDaemonService(t.Context(), t.TempDir()); err == nil {
		t.Fatal("a command without a native daemon service became a managed installation")
	}
}
