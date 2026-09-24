package serve

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestNormalizeRetainsExplicitPublicName(t *testing.T) {
	for _, name := range []string{"", "blog.shaulavo.dev", "my-blog.shaulavo.dev"} {
		t.Run(name, func(t *testing.T) {
			service, err := Normalize(Service{Name: "site", Kind: Proxy, Target: "3000", PublicName: name})
			if err != nil {
				t.Fatal(err)
			}
			if service.PublicName != name {
				t.Fatalf("public name = %q, want %q", service.PublicName, name)
			}
		})
	}
}

func TestNormalizeRejectsInvalidPublicName(t *testing.T) {
	invalid := []string{
		"shaulavo.dev",
		"api.blog.shaulavo.dev",
		"mesh.shaulavo.dev",
		"*.shaulavo.dev",
		"BLOG.shaulavo.dev",
		"blog.shaulavo.dev.",
		"shaulavo.dev.example.com",
		"notshaulavo.dev",
		"bad_.shaulavo.dev",
		"-bad.shaulavo.dev",
		"bad-.shaulavo.dev",
		"bad..shaulavo.dev",
		strings.Repeat("a", 64) + ".shaulavo.dev",
	}
	for _, name := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := Normalize(Service{Name: "site", Kind: Proxy, Target: "3000", PublicName: name}); err == nil {
				t.Fatalf("invalid public name %q succeeded", name)
			}
		})
	}
}

func TestCheckServiceDialsProxyUpstream(t *testing.T) {
	listener := listenLoopback(t)
	defer listener.Close() //nolint:errcheck // test cleanup
	service := Service{Name: "app", Kind: Proxy, Target: loopbackPort(t, listener)}

	status := CheckService(t.Context(), service)
	if !status.Healthy || status.Problem != "" {
		t.Fatalf("status with listener = %#v, want healthy", status)
	}
}

func TestCheckServiceReportsUnreachableProxyUpstream(t *testing.T) {
	for _, wake := range []bool{false, true} {
		t.Run("wake="+strconv.FormatBool(wake), func(t *testing.T) {
			port := closedLoopbackPort(t)
			// A wake route is not excused: wake-on-request wakes the origin
			// host, and a probe only runs while that host is already awake.
			service := Service{Name: "app", Kind: Proxy, Target: port, PublicName: "app.shaulavo.dev", WakeOnRequest: wake}

			status := CheckService(t.Context(), service)
			want := "upstream 127.0.0.1:" + port + " unreachable"
			if status.Healthy || status.Problem != want {
				t.Fatalf("status without listener = %#v, want unhealthy with %q", status, want)
			}
			if len(status.Problem) > MaximumServiceProblemBytes {
				t.Fatalf("problem is %d bytes, limit %d", len(status.Problem), MaximumServiceProblemBytes)
			}
		})
	}
}

func TestCheckServicesKeepsInputOrder(t *testing.T) {
	listener := listenLoopback(t)
	defer listener.Close() //nolint:errcheck // test cleanup
	live := loopbackPort(t, listener)
	dead := closedLoopbackPort(t)
	services := make([]Service, 0, 64)
	for index := range 64 {
		target := dead
		if index%2 == 0 {
			target = live
		}
		services = append(services, Service{Name: "app" + strconv.Itoa(index), Kind: Proxy, Target: target})
	}

	statuses := CheckServices(t.Context(), services)
	for index, status := range statuses {
		if status.Service != services[index] || status.Healthy != (index%2 == 0) {
			t.Fatalf("status %d = %#v, want service %#v healthy=%v", index, status, services[index], index%2 == 0)
		}
	}
}

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func loopbackPort(t *testing.T, listener net.Listener) string {
	t.Helper()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// closedLoopbackPort returns a port that had a listener a moment ago, which is
// the closest a test can get to one that nothing listens on.
func closedLoopbackPort(t *testing.T) string {
	t.Helper()
	listener := listenLoopback(t)
	port := loopbackPort(t, listener)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}
