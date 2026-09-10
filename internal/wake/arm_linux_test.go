package wake

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Taking over a device with a narrower [Match] makes Mesh's file the only one
// systemd applies, so anything the previous file configured has to come along.
// Dropping NamePolicy here renames the interface on the next boot.
func TestLinkFileInheritsTheSettingsItTakesOver(t *testing.T) {
	previous := []byte(`# 99-default.link
[Match]
OriginalName=*

[Link]
NamePolicy=keep kernel database onboard slot path
AlternativeNamesPolicy=database onboard slot path
MACAddressPolicy=persistent
WakeOnLan=off
`)
	inherited := parseLinkSettings(previous)
	for _, want := range []string{"NamePolicy=keep kernel database onboard slot path", "MACAddressPolicy=persistent"} {
		if !slicesContain(inherited, want) {
			t.Errorf("inherited settings %q, want %q", inherited, want)
		}
	}
	for _, unwanted := range inherited {
		if strings.HasPrefix(strings.ToLower(unwanted), "wakeonlan") {
			t.Errorf("inherited the previous WakeOnLan setting %q", unwanted)
		}
		if strings.HasPrefix(unwanted, "OriginalName") {
			t.Errorf("inherited the [Match] setting %q", unwanted)
		}
	}
	rendered := string(renderLinkFile("c8:7f:54:56:f7:47", inherited))
	for _, want := range []string{"MACAddress=c8:7f:54:56:f7:47", "WakeOnLan=magic", "NamePolicy=keep kernel database onboard slot path"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered link file = %q, want it to contain %q", rendered, want)
		}
	}
	if strings.Count(strings.ToLower(rendered), "wakeonlan=") != 1 {
		t.Errorf("rendered link file has more than one WakeOnLan setting:\n%s", rendered)
	}
}

// Persisted must mean "this NIC wakes after a reboot", not "some file exists".
func TestLinkFileArmsOnlyTheNICItNames(t *testing.T) {
	linkDir = t.TempDir()
	mac := "c8:7f:54:56:f7:47"
	armed, err := linkFileArms(mac)
	if err != nil || armed {
		t.Fatalf("armed=%t err=%v with no file, want false", armed, err)
	}
	for _, testCase := range []struct {
		name     string
		contents string
		want     bool
	}{
		{"matching", "[Match]\nMACAddress=" + mac + "\n\n[Link]\nWakeOnLan=magic\n", true},
		{"another NIC", "[Match]\nMACAddress=aa:bb:cc:dd:ee:ff\n\n[Link]\nWakeOnLan=magic\n", false},
		{"wake off", "[Match]\nMACAddress=" + mac + "\n\n[Link]\nWakeOnLan=off\n", false},
		{"no wake setting", "[Match]\nMACAddress=" + mac + "\n\n[Link]\nNamePolicy=keep\n", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(linkDir, linkFile), []byte(testCase.contents), 0o644); err != nil { //nolint:gosec // test fixture mirrors the real file mode
				t.Fatal(err)
			}
			armed, err := linkFileArms(mac)
			if err != nil {
				t.Fatal(err)
			}
			if armed != testCase.want {
				t.Fatalf("armed=%t, want %t", armed, testCase.want)
			}
		})
	}
	if err := removeLinkFile(); err != nil {
		t.Fatal(err)
	}
	if err := removeLinkFile(); err != nil {
		t.Fatalf("removing a missing link file must be a no-op: %v", err)
	}
}

// A board that lists the NIC as a disabled wake source ignores the packet no
// matter how the driver is armed, so the state has to be read, not assumed.
func TestFirmwareWakeStateReadsTheNICsOwnPCISlot(t *testing.T) {
	wakeup := []byte(`Device	S-state	  Status   Sysfs node
PEG1	  S4	*enabled   pci:0000:00:01.0
GLAN	  S4	*enabled   pci:0000:00:1f.6
XHCI	  S4	*disabled  pci:0000:00:14.0
`)
	for _, testCase := range []struct{ slot, want string }{
		{"pci:0000:00:1f.6", "enabled"},
		{"pci:0000:00:14.0", "disabled"},
		{"pci:0000:00:99.9", ""},
	} {
		if got := parseACPIWakeup(wakeup, testCase.slot); got != testCase.want {
			t.Errorf("parseACPIWakeup(%q) = %q, want %q", testCase.slot, got, testCase.want)
		}
	}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
