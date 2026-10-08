// Command config demonstrates pkg/core/config:
//
//   - a YAML file decoded into a struct via json tags
//   - tag semantics: default: / env: / optional
//   - ${VAR} and ${VAR:default} expansion
//   - env override wins over the file value
//   - missing required keys are an error
//
// Run:
//
//	go run ./examples/config
//	DB_DSN='postgres://localhost/demo' go run ./examples/config   # env 覆盖文件
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/smallnest/matex/pkg/core/config"
)

// Config mirrors a slice of a real config file. Field names resolve from
// json tags, falling back to the lowercased field name.
type Config struct {
	HTTP struct {
		Addr    string        `json:"addr" default:":8080"`
		Timeout time.Duration `json:"timeout" default:"10s"`
	} `json:"http"`

	DB struct {
		DSN          string `json:"dsn" env:"DB_DSN"` // env 优先于文件
		MaxOpenConns int    `json:"max_open_conns" default:"16"`
		MaxIdleConns int    `json:"max_idle_conns" optional:""` // 可缺省 → 零值
	} `json:"db"`

	Features []string `json:"features" default:"a,b"` // "a,b" 或 YAML 列表
	Debug    bool     `json:"debug" default:"false"`
}

func main() {
	conf := flag.String("conf", "examples/config/config.yaml", "config file")
	flag.Parse()

	var cfg Config
	if err := config.Load(*conf, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
	fmt.Printf("conf      = %s\n", *conf)
	fmt.Printf("http      = %s (timeout %s)\n", cfg.HTTP.Addr, cfg.HTTP.Timeout)
	fmt.Printf("db        = %s (max_open=%d max_idle=%d)\n", cfg.DB.DSN, cfg.DB.MaxOpenConns, cfg.DB.MaxIdleConns)
	fmt.Printf("features  = %v\n", cfg.Features)
	fmt.Printf("debug     = %v\n", cfg.Debug)

	// 没有 default 也没有 optional 的 key 缺失时会报错——配置错误在启动
	// 时就该炸，而不是等到运行时才 nil panic。
	var strict struct {
		Required string `json:"required"`
	}
	err := config.Parse([]byte("other: 1"), &strict)
	fmt.Printf("\n缺少必填 key：%v\n", err)

	// 也可以直接解析一段 YAML（不落盘），测试里很常用。
	var inline struct {
		Name string `json:"name" default:"fallback"`
	}
	_ = config.Parse([]byte("name: from-bytes"), &inline)
	fmt.Printf("inline    = %s\n", inline.Name)
}
