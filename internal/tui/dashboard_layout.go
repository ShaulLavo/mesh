package tui

// Only geometry lives across frames; ages, colors and metric values are painted anew.
type dashboardLayoutKey struct {
	width, height, hosts, overhead, summaries int
}

type dashboardLayout struct {
	key                     dashboardLayoutKey
	graph, remainder, cards int
}

func (m dashboardModel) layoutForSummaries(summaryHeight int) dashboardLayout {
	key := dashboardLayoutKey{m.width, m.height, len(m.hosts), m.cardOverhead(), summaryHeight}
	if key == m.layout.key {
		return m.layout
	}
	if m.renderWork != nil {
		m.renderWork.layouts++
	}
	layout := dashboardLayout{key: key, graph: 4}
	rows := (len(m.hosts) + 1) / 2
	if m.width >= 140 && m.height >= 40 {
		available := m.height - 5 - summaryHeight - key.overhead
		layout.graph = max(1, available/max(1, rows))
		layout.remainder = max(0, available) % max(1, rows)
	}
	layout.cards = key.overhead + rows*layout.graph + layout.remainder
	return layout
}

func (m dashboardModel) currentLayout() dashboardLayout {
	if m.layoutPrepared {
		return m.layout
	}
	return m.layoutForSummaries(m.summariesHeight(m.height))
}

func (m dashboardModel) summariesHeight(budget int) int {
	if m.width < 140 || m.height < 40 {
		return 0
	}
	attentionBudget := min(5, max(0, budget-4))
	if m.usageEnabled {
		budget = min(budget, 17)
		attentionBudget = min(7, max(0, budget-10))
	}
	attention := m.attentionHeight(m.summaryAttentionWidth(), attentionBudget)
	left := m.sessionSummaryHeight(budget)
	right := m.serviceSummaryHeight(budget-attention) + attention
	if !m.usageEnabled {
		return max(left, right)
	}
	_, usage := m.usageVisible(budget, false)
	return max(left, right, usage)
}

func (m dashboardModel) sessionSummaryHeight(budget int) int {
	if budget < 4 {
		return 0
	}
	total, live := 0, 0
	for _, host := range m.hosts {
		total += host.Sessions.Total
		if !dashboardSessionsCached(host, m.now) {
			live += len(host.Sessions.Rows)
		}
	}
	if total == 0 {
		return 0
	}
	height := 4 + min(live, budget-4)
	for _, host := range m.hosts {
		remaining := budget - height
		if !dashboardSessionsCached(host, m.now) || host.Sessions.Total == 0 || remaining < 2 {
			continue
		}
		height += 1 + min(len(host.Sessions.Rows), remaining-1)
	}
	return height
}

func (m dashboardModel) serviceSummaryHeight(budget int) int {
	if budget < 4 {
		return 0
	}
	total, rows := 0, 0
	for _, host := range m.hosts {
		total += host.Services.Total
		rows += len(host.Services.Rows)
	}
	if total == 0 {
		return 0
	}
	return 3 + min(rows, budget-3)
}

func (m dashboardModel) summaryAttentionWidth() int {
	if m.usageEnabled {
		remaining := m.width - 56
		return remaining - remaining*57/104
	}
	return m.width - (m.width-1)*3/5 - 1
}

func (m dashboardModel) attentionHeight(width, budget int) int {
	data := m.attentionForWidth(width)
	body := 0
	for _, group := range data.groups {
		available := max(0, budget-2-body)
		height := m.attentionGroupHeight(len(group), available)
		if height == 0 || height > available {
			continue
		}
		body += height
	}
	if body == 0 {
		return 0
	}
	return body + 2
}

func (m dashboardModel) attentionGroupHeight(height, budget int) int {
	if !m.usageEnabled || height <= budget {
		return height
	}
	if budget < min(2, height) {
		return 0
	}
	return budget
}
