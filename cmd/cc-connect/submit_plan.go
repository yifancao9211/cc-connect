package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
)

func runSubmitPlan(args []string) {
	var project, sessionKey, dataDir, plan string
	var useStdin bool

	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project", "-p":
			if i+1 < len(args) {
				i++
				project = args[i]
			}
		case "--session", "-s":
			if i+1 < len(args) {
				i++
				sessionKey = args[i]
			}
		case "--stdin":
			useStdin = true
		case "--data-dir":
			if i+1 < len(args) {
				i++
				dataDir = args[i]
			}
		case "--help", "-h":
			printSubmitPlanUsage()
			return
		default:
			positional = append(positional, args[i])
		}
	}

	if useStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading stdin: %v\n", err)
			os.Exit(1)
		}
		plan = strings.TrimSpace(string(data))
	}
	if plan == "" {
		plan = strings.Join(positional, " ")
	}
	if plan == "" {
		fmt.Fprintln(os.Stderr, "Error: plan text is required")
		printSubmitPlanUsage()
		os.Exit(1)
	}

	// Auto-detect session key from environment (injected by cc-connect)
	if sessionKey == "" {
		sessionKey = os.Getenv("CC_SESSION_KEY")
	}

	sockPath := resolveSocketPath(dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: cc-connect is not running (socket not found: %s)\n", sockPath)
		os.Exit(1)
	}

	payload, _ := json.Marshal(map[string]string{
		"project":     project,
		"session_key": sessionKey,
		"plan":        plan,
	})

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	resp, err := client.Post("http://unix/submit-plan", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	fmt.Println("Plan submitted for approval.")
}

func printSubmitPlanUsage() {
	fmt.Println(`Usage: cc-connect submit-plan [options] <plan text>
       cc-connect submit-plan [options] --stdin < file

Submit a plan for admin approval before executing code changes.

Options:
  -p, --project <name>     Target project (optional if only one project)
  -s, --session <key>      Session key (auto-detected from CC_SESSION_KEY if not set)
      --stdin              Read plan from stdin
      --data-dir <path>    Data directory (default: ~/.cc-connect)
  -h, --help               Show this help

Examples:
  cc-connect submit-plan "1. 重构 Email 模块 2. 添加白名单校验"
  cc-connect submit-plan --stdin <<'EOF'
    1. 补充邮件接收流程
    2. 添加发件人白名单校验
    3. 完善邮件发送实现
  EOF`)
}
