package cli

import (
	"fmt"
	"strings"
)

const DefaultDashboardTheme = "oled"

// DashboardSettings lives beside the address book in the standard Mesh config.
type DashboardSettings struct {
	Theme        string `json:"theme,omitempty"`
	UsageFeedURL string `json:"usageFeedURL,omitempty"`
}

func DashboardThemeNames() []string {
	return []string{"current", "rose-pine", "rose-pine-moon", DefaultDashboardTheme, "kanagawa", "gruvbox-material"}
}

func ValidateDashboardTheme(name string) error {
	for _, valid := range DashboardThemeNames() {
		if name == valid {
			return nil
		}
	}
	return fmt.Errorf("unknown dashboard theme %q; valid themes: %s", name, strings.Join(DashboardThemeNames(), ", "))
}

func dashboardConfiguredTheme(override string) (string, error) {
	config, err := loadHostConfig()
	if err != nil {
		return "", err
	}
	name := DefaultDashboardTheme
	if config.Dashboard != nil && config.Dashboard.Theme != "" {
		name = config.Dashboard.Theme
	}
	if override != "" {
		name = override
	}
	if err := ValidateDashboardTheme(name); err != nil {
		return "", err
	}
	return name, nil
}
