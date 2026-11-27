package ssh

import "testing"

func TestObservedDestinationAddress(t *testing.T) {
	const hopIP = "192.0.2.1"
	const serverIP = "10.0.0.12"
	for _, test := range []struct {
		name, reported, want string
		jump                 bool
	}{
		{name: "direct uses the dialed IP", reported: serverIP, want: hopIP},
		{name: "jump uses the target sshd address", reported: serverIP, want: serverIP, jump: true},
		{name: "missing target IP uses the configured host", want: "target.example", jump: true},
		{name: "invalid target IP uses the configured host", reported: "unknown", want: "target.example", jump: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination := Destination{Host: "target.example", PeerIP: hopIP, Jump: test.jump}
			if got := destination.ObservedAddress(test.reported); got != test.want {
				t.Fatalf("display address = %q, want %q", got, test.want)
			}
		})
	}
}
