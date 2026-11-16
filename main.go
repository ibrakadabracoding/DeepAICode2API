// DeepAICode2API reverse-engineers the upstream model service of the local DeepAI
// Code desktop app into an OpenAI / Anthropic compatible Go gateway.
//
// It reuses the API Key already logged in on the desktop app, so no separate key is
// needed; implemented with the pure Go standard library.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/envfile"
	"deepaicode2api/internal/logx"
	"deepaicode2api/internal/server"
	"deepaicode2api/internal/store"
)

// version is injected at build time: -ldflags "-X main.version=1.0.0"
var version = "dev"

func main() {
	// Logging comes before everything else: take over the standard library log
	// output as one JSON object per line. Existing log.Printf calls need no changes;
	// the msg field keeps the original text.
	logx.Setup(nil)

	// This must run before any environment variable is read: flag defaults (envOr)
	// and store.Open's master key read the process environment directly. Previously
	// .env was loaded lazily only in the registration pipeline, so
	// DEEPAI2API_MASTER_KEY never took effect for gateway startup (the state file
	// could only remain plaintext).
	envfile.Load()

	var (
		host       = flag.String("host", envOr("DEEPAI2API_HOST", "127.0.0.1"), "监听地址")
		port       = flag.Int("port", envInt("DEEPAI2API_PORT", 10087), "监听端口")
		dataPath   = flag.String("data", envOr("DEEPAI2API_DATA_PATH", ""), "状态文件路径（默认在可执行文件同目录）")
		deepaiDir  = flag.String("deepai-dir", envOr("DEEPAI2API_DATA_DIR", ""), "DeepAI Code 数据目录（留空自动探测）")
		importOnly = flag.Bool("import", false, "仅执行一次桌面端凭据导入后退出")
		noImport   = flag.Bool("no-import", false, "启动时不自动导入桌面端凭据")
		adminPw    = flag.String("admin-password", envOr("DEEPAI2API_ADMIN_PASSWORD", ""), "管理密码（非空时覆盖状态文件中的设置）")
		allowOpen  = flag.Bool("allow-insecure", envBool("DEEPAI2API_ALLOW_INSECURE", false), "允许非回环监听但不设管理密码（不安全）")
		seal       = flag.Bool("seal", false, "把状态/统计/日志中的存量明文封存为密文后退出（需 "+store.EnvMasterKey+"）")
		sealYes    = flag.Bool("yes", false, "配合 -seal：确认执行不可逆的封存")
		showVer    = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("deepaicode2api", version)
		return
	}

	if *dataPath == "" {
		*dataPath = defaultDataPath()
	}

	st, err := store.Open(*dataPath)
	if err != nil {
		log.Fatalf("打开状态文件失败: %v", err)
	}
	log.Printf("状态文件: %s", st.Path())
	if st.MasterKeySet() {
		log.Printf("状态文件加密: 已启用（AES-256-GCM，密钥指纹 %s，来自 %s）", st.MasterKeyFingerprint(), store.EnvMasterKey)
		if !st.Encrypted() {
			log.Printf("  磁盘上仍是明文：首次保存会自动重写；历史日志可用 -seal 立即封存。")
		}
	} else {
		log.Printf("⚠ 状态文件未加密：上游 API Key / 账号密码以明文落盘。")
		log.Printf("  把 %s=$(openssl rand -hex 32) 写进 .env 后重启即可（.env 现在会在启动时加载），"+
			"再执行 -seal 把历史明文一并封存。", store.EnvMasterKey)
	}

	if *seal {
		runSeal(st, *sealYes)
		return
	}

	machine := deepai.MachineID()
	if machine != "" {
		st.SetMachineID(machine)
		log.Printf("机器指纹: %s…（用于上游按天发放免费 Theos）", machine[:12])
	} else {
		log.Printf("⚠ 未能读取机器指纹：上游会拒绝所有模型请求（426 lane_version_unsupported）")
		log.Printf("  请设置 DEEPAI_MACHINE_ID=<原始机器UUID>；Windows 可执行：")
		log.Printf("  powershell -Command \"(Get-CimInstance Win32_ComputerSystemProduct).UUID\"")
	}

	if *importOnly {
		runImport(st, *deepaiDir)
		return
	}

	// Command-line / environment variables take precedence over the state file; persist immediately after setting.
	if *adminPw != "" {
		st.UpdateSettings(func(s *store.Settings) { s.AdminPassword = *adminPw })
		if err := st.Save(); err != nil {
			log.Printf("保存管理密码失败: %v", err)
		} else {
			log.Printf("已启用管理密码（来自命令行 / DEEPAI2API_ADMIN_PASSWORD）")
		}
	}

	if !*noImport {
		autoImport(st, *deepaiDir)
	}
	if envAcct := deepai.EnvAccount(); envAcct != nil {
		if isNew := st.UpsertAccount(envAcct); isNew {
			log.Printf("已从 DEEPAI_API_KEY 添加账号 %s", envAcct.Name)
			_ = st.Save()
		}
	}

	// Non-loopback listening means the gateway is exposed to the LAN / public
	// network, and two baselines cannot rely on "defaults" then: the admin side
	// must have a password, and the data plane must already have a key. Otherwise,
	// from startup until a key is created manually, the upstream account pool is an
	// open relay with no authentication.
	if !isLoopbackListen(*host) {
		if st.Settings().AdminPassword == "" && !*allowOpen {
			log.Fatalf("监听地址 %q 不是回环地址，但未设置管理密码：控制台会对局域网开放。\n"+
				"  请用 -admin-password 或 DEEPAI2API_ADMIN_PASSWORD 设置密码；\n"+
				"  确实需要无密码运行请显式加 -allow-insecure（或 DEEPAI2API_ALLOW_INSECURE=1）。", *host)
		}
		if !st.HasKeys() {
			k := st.AddKey("bootstrap", "default")
			if err := st.Save(); err != nil {
				log.Fatalf("生成引导密钥失败: %v", err)
			}
			log.Printf("⚠ 非回环监听且尚未签发密钥，已自动生成引导密钥（仅本次启动显示，请立即保存）：")
			log.Printf("    %s", k.Key)
			log.Printf("  之后可在控制台改名或吊销。")
		}
	}

	srv := server.New(st)
	srv.SetVersion(version)

	addr := net.JoinHostPort(*host, fmt.Sprintf("%d", *port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		// Request bodies are JSON (attachments up to 64MB go through extensions), so 60s is
		// enough; without it, slow requests could occupy connections indefinitely.
		ReadTimeout: 60 * time.Second,
		// Reclaim idle keep-alive connections; streaming responses may last several
		// minutes, so WriteTimeout is not set.
		IdleTimeout: 120 * time.Second,
	}

	// Background tasks: initial catalog fetch + periodic persistence + periodic model
	// catalog refresh. Deliberately started before listening: the initial catalog fetch
	// may hit the upstream's 20s timeout, so start it first and then start listening;
	// the two run in parallel without blocking each other.
	stopBG := make(chan struct{})
	// Lane probing: only hits the unauthenticated /models; persist on success, keep the
	// old value on failure, and do not block startup.
	go func() {
		lane, err := srv.RefreshLane(context.Background())
		if err != nil {
			log.Printf("车道探测失败（沿用 %s）: %v", srv.CurrentLane(), err)
			return
		}
		log.Printf("车道版本: %s（免鉴权 /models 探测）", lane)
	}()

	go background(st, srv, stopBG)
	go quotaGuard(srv, stopBG)

	go func() {
		// One structured summary line: things like encryption state, data path, and pool
		// size used to be scattered across multi-line Chinese logs, and machine
		// collection could only use regex. A JSON line can be grepped/jq'd directly.
		total, ready := st.AccountCounts()
		summary, _ := json.Marshal(map[string]any{
			"event":         "startup",
			"version":       version,
			"listen":        addr,
			"data":          st.Path(),
			"encrypted":     st.Encrypted(),
			"accounts":      total,
			"ready":         ready,
			"keys":          len(st.Keys()),
			"models":        len(st.Models()),
			"default_model": st.DefaultModel(),
			"machine_ready": st.MachineID() != "",
		})
		log.Printf("startup %s", summary)

		log.Printf("DeepAICode2API %s 已启动: http://%s/", version, addr)
		if st.HasKeys() {
			log.Printf("已签发 API 密钥，调用需携带 Authorization: Bearer sk-...")
		} else {
			log.Printf("尚未签发 API 密钥：仅本机回环可无密钥调用，非回环来源返回 503（请在控制台创建）")
		}
		if st.Settings().AdminPassword != "" {
			log.Printf("管理接口已启用密码校验（X-Admin-Password / Bearer）")
		} else {
			log.Printf("未设置管理密码，管理接口仅允许本机访问（可用 -admin-password 设置）")
		}
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("监听失败: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	close(stopBG)
	log.Printf("正在关闭…")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("关闭异常: %v", err)
	}
	// Wait for background probing / re-probing to persist results already written in
	// memory, then save on exit. The order matters: reversing it would let the final
	// Save be overwritten by a background Save with an older snapshot.
	srv.Close(ctx)
	if err := st.Save(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}
	log.Printf("已退出")
}

// background handles the initial model catalog fetch, periodic persistence, and
// periodic catalog refresh.
//
// The initial fetch lives here rather than running synchronously in main(): when the
// upstream is unreachable, RefreshModels waits out the full 20s timeout (in measured
// logs, the machine-fingerprint line and the started line are exactly 20 seconds
// apart), and doing it synchronously would delay "listening" as a whole. When the
// catalog is empty, /v1/models and model resolution fall back to built-in fallback
// entries (server.catalog / registry.Resolve), so listening first and filling in the
// catalog later is safe.
