package main

import (
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var logger *zap.Logger

// initLogger 初始化 zap 日志，按日期切片
func initLogger(logDir string) {
	// 确保日志目录存在
	if err := os.MkdirAll(logDir, 0755); err != nil {
		panic(fmt.Sprintf("创建日志目录失败: %v", err))
	}

	// 日志文件路径
	logFile := filepath.Join(logDir, "proxy.log")

	// 配置 lumberjack 进行日志切片
	lumberJackLogger := &lumberjack.Logger{
		Filename:   logFile,
		MaxSize:    100,  // 每个日志文件最大 100MB
		MaxBackups: 30,   // 保留最近 30 个备份
		MaxAge:     30,   // 保留 30 天
		Compress:   true, // 压缩旧日志
		LocalTime:  true, // 使用本地时间
	}

	// 编码器配置
	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.TimeEncoderOfLayout("2006-01-02 15:04:05.000"),
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	// 文件输出使用 JSON 格式
	fileEncoder := zapcore.NewJSONEncoder(encoderConfig)

	// 控制台输出使用彩色格式
	consoleEncoderConfig := encoderConfig
	consoleEncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	consoleEncoder := zapcore.NewConsoleEncoder(consoleEncoderConfig)

	// 多输出：文件 + 控制台
	core := zapcore.NewTee(
		zapcore.NewCore(fileEncoder, zapcore.AddSync(lumberJackLogger), zapcore.InfoLevel),
		zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), zapcore.InfoLevel),
	)

	logger = zap.New(core, zap.AddCaller())
}
