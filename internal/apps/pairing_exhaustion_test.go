package apps

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnonymousGETPairingExhaustionPoC(t *testing.T) {
	f := newAppFixture(t)
	for range 256 {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/", nil)
		request.RemoteAddr = "192.0.2.1:1234"
		f.edge.ServeHost(response, request, ManagementHost)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/pair", nil)
	request.RemoteAddr = "192.0.2.2:1234"
	f.edge.ServeHost(response, request, ManagementHost)
	if response.Code != http.StatusOK {
		t.Fatalf("after 256 anonymous GETs, owner's pairing page = %d, want 200", response.Code)
	}
}
