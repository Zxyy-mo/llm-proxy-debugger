package config

import "flag"

// Config 汇聚所有命令行参数
type Config struct {
	ListenAddr     string
	TargetAddr     string
	LogDir         string
	MaxBodyLogSize int
}

// Load 解析命令行参数并返回 Config
func Load() *Config {
	cfg := &Config{}
	flag.StringVar(&cfg.ListenAddr, "listen", "0.0.0.0:12337", "监听地址")
	flag.StringVar(&cfg.TargetAddr, "target", "http://127.0.0.1:28000", "转发目标地址")
	flag.StringVar(&cfg.LogDir, "logdir", "log", "日志目录")
	flag.IntVar(&cfg.MaxBodyLogSize, "maxbody", 10240, "最大记录的 body 大小（字节），超过则截断")
	flag.Parse()
	return cfg
}
