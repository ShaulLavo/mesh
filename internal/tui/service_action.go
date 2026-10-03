package tui

import (
	"fmt"
	"reflect"

	tea "charm.land/bubbletea/v2"
	"github.com/shaul/mesh/internal/cli"
)

type serviceActionResultMsg struct {
	request cli.PickerServiceActionRequest
	before  *cli.ServiceCatalogRow
	result  cli.PickerServiceActionResult
	err     error
}

func serviceActionLabel(action cli.PickerServiceAction) string {
	switch action {
	case cli.PickerStopService:
		return "Stopping"
	case cli.PickerRestartService:
		return "Restarting"
	case cli.PickerPingService:
		return "Pinging"
	case cli.PickerOpenService:
		return "Opening"
	default:
		return "Updating"
	}
}

func (m *model) handleServiceKey(key tea.KeyPressMsg) (bool, tea.Cmd) {
	selected, ok := m.list.SelectedItem().(serviceItem)
	if !ok {
		return false, nil
	}
	actions := map[string]cli.PickerServiceAction{"s": cli.PickerStopService, "r": cli.PickerRestartService, "p": cli.PickerPingService, "o": cli.PickerOpenService, pickerEnterKey: cli.PickerOpenService}
	action, ok := actions[key.String()]
	if !ok {
		return false, nil
	}
	if m.pendingService != nil {
		m.notice = "Service action in progress"
		return true, nil
	}
	if m.serviceAct == nil {
		m.notice = "Service actions unavailable"
		return true, nil
	}
	request := cli.PickerServiceActionRequest{HostID: selected.target.hostID, ServiceName: selected.target.route, Action: action}
	m.pendingService = &request
	m.notice = ""
	m.refreshMainDelegate()
	act, ctx := m.serviceAct, m.ctx
	before := selected.website.row
	return true, func() tea.Msg {
		result, err := act(ctx, request)
		return serviceActionResultMsg{request: request, before: &before, result: result, err: err}
	}
}

func (m model) applyServiceAction(message serviceActionResultMsg) model {
	if m.pendingService == nil || *m.pendingService != message.request {
		return m
	}
	m.pendingService = nil
	target := serviceTarget{message.request.HostID, message.request.ServiceName}
	feedback := m.serviceFeedback[target]
	website := m.serviceWebsite(target)
	if message.err != nil {
		feedback = "Failed: " + message.err.Error()
		if website != nil {
			m.serviceFeedback[target] = feedback
		}
		m.notice = feedback
		m.refreshMainDelegate()
		return m
	}
	m.notice = ""
	row := message.result.Row
	if website == nil {
		m.refreshMainDelegate()
		return m
	}
	// A watch update can arrive while an action's older snapshot is in flight.
	if message.before == nil || reflect.DeepEqual(website.row, *message.before) {
		*website = servedWebsites([]cli.ServiceCatalogRow{row}, false)[0]
	}
	switch message.request.Action {
	case cli.PickerStopService:
		feedback = "Stopped · next connection starts it"
	case cli.PickerRestartService:
		feedback = "Restarted"
	case cli.PickerPingService:
		feedback = fmt.Sprintf("ping %s %d ms", row.Health(), message.result.Latency.Milliseconds())
	case cli.PickerOpenService:
		feedback = "Opened"
	}
	m.serviceFeedback[target] = feedback
	if m.screen == hostScreen {
		_ = m.resetMainItems(mainItemKey(m.list.SelectedItem()), m.list.Index())
	}
	return m
}

func (m model) serviceWebsite(target serviceTarget) *servedWebsite {
	for hostIndex := range m.hosts {
		if m.hosts[hostIndex].id != target.hostID {
			continue
		}
		for index := range m.hosts[hostIndex].served {
			if m.hosts[hostIndex].served[index].route == target.route {
				return &m.hosts[hostIndex].served[index]
			}
		}
	}
	return nil
}
