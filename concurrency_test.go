package mission

import (
	"sync"
	"testing"
	"time"
)

// TestConcurrentStartSingleWinner 两个席位并发为资源重叠的计划争取执行权，
// 无论调度如何交错，只能有一项计划成功取得执行权。
func TestConcurrentStartSingleWinner(t *testing.T) {
	svc, _ := newTestService(t)
	station := "GS-RACE"
	now := svc.now()
	if err := svc.RegisterStation(station, "竞争站"); err != nil {
		t.Fatal(err)
	}
	var planIDs []string
	for i := 0; i < 8; i++ {
		sat := "RACE-" + string(rune('a'+i))
		if err := svc.RegisterSatellite(sat, sat); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ScheduleWindow(ScheduleWindowInput{ID: "w-" + sat, SatelliteID: sat, StationID: station,
			Start: now.Add(-time.Minute), End: now.Add(10 * time.Minute), Version: 1}); err != nil {
			t.Fatal(err)
		}
		ingestOK(t, svc, FrameInput{SatelliteID: sat, SourceID: "bus", SourceSequence: 1,
			SpacecraftTime: now.Add(-time.Second), ReceivedAt: now,
			Quality: QualityVerified, Data: map[string]any{"ok": true}})
		pl := makePlan(t, svc, sat, "w-"+sat, "payload_on")
		approveBoth(t, svc, pl.ID)
		planIDs = append(planIDs, pl.ID)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := map[string]bool{}
	for _, pid := range planIDs {
		wg.Add(1)
		go func(pid string) {
			defer wg.Done()
			if _, _, err := svc.StartExecution(StartExecutionInput{PlanID: pid, Operator: "op-" + pid}); err == nil {
				mu.Lock()
				winners[pid] = true
				mu.Unlock()
			}
		}(pid)
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("8 个并发计划抢占同一地面站，必须恰好 1 个胜出，实际 %d: %v", len(winners), winners)
	}
	// 唯一胜者处于执行中，其余计划未取得锁。
	executing := 0
	for _, pid := range planIDs {
		st, err := svc.CheckPlan(pid)
		if err != nil {
			t.Fatal(err)
		}
		if st.Plan.State == PlanExecuting {
			executing++
		}
		if len(st.ConflictWith) > 0 && st.Plan.State != PlanExecuting {
			// 落选者应能看到持有者。
			if st.ConflictWith[0] == "" {
				t.Fatal("冲突方信息不能为空")
			}
		}
	}
	if executing != 1 {
		t.Fatalf("执行中计划必须恰好 1 条，实际 %d", executing)
	}
}

// TestConcurrentIngestSameSequence 并发上报同一帧，只入库一次且裁决稳定。
func TestConcurrentIngestSameSequence(t *testing.T) {
	svc, _ := newTestService(t)
	sat := "RACE-TM"
	if err := svc.RegisterSatellite(sat, sat); err != nil {
		t.Fatal(err)
	}
	now := svc.now()
	in := FrameInput{SatelliteID: sat, SourceID: "bus", SourceSequence: 1,
		SpacecraftTime: now.Add(-time.Second), ReceivedAt: now,
		Quality: QualityVerified, Data: map[string]any{"v": 1.0}}
	var wg sync.WaitGroup
	var current, duplicate int
	var mu sync.Mutex
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := svc.IngestFrame(in)
			if err != nil {
				t.Errorf("并发去重上报失败: %v", err)
				return
			}
			mu.Lock()
			if r.Duplicate {
				duplicate++
			} else if r.BecameCurrent {
				current++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if current != 1 || duplicate != 15 {
		t.Fatalf("同一帧并发上报应有 1 次入库、15 次去重，实际 current=%d duplicate=%d", current, duplicate)
	}
}
