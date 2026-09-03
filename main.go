package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	cfg := LoadConfig()
	log := NewLogger(cfg.LogLevel)

	uuidBytes, err := uuidToBytes(cfg.UUID)
	if err != nil {
		log.Error("uuid error:", err.Error())
		os.Exit(1)
	}

	printBanner(cfg)

	if cfg.EnableTun {
		tunSrv := NewTunServer(cfg, log, uuidBytes)
		if err := tunSrv.ListenAndServe(); err != nil {
			log.Error("fatal:", err.Error())
			os.Exit(1)
		}
		return
	}

	srv := NewProxyServer(cfg, log, uuidBytes)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("fatal:", err.Error())
		os.Exit(1)
	}
}

func printBanner(cfg *Config) {
	const w = 60
	bar := strings.Repeat("═", w)
	row := func(s string) string {
		if len(s) < w-2 {
			s = s + strings.Repeat(" ", w-2-len(s))
		}
		return "║  " + s + "║"
	}
	fmt.Printf("\x1b[36m╔%s╗\n", bar)
	fmt.Println(row("VLESS WebSocket Client (Go) — READY"))
	fmt.Printf("╠%s╣\n", bar)
	fmt.Println(row(fmt.Sprintf("Server   : %s", cfg.ServerURL())))
	fmt.Println(row(fmt.Sprintf("UUID     : %s", cfg.UUID)))
	tokState := "disabled"
	if cfg.Token != "" {
		tokState = "enabled"
	}
	fmt.Println(row(fmt.Sprintf("Token    : %s", tokState)))
	if cfg.EnableTun {
		fmt.Println(row(fmt.Sprintf("Mode     : TUN (网卡 %s, 地址 %s)", cfg.TunName, cfg.TunAddr4)))
	} else {
		fmt.Println(row(fmt.Sprintf("Local    : %s (SOCKS5 + HTTP(S) 共用端口)", cfg.ListenAddr())))
	}
	if cfg.SNI != "" || cfg.WSHost != "" {
		fmt.Println(row(fmt.Sprintf("SNI      : %s", cfg.EffectiveSNI())))
		fmt.Println(row(fmt.Sprintf("WS Host  : %s", cfg.EffectiveWSHost())))
	}
	fmt.Println(row(fmt.Sprintf("Retries  : %d次 (base %dms) / KeepAlive %ds", cfg.ConnectRetries, cfg.RetryBaseMs, cfg.KeepAliveSec)))
	fmt.Println(row(fmt.Sprintf("Insecure : %v", cfg.Insecure)))
	fmt.Printf("╚%s╝\x1b[0m\n\n", bar)
}
