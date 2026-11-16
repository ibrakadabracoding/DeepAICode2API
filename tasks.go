package main

import (
	"context"
	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/server"
	"deepaicode2api/internal/store"
	"fmt"
	"log"
	"time"
)

// Background tasks and one-shot commands (import / seal).
// This group all assumes process-level scope: does not accept request-scoped ctx; exit is controlled by the stop channel.

func background(st *store.Store, srv *server.Server, stop <-chan struct{}) {
	// Initial catalog fetch: runs in parallel with listening; failure only affects the model list, not service availability.
	// Use an independent ctx: even if stop is closed, let it time out on its own without blocking the exit flow.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		models, err := srv.RefreshModels(ctx)
		cancel()
		if err != nil {
			log.Printf("拉取模型目录失败（先用缓存/兜底）: %v", err)
		} else {
			log.Printf("模型目录已更新：%d 个模型，默认 %s", len(models), st.DefaultModel())
		}
	}

	saveTick := time.NewTicker(30 * time.Second)
	defer saveTick.Stop()
	modelTick := time.NewTicker(6 * time.Hour)
	defer modelTick.Stop()

	// Connection warm-up: hit /models once per egress actually used in the account pool, completing DNS/TCP/TLS
	// (including proxy CONNECT) in advance. The first real inference request then avoids paying for a full handshake.
	//
	// Run in a separate goroutine and fail silently: warm-up is purely an optimization; it must not, just because some proxy
	// is dead, slow down listening or log errors. The total 60s budget is completely unrelated to the request path.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		start := time.Now()
		n := srv.WarmUpstream(ctx)
		if n > 0 {
			log.Printf("[warmup] 已预热 %d 个出口的上游连接（%s）", n, time.Since(start).Round(time.Millisecond))
		}
	}()

	for {
		select {
		case <-stop:
			return
		case <-saveTick.C:
			// status=error only means "the last request failed". If no new failure appears, that was historical noise,
			// change it back to ok, otherwise the badge / account list will stay gray forever (docs/changes.md §45).
			if n := st.RecoverStaleErrors(time.Now()); n > 0 {
				log.Printf("[decay] %d 个账号的错误标记已超过 %s 未复现，重置为 ok", n, deepai.ErrorStaleAfter)
			}
			if err := st.Save(); err != nil {
				log.Printf("定期保存状态失败: %v", err)
			}
		case <-modelTick.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			models, err := srv.RefreshModels(ctx)
			cancel()
			if err != nil {
				log.Printf("定期刷新模型目录失败: %v", err)
			} else {
				log.Printf("模型目录已刷新：%d 个模型", len(models))
			}
		}
	}
}

// quotaGuard is a background quota guard: re-probes in batches by refresh_at / staleness,
// no longer requiring manual clicking of "Refresh All Quotas" (a full probe, No. 653, takes 11 minutes).
//
// A separate goroutine instead of stuffing it into background: when upstream hangs, one batch takes several minutes
// (24 items / 4 concurrency × 20s timeout), which would hold up the 30s periodic save.
func quotaGuard(srv *server.Server, stop <-chan struct{}) {
	tick := time.NewTicker(server.QuotaGuardInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if n := srv.ReprobeStale(server.QuotaGuardBatch); n > 0 {
				log.Printf("[quota] 后台已复探 %d 个账号的额度", n)
			}
		}
	}
}

// autoImport attempts to import local desktop credentials at startup.
func autoImport(st *store.Store, dir string) {
	results, err := deepai.ImportAllFromDesktop(dir)
	if err != nil {
		log.Printf("未自动导入桌面端凭据（可在控制台手动导入）: %v", err)
		return
	}
	added, updated := 0, 0
	for _, res := range results {
		if st.UpsertAccount(res.Account) {
			added++
		} else {
			updated++
		}
	}
	if err := st.Save(); err != nil {
		log.Printf("保存账号失败: %v", err)
		return
	}
	for _, res := range results {
		log.Printf("已导入账号 %s（%s，username=%s）", res.Account.Name, res.Path, res.Account.Username)
	}
	log.Printf("桌面端凭据导入完成：新增 %d，更新 %d", added, updated)
}

func runImport(st *store.Store, dir string) {
	results, err := deepai.ImportAllFromDesktop(dir)
	if err != nil {
		log.Fatalf("导入失败: %v", err)
	}
	added, updated := 0, 0
	for _, res := range results {
		isNew := st.UpsertAccount(res.Account)
		state := "更新"
		if isNew {
			state = "新增"
			added++
		} else {
			updated++
		}
		fmt.Printf("%s账号 %s\n凭据文件: %s\nusername: %s\nKey: %s\n\n",
			state, res.Account.Name, res.Path, res.Account.Username, deepai.MaskSecret(res.Account.APIKey))
	}
	if err := st.Save(); err != nil {
		log.Fatalf("保存失败: %v", err)
	}
	fmt.Printf("共导入 %d 个账号（新增 %d，更新 %d）→ %s\n", len(results), added, updated, st.Path())
}

// runSeal performs a one-time seal of existing data: rewrites all plaintext in the main file, stats, and logs as ciphertext.
//
// Difference from regular Save: Save only rewrites content that changes afterward; append-only logs
// are not backfilled, so a separate -seal entry point is needed.
func runSeal(st *store.Store, yes bool) {
	if !st.MasterKeySet() {
		log.Fatalf("未设置 %s：先写入 .env 或导出环境变量（openssl rand -hex 32）\n"+
			"  注意：封存后密钥丢失即号池不可恢复。", store.EnvMasterKey)
	}
	if !yes {
		log.Fatalf("将把 %s 及其统计/日志旁路文件改写为密文（不可逆）。\n"+
			"  请先停止正在运行的网关，再执行：-seal -yes\n  密钥指纹: %s",
			st.Path(), st.MasterKeyFingerprint())
	}
	rep, err := st.Reseal()
	if err != nil {
		log.Fatalf("封存失败: %v", err)
	}
	fmt.Printf("已封存 %d 个文件（账号 %d，历史明文日志行 %d，原样保留 %d）\n",
		len(rep.Files), rep.Accounts, rep.SealedLines, rep.KeptLines)
	for _, f := range rep.Files {
		fmt.Printf("  - %s\n", f)
	}
	fmt.Printf("密钥指纹: %s\n", st.MasterKeyFingerprint())
	fmt.Printf("⚠ 务必备份 %s：丢失后号池无法解密。\n", store.EnvMasterKey)
}

// defaultDataPath returns the default state file path.
//
// Prefer the user's private config directory (on Windows %AppData%, on Unix ~/.config):
// The state file contains the upstream Key and account password; when placed in the same directory as the executable, it often ends up in
// a shared directory (Program Files, shared drive), where any user on the same machine can read it.
//
// To avoid disrupting existing deployments: if a state file already exists in the old location (the same directory as the executable), continue using it,
// otherwise the account pool will appear to "vanish into thin air".
