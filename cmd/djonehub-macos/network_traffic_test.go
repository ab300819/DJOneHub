package main

import "testing"

func TestParseMacInterfaceCountersUsesLinkRow(t *testing.T) {
	input := `Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll
en9 1500 <Link#22> aa:bb:cc:dd:ee:ff 120 0 4096 80 0 2048 0
en9 1500 192.168.225 192.168.225.20 120 - 4096 80 - 2048 -`

	got := parseMacInterfaceCounters(input)["en9"]
	if got.RX != 4096 || got.TX != 2048 {
		t.Fatalf("en9 counters = %+v, want RX=4096 TX=2048", got)
	}
}

func TestParseNettopActivityTracksProcessAndInterface(t *testing.T) {
	input := `time,,interface,state,bytes_in,bytes_out,
23:01:00,Google Chrome H.976,,,69502,80212,
23:01:00,tcp4 100.65.155.123:54780<->104.18.32.47:443,utun14,Established,4355,28456,
23:01:00,io.tailscale.ip.35082,,,169811,1687128,
23:01:00,tcp4 192.168.225.23:53344<->205.147.105.30:443,en9,Established,7606,21720,
`
	records := parseNettopActivity(input)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if records[0].Process != "Google Chrome H" || records[0].Interface != "utun14" || records[0].IP != "104.18.32.47" || records[0].Port != "443" {
		t.Fatalf("first record = %+v", records[0])
	}
	if records[1].Process != "io.tailscale.ip" || records[1].Interface != "en9" {
		t.Fatalf("second record = %+v", records[1])
	}
}

func TestParseNettopFlowSupportsIPv6(t *testing.T) {
	protocol, host, port := parseNettopFlow("tcp6 2408:1::2.53147<->2606:b740:49::113.80")
	if protocol != "tcp6" || host != "2606:b740:49::113" || port != "80" {
		t.Fatalf("flow = %q %q %q", protocol, host, port)
	}
}
