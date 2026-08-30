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
	edgeListPageSize        = 100
	maximumEdgeRoutes       = 8192
	maximumEdgePages        = maximumEdgeRoutes/edgeListPageSize + 2
	maximumRemoteErrorBytes = 512
)

func dialControlHost(ctx context.Context, host HostRecord) (transport.Conn, error) {
	return transport.DialOnce(ctx, host.Endpoint, transport.DialOptions{})
}

func previewRemoteService(ctx context.Context, host HostRecord, dial HostDialer, service protocol.ServiceInfo, allowCredentials bool) (protocol.ServicePreview, string, error) {
	response, privateName, err := remoteServiceRequest(ctx, host, dial, protocol.Control{
		Type: protocol.TypeServicePreview, Service: &service, AllowCredentials: allowCredentials,
	}, nil)
	if err != nil {
		return protocol.ServicePreview{}, "", err
	}
	if response.Type == protocol.TypeError {
		return protocol.ServicePreview{}, "", remoteServiceResponseError(host, "service preview", response)
	}
	if response.Type != protocol.TypeServicePreviewed || response.ServicePreview == nil {
		return protocol.ServicePreview{}, "", fmt.Errorf("host %s returned an invalid service preview", host.Alias)
	}
	preview := *response.ServicePreview
	if _, err := validateRemoteService(preview.Service); err != nil {
		return protocol.ServicePreview{}, "", fmt.Errorf("host %s returned an invalid service preview: %w", host.Alias, err)
	}
	if preview.FileCount > meshserve.MaximumPreviewEntries {
		return protocol.ServicePreview{}, "", fmt.Errorf("host %s returned an invalid service file count", host.Alias)
	}
	if preview.Service.Kind == string(meshserve.Proxy) && preview.FileCount != 0 {
		return protocol.ServicePreview{}, "", fmt.Errorf("host %s returned files for a proxy preview", host.Alias)
	}
	if preview.Service.Name != service.Name || preview.Service.PublicName != service.PublicName || preview.Service.WakeOnRequest != service.WakeOnRequest || service.Kind != "" && preview.Service.Kind != service.Kind {
		return protocol.ServicePreview{}, "", fmt.Errorf("host %s changed service semantics in its preview", host.Alias)
	}
	if err := validatePreviewInference(service, preview.Service); err != nil {
		return protocol.ServicePreview{}, "", fmt.Errorf("host %s returned an invalid service preview: %w", host.Alias, err)
	}
	return preview, privateName, nil
}

func upsertRemoteService(ctx context.Context, host HostRecord, dial HostDialer, requested protocol.ServiceInfo, preview protocol.ServicePreview, privateName string, allowCredentials bool) (protocol.ServiceInfo, string, error) {
	var expectedPrivateName *string
	if requested.PublicName == "" && privateName != "" {
		expectedPrivateName = &privateName
	}
	response, currentPrivateName, err := remoteServiceRequest(ctx, host, dial, protocol.Control{
		Type: protocol.TypeServiceUpsert, Service: &requested, ServicePreview: &preview, AllowCredentials: allowCredentials,
	}, expectedPrivateName)
	if err != nil {
		return protocol.ServiceInfo{}, "", err
	}
	if response.Type == protocol.TypeError {
		return protocol.ServiceInfo{}, "", remoteServiceResponseError(host, "service publication", response)
	}
	if response.Type != protocol.TypeServiceUpserted || response.Service == nil {
		return protocol.ServiceInfo{}, "", fmt.Errorf("host %s returned an invalid service publication acknowledgement", host.Alias)
	}
	acknowledged, err := validateRemoteService(*response.Service)
	if err != nil {
		return protocol.ServiceInfo{}, "", err
	}
	if !sameServiceDefinition(acknowledged, preview.Service) {
		return protocol.ServiceInfo{}, "", fmt.Errorf("host %s acknowledged a different service definition", host.Alias)
	}
	return acknowledged, currentPrivateName, nil
}

type remoteServiceSnapshot struct {
	PrivateName string
	Services    []protocol.ServiceInfo
}

func listRemoteServices(ctx context.Context, host HostRecord, dial HostDialer) (remoteServiceSnapshot, error) {
	response, privateName, err := remoteServiceRequest(ctx, host, dial, protocol.Control{Type: protocol.TypeServiceList}, nil)
	if err != nil {
		return remoteServiceSnapshot{}, err
	}
	if response.Type == protocol.TypeError {
		return remoteServiceSnapshot{}, remoteServiceResponseError(host, "service list", response)
	}
	if response.Type != protocol.TypeServiceListed || len(response.Services) > meshserve.MaximumServices {
		return remoteServiceSnapshot{}, fmt.Errorf("host %s returned an invalid service list", host.Alias)
	}
	services := make([]protocol.ServiceInfo, len(response.Services))
	seen := make(map[string]struct{}, len(response.Services))
	previous := ""
	for index, candidate := range response.Services {
		service, err := validateRemoteService(candidate)
		if err != nil {
			return remoteServiceSnapshot{}, fmt.Errorf("host %s returned an invalid service list: %w", host.Alias, err)
		}
		if _, exists := seen[service.Name]; exists {
			return remoteServiceSnapshot{}, fmt.Errorf("host %s returned duplicate service %s", host.Alias, service.Name)
		}
		if index > 0 && service.Name <= previous {
			return remoteServiceSnapshot{}, fmt.Errorf("host %s returned services out of canonical order", host.Alias)
		}
		seen[service.Name] = struct{}{}
		previous = service.Name
		services[index] = service
	}
	return remoteServiceSnapshot{PrivateName: privateName, Services: services}, nil
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
		return fmt.Errorf("host %s returned an invalid service deletion acknowledgement", host.Alias)
	}
	return nil
}

func listRemoteEdge(ctx context.Context, host HostRecord, dial HostDialer) ([]protocol.EdgeRouteInfo, error) {
	conn, err := openVerifiedHost(ctx, host, dial)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck // request result is authoritative
	cursor := ""
	routes := make([]protocol.EdgeRouteInfo, 0)
	seen := make(map[string]struct{})
	previousKey := ""
	for page := 0; page < maximumEdgePages; page++ {
		requestID, err := newDaemonRequestID()
		if err != nil {
			return nil, err
		}
		response, err := controlRequest(ctx, conn, protocol.Control{
			Type: protocol.TypeEdgeList, RequestID: requestID, EdgeCursor: cursor, EdgeLimit: edgeListPageSize,
		})
		if err != nil {
			return nil, err
		}
		if response.Type == protocol.TypeError {
			return nil, remoteServiceResponseError(host, "public edge list", response)
		}
		if response.Type != protocol.TypeEdgeListed || len(response.EdgeRoutes) > edgeListPageSize {
			return nil, fmt.Errorf("host %s returned an invalid public edge page", host.Alias)
		}
		for _, route := range response.EdgeRoutes {
			if err := validateRemoteEdgeRoute(route); err != nil {
				return nil, fmt.Errorf("host %s returned an invalid public edge route: %w", host.Alias, err)
			}
			key := route.PublicName + "\x00" + route.ServiceName
			if _, exists := seen[key]; exists {
				return nil, fmt.Errorf("host %s returned a duplicate public edge route", host.Alias)
			}
			if previousKey != "" && key <= previousKey {
				return nil, fmt.Errorf("host %s returned public edge routes out of canonical order", host.Alias)
			}
			seen[key] = struct{}{}
			previousKey = key
			routes = append(routes, route)
			if len(routes) > maximumEdgeRoutes {
				return nil, fmt.Errorf("host %s public edge list exceeds %d routes", host.Alias, maximumEdgeRoutes)
			}
		}
		next := response.EdgeNextCursor
		if next == "" {
			return routes, nil
		}
		if next == cursor || len(next) > 786 || len(response.EdgeRoutes) == 0 {
			return nil, fmt.Errorf("host %s returned an invalid public edge cursor", host.Alias)
		}
		cursor = next
	}
	return nil, fmt.Errorf("host %s public edge list did not terminate", host.Alias)
}

func remoteServiceRequest(ctx context.Context, host HostRecord, dial HostDialer, request protocol.Control, expectedPrivateName *string) (protocol.Control, string, error) {
	if ctx == nil {
		return protocol.Control{}, "", errors.New("cli: nil service request context")
	}
	conn, info, err := openVerifiedHostInfo(ctx, host, dial)
	if err != nil {
		return protocol.Control{}, "", err
	}
	defer conn.Close() //nolint:errcheck // request result is authoritative
	if expectedPrivateName != nil && info.PrivateName != *expectedPrivateName {
		return protocol.Control{}, "", fmt.Errorf("host %s private name changed after preview", host.Alias)
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return protocol.Control{}, "", err
	}
	request.RequestID = requestID
	response, err := controlRequest(ctx, conn, request)
	return response, info.PrivateName, err
}

func validateRemoteService(info protocol.ServiceInfo) (protocol.ServiceInfo, error) {
	service, err := meshserve.Normalize(meshserve.Service{
		Name: info.Name, Kind: meshserve.Kind(info.Kind), Target: info.Target,
		PublicName: info.PublicName, WakeOnRequest: info.WakeOnRequest,
	})
	if err != nil {
		return protocol.ServiceInfo{}, errors.New("service definition is invalid")
	}
	if service.Name != info.Name || string(service.Kind) != info.Kind || service.Target != info.Target || service.PublicName != info.PublicName {
		return protocol.ServiceInfo{}, errors.New("service definition is not canonical")
	}
	if len(info.Problem) > meshserve.MaximumServiceProblemBytes || info.Healthy && info.Problem != "" || !utf8.ValidString(info.Problem) {
		return protocol.ServiceInfo{}, errors.New("service health is invalid")
	}
	return info, nil
}

func sameServiceDefinition(left, right protocol.ServiceInfo) bool {
	return left.Name == right.Name && left.Kind == right.Kind && left.Target == right.Target &&
		left.PublicName == right.PublicName && left.WakeOnRequest == right.WakeOnRequest
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

func validateRemoteEdgeRoute(route protocol.EdgeRouteInfo) error {
	if err := meshserve.ValidatePublicName(route.PublicName); err != nil || route.PublicName == "" {
		return errors.New("invalid public name")
	}
	if err := meshserve.ValidateName(route.ServiceName); err != nil {
		return errors.New("invalid service name")
	}
	if route.DisplayAlias == "" || len(route.DisplayAlias) > 63 || strings.IndexFunc(route.DisplayAlias, func(character rune) bool { return !unicode.IsPrint(character) }) >= 0 {
		return errors.New("invalid display alias")
	}
	if route.LastSeenAt.IsZero() || route.LastSeenAt.UnixMilli() < 0 {
		return errors.New("invalid last-seen time")
	}
	return nil
}

func remoteServiceResponseError(host HostRecord, operation string, response protocol.Control) error {
	detail := safeRemoteText(response.Message)
	base := fmt.Sprintf("host %s rejected %s", host.Alias, operation)
	switch response.ErrorCode {
	case protocol.ErrorCodeCredentialsFound:
		if detail == "" {
			return fmt.Errorf("%w: %s", meshserve.ErrCredentialsFound, base)
		}
		return fmt.Errorf("%w: %s: %s", meshserve.ErrCredentialsFound, base, detail)
	case protocol.ErrorCodeEdgeRouteCollision:
		return fmt.Errorf("%s: public route is already claimed", base)
	case protocol.ErrorCodeEdgeWakeUnavailable:
		return fmt.Errorf("%s: wake-on-request is not configured", base)
	case protocol.ErrorCodeEdgeStaleSequence, protocol.ErrorCodeEdgeConflict:
		return fmt.Errorf("%s: public edge state changed; retry", base)
	}
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
