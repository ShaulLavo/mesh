package dnsname

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
)

const (
	maximumAuthoritativeServers = 16
	maximumServerAddresses      = 16
)

// TXTObservation is one authoritative nameserver's current TXT answer.
type TXTObservation struct {
	Server string
	Values []string
}

// TXTObserver reads a TXT answer from every authoritative nameserver.
type TXTObserver interface {
	ObserveTXT(context.Context, string, string) ([]TXTObservation, error)
}

// AuthoritativeObserver discovers a zone's NS records, then asks each server
// directly. It does not infer propagation from the provider API.
type AuthoritativeObserver struct {
	Bootstrap *net.Resolver
	Dialer    *net.Dialer
}

// ObserveTXT returns one answer from every authoritative nameserver.
func (o AuthoritativeObserver) ObserveTXT(ctx context.Context, zone, name string) ([]TXTObservation, error) {
	if ctx == nil {
		return nil, errors.New("dnsname: observe TXT with nil context")
	}
	bootstrap := o.Bootstrap
	if bootstrap == nil {
		bootstrap = net.DefaultResolver
	}
	dialer := o.Dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 3 * time.Second}
	}
	servers, err := bootstrap.LookupNS(ctx, zone)
	if err != nil {
		return nil, fmt.Errorf("dnsname: discover authoritative servers for %s: %w", zone, err)
	}
	if len(servers) == 0 || len(servers) > maximumAuthoritativeServers {
		return nil, fmt.Errorf("dnsname: zone %s returned %d authoritative servers", zone, len(servers))
	}
	slices.SortFunc(servers, func(a, b *net.NS) int { return strings.Compare(a.Host, b.Host) })
	observations := make([]TXTObservation, 0, len(servers))
	for _, server := range servers {
		host := strings.TrimSuffix(server.Host, ".")
		addresses, err := bootstrap.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("dnsname: resolve authoritative server %s: %w", host, err)
		}
		if len(addresses) == 0 || len(addresses) > maximumServerAddresses {
			return nil, fmt.Errorf("dnsname: authoritative server %s returned %d addresses", host, len(addresses))
		}
		var values []string
		var lookupErrors []error
		for _, address := range addresses {
			endpoint := net.JoinHostPort(address.IP.String(), "53")
			resolver := &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, network, endpoint)
				},
			}
			values, err = resolver.LookupTXT(ctx, name)
			if err == nil {
				break
			}
			lookupErrors = append(lookupErrors, err)
		}
		if err != nil {
			return nil, fmt.Errorf("dnsname: query authoritative server %s for %s: %w", host, name, errors.Join(lookupErrors...))
		}
		slices.Sort(values)
		observations = append(observations, TXTObservation{Server: host, Values: values})
	}
	return observations, nil
}

// WaitForTXT polls until every authoritative nameserver returns value.
func WaitForTXT(ctx context.Context, observer TXTObserver, zone, name, value string, interval time.Duration) error {
	if ctx == nil {
		return errors.New("dnsname: wait for TXT with nil context")
	}
	if observer == nil {
		return errors.New("dnsname: wait for TXT with nil observer")
	}
	if interval <= 0 {
		return errors.New("dnsname: TXT polling interval must be positive")
	}
	var lastErr error
	for {
		observations, err := observer.ObserveTXT(ctx, zone, name)
		if err == nil && len(observations) > 0 {
			missing := make([]string, 0, len(observations))
			for _, observation := range observations {
				if !slices.Contains(observation.Values, value) {
					missing = append(missing, observation.Server)
				}
			}
			if len(missing) == 0 {
				return nil
			}
			lastErr = fmt.Errorf("TXT value is absent on %s", strings.Join(missing, ", "))
		} else if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New("no authoritative TXT observations")
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("dnsname: wait for TXT %s propagation: %w: %v", name, ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
}
