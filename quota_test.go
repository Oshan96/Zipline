package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

// D7: daily and monthly periods, with the plan's reset day (short months end early).
func TestQuotaPeriods(t *testing.T) {
	for _, c := range []struct {
		q        Quota
		now      string
		from, to string
	}{
		{Quota{Period: "day"}, "2026-10-11 23:59", "2026-10-11 00:00", "2026-10-12 00:00"},
		{Quota{Period: "day"}, "2026-12-31 08:00", "2026-12-31 00:00", "2027-01-01 00:00"},
		{Quota{Period: "month", ResetDay: 15}, "2026-10-11 12:00", "2026-09-15 00:00", "2026-10-15 00:00"},
		{Quota{Period: "month", ResetDay: 15}, "2026-10-15 00:00", "2026-10-15 00:00", "2026-11-15 00:00"},
		{Quota{Period: "month", ResetDay: 1}, "2026-01-01 00:30", "2026-01-01 00:00", "2026-02-01 00:00"},
		{Quota{Period: "month", ResetDay: 1}, "2026-12-31 23:00", "2026-12-01 00:00", "2027-01-01 00:00"},
		{Quota{Period: "month", ResetDay: 31}, "2026-02-27 10:00", "2026-01-31 00:00", "2026-02-28 00:00"}, // February ends early
		{Quota{Period: "month", ResetDay: 31}, "2026-02-28 10:00", "2026-02-28 00:00", "2026-03-31 00:00"},
		{Quota{Period: "month", ResetDay: 30}, "2028-02-29 10:00", "2028-02-29 00:00", "2028-03-30 00:00"}, // a leap year
		{Quota{Period: "month", ResetDay: 20}, "2027-01-05 10:00", "2026-12-20 00:00", "2027-01-20 00:00"}, // across the new year
	} {
		from, to := c.q.period(day(c.now))
		if !from.Equal(day(c.from)) || !to.Equal(day(c.to)) {
			t.Errorf("%+v at %s: %s – %s, want %s – %s", c.q, c.now, from.Format("2006-01-02 15:04"), to.Format("2006-01-02 15:04"), c.from, c.to)
		}
	}
	q := Quota{Period: "month", ResetDay: 15}
	if a, b := q.periodKey(day("2026-10-14 23:00")), q.periodKey(day("2026-10-15 01:00")); a == b {
		t.Fatal("a new period has a new key")
	}
}

// D7: counting downloads, uploads or both, per network, without the free hours.
func TestQuotaCounting(t *testing.T) {
	dm := newDayMeter(filepath.Join(t.TempDir(), "usage-days.json"))
	now := day("2026-10-11 10:00")
	dm.add("gw:home", "Home Wi-Fi", now, 100, false, false)
	dm.add("gw:home", "Home Wi-Fi", now, 40, true, false)
	dm.add("gw:home", "Home Wi-Fi", now, 1000, false, true) // downloaded in free hours
	dm.add("gw:home", "Home Wi-Fi", now, 7, true, true)     // uploaded in free hours
	dm.add("gw:phone", "Phone", now, 50, false, false)
	dm.add("gw:home", "Home Wi-Fi", now.AddDate(0, 0, -1), 5000, false, false) // yesterday
	home := Quota{Network: "gw:home", Period: "day"}
	for _, c := range []struct {
		count  string
		upPaid bool
		want   int64
	}{
		{"both", false, 140}, {"down", false, 100}, {"up", false, 40}, {"both", true, 147}, {"up", true, 47},
	} {
		if got := dm.used(home, c.count, c.upPaid, now); got != c.want {
			t.Errorf("home, %s, uploads paid in free hours %v: %d, want %d", c.count, c.upPaid, got, c.want)
		}
	}
	if got := dm.used(Quota{Network: "gw:phone", Period: "day"}, "both", false, now); got != 50 {
		t.Fatalf("the phone's network: %d", got)
	}
	if got := dm.used(Quota{Period: "day"}, "both", false, now); got != 190 {
		t.Fatalf("every network: %d", got)
	}
	if got := dm.used(Quota{Network: "gw:home", Period: "month", ResetDay: 1}, "down", false, now); got != 5100 {
		t.Fatalf("this month at home: %d", got)
	}
	if nets := dm.networks(); len(nets) != 2 || nets[0].Name == "" {
		t.Fatalf("networks seen: %+v", nets)
	}
}

// D7: daily totals are kept in their own file for 13 months.
func TestDayMeterKeepsThirteenMonths(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage-days.json")
	dm := newDayMeter(file)
	now := time.Now()
	dm.add("pc", "This computer", now, 10, false, false)
	dm.add("pc", "This computer", now.AddDate(0, -13, 0), 20, false, false)
	dm.add("pc", "This computer", now.AddDate(-2, 0, 0), 30, false, false) // too old to keep
	dm.setCarryOn("q1", "day:x")
	dm.save(file)
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	back := newDayMeter(file)
	if got := back.used(Quota{Period: "day"}, "both", false, now); got != 10 {
		t.Fatalf("today after reloading: %d", got)
	}
	if d := back.day("pc", now.AddDate(0, -13, 0)); d.Down != 20 {
		t.Fatalf("13 months ago: %+v", d)
	}
	if d := back.day("pc", now.AddDate(-2, 0, 0)); d.Down != 0 {
		t.Fatalf("two years ago should be forgotten: %+v", d)
	}
	if !back.carryingOn("q1", "day:x") {
		t.Fatal("carry on wasn't kept")
	}
}

// D7: a warning at the set level, then a stop that holds downloads and uploads back, "carry on
// anyway", and a fresh allowance the next period. Bytes in free hours don't count, and a quota
// for another network doesn't hold this one back.
func TestQuotaWarnsAndStops(t *testing.T) {
	m := newTestManager(t)
	home := netInfo{key: "gw:192.168.1.1/aa:bb:cc:dd:ee:ff", name: "Home Wi-Fi"}
	m.gate.netBusy.Store(true) // (the gate's clock doesn't read the real network meanwhile)
	m.gate.net.Store(&home)
	m.mu.Lock()
	m.settings.Quota = QuotaSettings{Warn: 80, Rules: []Quota{
		{On: true, Network: home.key, NetName: home.name, Period: "day", MB: 10},
		{On: true, Network: "gw:other", NetName: "Office", Period: "day", MB: 1},
	}}
	m.normalizeSettings()
	m.applyLimit()
	id := m.settings.Quota.Rules[0].ID
	m.mu.Unlock()
	told := func(kind string) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, n := range m.notices {
			if n.Kind == kind && n.ID == id {
				return true
			}
		}
		return false
	}
	recompute := func(now time.Time) QuotasView {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.gateRecompute(now)
		return m.gate.quotas
	}
	m.countDown(5 << 20)
	if v := recompute(time.Now()); v.Show == nil || v.Show.ID != id || v.Show.Used != 5<<20 || v.Show.Warn || told("quotaWarn") {
		t.Fatalf("half used: %+v", v.Show)
	}
	if m.gate.downHeld() {
		t.Fatal("the office's quota (another network) holds this one back")
	}
	m.countUp(3 << 20)
	if v := recompute(time.Now()); !v.Show.Warn || !told("quotaWarn") || told("quotaStop") {
		t.Fatalf("80%%: %+v", v.Show)
	}
	m.gate.freeNow.Store(true) // free data hours: nothing counts
	m.countDown(50 << 20)
	if v := recompute(time.Now()); v.Show.Used != 8<<20 {
		t.Fatalf("free hours counted: %d", v.Show.Used)
	}
	m.mu.Lock()
	m.settings.Plan = PlanSettings{} // the free hours are over
	m.mu.Unlock()
	recompute(time.Now())
	m.countDown(2 << 20)
	if !m.gate.downHeld() || !m.gate.upHeld() {
		t.Fatal("used up: downloads and uploads should wait at once")
	}
	if v := recompute(time.Now()); !v.Show.Over || !told("quotaStop") {
		t.Fatalf("100%%: %+v", v.Show)
	}
	if err := m.QuotaCarryOn(id); err != nil {
		t.Fatal(err)
	}
	if v := recompute(time.Now()); m.gate.downHeld() || !v.Show.CarryOn {
		t.Fatalf("carry on anyway: held %v, %+v", m.gate.downHeld(), v.Show)
	}
	tomorrow := dayStart(time.Now(), -1).Add(time.Hour)
	if v := recompute(tomorrow); v.Show.Used != 0 || v.Show.Over || v.Show.CarryOn || m.gate.downHeld() {
		t.Fatalf("the next day: %+v", v.Show)
	}
}

func TestNetworkIdentity(t *testing.T) {
	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"wlan0\t00000000\t0101A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t010200C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0\t000200C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n"
	gw, ifName := parseProcRoute(route)
	if gw != "192.0.2.1" || ifName != "eth0" {
		t.Fatalf("default route: %s on %s", gw, ifName)
	}
	arp := "IP address       HW type     Flags       HW address            Mask     Device\n192.0.2.1        0x1         0x2         02:FC:00:00:00:05     *        eth0\n"
	mac := parseProcARP(arp, gw)
	if n := gatewayNet(gw, mac, ifName, ""); n.key != "gw:192.0.2.1/02:fc:00:00:00:05" || n.name != "eth0 · 192.0.2.1" {
		t.Fatalf("%+v", n)
	}
	if n := gatewayNet("192.168.1.1", "", "wlan0", "Home Wi-Fi"); n.key != "gw:192.168.1.1" || n.name != "Home Wi-Fi" {
		t.Fatalf("%+v", n)
	}
	if n := gatewayNet("", "", "wg0", ""); n.key != "if:wg0" {
		t.Fatalf("%+v", n)
	}
	if n := gatewayNet("", "", "", ""); n != thisComputer {
		t.Fatalf("%+v", n)
	}
	if n := netIdentify(); n.key == "" || n.name == "" { // whatever this computer is on
		t.Fatalf("%+v", n)
	}
}
