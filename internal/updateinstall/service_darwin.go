package updateinstall

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

func defaultServiceSpec() (ServiceSpec, error) {
	dir, err := serviceDirectory("launchd")
	if err != nil {
		return ServiceSpec{}, err
	}
	name := "dev.shaulavo.mesh"
	for _, prefix := range []string{"gui", "user"} {
		domain := fmt.Sprintf("%s/%d", prefix, os.Getuid())
		if _, err := runCommand(context.Background(), "launchctl", "print", domain+"/"+name); err == nil {
			return ServiceSpec{Kind: "launchd", Name: name, Domain: domain, ConfigPath: filepath.Join(dir, name+".plist")}, nil
		}
	}
	return ServiceSpec{}, fmt.Errorf("cannot locate existing Mesh launchd service; configure its domain and plist explicitly")
}
