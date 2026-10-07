// Command scenario 列出可用市场状态，并把同一批标的放到一个或多个
// 市场状态下并发跑估值对比。
//
// 用法：
//
//	go run ./cmd/scenario list
//	go run ./cmd/scenario run -states normal,liquidity-crunch,price-gap
//	go run ./cmd/scenario run -states normal,ghost -workers 8 -budget 2000000
//
// 指定了不存在的状态时直接报错退出（错误中列出可用状态），
// 不会悄悄退回任何默认状态。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/highcumontoa/concurrent-option-pricing-go/engine"
	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
	"github.com/highcumontoa/concurrent-option-pricing-go/scenario"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "list":
		cmdList()
	case "run":
		cmdRun(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法:
  scenario list                      列出全部可用市场状态
  scenario run -states a,b,c [flags] 多状态并发对比估值

run 参数:
  -states    逗号分隔的市场状态名（必填，必须全部存在）
  -workers   跨状态共享并发上限（默认 4）
  -budget    整批蒙特卡洛路径总预算（默认不限）
  -paths     每个 MC 请求的路径数（默认 100000）
  -timeout   每个状态各自的超时，如 2s（默认不限）
  -no-degrade 关闭方法不适用/未收敛时的确定性降级`)
}

func cmdList() {
	reg := scenario.DefaultRegistry()
	fmt.Println("可用市场状态:")
	for _, s := range reg.List() {
		fmt.Printf("  %-18s %s\n", s.Name, s.Description)
	}
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	stateArg := fs.String("states", "", "逗号分隔的市场状态名")
	workers := fs.Int("workers", 4, "跨状态共享并发上限")
	budget := fs.Int64("budget", 0, "整批蒙特卡洛路径总预算")
	paths := fs.Int("paths", 100000, "每个 MC 请求的路径数")
	timeout := fs.Duration("timeout", 0, "每个状态各自的超时")
	noDegrade := fs.Bool("no-degrade", false, "关闭确定性降级")
	_ = fs.Parse(args)

	if strings.TrimSpace(*stateArg) == "" {
		fmt.Fprintln(os.Stderr, "run: 必须用 -states 显式指定至少一个市场状态")
		os.Exit(2)
	}
	names := []string{}
	for _, n := range strings.Split(*stateArg, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	reg := scenario.DefaultRegistry()
	cfg := scenario.RunConfig{
		MaxWorkers:      *workers,
		PathBudget:      *budget,
		PerReqPaths:     *paths,
		Timeout:         *timeout,
		Degrade:         !*noDegrade,
		StateCacheLimit: 4096,
	}
	rn := scenario.NewRunner(reg, cfg)
	rep, err := rn.Run(context.Background(), names, demoRequests())
	if err != nil {
		if errors.Is(err, scenario.ErrUnknownState) {
			fmt.Fprintf(os.Stderr, "拒绝执行: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "运行失败: %v\n", err)
		os.Exit(1)
	}
	printReport(rep)
}

// demoRequests 是命令行自带的一组示例标的，覆盖三种估值方法。
func demoRequests() []engine.Request {
	mk := func(id string, right marketdata.Right, style pricing.Style, K, T float64, m pricing.Method) engine.Request {
		return engine.Request{
			ID:     id,
			Spec:   pricing.Spec{Right: right, Style: style, Strike: K, Expiry: T},
			Method: m,
			Seed:   20261007,
			Paths:  200000,
		}
	}
	return []engine.Request{
		mk("ATM-C-1y", marketdata.Call, pricing.European, 100, 1.0, pricing.Analytic),
		mk("ITM-P-0.5y", marketdata.Put, pricing.European, 110, 0.5, pricing.Binomial),
		mk("OTM-C-1.5y", marketdata.Call, pricing.European, 110, 1.5, pricing.MonteCarlo),
		mk("US-PUT-1y", marketdata.Put, pricing.American, 100, 1.0, pricing.Analytic),
	}
}

func printReport(rep *scenario.Report) {
	header := []string{"instrument"}
	header = append(header, rep.States...)
	fmt.Println(strings.Join(header, " | "))
	for _, row := range rep.Rows {
		cols := []string{row.Instrument}
		for _, c := range row.Cells {
			switch c.Outcome {
			case scenario.OutcomePriced:
				cols = append(cols, fmt.Sprintf("%.4f", c.Price))
			case scenario.OutcomeDegraded:
				cols = append(cols, fmt.Sprintf("%.4f(degraded:%s)", c.Price, c.Reason))
			default:
				cols = append(cols, "REJECTED:"+c.Reason.String())
			}
		}
		fmt.Println(strings.Join(cols, " | "))
	}
	fmt.Println()
	for _, d := range rep.Diag {
		status := "OK"
		if !d.LoadOK {
			status = "LOAD-FAIL:" + d.LoadReason.String()
		}
		fmt.Printf("状态 %-18s %s  priced=%d degraded=%d rejected=%d cached=%d\n",
			d.Name, status, d.Priced, d.Degraded, d.Rejected, d.CachedResults)
		if !d.LoadOK {
			fmt.Printf("    数据问题: %s\n", d.LoadDetail)
		}
	}
}
