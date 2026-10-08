package edge

import "testing"

func TestParentCookiesFilteredAcrossConfiguredDomains(t *testing.T) {
	for _, domain := range []string{"mesh.test", "OLD.TEST"} {
		if !sharedParentCookie("session=value; Secure; Domain=." + domain) {
			t.Fatalf("parent cookie accepted: %s", domain)
		}
	}
	if sharedParentCookie("session=value; Secure; Domain=app.old.test") {
		t.Fatal("host cookie rejected")
	}
}
