package main

import "flag"

var (
	listenAddr     = flag.String("listen", "0.0.0.0:12337", "监听地址")
	targetAddr     = flag.String("target", "http://127.0.0.1:28000", "转发目标地址")
	logDir         = flag.String("logdir", "log", "日志目录")
	maxBodyLogSize = flag.Int("maxbody", 10240, "最大记录的 body 大小（字节），超过则截断")
	routesFile     = flag.String("routes", "routes.json", "模型路由配置文件路径")
)
