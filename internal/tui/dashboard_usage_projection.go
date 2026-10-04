package tui

import (
	"strings"
	"unicode"

	"github.com/shaul/mesh/internal/usagefeed"
)

const dashboardUsageCapacity = 12

type dashboardUsage struct {
	accounts []dashboardUsageAccount
	total    int
}

type dashboardUsageAccount struct {
	usagefeed.Account
	first        bool
	extraWindows int
}

type dashboardUsageGroup struct {
	accounts []usagefeed.Account
}

// Group and bound once on publication; clock frames never visit hidden observations.
func projectDashboardUsage(snapshot *usagefeed.Snapshot) dashboardUsage {
	result := dashboardUsage{total: len(snapshot.Accounts)}
	groups := []dashboardUsageGroup{}
	positions := make(map[string]int)
	for _, account := range snapshot.Accounts {
		position, found := positions[account.Provider]
		if !found {
			position = len(groups)
			positions[account.Provider] = position
			groups = append(groups, dashboardUsageGroup{})
		}
		groups[position].accounts = append(groups[position].accounts, account)
	}
	for _, group := range groups {
		for index, account := range group.accounts {
			if len(result.accounts) == dashboardUsageCapacity {
				return result
			}
			result.accounts = append(result.accounts, projectUsageAccount(account, index == 0))
		}
	}
	return result
}

func projectUsageAccount(account usagefeed.Account, first bool) dashboardUsageAccount {
	result := dashboardUsageAccount{Account: account, first: first}
	result.Windows = nil
	for _, window := range account.Windows {
		if !usageWindowHasReading(window) {
			continue
		}
		if len(result.Windows) == 2 {
			result.extraWindows++
			continue
		}
		result.Windows = append(result.Windows, window)
	}
	return result
}

func dashboardUsageTitle(value string) string {
	runes := []rune(value)
	if len(runes) == 0 {
		return value
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func usagePlan(value string) string {
	if len(value) == 0 {
		return value
	}
	return dashboardUsageTitle(strings.ToLower(value))
}
