package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

var aiMutex sync.Mutex

var (
	initialTermState string
	appCtx           context.Context
	appCancel        context.CancelFunc
	appCtxMutex      sync.Mutex
)

// ═══════════════════════════════════════════════════════════════════════════════
// ANSI STYLES
// ═══════════════════════════════════════════════════════════════════════════════

const (
	Reset      = "\033[0m"
	Bold       = "\033[1m"
	Dim        = "\033[2m"
	Italic     = "\033[3m"
	Red        = "\033[31m"
	Green      = "\033[32m"
	Yellow     = "\033[33m"
	Blue       = "\033[34m"
	Magenta    = "\033[35m"
	Cyan       = "\033[36m"
	White      = "\033[37m"
	Gray       = "\033[90m"
	BgGray     = "\033[48;5;236m"
	BgDarkGray = "\033[48;5;234m"
	ClearLine  = "\033[2K"
)

// ═══════════════════════════════════════════════════════════════════════════════
// CONFIG & STATE
// ═══════════════════════════════════════════════════════════════════════════════

func getConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".axiom")
}

func getConfigPath() string {
	return filepath.Join(getConfigDir(), "config.json")
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Image   string `json:"image,omitempty"`
}

type ProviderConfig struct {
	Endpoint   string `json:"endpoint"`
	APIKey     string `json:"apiKey"`
	Model      string `json:"model"`
	ImageModel string `json:"imageModel"` // Dedicated Image Model config
}

type SavedConfig struct {
	Provider   string                    `json:"provider"`
	Configs    map[string]ProviderConfig `json:"configs"`
	MCPServers map[string][]string       `json:"mcp_servers,omitempty"`
}

var mcpErrors []string

var state = struct {
	provider   string
	configs    map[string]ProviderConfig
	connected  bool
	history    []Message
	cmdHistory []string
	startTime  time.Time
	toolCalls  int
	mcpConfig  map[string][]string
	mcpServers map[string]*MCPServer
	mcpTools   []MCPTool
	mcpToolMap map[string]string
}{
	provider:   "cloudflare",
	configs:    make(map[string]ProviderConfig),
	history:    []Message{},
	startTime:  time.Now(),
	mcpConfig:  make(map[string][]string),
	mcpServers: make(map[string]*MCPServer),
	mcpTools:   []MCPTool{},
	mcpToolMap: make(map[string]string),
}

type APIRequest struct {
	URL     string
	Headers map[string]string
	Body    interface{}
}

type ProviderDef struct {
	Name            string
	Type            string
	DefaultModel    string
	DefaultEndpoint string
	BuildRequest    func(msgs []Message, cfg ProviderConfig) APIRequest
}

var PROVIDERS map[string]ProviderDef

// ═══════════════════════════════════════════════════════════════════════════════
// MCP (MODEL CONTEXT PROTOCOL) CLIENT
// ═══════════════════════════════════════════════════════════════════════════════

type RPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type MCPServer struct {
	Name    string
	Cmd     *exec.Cmd
	Stdin   io.WriteCloser
	Stdout  io.ReadCloser
	Pending map[int]chan RPCMessage
	NextID  int
	Mutex   sync.Mutex
}

type MCPTool struct {
	ServerName  string
	Name        string
	Description string
	InputSchema string
}

func (m *MCPServer) SendRequest(ctx context.Context, method string, params interface{}, timeout time.Duration) (json.RawMessage, error) {
	m.Mutex.Lock()
	id := m.NextID
	m.NextID++
	ch := make(chan RPCMessage, 1)
	m.Pending[id] = ch
	m.Mutex.Unlock()

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	m.Stdin.Write(append(b, '\n'))

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("rpc error: %s", string(resp.Error))
		}
		return resp.Result, nil
	case <-time.After(timeout):
		m.Mutex.Lock()
		delete(m.Pending, id)
		m.Mutex.Unlock()
		return nil, fmt.Errorf("timeout waiting for %s", method)
	case <-ctx.Done():
		m.Mutex.Lock()
		delete(m.Pending, id)
		m.Mutex.Unlock()
		return nil, ctx.Err()
	}
}

func (m *MCPServer) readLoop() {
	scanner := bufio.NewScanner(m.Stdout)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	for scanner.Scan() {
		var msg RPCMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err == nil {
			if msg.ID != nil {
				m.Mutex.Lock()
				ch, ok := m.Pending[*msg.ID]
				if ok {
					delete(m.Pending, *msg.ID)
					ch <- msg
				}
				m.Mutex.Unlock()
			}
		}
	}
}

func initMCPServer(name string, command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir, _ = os.Getwd()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = nil

	server := &MCPServer{
		Name:    name,
		Cmd:     cmd,
		Stdin:   stdin,
		Stdout:  stdout,
		Pending: make(map[int]chan RPCMessage),
		NextID:  1,
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	go server.readLoop()

	initParams := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"clientInfo": map[string]interface{}{
			"name": "axiom", "version": "1.0.0",
		},
		"capabilities": map[string]interface{}{},
	}

	_, err = server.SendRequest(context.Background(), "initialize", initParams, 10*time.Second)
	if err != nil {
		return fmt.Errorf("initialize failed: %v", err)
	}

	initNotif := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	b, _ := json.Marshal(initNotif)
	server.Stdin.Write(append(b, '\n'))

	toolsRes, err := server.SendRequest(context.Background(), "tools/list", nil, 10*time.Second)
	if err != nil {
		return fmt.Errorf("tools/list failed: %v", err)
	}

	var toolsData struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(toolsRes, &toolsData); err != nil {
		return fmt.Errorf("failed to parse tools: %v", err)
	}

	for _, t := range toolsData.Tools {
		state.mcpTools = append(state.mcpTools, MCPTool{
			ServerName:  name,
			Name:        t.Name,
			Description: t.Description,
			InputSchema: string(t.InputSchema),
		})
		state.mcpToolMap[t.Name] = name
	}

	state.mcpServers[name] = server
	return nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// PAYLOAD BUILDERS
// ═══════════════════════════════════════════════════════════════════════════════

func isVisionModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "vision") || strings.Contains(m, "gpt-4o") ||
		strings.Contains(m, "claude-3") || strings.Contains(m, "gemini") ||
		strings.Contains(m, "pixtral") || strings.Contains(m, "llama-3.2")
}

func buildOpenAIMessages(msgs []Message, model string) []map[string]interface{} {
	vision := isVisionModel(model)
	var out []map[string]interface{}
	for _, m := range msgs {
		if m.Image != "" && vision {
			out = append(out, map[string]interface{}{
				"role": m.Role,
				"content": []map[string]interface{}{
					{"type": "text", "text": m.Content},
					{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + m.Image}},
				},
			})
		} else {
			out = append(out, map[string]interface{}{"role": m.Role, "content": m.Content})
		}
	}
	return out
}

func buildAnthropicMessages(msgs []Message, model string) []map[string]interface{} {
	vision := isVisionModel(model)
	var out []map[string]interface{}
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		if m.Image != "" && vision {
			out = append(out, map[string]interface{}{
				"role": m.Role,
				"content": []map[string]interface{}{
					{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": m.Image}},
					{"type": "text", "text": m.Content},
				},
			})
		} else {
			out = append(out, map[string]interface{}{"role": m.Role, "content": m.Content})
		}
	}
	return out
}

func buildGeminiMessages(msgs []Message, model string) []map[string]interface{} {
	vision := isVisionModel(model)
	var contents []map[string]interface{}
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		parts := []map[string]interface{}{{"text": m.Content}}
		if m.Image != "" && vision {
			parts = append(parts, map[string]interface{}{
				"inline_data": map[string]interface{}{"mime_type": "image/png", "data": m.Image},
			})
		}
		if len(contents) > 0 && contents[len(contents)-1]["role"] == role {
			prevParts := contents[len(contents)-1]["parts"].([]map[string]interface{})
			contents[len(contents)-1]["parts"] = append(prevParts, parts...)
		} else {
			contents = append(contents, map[string]interface{}{"role": role, "parts": parts})
		}
	}
	return contents
}

func init() {
	PROVIDERS = map[string]ProviderDef{
		"cloudflare": {
			Name: "Cloudflare", Type: "custom", DefaultModel: "@cf/nvidia/nemotron-3-120b-a12b",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				headers := map[string]string{"Content-Type": "application/json"}
				if cfg.APIKey != "" {
					headers["Authorization"] = "Bearer " + cfg.APIKey
				}
				return APIRequest{URL: cfg.Endpoint, Headers: headers, Body: map[string]interface{}{"model": cfg.Model, "messages": buildOpenAIMessages(msgs, cfg.Model)}}
			},
		},
		"openrouter": {
			Name: "OpenRouter", Type: "cloud", DefaultModel: "anthropic/claude-3.5-sonnet",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				return APIRequest{
					URL: "https://openrouter.ai/api/v1/chat/completions",
					Headers: map[string]string{
						"Authorization": "Bearer " + cfg.APIKey,
						"Content-Type":  "application/json",
						"HTTP-Referer":  "https://github.com/axiom",
						"X-Title":       "Axiom CLI",
					},
					Body: map[string]interface{}{"model": cfg.Model, "messages": buildOpenAIMessages(msgs, cfg.Model), "temperature": 0.7},
				}
			},
		},
		"google": {
			Name: "Gemini", Type: "cloud", DefaultModel: "gemini-2.5-flash",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				return APIRequest{
					URL:     "https://generativelanguage.googleapis.com/v1beta/models/" + cfg.Model + ":generateContent?key=" + cfg.APIKey,
					Headers: map[string]string{"Content-Type": "application/json"},
					Body: map[string]interface{}{
						"contents":         buildGeminiMessages(msgs, cfg.Model),
						"generationConfig": map[string]interface{}{"temperature": 0.7, "maxOutputTokens": 8192},
					},
				}
			},
		},
		"openai": {
			Name: "OpenAI", Type: "cloud", DefaultModel: "gpt-4o",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				return APIRequest{
					URL:     "https://api.openai.com/v1/chat/completions",
					Headers: map[string]string{"Authorization": "Bearer " + cfg.APIKey, "Content-Type": "application/json"},
					Body:    map[string]interface{}{"model": cfg.Model, "messages": buildOpenAIMessages(msgs, cfg.Model), "temperature": 0.7},
				}
			},
		},
		"anthropic": {
			Name: "Anthropic", Type: "cloud", DefaultModel: "claude-3-5-sonnet-20241022",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				return APIRequest{
					URL: "https://api.anthropic.com/v1/messages",
					Headers: map[string]string{
						"x-api-key": cfg.APIKey, "Content-Type": "application/json",
						"anthropic-version": "2023-06-01", "anthropic-dangerous-direct-browser-access": "true",
					},
					Body: map[string]interface{}{"model": cfg.Model, "max_tokens": 8192, "messages": buildAnthropicMessages(msgs, cfg.Model)},
				}
			},
		},
		"groq": {
			Name: "Groq", Type: "cloud", DefaultModel: "llama-3.3-70b-versatile",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				return APIRequest{
					URL:     "https://api.groq.com/openai/v1/chat/completions",
					Headers: map[string]string{"Authorization": "Bearer " + cfg.APIKey, "Content-Type": "application/json"},
					Body:    map[string]interface{}{"model": cfg.Model, "messages": buildOpenAIMessages(msgs, cfg.Model), "temperature": 0.7},
				}
			},
		},
		"ollama": {
			Name: "Ollama", Type: "local", DefaultModel: "llama3.2", DefaultEndpoint: "http://localhost:11434/v1/chat/completions",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				headers := map[string]string{"Content-Type": "application/json"}
				if cfg.APIKey != "" {
					headers["Authorization"] = "Bearer " + cfg.APIKey
				}
				return APIRequest{URL: cfg.Endpoint, Headers: headers, Body: map[string]interface{}{"model": cfg.Model, "messages": buildOpenAIMessages(msgs, cfg.Model), "stream": false}}
			},
		},
		"lmstudio": {
			Name: "LM Studio", Type: "local", DefaultModel: "local-model", DefaultEndpoint: "http://localhost:1234/v1/chat/completions",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				headers := map[string]string{"Content-Type": "application/json"}
				if cfg.APIKey != "" {
					headers["Authorization"] = "Bearer " + cfg.APIKey
				}
				return APIRequest{URL: cfg.Endpoint, Headers: headers, Body: map[string]interface{}{"model": cfg.Model, "messages": buildOpenAIMessages(msgs, cfg.Model), "temperature": 0.7}}
			},
		},
		"custom": {
			Name: "Custom (OpenAI Compatible)", Type: "custom", DefaultModel: "gpt-4o", DefaultEndpoint: "https://api.openai.com/v1/chat/completions",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				headers := map[string]string{"Content-Type": "application/json"}
				if cfg.APIKey != "" {
					headers["Authorization"] = "Bearer " + cfg.APIKey
				}
				return APIRequest{URL: cfg.Endpoint, Headers: headers, Body: map[string]interface{}{"model": cfg.Model, "messages": buildOpenAIMessages(msgs, cfg.Model), "temperature": 0.7}}
			},
		},
	}
}

func loadConfig() error {
	data, err := os.ReadFile(getConfigPath())
	if err != nil {
		return err
	}
	var saved SavedConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		return err
	}
	if saved.Provider != "" {
		state.provider = saved.Provider
	}
	if saved.Configs != nil {
		state.configs = saved.Configs
		if _, ok := state.configs[state.provider]; ok {
			state.connected = true
		}
	}
	if saved.MCPServers != nil {
		state.mcpConfig = saved.MCPServers
		for name, cmdArgs := range saved.MCPServers {
			if len(cmdArgs) > 0 {
				err := initMCPServer(name, cmdArgs)
				if err != nil {
					mcpErrors = append(mcpErrors, fmt.Sprintf("%s: %v", name, err))
				}
			}
		}
	}
	return nil
}

func saveConfig() error {
	os.MkdirAll(getConfigDir(), 0755)
	data, _ := json.MarshalIndent(SavedConfig{
		Provider:   state.provider,
		Configs:    state.configs,
		MCPServers: state.mcpConfig,
	}, "", "  ")
	return os.WriteFile(getConfigPath(), data, 0600)
}

// ═══════════════════════════════════════════════════════════════════════════════
// UI COMPONENTS
// ═══════════════════════════════════════════════════════════════════════════════

func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

func getTermWidth() int {
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err == nil {
		parts := strings.Fields(string(out))
		if len(parts) >= 2 {
			if w, err := strconv.Atoi(parts[1]); err == nil {
				return w
			}
		}
	}
	return 80
}

func stripAnsi(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAllString(s, "")
}

func renderMarkdown(text string) string {
	codeBlockRe := regexp.MustCompile("(?s)```(\\w*)\\n(.*?)```")
	text = codeBlockRe.ReplaceAllStringFunc(text, func(match string) string {
		parts := codeBlockRe.FindStringSubmatch(match)
		return renderCodeBlock(parts[2], parts[1])
	})

	inlineCodeRe := regexp.MustCompile("`([^`]+)`")
	text = inlineCodeRe.ReplaceAllString(text, BgDarkGray+Cyan+"$1"+Reset)
	boldRe := regexp.MustCompile(`\*\*([^*]+)\*\*`)
	text = boldRe.ReplaceAllString(text, Bold+"$1"+Reset)
	italicRe := regexp.MustCompile(`\*([^*]+)\*`)
	text = italicRe.ReplaceAllString(text, Italic+"$1"+Reset)
	bulletRe := regexp.MustCompile(`(?m)^(\s*)[-*] (.+)$`)
	text = bulletRe.ReplaceAllString(text, "$1"+Cyan+"•"+Reset+" $2")
	return text
}

func renderCodeBlock(code, lang string) string {
	lines := strings.Split(strings.TrimRight(code, "\n"), "\n")
	var result strings.Builder

	result.WriteString("\n" + Gray + "  ┌─")
	if lang != "" {
		result.WriteString(" " + lang + " ")
	}
	result.WriteString("─────────────────────────────────────────" + Reset + "\n")

	for _, line := range lines {
		result.WriteString(Gray + "  │ " + Reset + line + "\n")
	}

	result.WriteString(Gray + "  └──────────────────────────────────────────────" + Reset + "\n")
	return result.String()
}

func printLogo() {
	logo := `
   ` + Cyan + Bold + `▄▀▄ ▀▄▀ █ ▄▀▄ █▄ ▄█` + Reset + `
   ` + Cyan + `█▀█ ▄▀▄ █ ▀▄▀ █ ▀ █` + Reset + `
`
	fmt.Print(logo)
	fmt.Println(Gray + "   AI Bot" + Reset)
	fmt.Println()
}

func printStatusBar() {
	width := getTermWidth()
	cwd, _ := os.Getwd()
	cwd = filepath.Base(cwd)

	providerName := "disconnected"
	providerColor := Red
	model := ""

	if state.connected {
		if p, ok := PROVIDERS[state.provider]; ok {
			providerName = strings.ToLower(p.Name)
			providerColor = Green
			cfg := state.configs[state.provider]
			model = cfg.Model
			if model == "" {
				model = p.DefaultModel
			}
			if len(model) > 25 {
				model = model[:22] + "..."
			}
		}
	}

	left := fmt.Sprintf(" %s%s%s", Cyan, cwd, Reset)
	right := fmt.Sprintf("%s%s●%s %s", providerColor, Bold, Reset, providerName)
	if model != "" {
		right += fmt.Sprintf(" %s│%s %s", Gray, Reset, model)
	}
	right += " "

	leftLen := utf8.RuneCountInString(stripAnsi(left))
	rightLen := utf8.RuneCountInString(stripAnsi(right))
	padding := width - leftLen - rightLen
	if padding < 0 {
		padding = 0
	}

	fmt.Printf("%s%s%s%s%s%s\n", BgGray, left, strings.Repeat(" ", padding), right, Reset, "")
}

func printFileDiff(filename string, oldLines, newLines []string, startLine int) {
	fmt.Printf("\n  %s%s─── %s%s\n", Gray, Bold, filename, Reset)
	maxOld, maxNew := 3, 5

	shown := 0
	for i, line := range oldLines {
		if shown >= maxOld {
			fmt.Printf("  %s- ... +%d lines removed%s\n", Red+Dim, len(oldLines)-maxOld, Reset)
			break
		}
		fmt.Printf("  %s%d │- %s%s\n", Red+Dim, startLine+i, line, Reset)
		shown++
	}

	shown = 0
	for i, line := range newLines {
		if shown >= maxNew {
			fmt.Printf("  %s+ ... +%d lines added%s\n", Green+Dim, len(newLines)-maxNew, Reset)
			break
		}
		fmt.Printf("  %s%d │+ %s%s\n", Green, startLine+i, line, Reset)
		shown++
	}
}

func printToolCall(name string, args map[string]interface{}, result map[string]interface{}) {
	icon := "●"
	color := Yellow
	switch name {
	case "read_file":
		icon, color = "◉", Blue
	case "write_file", "edit_lines":
		icon, color = "◈", Green
	case "take_screenshot":
		icon, color = "📷", Magenta
	case "generate_image":
		icon, color = "🎨", Magenta
	case "shell":
		icon, color = "▶", Yellow
	}

	if _, isMcp := state.mcpToolMap[name]; isMcp {
		icon, color = "🔌", Cyan
	}

	fmt.Printf("\n  %s%s%s %s%s%s", color, icon, Reset, Bold, name, Reset)

	if filename, ok := args["filename"].(string); ok {
		fmt.Printf(" %s%s%s", Gray, filename, Reset)
	}
	if promptStr, ok := args["prompt"].(string); ok {
		if len(promptStr) > 30 {
			promptStr = promptStr[:27] + "..."
		}
		fmt.Printf(" %s%s%s", Gray, promptStr, Reset)
	}
	if cmd, ok := args["command"].(string); ok {
		if len(cmd) > 40 {
			cmd = cmd[:37] + "..."
		}
		fmt.Printf(" %s%s%s", Gray, cmd, Reset)
	}
	if msg, ok := args["message"].(string); ok {
		if len(msg) > 30 {
			msg = msg[:27] + "..."
		}
		fmt.Printf(" %s%s%s", Gray, msg, Reset)
	}

	if _, ok := result["error"]; ok {
		fmt.Printf(" %s✗ %s%s\n", Red, result["error"], Reset)
	} else if size, ok := result["size_bytes"]; ok {
		fmt.Printf(" %s✓ (%v bytes)%s\n", Green, size, Reset)
	} else {
		fmt.Printf(" %s✓%s\n", Green, Reset)
	}

	if stdout, ok := result["stdout"].(string); ok && strings.TrimSpace(stdout) != "" {
		fmt.Print(renderCodeBlock(strings.TrimSpace(stdout), "stdout"))
	}
	if stderr, ok := result["stderr"].(string); ok && strings.TrimSpace(stderr) != "" {
		fmt.Print(renderCodeBlock(strings.TrimSpace(stderr), "stderr"))
	}
	if mcpRes, ok := result["mcp_result"].(string); ok && strings.TrimSpace(mcpRes) != "" {
		fmt.Print(renderCodeBlock(strings.TrimSpace(mcpRes), "mcp"))
	}
}

func printAssistantMessage(content string) {
	fmt.Println()
	rendered := renderMarkdown(content)
	lines := strings.Split(rendered, "\n")
	for _, line := range lines {
		if line == "" {
			fmt.Println()
		} else {
			fmt.Println("  " + line)
		}
	}
	fmt.Println()
}

func printError(msg string) {
	fmt.Printf("\n  %s✗ %s%s\n", Red, msg, Reset)
}

func printSuccess(msg string) {
	fmt.Printf("\n  %s✓%s %s\n", Green, Reset, msg)
}

func printSpinner(done chan bool, message string) {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	i := 0
	for {
		select {
		case <-done:
			fmt.Printf("\r%s\r", ClearLine)
			return
		default:
			fmt.Printf("\r  %s%s%s %s", Cyan, frames[i%len(frames)], Reset, Dim+message+Reset)
			time.Sleep(80 * time.Millisecond)
			i++
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// TERMINAL INPUT & SIGNAL HANDLING
// ═══════════════════════════════════════════════════════════════════════════════

var fallbackReader *bufio.Reader

func initTermState() {
	if runtime.GOOS != "windows" {
		cmd := exec.Command("stty", "-g")
		cmd.Stdin = os.Stdin
		out, err := cmd.Output()
		if err == nil {
			initialTermState = strings.TrimSpace(string(out))
		}
	}
}

func setupSignals() {
	var globalLastSig time.Time
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range c {
			appCtxMutex.Lock()
			cancel := appCancel
			appCtxMutex.Unlock()

			if cancel != nil {
				cancel()
			} else {
				if !globalLastSig.IsZero() && time.Since(globalLastSig) < 2*time.Second {
					restoreMode(initialTermState)
					fmt.Println()
					os.Exit(0)
				}
				globalLastSig = time.Now()
				fmt.Print("\r\n  \033[33m(Press Ctrl+C again to exit)\033[0m\r\n\033[36m❯\033[0m ")
			}
		}
	}()
}

func setRawMode() (string, error) {
	if runtime.GOOS == "windows" {
		return "", fmt.Errorf("stty not supported on windows")
	}
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	// -isig explicitly turns off signal generation so readLine handles byte 3 directly
	cmd2 := exec.Command("stty", "-icanon", "-echo", "-isig")
	cmd2.Stdin = os.Stdin
	cmd2.Run()
	return strings.TrimSpace(string(out)), nil
}

func restoreMode(state string) {
	if state == "" || runtime.GOOS == "windows" {
		return
	}
	cmd := exec.Command("stty", state)
	cmd.Stdin = os.Stdin
	cmd.Run()
}

func readLine(prompt string, history []string) string {
	state, err := setRawMode()
	if err != nil {
		fmt.Print(prompt)
		if fallbackReader == nil {
			fallbackReader = bufio.NewReader(os.Stdin)
		}
		text, _ := fallbackReader.ReadString('\n')
		return strings.TrimRight(text, "\r\n")
	}
	defer restoreMode(state)

	termWidth := getTermWidth()
	if termWidth < 10 {
		termWidth = 80
	}
	promptLen := utf8.RuneCountInString(stripAnsi(prompt))

	var buf []rune
	cursor := 0
	histIdx := len(history)

	var prevLineCount int
	var cursorLines int
	var lastCtrlC time.Time

	redraw := func() {
		if prevLineCount > 0 {
			fmt.Printf("\033[%dA", prevLineCount)
		}
		fmt.Print("\r\033[J")

		text := prompt + string(buf)
		fmt.Print(text)

		totalLen := promptLen + len(buf)
		if totalLen > 0 {
			prevLineCount = (totalLen - 1) / termWidth
		} else {
			prevLineCount = 0
		}

		cursorPos := promptLen + cursor
		if cursorPos > 0 {
			cursorLines = (cursorPos - 1) / termWidth
		} else {
			cursorLines = 0
		}

		if prevLineCount > cursorLines {
			fmt.Printf("\033[%dA", prevLineCount-cursorLines)
		}

		cursorCol := cursorPos
		if cursorLines > 0 {
			cursorCol = cursorPos - cursorLines*termWidth
		}
		if cursorCol > 0 {
			fmt.Printf("\r\033[%dC", cursorCol)
		} else {
			fmt.Print("\r")
		}
	}

	redraw()

	b := make([]byte, 1024)
	for {
		n, err := os.Stdin.Read(b)
		if err != nil || n == 0 {
			return string(buf)
		}

		i := 0
		for i < n {
			c := b[i]

			if c == 3 { // Ctrl+C
				if !lastCtrlC.IsZero() && time.Since(lastCtrlC) < 2*time.Second {
					restoreMode(initialTermState)
					fmt.Println()
					os.Exit(0)
				}
				lastCtrlC = time.Now()
				buf = []rune{}
				cursor = 0
				prevLineCount = 0
				cursorLines = 0
				fmt.Print("\r\n  \033[33m(Press Ctrl+C again to exit)\033[0m\r\n")
				redraw()
				i++
				continue
			} else if c == 4 { // Ctrl+D
				if len(buf) == 0 {
					if prevLineCount > cursorLines {
						fmt.Printf("\033[%dB", prevLineCount-cursorLines)
					}
					fmt.Println()
					restoreMode(initialTermState)
					os.Exit(0)
				}
				i++
			} else if c == 13 || c == 10 { // Enter
				if prevLineCount > cursorLines {
					fmt.Printf("\033[%dB", prevLineCount-cursorLines)
				}
				fmt.Println()
				return string(buf)
			} else if c == 12 { // Ctrl+L
				clearScreen()
				prevLineCount = 0
				cursorLines = 0
				redraw()
				i++
			} else if c == 127 || c == 8 { // Backspace
				if cursor > 0 {
					buf = append(buf[:cursor-1], buf[cursor:]...)
					cursor--
					redraw()
				}
				i++
			} else if c == 27 { // ESC
				if i+2 < n && b[i+1] == '[' {
					if b[i+2] >= 'A' && b[i+2] <= 'D' {
						seq := b[i+2]
						i += 3
						switch seq {
						case 'A':
							if histIdx > 0 {
								histIdx--
								buf = []rune(history[histIdx])
								cursor = len(buf)
								redraw()
							}
						case 'B':
							if histIdx < len(history)-1 {
								histIdx++
								buf = []rune(history[histIdx])
								cursor = len(buf)
								redraw()
							} else if histIdx == len(history)-1 {
								histIdx++
								buf = []rune{}
								cursor = 0
								redraw()
							}
						case 'C':
							if cursor < len(buf) {
								cursor++
								redraw()
							}
						case 'D':
							if cursor > 0 {
								cursor--
								redraw()
							}
						}
						continue
					}
					if i+3 < n && b[i+2] == '3' && b[i+3] == '~' { // Delete Key
						if cursor < len(buf) {
							buf = append(buf[:cursor], buf[cursor+1:]...)
							redraw()
						}
						i += 4
						continue
					}
					i = n
				} else {
					fmt.Println()
					return ""
				}
			} else {
				r, size := utf8.DecodeRune(b[i:n])
				if r != utf8.RuneError || size > 1 {
					if strconv.IsPrint(r) {
						buf = append(buf[:cursor], append([]rune{r}, buf[cursor:]...)...)
						cursor++
						redraw()
					}
					i += size
				} else {
					if strconv.IsPrint(rune(c)) {
						buf = append(buf[:cursor], append([]rune{rune(c)}, buf[cursor:]...)...)
						cursor++
						redraw()
					}
					i++
				}
			}
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// OS TOOLS & MCP ROUTER
// ═══════════════════════════════════════════════════════════════════════════════

func takeScreenshot(ctx context.Context, filename string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		ps := `Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; $s =[System.Windows.Forms.SystemInformation]::VirtualScreen; $b = New-Object System.Drawing.Bitmap $s.Width, $s.Height; $g = [System.Drawing.Graphics]::FromImage($b); $g.CopyFromScreen($s.Left, $s.Top, 0, 0, $b.Size); $b.Save('` + filename + `');`
		cmd = exec.CommandContext(ctx, "powershell", "-Command", ps)
	case "darwin":
		cmd = exec.CommandContext(ctx, "screencapture", "-x", filename)
	default: // Linux
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			if _, err := exec.LookPath("grim"); err == nil {
				cmd = exec.CommandContext(ctx, "grim", filename)
			} else if _, err := exec.LookPath("hyprshot"); err == nil {
				cmd = exec.CommandContext(ctx, "hyprshot", "-m", "output", "-s", "-f", filename)
			}
		}
		if cmd == nil {
			if _, err := exec.LookPath("gnome-screenshot"); err == nil {
				cmd = exec.CommandContext(ctx, "gnome-screenshot", "-f", filename)
			} else if _, err := exec.LookPath("scrot"); err == nil {
				cmd = exec.CommandContext(ctx, "scrot", filename)
			} else {
				return fmt.Errorf("No screenshot tool found.")
			}
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Execution failed: %v | Output: %s", err, string(out))
	}
	return nil
}

func runTool(ctx context.Context, name string, args map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	defer func() {
		if r := recover(); r != nil {
			result["error"] = fmt.Sprintf("%v", r)
		}
	}()

	getString := func(k string) string { v, _ := args[k].(string); return v }
	getInt := func(k string) int {
		if v, ok := args[k].(float64); ok {
			return int(v)
		}
		if v, ok := args[k].(string); ok {
			if i, err := strconv.Atoi(v); err == nil {
				return i
			}
		}
		return -1
	}

	switch name {
	case "take_screenshot":
		filename := "screenshot_" + time.Now().Format("20060102_150405") + ".png"
		err := takeScreenshot(ctx, filename)
		if err != nil {
			result["error"] = fmt.Sprintf("Screenshot failed: %v", err)
		} else {
			info, err := os.Stat(filename)
			if err != nil || info.Size() == 0 {
				result["error"] = "Empty or missing image."
			} else {
				result["success"] = true
				result["filename"] = filename
				result["size_bytes"] = info.Size()
				result["_image_file"] = filename
			}
		}

	case "generate_image":
		promptStr := getString("prompt")
		modelStr := getString("model")
		if promptStr == "" || modelStr == "" {
			result["error"] = "prompt and model are required parameters"
			break
		}

		cfg, ok := state.configs[state.provider]
		if !ok || cfg.Endpoint == "" {
			result["error"] = "No API endpoint configured for current provider. Run /config to set it up."
			break
		}

		url := strings.ReplaceAll(cfg.Endpoint, "{model}", modelStr)

		reqBody, _ := json.Marshal(map[string]interface{}{
			"prompt": promptStr,
			"model":  modelStr,
		})

		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
		if err != nil {
			result["error"] = err.Error()
			break
		}

		req.Header.Set("Content-Type", "application/json")
		if cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}

		client := &http.Client{Timeout: 90 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			result["error"] = err.Error()
			break
		}
		defer resp.Body.Close()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			result["error"] = err.Error()
			break
		}

		if len(bodyBytes) > 0 && bodyBytes[0] == '{' {
			var jsonRes map[string]interface{}
			if err := json.Unmarshal(bodyBytes, &jsonRes); err == nil {
				if success, ok := jsonRes["success"].(bool); ok && !success {
					result["error"] = fmt.Sprintf("API Error: %s", string(bodyBytes))
					break
				}
				if resMap, ok := jsonRes["result"].(map[string]interface{}); ok {
					if b64img, ok := resMap["image"].(string); ok {
						if dec, err := base64.StdEncoding.DecodeString(b64img); err == nil {
							bodyBytes = dec
						}
					}
				} else if b64img, ok := jsonRes["image"].(string); ok {
					if dec, err := base64.StdEncoding.DecodeString(b64img); err == nil {
						bodyBytes = dec
					}
				}
			}
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			result["error"] = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
			break
		}

		fileName := fmt.Sprintf("generated_image_%s.png", time.Now().Format("20060102_150405"))
		err = os.WriteFile(fileName, bodyBytes, 0644)
		if err != nil {
			result["error"] = err.Error()
		} else {
			result["success"] = true
			result["filename"] = fileName
			result["size_bytes"] = len(bodyBytes)
			result["_image_file"] = fileName
		}

	case "list_files":
		var files []string
		filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
			if err != nil || (info.IsDir() && info.Name() != "." && strings.HasPrefix(info.Name(), ".")) {
				return filepath.SkipDir
			}
			if !info.IsDir() && !strings.HasPrefix(info.Name(), ".") {
				files = append(files, path)
			}
			return nil
		})
		result["files"] = files

	case "search_file":
		filename := getString("filename")
		query := strings.ToLower(getString("query"))
		content, err := os.ReadFile(filename)
		if err != nil {
			result["error"] = "File not found"
			break
		}
		lines := strings.Split(string(content), "\n")
		var results []string
		for i, line := range lines {
			if strings.Contains(strings.ToLower(line), query) {
				results = append(results, fmt.Sprintf("%d | %s", i+1, line))
			}
		}
		if len(results) == 0 {
			result["error"] = "No matches"
		} else {
			result["matches"] = strings.Join(results, "\n")
		}

	case "read_file":
		filename := getString("filename")
		start, end := getInt("start_line"), getInt("end_line")
		content, err := os.ReadFile(filename)
		if err != nil {
			result["error"] = "File not found"
			break
		}
		lines := strings.Split(string(content), "\n")
		if start < 1 {
			start = 1
		}
		if end < 1 || end > len(lines) {
			end = len(lines)
		}
		var numbered []string
		for i := start - 1; i < end && i < len(lines); i++ {
			numbered = append(numbered, fmt.Sprintf("%d | %s", i+1, lines[i]))
		}
		result["content"] = strings.Join(numbered, "\n")

	case "write_file":
		filename := getString("filename")
		content := getString("content")
		os.MkdirAll(filepath.Dir(filename), 0755)
		err := os.WriteFile(filename, []byte(content), 0644)
		if err != nil {
			result["error"] = err.Error()
		} else {
			result["success"] = true
		}

	case "edit_lines":
		filename := getString("filename")
		start := getInt("start_line") - 1
		end := getInt("end_line")
		replaceWith := getString("replace_with")

		content, err := os.ReadFile(filename)
		if err != nil {
			result["error"] = "File not found"
			break
		}
		lines := strings.Split(string(content), "\n")
		if start < 0 || end > len(lines) || start > end {
			result["error"] = "Invalid range"
			break
		}

		oldLines := lines[start:end]
		replacementLines := strings.Split(replaceWith, "\n")
		newContent := append(lines[:start], append(replacementLines, lines[end:]...)...)

		err = os.WriteFile(filename, []byte(strings.Join(newContent, "\n")), 0644)
		if err != nil {
			result["error"] = err.Error()
		} else {
			result["success"] = true
			result["old_lines"] = oldLines
			result["new_lines"] = replacementLines
			result["start_line"] = start + 1
			result["filename"] = filename
		}

	case "shell":
		command := getString("command")
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "cmd", "/C", command)
		} else {
			cmd = exec.CommandContext(ctx, "sh", "-c", command)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		cmd.Dir, _ = os.Getwd()
		err := cmd.Run()
		result["stdout"] = stdout.String()
		result["stderr"] = stderr.String()
		if err != nil {
			result["exit_code"] = 1
		} else {
			result["exit_code"] = 0
			result["success"] = true
		}

	default:
		if serverName, ok := state.mcpToolMap[name]; ok {
			server := state.mcpServers[serverName]
			res, err := server.SendRequest(ctx, "tools/call", map[string]interface{}{
				"name":      name,
				"arguments": args,
			}, 30*time.Second)

			if err != nil {
				result["error"] = err.Error()
			} else {
				var callRes struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
					IsError bool `json:"isError"`
				}
				json.Unmarshal(res, &callRes)

				output := ""
				for _, c := range callRes.Content {
					if c.Type == "text" {
						output += c.Text + "\n"
					}
				}
				if callRes.IsError {
					result["error"] = output
				} else {
					result["mcp_result"] = output
					result["success"] = true
				}
			}
		} else {
			result["error"] = "Unknown tool: " + name
		}
	}

	return result
}

// ═══════════════════════════════════════════════════════════════════════════════
// AI LOGIC
// ═══════════════════════════════════════════════════════════════════════════════

func parseAIResponse(data map[string]interface{}, rawString string) string {
	if result, ok := data["result"].(map[string]interface{}); ok {
		if _, hasChoices := result["choices"]; hasChoices {
			data = result
		} else if respContent, hasResp := result["response"].(string); hasResp {
			return respContent
		}
	}

	if choices, ok := data["choices"].([]interface{}); ok && len(choices) > 0 {
		choice := choices[0].(map[string]interface{})
		if message, ok := choice["message"].(map[string]interface{}); ok {
			if toolCalls, ok := message["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
				tc := toolCalls[0].(map[string]interface{})
				funcObj, hasFunc := tc["function"].(map[string]interface{})
				name := ""
				args := interface{}("")
				if hasFunc {
					name, _ = funcObj["name"].(string)
					args = funcObj["arguments"]
				} else {
					name, _ = tc["name"].(string)
					args = tc["arguments"]
				}
				jsn, _ := json.Marshal(map[string]interface{}{"tool": name, "args": args})
				return string(jsn)
			}
			content, _ := message["content"].(string)
			return strings.TrimSpace(content)
		}
	}

	if cands, ok := data["candidates"].([]interface{}); ok && len(cands) > 0 {
		if content, ok := cands[0].(map[string]interface{})["content"].(map[string]interface{}); ok {
			if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
				if text, ok := parts[0].(map[string]interface{})["text"].(string); ok {
					return text
				}
			}
		}
	}

	if resp, ok := data["response"].(string); ok {
		return resp
	}
	if text, ok := data["text"].(string); ok {
		return text
	}
	return rawString
}

func runAI() {
	maxIter := 25
	p := PROVIDERS[state.provider]
	cfg := state.configs[state.provider]

	fullCfg := ProviderConfig{
		APIKey:   cfg.APIKey,
		Model:    cfg.Model,
		Endpoint: cfg.Endpoint,
	}
	if fullCfg.Model == "" {
		fullCfg.Model = p.DefaultModel
	}
	if fullCfg.Endpoint == "" {
		fullCfg.Endpoint = p.DefaultEndpoint
	}

	ctx, cancel := context.WithCancel(context.Background())
	appCtxMutex.Lock()
	appCtx = ctx
	appCancel = cancel
	appCtxMutex.Unlock()

	defer func() {
		appCtxMutex.Lock()
		appCtx = nil
		appCancel = nil
		appCtxMutex.Unlock()
	}()

	for iter := 0; iter < maxIter; iter++ {
		done := make(chan bool)
		go printSpinner(done, "thinking...")

		reqData := p.BuildRequest(state.history, fullCfg)
		bodyBytes, _ := json.Marshal(reqData.Body)
		req, _ := http.NewRequestWithContext(ctx, "POST", reqData.URL, bytes.NewBuffer(bodyBytes))
		for k, v := range reqData.Headers {
			req.Header.Set(k, v)
		}

		client := &http.Client{Timeout: 120 * time.Second}
		resp, err := client.Do(req)

		done <- true
		time.Sleep(50 * time.Millisecond)

		if err != nil {
			if errors.Is(err, context.Canceled) {
				printError("Process canceled by user.")
			} else {
				printError(fmt.Sprintf("Request failed: %v", err))
			}
			break
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			printError(fmt.Sprintf("API Error %d: %s", resp.StatusCode, string(respBody)))
			break
		}

		var data map[string]interface{}
		json.Unmarshal(respBody, &data)

		content := parseAIResponse(data, string(respBody))

		if content != "" {
			var toolCmd map[string]interface{}
			jsonStr := ""

			toolMatch := regexp.MustCompile(`(?is)\x60\x60\x60(?:json)?\s*(\{[\s\S]*?\})\s*\x60\x60\x60`).FindStringSubmatch(content)
			if len(toolMatch) > 1 {
				jsonStr = toolMatch[1]
			} else if strings.Contains(content, "\"tool\"") && strings.Contains(content, "{") {
				start := strings.Index(content, "{")
				end := strings.LastIndex(content, "}")
				if start > -1 && end > start {
					jsonStr = content[start : end+1]
				}
			}

			if jsonStr != "" {
				err := json.Unmarshal([]byte(jsonStr), &toolCmd)
				if err != nil {
					toolCmd = make(map[string]interface{})
					unescapeStr := func(str string) string {
						str = strings.ReplaceAll(str, "\\n", "\n")
						str = strings.ReplaceAll(str, "\\\"", "\"")
						return str
					}
					if strings.Contains(jsonStr, `"take_screenshot"`) {
						toolCmd = map[string]interface{}{"tool": "take_screenshot", "args": map[string]interface{}{}}
					} else if strings.Contains(jsonStr, `"shell"`) {
						cmdMatch := regexp.MustCompile(`"command"\s*:\s*"([^"]+)"`).FindStringSubmatch(jsonStr)
						if len(cmdMatch) > 1 {
							toolCmd = map[string]interface{}{"tool": "shell", "args": map[string]interface{}{"command": unescapeStr(cmdMatch[1])}}
						}
					}
				}
			}

			if toolCmd != nil && len(toolCmd) > 0 {
				tName, _ := toolCmd["tool"].(string)
				if tName == "" {
					tName, _ = toolCmd["name"].(string)
				}

				var tArgs interface{} = toolCmd["args"]
				if tArgs == nil {
					tArgs = toolCmd["arguments"]
				}

				if tName != "" && tArgs != nil {
					if argsMap, ok := tArgs.(map[string]interface{}); ok {
						state.toolCalls++
						result := runTool(ctx, tName, argsMap)
						printToolCall(tName, argsMap, result)

						if tName == "edit_lines" {
							if oldLines, ok := result["old_lines"].([]string); ok {
								if newLines, ok := result["new_lines"].([]string); ok {
									if filename, ok := result["filename"].(string); ok {
										if startLine, ok := result["start_line"].(int); ok {
											printFileDiff(filename, oldLines, newLines, startLine)
										}
									}
								}
							}
						}

						var b64Img string
						if imgFile, ok := result["_image_file"].(string); ok {
							delete(result, "_image_file")
							imgData, err := os.ReadFile(imgFile)
							if err == nil {
								b64Img = base64.StdEncoding.EncodeToString(imgData)
							}
						}

						resBytes, _ := json.Marshal(result)
						state.history = append(state.history, Message{Role: "assistant", Content: content})

						msg := Message{
							Role:    "user",
							Content: fmt.Sprintf("Tool result: %s\n\nContinue or summarize.", string(resBytes)),
						}

						if b64Img != "" {
							msg.Image = b64Img
						}

						state.history = append(state.history, msg)
						continue
					}
				}
			}

			printAssistantMessage(content)
			state.history = append(state.history, Message{Role: "assistant", Content: content})
		} else {
			printError("Empty response from API")
		}
		break
	}
}

func getSystemPrompt() string {
	cwd, _ := os.Getwd()

	imgModel := "@cf/black-forest-labs/flux-2-klein-9b"
	if cfg, ok := state.configs[state.provider]; ok && cfg.ImageModel != "" {
		imgModel = cfg.ImageModel
	}

	prompt := fmt.Sprintf(`You are Axiom, an elite, highly capable multimodal AI agent.
WORKSPACE: %s
OS: %s

CRITICAL DIRECTIVES:
1. YOU HAVE FULL INTERNET ACCESS via 'shell'.
2. DO NOT claim you are in a sandbox.
3. Keep responses brutally concise and direct.

TOOLS (respond with JSON in code blocks):

1. **list_files**
   `+"```json\n"+`{"tool": "list_files", "args": {}}`+"\n```"+`
2. **read_file**
   `+"```json\n"+`{"tool": "read_file", "args": {"filename": "main.go", "start_line": 1, "end_line": 50}}`+"\n```"+`
3. **search_file**
   `+"```json\n"+`{"tool": "search_file", "args": {"filename": "main.go", "query": "func main"}}`+"\n```"+`
4. **write_file**
   `+"```json\n"+`{"tool": "write_file", "args": {"filename": "hello.py", "content": "print('hi')"}}`+"\n```"+`
5. **edit_lines** (inclusive, 1-indexed)
   `+"```json\n"+`{"tool": "edit_lines", "args": {"filename": "main.go", "start_line": 10, "end_line": 12, "replace_with": "newCode();"}}`+"\n```"+`
6. **shell**
   `+"```json\n"+`{"tool": "shell", "args": {"command": "curl -s 'https://api.coingecko.com/api/v3/simple/price?ids=bitcoin&vs_currencies=usd'"}}`+"\n```"+`
7. **take_screenshot** (Take a picture of the user's screen and look at it!)
   `+"```json\n"+`{"tool": "take_screenshot", "args": {}}`+"\n```"+`
8. **generate_image** (Generate an image dynamically using the active API endpoint)
   `+"```json\n"+`{"tool": "generate_image", "args": {"prompt": "A cyberpunk cat", "model": "%s"}}`+"\n```", cwd, runtime.GOOS, imgModel)

	if len(state.mcpTools) > 0 {
		prompt += "\n\nEXTERNAL MCP Tools (Available via standard JSON tool calls):\n"
		for _, t := range state.mcpTools {
			prompt += fmt.Sprintf("- **%s** (from %s)\n  Description: %s\n  Schema: %s\n", t.Name, t.ServerName, t.Description, t.InputSchema)
		}
		prompt += "\nUsage for MCP Tools:\n```json\n{\"tool\": \"tool_name\", \"args\": {\"param1\": \"value\"}}\n```\n"
	}

	return prompt
}

func sendMessage(text string) {
	if text == "" {
		return
	}
	if !state.connected {
		printError("No provider configured. Run /config")
		return
	}

	aiMutex.Lock()
	defer aiMutex.Unlock()

	state.cmdHistory = append(state.cmdHistory, text)

	if len(state.history) == 0 {
		state.history = append(state.history, Message{Role: "user", Content: "SYSTEM: " + getSystemPrompt()})
		state.history = append(state.history, Message{Role: "assistant", Content: "Ready!"})
	}

	state.history = append(state.history, Message{Role: "user", Content: text})
	runAI()
}

// ═══════════════════════════════════════════════════════════════════════════════
// COMMANDS
// ═══════════════════════════════════════════════════════════════════════════════

func handleConfig() {
	fmt.Println()
	fmt.Printf("  %sPROVIDERS%s\n\n", Bold, Reset)
	fmt.Printf("  %sCloud:%s    cloudflare google openai anthropic groq openrouter\n", Dim, Reset)
	fmt.Printf("  %sLocal:%s    ollama lmstudio custom\n", Dim, Reset)
	fmt.Println()

	prompt := fmt.Sprintf("  %sProvider%s [%s]: ", Bold, Reset, state.provider)
	p := strings.TrimSpace(readLine(prompt, nil))
	if p == "" {
		p = state.provider
	}

	if _, ok := PROVIDERS[p]; !ok {
		printError("Invalid provider: " + p)
		return
	}
	state.provider = p
	prov := PROVIDERS[p]
	cfg := state.configs[p]

	if prov.Type == "local" || prov.Type == "custom" {
		defaultEp := prov.DefaultEndpoint
		if cfg.Endpoint != "" {
			defaultEp = cfg.Endpoint
		}
		prompt := fmt.Sprintf("  %sEndpoint%s [%s]: ", Bold, Reset, defaultEp)
		ep := strings.TrimSpace(readLine(prompt, nil))
		if ep != "" {
			cfg.Endpoint = ep
		} else if cfg.Endpoint == "" {
			cfg.Endpoint = prov.DefaultEndpoint
		}
	}

	if prov.Type != "local" || p == "ollama" || p == "lmstudio" || p == "custom" {
		keyHint := "not set"
		if cfg.APIKey != "" {
			keyHint = "****" + cfg.APIKey[max(0, len(cfg.APIKey)-4):]
		}
		prompt := fmt.Sprintf("  %sAPI Key%s [%s]: ", Bold, Reset, keyHint)
		k := strings.TrimSpace(readLine(prompt, nil))
		if k != "" {
			cfg.APIKey = k
		}
	}

	defaultModel := prov.DefaultModel
	if cfg.Model != "" {
		defaultModel = cfg.Model
	}
	promptM := fmt.Sprintf("  %sChat Model%s [%s]: ", Bold, Reset, defaultModel)
	m := strings.TrimSpace(readLine(promptM, nil))
	if m != "" {
		cfg.Model = m
	} else if cfg.Model == "" {
		cfg.Model = prov.DefaultModel
	}

	defaultImgModel := "@cf/black-forest-labs/flux-2-klein-9b"
	if cfg.ImageModel != "" {
		defaultImgModel = cfg.ImageModel
	}
	promptImg := fmt.Sprintf("  %sImage Model%s [%s]: ", Bold, Reset, defaultImgModel)
	imgM := strings.TrimSpace(readLine(promptImg, nil))
	if imgM != "" {
		cfg.ImageModel = imgM
	} else if cfg.ImageModel == "" {
		cfg.ImageModel = defaultImgModel
	}

	state.configs[p] = cfg
	state.connected = true
	saveConfig()

	state.history = []Message{}

	printSuccess(fmt.Sprintf("Connected to %s (%s)", prov.Name, cfg.Model))
	printStatusBar()
}

func handleStatus() {
	fmt.Println()
	printStatusBar()
	if !state.connected {
		fmt.Printf("  %sStatus:%s %sDisconnected%s\n", Bold, Reset, Red, Reset)
		return
	}
	prov := PROVIDERS[state.provider]
	cfg := state.configs[state.provider]
	model := cfg.Model
	if model == "" {
		model = prov.DefaultModel
	}
	elapsed := time.Since(state.startTime).Truncate(time.Second)

	fmt.Printf("\n  %sProvider%s      %s%s%s\n", Dim, Reset, Green, prov.Name, Reset)
	fmt.Printf("  %sChat Model%s    %s\n", Dim, Reset, model)
	if cfg.ImageModel != "" {
		fmt.Printf("  %sImage Model%s   %s\n", Dim, Reset, cfg.ImageModel)
	}
	fmt.Printf("  %sSession%s       %s\n", Dim, Reset, elapsed)
	fmt.Printf("  %sMessages%s      %d\n", Dim, Reset, len(state.history))

	fmt.Printf("  %sMCP Servers%s   %d\n", Dim, Reset, len(state.mcpServers))
	if len(state.mcpServers) > 0 {
		var names []string
		for k := range state.mcpServers {
			names = append(names, k)
		}
		fmt.Printf("  %sActive MCP%s    %s (%d tools)\n", Dim, Reset, strings.Join(names, ", "), len(state.mcpTools))
	}

	fmt.Println()
}

func printHelp() {
	fmt.Println()
	fmt.Printf("  %s%sCOMMANDS%s\n\n", Bold, Cyan, Reset)
	commands := [][]string{
		{"/config", "Configure AI provider"},
		{"/status", "Show session & MCP connection details"},
		{"/clear", "Clear conversation history"},
		{"/exit", "Exit Axiom"},
	}
	for _, cmd := range commands {
		fmt.Printf("  %s%-12s%s %s\n", Yellow, cmd[0], Reset, Dim+cmd[1]+Reset)
	}
	fmt.Println()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func printWelcome() {
	clearScreen()
	printLogo()
	if state.connected {
		printStatusBar()
	} else {
		fmt.Printf("  %sType %s/config%s to get started.%s\n\n", Dim, Yellow, Dim, Reset)
	}

	for _, errStr := range mcpErrors {
		printError("MCP Error: " + errStr)
	}
	if len(state.mcpServers) > 0 {
		fmt.Printf("\n  %s✓ Connected to %d MCP servers (%d tools)%s\n\n", Green, len(state.mcpServers), len(state.mcpTools), Reset)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// MAIN
// ═══════════════════════════════════════════════════════════════════════════════

func main() {
	initTermState()
	defer restoreMode(initialTermState)
	setupSignals()
	loadConfig()
	printWelcome()

	for {
		prompt := fmt.Sprintf("%s❯%s ", Cyan, Reset)
		input := strings.TrimSpace(readLine(prompt, state.cmdHistory))

		if input == "" {
			continue
		}

		switch {
		case input == "/exit", input == "/quit", input == "/q":
			fmt.Printf("\n  %sGoodbye!%s\n\n", Dim, Reset)
			return
		case input == "/help", input == "/?":
			printHelp()
		case input == "/config":
			handleConfig()
		case input == "/clear", input == "/reset":
			state.history = []Message{}
			state.toolCalls = 0
			printSuccess("Conversation cleared")
		case input == "/status":
			handleStatus()
		case strings.HasPrefix(input, "/"):
			printError("Unknown command. Try /help")
		default:
			sendMessage(input)
		}
	}
}
