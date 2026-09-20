package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"example.com/constellation-handover"
)

func main() {
	addr := envOr("HANDOVER_ADDR", ":8080")
	dataDir := envOr("HANDOVER_DATA_DIR", "./data")
	sweepInterval := envDuration("HANDOVER_SWEEP_INTERVAL", time.Second)
	snapshotEvery := envInt("HANDOVER_SNAPSHOT_EVENTS", 200)
	defaultTimeout := envDuration("HANDOVER_DEFAULT_TIMEOUT", 90*time.Second)
	flag.Parse()

	svc, err := mission.OpenService(mission.Config{
		Dir:            dataDir,
		SnapshotEvery:  snapshotEvery,
		DefaultTimeout: defaultTimeout,
	})
	if err != nil {
		log.Fatalf("打开中枢服务失败: %v", err)
	}

	// 周期扫描执行超时：即使值班主管不在线，超时计划也会被裁定、
	// 释放资源并开立可续办处置链；重启后扫描同样能找回未完成尝试。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go runSweeper(ctx, svc, sweepInterval)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mission.NewServer(svc).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("星座交付协同中枢已启动，监听 %s，数据目录 %s", addr, dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务退出: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("收到退出信号，开始落盘关闭…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP 关闭异常: %v", err)
	}
	if err := svc.Close(); err != nil {
		log.Printf("中枢关闭异常: %v", err)
	}
	log.Println("已安全退出")
}

func runSweeper(ctx context.Context, svc *mission.Service, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			res, err := svc.SweepTimeouts()
			if err != nil {
				log.Printf("超时扫描失败: %v", err)
				continue
			}
			for _, r := range res {
				log.Printf("计划 %s 执行超时已裁定，处置单 %s", r.PlanID, r.CaseID)
			}
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
