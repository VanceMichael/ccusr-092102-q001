// 在轨交付协同中枢服务入口。
//
// 事件日志默认写入 data/mission.log（JSON Lines，每条 fsync），
// 进程重启后完整回放，未完成的处置链与等待回执的计划都会被找回。
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"example.com/constellation-handover/internal/api"
	"example.com/constellation-handover/internal/mission"
)

func main() {
	addr := flag.String("addr", ":8080", "监听地址")
	logPath := flag.String("log", "data/mission.log", "事件日志路径")
	leaseTTL := flag.Duration("lease-ttl", 3*time.Minute, "执行权默认超时（也受窗口结束时间截断）")
	simMode := flag.Bool("sim-clock", false, "开启演练时钟：接受 X-Sim-At 头驱动命令时间（生产勿开）")
	flag.Parse()

	store, _, err := mission.OpenStore(*logPath)
	if err != nil {
		log.Fatalf("打开事件日志失败: %v", err)
	}
	defer store.Close()

	clk := mission.NewSimClock()
	svc, err := mission.NewService(store, clk.Now, mission.Config{LeaseTTL: *leaseTTL})
	if err != nil {
		log.Fatalf("中枢初始化失败: %v", err)
	}
	stopSweeper := svc.StartSweeper(time.Second)
	defer stopSweeper()

	srv := &api.Server{Svc: svc, Clk: clk, SimMode: *simMode}
	log.Printf("在轨交付中枢监听 %s（事件日志 %s，执行权 TTL %s）", *addr, *logPath, *leaseTTL)
	if err := http.ListenAndServe(*addr, srv.NewRouter()); err != nil {
		log.Fatal(err)
	}
}
