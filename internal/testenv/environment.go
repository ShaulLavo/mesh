// Package testenv controls the environment of subprocess fixtures.
package testenv

import "os"

// ForProcess keeps provider configuration out of subprocess fixtures.
func ForProcess(home string) []string {
	environment := []string{"HOME=" + home}
	for _, name := range []string{"PATH", "TMPDIR", "TERM", "LANG", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}
