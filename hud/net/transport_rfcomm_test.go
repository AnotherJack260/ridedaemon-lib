package net

import (
	"encoding/hex"
	"io"
	stdnet "net"
	"testing"
	"time"
)

// The bootstrap against a fake dash: connect, client info in CarbitRide's key order, a "not
// ready" poll, then the network.
func TestRfcommBuildNetwork(t *testing.T) {
	phone, dash := stdnet.Pipe()
	defer phone.Close()
	defer dash.Close()
	_ = dash.SetDeadline(time.Now().Add(3 * time.Second))
	client := NewRfcommClient(phone)
	client.pace = 0

	type result struct {
		n   *RfcommNetwork
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := client.BuildNetwork(RfcommClientInfo{PhoneID: "id1", PhoneName: "Pixel", PackageName: "io.motohub.android",
			NetInterface: []RfcommInterface{{Name: "wlan0", Addr: "192.168.1.5", Mask: "255.255.255.0"}}})
		done <- result{n, err}
	}()

	expect := func(cmd uint32, body string) {
		t.Helper()
		got, err := readPXCFrame(dash)
		if err != nil {
			t.Fatal(err)
		}
		if got.Command != cmd || string(got.Body) != body {
			t.Fatalf("phone sent 0x%x %q, want 0x%x %q", got.Command, got.Body, cmd, body)
		}
	}
	answer := func(cmd uint32, body string) {
		t.Helper()
		if _, err := dash.Write(pxcFrame(cmd, []byte(body))); err != nil {
			t.Fatal(err)
		}
	}

	header := make([]byte, pxcHeaderSize)
	if _, err := io.ReadFull(dash, header); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(header); got != "00000800100000001000080000000000" {
		t.Fatalf("EBT_CONN_TYPE_P2C = %s", got)
	}
	answer(rfcommConnect+1, "")
	expect(rfcommClientInfo, `{"phoneType":0,"phoneID":"id1","phoneName":"Pixel","packageName":"io.motohub.android","netInterface":[{"name":"wlan0","addr":"192.168.1.5","mask":"255.255.255.0"}]}`)
	answer(rfcommClientInfo+1, "")
	expect(rfcommBuildNet, "")
	answer(rfcommBuildNet+1, `{"status":-1}`)
	expect(rfcommBuildNet, "")
	answer(rfcommBuildNet+1, `{"status":"1","phoneApFrequency":5,"carNetDeviceInfo":{"ssid":"DASH","pwd":"secret12","auth":"WPA2","mac":"aa:bb","name":"dash","mode":2},"packagename":"net.easyconn.easyride.wws","channel":"21321","flavor":51,"HUName":"JCDZ21","HUID":"HU1"}`)

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.n.Status != 1 || r.n.CarNetDeviceInfo.SSID != "DASH" || r.n.CarNetDeviceInfo.Mode != 2 ||
		r.n.PackageName != "net.easyconn.easyride.wws" || r.n.Flavor != 51 || r.n.HUID != "HU1" {
		t.Fatalf("network = %+v", r.n)
	}

	// Status 2's hotspot report, the failure report, and the "network up" notice.
	go func() { _ = client.NotifyAPInfo("PHONE", "pw123456", "WPA2", "192.168.43.1") }()
	expect(rfcommNotifyAP, `{"ssid":"PHONE","pwd":"pw123456","auth":"WPA2","ip":"192.168.43.1"}`)
	answer(rfcommNotifyAP+1, "")
	go func() { _ = client.NotifyAPFailed("no hotspot") }()
	expect(PxcStatusError, "no hotspot")
	go func() { _ = client.NotifyNetworkReady() }()
	expect(rfcommConnect, "")
	answer(rfcommConnect+1, "")
	expect(rfcommBuildNetDone, "")
	answer(rfcommBuildNetDone+1, "")
}
