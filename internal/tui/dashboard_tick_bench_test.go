package tui

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

func dashboardTickFixture(t testing.TB, usage bool, hosts int) dashboardModel {
	t.Helper()
	model := dashboardPerformanceFixture()
	if usage {
		model = usageFixture(t, "normal")
	}
	fleet := model.hosts
	for len(model.hosts) < hosts {
		host := fleet[len(model.hosts)%len(fleet)]
		host.Host.ID = fmt.Sprintf("host-%d", len(model.hosts))
		host.Host.MachineName = fmt.Sprintf("host-%02d", len(model.hosts))
		model.hosts = append(model.hosts, host)
	}
	model.wall = true
	return model
}

func BenchmarkDashboardRetainedTick(b *testing.B) {
	for _, usage := range []bool{false, true} {
		for _, hosts := range []int{4, 32} {
			b.Run(fmt.Sprintf("usage=%t/hosts=%d", usage, hosts), func(b *testing.B) {
				model := dashboardTickFixture(b, usage, hosts)
				start := model.now
				retained := [2]dashboardModel{model, model}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Repeat a short live window so benchmark duration cannot age away the fixture's work.
					next, _ := model.Update(dashboardTickMsg(start.Add(time.Duration(i%5) * time.Second)))
					model = next.(dashboardModel)
					retained[i%len(retained)] = model
				}
				runtime.KeepAlive(retained)
			})
		}
	}
}

func BenchmarkDashboardUsageServiceRows(b *testing.B) {
	for _, hosts := range []int{4, 32} {
		b.Run(fmt.Sprintf("hosts=%d", hosts), func(b *testing.B) {
			model := dashboardTickFixture(b, true, hosts)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows, total := model.summaryRows(true, 48)
				runtime.KeepAlive(rows)
				runtime.KeepAlive(total)
			}
		})
	}
}

func TestDashboardRetainedTickAllocationBudget(t *testing.T) {
	model := dashboardTickFixture(t, true, 4)
	start := model.now
	retained := [2]dashboardModel{model, model}
	tick := 0
	allocations := testing.AllocsPerRun(10, func() {
		next, _ := model.Update(dashboardTickMsg(start.Add(time.Duration(tick%5) * time.Second)))
		model = next.(dashboardModel)
		retained[tick%len(retained)] = model
		tick++
	})
	runtime.KeepAlive(retained)
	t.Logf("full retained tick: %.0f allocations", allocations)
	if allocations > 10000 {
		t.Fatalf("full retained tick allocated %.0f objects; budget is 10000", allocations)
	}
}

func TestDashboardRetainedTickWorkBudget(t *testing.T) {
	for _, hosts := range []int{4, 32} {
		t.Run(fmt.Sprintf("hosts=%d", hosts), func(t *testing.T) {
			model := dashboardTickFixture(t, true, hosts)
			work := dashboardRenderWork{}
			model.renderWork = &work
			next, _ := model.Update(dashboardTickMsg(model.now.Add(time.Second)))
			runtime.KeepAlive(next)
			t.Logf("tick work: %d summary builds, %d service-width host visits", work.summaries, work.serviceWidthVisits)
			if work.summaries > 1 {
				t.Errorf("built summaries %d times; budget is once per tick", work.summaries)
			}
			if work.serviceWidthVisits > hosts {
				t.Errorf("service sizing visited %d hosts; budget is one fleet scan (%d)", work.serviceWidthVisits, hosts)
			}
		})
	}
}
