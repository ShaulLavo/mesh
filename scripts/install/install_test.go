package install

import (
	"strings"
	"testing"
)

func TestRenderServicePreservesDetachedWorkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		goos string
		want string
	}{
		{goos: "linux", want: "KillMode=process"},
		{goos: "darwin", want: "<key>AbandonProcessGroup</key>\n\t<true/>"},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			service, err := RenderService(tt.goos, ServiceOptions{
				DaemonPort:    7337,
				SSHPort:       2222,
				WebSocketPath: "/mesh",
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(service, "@MESH_") {
				t.Fatalf("service contains unresolved token:\n%s", service)
			}
			if !strings.Contains(service, tt.want) {
				t.Fatalf("service does not contain %q:\n%s", tt.want, service)
			}
		})
	}
}

func TestRenderServiceRejectsInjectedLine(t *testing.T) {
	t.Parallel()

	_, err := RenderService("linux", ServiceOptions{
		DaemonPort:    7337,
		SSHPort:       2222,
		WebSocketPath: "/mesh\nExecStart=/bin/false",
	})
	if err == nil {
		t.Fatal("RenderService() accepted a newline")
	}
}

func TestLaunchdNativePathsDoNotContainShellVariables(t *testing.T) {
	t.Parallel()

	service, err := RenderService("darwin", ServiceOptions{DaemonPort: 7337, SSHPort: 2222, WebSocketPath: "/mesh"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if strings.Contains(service, "<key>"+key+"</key>") {
			t.Fatalf("launchd service uses native %s, which cannot expand ${HOME}:\n%s", key, service)
		}
	}
}

func TestLinuxInstallerVerifiesFullLingeringProperty(t *testing.T) {
	t.Parallel()

	script, ok := Script("linux")
	if !ok {
		t.Fatal("Linux installer is missing")
	}
	if !strings.Contains(script, `loginctl show-user "$remote_user" --property=Linger`) || !strings.Contains(script, `[ "$linger" = Linger=yes ]`) || strings.Contains(script, "--property=Linger --value") {
		t.Fatalf("Linux installer does not verify Linger=yes:\n%s", script)
	}
}
