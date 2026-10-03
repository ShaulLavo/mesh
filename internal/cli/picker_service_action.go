package cli

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

const (
	pickerLinuxOS  = "linux"
	pickerDarwinOS = "darwin"
)

type PickerServiceAction uint8

const (
	PickerStopService PickerServiceAction = iota + 1
	PickerRestartService
	PickerPingService
	PickerOpenService
)

type PickerServiceActionRequest struct {
	HostID      string
	ServiceName string
	Action      PickerServiceAction
}

type PickerServiceActionResult struct {
	Row     ServiceCatalogRow
	Latency time.Duration
}

type PickerServiceActionFunc func(context.Context, PickerServiceActionRequest) (PickerServiceActionResult, error)

func pickerServiceAction(ctx context.Context, hosts []HostRecord, dial HostDialer, open func(context.Context, string) error, request PickerServiceActionRequest) (PickerServiceActionResult, error) {
	if err := meshserve.ValidateName(request.ServiceName); err != nil {
		return PickerServiceActionResult{}, fmt.Errorf("service route name: %w", err)
	}
	if request.Action < PickerStopService || request.Action > PickerOpenService {
		return PickerServiceActionResult{}, fmt.Errorf("unknown service action %d", request.Action)
	}
	host, err := pickerServiceHost(hosts, request.HostID)
	if err != nil {
		return PickerServiceActionResult{}, err
	}
	started := time.Now()
	readCtx, cancel := context.WithTimeout(ctx, defaultServiceListTimeout)
	snapshot, err := listRemoteServices(readCtx, host, dial)
	cancel()
	if err != nil {
		return PickerServiceActionResult{}, err
	}
	row, err := pickerServiceRow(host, snapshot, request.ServiceName)
	if err != nil {
		return PickerServiceActionResult{}, err
	}
	result := PickerServiceActionResult{Row: row, Latency: time.Since(started)}
	switch request.Action {
	case PickerStopService, PickerRestartService:
	case PickerPingService:
		return result, nil
	case PickerOpenService:
		address, err := pickerReachableURL(row)
		if err != nil {
			return result, err
		}
		return result, open(ctx, address)
	}
	if row.Service.Run == nil {
		return result, fmt.Errorf("route /%s on %s has no --run command", request.ServiceName, host.Alias)
	}
	stopCtx, cancel := context.WithTimeout(ctx, serviceMutationTimeout)
	stopped, err := pickerMutateService(stopCtx, host, dial, request.ServiceName, protocol.TypeServiceStop)
	cancel()
	if err != nil {
		return result, err
	}
	result.Row.Service = stopped
	if request.Action == PickerStopService {
		return result, nil
	}
	startCtx, cancel := context.WithTimeout(ctx, time.Duration(row.Service.Run.ReadyTimeoutMillis)*time.Millisecond+serviceStartMargin)
	running, err := pickerMutateService(startCtx, host, dial, request.ServiceName, protocol.TypeServiceStart)
	cancel()
	if err != nil {
		return result, err
	}
	result.Row.Service = running
	return result, nil
}

func pickerServiceHost(hosts []HostRecord, id string) (HostRecord, error) {
	for _, host := range hosts {
		if host.ID == id {
			return host, nil
		}
	}
	return HostRecord{}, fmt.Errorf("service host is no longer in the picker catalog")
}

func pickerServiceRow(host HostRecord, snapshot remoteServiceSnapshot, name string) (ServiceCatalogRow, error) {
	for _, row := range liveServiceCatalogRows(host, snapshot) {
		if row.Service.Name == name {
			return row, nil
		}
	}
	return ServiceCatalogRow{}, fmt.Errorf("route /%s is no longer served on %s", name, host.Alias)
}

func pickerMutateService(ctx context.Context, host HostRecord, dial HostDialer, name, kind string) (protocol.ServiceInfo, error) {
	response, _, err := remoteServiceRequest(ctx, host, dial, protocol.Control{Type: kind, ServiceName: name}, nil)
	if err != nil {
		return protocol.ServiceInfo{}, err
	}
	if response.Type == protocol.TypeError {
		return protocol.ServiceInfo{}, remoteServiceResponseError(host, kind, response)
	}
	if response.Type != protocol.TypeOK || response.ServiceName != name || response.Service == nil || response.Service.Name != name {
		return protocol.ServiceInfo{}, fmt.Errorf("host %s returned an invalid %s acknowledgement", host.Alias, kind)
	}
	return validateRemoteService(*response.Service)
}

func pickerReachableURL(row ServiceCatalogRow) (string, error) {
	if row.Service.LocalOnly && row.Host.Alias != localHostAlias {
		return "", fmt.Errorf("route :%s is reachable on %s itself", row.Service.Name, row.Host.Alias)
	}
	address := row.URL()
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("route /%s has no reachable URL", row.Service.Name)
	}
	return address, nil
}

func openPickerURL(ctx context.Context, address string) error {
	return runPickerOpener(ctx, runtime.GOOS, address, func(ctx context.Context, name, address string) error {
		// #nosec G204 -- The OS selects a fixed opener; the validated URL is one argument.
		return exec.CommandContext(ctx, name, address).Run()
	})
}

func runPickerOpener(ctx context.Context, goos, address string, run func(context.Context, string, string) error) error {
	name := "xdg-open"
	switch goos {
	case pickerDarwinOS:
		name = "open"
	case pickerLinuxOS:
	default:
		return fmt.Errorf("opening service URLs is unavailable on %s", goos)
	}
	if err := run(ctx, name, address); err != nil {
		return fmt.Errorf("open service URL: %w", err)
	}
	return nil
}
