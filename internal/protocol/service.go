package protocol

import (
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
)

// ServiceFromInfo reads the definition half of info. Health and demand state
// are the daemon's to report, so a client cannot set them.
func ServiceFromInfo(info ServiceInfo) meshserve.Service {
	service := meshserve.Service{
		Name:          info.Name,
		Kind:          meshserve.Kind(info.Kind),
		Target:        info.Target,
		PublicName:    info.PublicName,
		WakeOnRequest: info.WakeOnRequest,
		Isolate:       info.Isolate,
		LocalOnly:     info.LocalOnly,
	}
	for _, listen := range info.Listens {
		service.Listens = append(service.Listens, meshserve.Listen{
			Public: wirePort(listen.Public), Upstream: wirePort(listen.Upstream),
		})
	}
	if info.Run != nil {
		service.Demand = &meshserve.Demand{
			Command:      info.Run.Command,
			Cwd:          info.Run.Cwd,
			Env:          append([]string(nil), info.Run.Env...),
			Idle:         time.Duration(info.Run.IdleMillis) * time.Millisecond,
			ReadyTimeout: time.Duration(info.Run.ReadyTimeoutMillis) * time.Millisecond,
		}
	}
	return service
}

// ServiceDefinitionInfo is the wire form of a service definition, without
// health or demand state.
func ServiceDefinitionInfo(service meshserve.Service) ServiceInfo {
	info := ServiceInfo{
		Name:          service.Name,
		Kind:          string(service.Kind),
		Target:        service.Target,
		PublicName:    service.PublicName,
		WakeOnRequest: service.WakeOnRequest,
		Isolate:       service.Isolate,
		LocalOnly:     service.LocalOnly,
	}
	for _, listen := range service.Listens {
		info.Listens = append(info.Listens, ServiceListen{Public: int(listen.Public), Upstream: int(listen.Upstream)})
	}
	if service.Demand != nil {
		info.Run = &ServiceRun{
			Command:            service.Demand.Command,
			Cwd:                service.Demand.Cwd,
			Env:                append([]string(nil), service.Demand.Env...),
			IdleMillis:         service.Demand.Idle.Milliseconds(),
			ReadyTimeoutMillis: service.Demand.ReadyTimeout.Milliseconds(),
		}
	}
	return info
}

// wirePort maps an out-of-range port to 0, which validation then refuses,
// rather than letting it wrap into some other valid port.
func wirePort(port int) uint16 {
	if port < 1 || port > 65535 {
		return 0
	}
	return uint16(port)
}
