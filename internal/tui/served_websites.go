package tui

import (
	"fmt"

	"github.com/shaul/mesh/internal/cli"
)

const maximumVisibleServedWebsites = 3

func servedWebsites(rows []cli.ServiceCatalogRow, catalogStale bool) []servedWebsite {
	websites := make([]servedWebsite, len(rows))
	for index, row := range rows {
		websites[index] = servedWebsite{
			url:    row.URL(),
			health: row.Health(),
			stale:  catalogStale || row.Stale || !row.Live,
		}
	}
	return websites
}

func anyServedWebsiteStale(websites []servedWebsite) bool {
	for _, website := range websites {
		if website.stale {
			return true
		}
	}
	return false
}

func servedSummary(current host) string {
	switch {
	case !current.servedKnown:
		return "served loading"
	case current.servedStale && len(current.served) == 0:
		return "served unavailable"
	case current.servedStale:
		return fmt.Sprintf("%d served cached", len(current.served))
	default:
		return fmt.Sprintf("%d served", len(current.served))
	}
}

func (m model) visibleServedWebsiteRows(bodyRows int) []string {
	if len(m.currentHost().served) == 0 {
		return nil
	}
	reservedRows := 1
	if _, _, selected := m.currentSession(); selected {
		reservedRows = detailPanelBaseRows + 2
	}
	rowBudget := bodyRows - reservedRows
	if rowBudget < 3 {
		return nil
	}
	return m.servedWebsiteRows(rowBudget)
}

func (m model) servedWebsiteRows(rowBudget int) []string {
	current := m.currentHost()
	if len(current.served) == 0 || rowBudget < 3 {
		return nil
	}
	websiteRows := min(maximumVisibleServedWebsites, len(current.served), rowBudget-2)
	showMoreRow := len(current.served) > maximumVisibleServedWebsites && websiteRows == maximumVisibleServedWebsites
	if showMoreRow {
		websiteRows--
	}
	heading := "served websites"
	if len(current.served) > websiteRows && !showMoreRow {
		heading += fmt.Sprintf("  ·  %d total", len(current.served))
	}
	rows := make([]string, 0, websiteRows+3)
	rows = append(rows, "", m.styles.muted.Render(heading))
	for _, website := range current.served[:websiteRows] {
		status := website.health
		style := m.styles.muted
		switch {
		case current.servedStale || website.stale:
			status = "offline/stale"
			style = m.styles.warning
		case status != "healthy":
			style = m.styles.warning
		}
		rows = append(rows, "  "+style.Render(status)+"  "+safeText(website.url))
	}
	if hidden := len(current.served) - websiteRows; showMoreRow && hidden > 0 {
		rows = append(rows, m.styles.muted.Render(fmt.Sprintf("  … %d more  ·  mesh serve ls", hidden)))
	}
	return rows
}
