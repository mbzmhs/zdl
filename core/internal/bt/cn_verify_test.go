package bt

import "testing"

func TestClientNameVerify(t *testing.T) {
	cases := map[string]string{
		"-BC0199-feF3Cq30ths4": "BitComet 1.99",
		"-BC0192-xxxxxxxxxxxx": "BitComet 1.92",
		"-qB5010-abcdefghijk":  "qBittorrent 5.0.1",
		"-qB5140-abcdefghijk":  "qBittorrent 5.1.4",
		"-TR4130-abcdefghijk":  "Transmission 4.1.3",
		"-UT360W-abcdefghijk":  "uTorrent 3.6",
		"-LT1220-abcdefghijk":  "libtorrent 1.2.2",
		"-XX1234-abcdefghijk":  "Xtorrent 1.2.3.4",
	}
	for id, want := range cases {
		var pid [20]byte
		copy(pid[:], id)
		if got := ClientName(pid); got != want {
			t.Errorf("%s → %q,期望 %q", id, got, want)
		}
	}
}
