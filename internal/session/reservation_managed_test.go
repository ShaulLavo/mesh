package session

import "testing"

func TestManagedReservationRequiresKnownBoot(t *testing.T) {
	if _, err := NewReservation("ABCD", "", Managed); err == nil {
		t.Fatal("managed reservation accepted unknown boot")
	}
	if _, err := NewReservation("ABCD", "", Generic); err != nil {
		t.Fatal(err)
	}
}
