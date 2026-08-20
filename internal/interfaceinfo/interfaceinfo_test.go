package interfaceinfo

import "testing"

func TestListIncludesLoopback(t *testing.T) {
	interfaces, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags.String() == "" {
			t.Errorf("interface %q has empty flags", iface.Name)
		}
		if iface.Name == "lo0" {
			if !iface.Up() || !iface.Running() {
				t.Fatalf("loopback flags = %s, want up and running", iface.Flags)
			}
			return
		}
	}
	t.Fatal("loopback interface lo0 was not listed")
}
