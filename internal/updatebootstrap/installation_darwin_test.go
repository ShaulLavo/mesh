package updatebootstrap

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/updateinstall"
)

const emptyLaunchAgent = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict/>
</plist>
`

// launchAgentsHome returns an invocation whose HOME holds the given launch agents.
func launchAgentsHome(t *testing.T, agents map[string]string) processInvocation {
	t.Helper()
	home := t.TempDir()
	directory := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range agents {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return processInvocation{Environment: map[string]string{"HOME": home}}
}

// labelledLaunchAgent is valid but never loaded, so launchctl finds no owner.
func labelledLaunchAgent(t *testing.T) string {
	t.Helper()
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Label</key><string>mesh-inspection-fixture-` + hex.EncodeToString(suffix) + `</string></dict></plist>
`
}

func TestInspectDaemonServiceSkipsLaunchAgentsWithoutLabel(t *testing.T) {
	invocation := launchAgentsHome(t, map[string]string{
		"com.google.keystone.agent.plist":      emptyLaunchAgent,
		"com.google.keystone.xpcservice.plist": emptyLaunchAgent,
		"test.inspection.fixture.plist":        labelledLaunchAgent(t),
	})
	_, err := inspectDaemonService(t.Context(), os.Getpid(), &updateinstall.Settings{}, invocation)
	if err == nil {
		t.Fatal("no loaded agent owns this process, yet inspection matched one")
	}
	message := err.Error()
	if !strings.HasPrefix(message, "no same-account launch agent owns the connected daemon PID") {
		t.Fatalf("an agent without a Label stopped the inspection: %v", err)
	}
	if !strings.Contains(message, "skipped com.google.keystone.agent.plist") || !strings.Contains(message, "skipped com.google.keystone.xpcservice.plist") {
		t.Fatalf("skipped agents are not named in the failure: %v", err)
	}
	if strings.Contains(message, "test.inspection.fixture.plist") {
		t.Fatalf("a readable agent was reported as skipped: %v", err)
	}
}

func TestInspectDaemonServiceFailsOnMeshAgentWithoutLabel(t *testing.T) {
	invocation := launchAgentsHome(t, map[string]string{
		"com.google.keystone.agent.plist": emptyLaunchAgent,
		"dev.shaulavo.mesh.plist":         emptyLaunchAgent,
	})
	_, err := inspectDaemonService(t.Context(), os.Getpid(), &updateinstall.Settings{}, invocation)
	if err == nil || !strings.HasPrefix(err.Error(), "inspect daemon service using plutil") {
		t.Fatalf("Mesh's own unreadable launch agent was skipped: %v", err)
	}
}
