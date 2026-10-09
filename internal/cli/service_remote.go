package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/transport"
)

const (
	maximumRemoteErrorBytes = 512
)

func dialControlHost(ctx context.Context, host HostRecord) (transport.Conn, error) {
	conn, err := transport.DialPinned(ctx, host.Endpoint, host.MeshIdentity)
	if err != nil {
		return nil, fmt.Errorf("authenticate control host: %w", err)
	}
	return conn, nil
}

func previewRemoteService(ctx context.Context, host HostRecord, dial HostDialer, service protocol.ServiceInfo) (protocol.ServicePreview, string, error) {
	response, info, err := remoteServiceRequest(ctx, host, dial, protocol.Control{
		Type: protocol.TypeServicePreview, Service: &service,
	}, nil)
	if err != nil {
		return protocol.ServicePreview{}, "", err
	}
	if response.Type == protocol.TypeError {
		return protocol.ServicePreview{}, "", remoteServiceResponseError(host, "service preview", response)
	}
	if response.Type != protocol.TypeServicePreviewed || response.ServicePreview == nil {
		return protocol.ServicePreview{}, "", fmt.Errorf("host %s returned an invalid service preview", HostLabel(host))
	}
	preview := *response.ServicePreview
	if _, err := validateRemoteService(preview.Service); err != nil {
		return protocol.ServicePreview{}, "", fmt.Errorf("host %s returned an invalid service preview: %w", HostLabel(host), err)
	}

	if service.DisplayName != "" && preview.Service.DisplayName != service.DisplayName {
		return protocol.ServicePreview{}, "", fmt.Errorf("host %s did not preserve the display name", HostLabel(host))
	}
	if preview.Service.Name != service.Name || preview.Service.PrivateHost != service.PrivateHost || preview.Service.Isolate != service.Isolate || service.Kind != "" && preview.Service.Kind != service.Kind {
		return protocol.ServicePreview{}, "", fmt.Errorf("host %s changed service semantics in its preview", HostLabel(host))
	}
	if err := validatePreviewInference(service, preview.Service); err != nil {
		return protocol.ServicePreview{}, "", fmt.Errorf("host %s returned an invalid service preview: %w", HostLabel(host), err)
	}
	if !sameDemandDefinition(service, preview.Service) {

		return protocol.ServicePreview{}, "", fmt.Errorf("host %s changed the listeners or launch recipe in its preview", HostLabel(host))
	}
	return preview, info.PrivateName, nil
}

type remoteServicePublication struct {
	Service     protocol.ServiceInfo
	PrivateName string
	Warning     string
}

func upsertRemoteService(ctx context.Context, host HostRecord, dial HostDialer, requested protocol.ServiceInfo, preview protocol.ServicePreview, privateName string) (remoteServicePublication, error) {
	var expectedPrivateName *string
	if privateName != "" {
		expectedPrivateName = &privateName
	}
	response, info, err := remoteServiceRequest(ctx, host, dial, protocol.Control{
		Type: protocol.TypeServiceUpsert, Service: &requested, ServicePreview: &preview,
	}, expectedPrivateName)
	if err != nil {
		return remoteServicePublication{}, err
	}
	if response.Type == protocol.TypeError {
		return remoteServicePublication{}, remoteServiceResponseError(host, "service publication", response)
	}
	if response.Type != protocol.TypeServiceUpserted || response.Service == nil {
		return remoteServicePublication{}, fmt.Errorf("host %s returned an invalid service publication acknowledgement", HostLabel(host))
	}
	acknowledged, err := validateRemoteService(*response.Service)
	if err != nil {
		return remoteServicePublication{}, err
	}
	if !sameServiceDefinition(acknowledged, preview.Service) {
		return remoteServicePublication{}, fmt.Errorf("host %s acknowledged a different service definition", HostLabel(host))
	}
	return remoteServicePublication{Service: acknowledged, PrivateName: info.PrivateName, Warning: response.Message}, nil
}

type remoteServiceSnapshot struct {
	Host                   HostRecord
	PrivateName            string
	Services               []protocol.ServiceInfo
	ServiceHealthSupported bool
}

func listRemoteServices(ctx context.Context, host HostRecord, dial HostDialer) (remoteServiceSnapshot, error) {
	response, info, err := remoteServiceRequest(ctx, host, dial, protocol.Control{Type: protocol.TypeServiceList}, nil)
	if err != nil {
		return remoteServiceSnapshot{}, err
	}
	if response.Type == protocol.TypeError {
		return remoteServiceSnapshot{}, remoteServiceResponseError(host, "service list", response)
	}
	if response.Type != protocol.TypeServiceListed || len(response.Services) > meshserve.MaximumServices {
		return remoteServiceSnapshot{}, fmt.Errorf("host %s returned an invalid service list", HostLabel(host))
	}
	services := make([]protocol.ServiceInfo, len(response.Services))
	seen := make(map[string]struct{}, len(response.Services))
	previous := ""
	for index, candidate := range response.Services {
		service, err := validateRemoteService(candidate)
		if err != nil {
			return remoteServiceSnapshot{}, fmt.Errorf("host %s returned an invalid service list: %w", HostLabel(host), err)
		}
		if _, exists := seen[service.Name]; exists {
			return remoteServiceSnapshot{}, fmt.Errorf("host %s returned duplicate service %s", HostLabel(host), service.Name)
		}
		if index > 0 && service.Name <= previous {
			return remoteServiceSnapshot{}, fmt.Errorf("host %s returned services out of canonical order", HostLabel(host))
		}
		seen[service.Name] = struct{}{}
		previous = service.Name
		services[index] = service
	}
	host.NameVerified = info.MachineName != ""
	if host.NameVerified {
		host.MachineName, host.NameRevision = info.MachineName, info.NameRevision
	}
	return remoteServiceSnapshot{Host: host, PrivateName: info.PrivateName, Services: services, ServiceHealthSupported: info.ServiceHealthSupported}, nil
}

func deleteRemoteService(ctx context.Context, host HostRecord, dial HostDialer, name string) error {
	if err := meshserve.ValidateName(name); err != nil {
		return err
	}
	response, _, err := remoteServiceRequest(ctx, host, dial, protocol.Control{Type: protocol.TypeServiceDelete, ServiceName: name}, nil)
	if err != nil {
		return err
	}
	if response.Type == protocol.TypeError {
		return remoteServiceResponseError(host, "service deletion", response)
	}
	if response.Type != protocol.TypeServiceDeleted || response.ServiceName != name {
		return fmt.Errorf("host %s returned an invalid service deletion acknowledgement", HostLabel(host))
	}
	return nil
}

func remoteServiceRequest(ctx context.Context, host HostRecord, dial HostDialer, request protocol.Control, expectedPrivateName *string) (protocol.Control, protocol.HostInfo, error) {
	if ctx == nil {
		return protocol.Control{}, protocol.HostInfo{}, errors.New("cli: nil service request context")
	}
	conn, info, err := openVerifiedHostInfo(ctx, host, dial)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, err
	}
	defer conn.Close() //nolint:errcheck // request result is authoritative
	if expectedPrivateName != nil && info.PrivateName != *expectedPrivateName {
		return protocol.Control{}, protocol.HostInfo{}, fmt.Errorf("host %s private name changed after preview", HostLabel(host))
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, err
	}
	request.RequestID = requestID
	response, err := controlRequest(ctx, conn, request)
	return response, info, err
}

func validateRemoteService(info protocol.ServiceInfo) (protocol.ServiceInfo, error) {
	received := protocol.ServiceFromInfo(info)
	service, err := meshserve.Normalize(received)
	if err != nil {
		return protocol.ServiceInfo{}, errors.New("service definition is invalid")
	}
	if !service.Equal(received) {
		return protocol.ServiceInfo{}, errors.New("service definition is not canonical")
	}
	if err := validateServiceDemand(info.Demand); err != nil {
		return protocol.ServiceInfo{}, err
	}
	if len(info.Problem) > meshserve.MaximumServiceProblemBytes || info.Healthy && info.Problem != "" || !utf8.ValidString(info.Problem) {
		return protocol.ServiceInfo{}, errors.New("service health is invalid")
	}
	return info, nil
}

func sameServiceDefinition(left, right protocol.ServiceInfo) bool {
	return protocol.ServiceFromInfo(left).Equal(protocol.ServiceFromInfo(right))
}

func validatePreviewInference(requested, preview protocol.ServiceInfo) error {
	if numericCLIServiceTarget(requested.Target) {
		port, err := strconv.ParseUint(requested.Target, 10, 16)
		if err != nil || port == 0 {
			return errors.New("numeric target is not a port from 1 to 65535")
		}
		if preview.Kind != string(meshserve.Proxy) || preview.Target != strconv.FormatUint(port, 10) {
			return errors.New("numeric target was not returned as the exact canonical proxy port")
		}
		return nil
	}
	expectedKind := string(meshserve.Static)
	if requested.Kind == string(meshserve.Files) {
		expectedKind = string(meshserve.Files)
	}
	if preview.Kind != expectedKind {
		return errors.New("directory target was returned with a different service kind")
	}
	return nil
}

func remoteServiceResponseError(host HostRecord, operation string, response protocol.Control) error {
	detail := safeRemoteText(response.Message)
	base := fmt.Sprintf("host %s rejected %s", HostLabel(host), operation)

	if detail == "" {
		return errors.New(base)
	}
	return fmt.Errorf("%s: %s", base, detail)
}

func safeRemoteText(value string) string {
	value = strings.ToValidUTF8(value, "?")
	value = strings.Map(func(character rune) rune {
		if !unicode.IsPrint(character) {
			return ' '
		}
		return character
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= maximumRemoteErrorBytes {
		return value
	}
	limit := maximumRemoteErrorBytes - len("…")
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit] + "…"
}
