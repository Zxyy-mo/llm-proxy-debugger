package config

import (
	"flag"
	"path/filepath"
)

// Config 汇聚所有命令行参数
type Config struct {
	ListenAddr     string
	TargetAddr     string
	LogDir         string
	MaxBodyLogSize int
	DataFile       string
	Insecure       bool
	WebDir         string
}

// Load 解析命令行参数并返回 Config
func Load() *Config {
	cfg := &Config{}
	flag.StringVar(&cfg.ListenAddr, "listen", "127.0.0.1:12337", "监听地址")
	flag.StringVar(&cfg.TargetAddr, "target", "http://127.0.0.1:28000", "转发目标地址")
	flag.StringVar(&cfg.LogDir, "logdir", "log", "日志目录")
	flag.IntVar(&cfg.MaxBodyLogSize, "maxbody", 10240, "最大记录的 body 大小（字节），超过则截断")
	flag.StringVar(&cfg.DataFile, "data", "", "SQLite 元数据文件，默认 logdir/gateway.db；- 表示仅内存")
	flag.BoolVar(&cfg.Insecure, "insecure", false, "允许不可信上游 TLS 证书（仅本地调试）")
	flag.StringVar(&cfg.WebDir, "web", "web/dist", "前端构建目录，提供 /ui/ 页面；- 表示禁用")
	flag.Parse()
	if cfg.DataFile == "" {
		cfg.DataFile = filepath.Join(cfg.LogDir, "gateway.db")
	}
	return cfg
}
