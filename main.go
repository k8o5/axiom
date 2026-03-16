package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Global mutex to prevent background bot messages from colliding with CLI typing
var aiMutex sync.Mutex

// ═══════════════════════════════════════════════════════════════════════════════
// ANSI STYLES
// ═══════════════════════════════════════════════════════════════════════════════

const (
	Reset      = "\033[0m"
	Bold       = "\033[1m"
	Dim        = "\033[2m"
	Italic     = "\033[3m"
	Underline  = "\033[4m"
	Black      = "\033[30m"
	Red        = "\033[31m"
	Green      = "\033[32m"
	Yellow     = "\033[33m"
	Blue       = "\033[34m"
	Magenta    = "\033[35m"
	Cyan       = "\033[36m"
	White      = "\033[37m"
	Gray       = "\033[90m"
	BrightCyan = "\033[96m"
	BgBlack    = "\033[40m"
	BgRed      = "\033[41m"
	BgGreen    = "\033[42m"
	BgYellow   = "\033[43m"
	BgBlue     = "\033[44m"
	BgMagenta  = "\033[45m"
	BgCyan     = "\033[46m"
	BgGray     = "\033[48;5;236m"
	BgDarkGray = "\033[48;5;234m"
	ClearLine  = "\033[2K"
	CursorUp   = "\033[1A"
	HideCursor = "\033[?25l"
	ShowCursor = "\033[?25h"
)

// Syntax highlighting colors (using standard ANSI for perfect terminal compatibility)
const (
	SynKeyword = Magenta
	SynString  = Green
	SynNumber  = Yellow
	SynComment = Gray
	SynFunc    = Cyan
	SynType    = Cyan
)

// ═══════════════════════════════════════════════════════════════════════════════
// CONFIG
// ═══════════════════════════════════════════════════════════════════════════════

func getConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".axiom")
}

func getConfigPath() string {
	return filepath.Join(getConfigDir(), "config.json")
}

// ═══════════════════════════════════════════════════════════════════════════════
// STATE & TYPES
// ═══════════════════════════════════════════════════════════════════════════════

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ProviderConfig struct {
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"apiKey"`
	Model    string `json:"model"`
}

type BotConfig struct {
	TelegramToken    string `json:"telegram_token"`
	DiscordAppID     string `json:"discord_app_id"`
	DiscordPublicKey string `json:"discord_public_key"`
	DiscordBotToken  string `json:"discord_bot_token"`
	WhatsAppToken    string `json:"whatsapp_token"`
	WhatsAppPhoneID  string `json:"whatsapp_phone_id"`
	WhatsAppVerify   string `json:"whatsapp_verify"`
	ServerPort       string `json:"server_port"`
}

type SavedConfig struct {
	Provider string                    `json:"provider"`
	Configs  map[string]ProviderConfig `json:"configs"`
	Bots     BotConfig                 `json:"bots"`
}

var state = struct {
	provider   string
	configs    map[string]ProviderConfig
	bots       BotConfig
	connected  bool
	history    []Message
	cmdHistory []string
	startTime  time.Time
	toolCalls  int
	lastCost   float64
	tokens     int
}{
	provider:  "google",
	configs:   make(map[string]ProviderConfig),
	history:   []Message{},
	startTime: time.Now(),
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

func init() {
	PROVIDERS = map[string]ProviderDef{
		"google": {
			Name: "Gemini", Type: "cloud", DefaultModel: "gemini-2.0-flash-exp",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				var contents []map[string]interface{}
				for _, m := range msgs {
					role := "user"
					if m.Role == "assistant" {
						role = "model"
					}
					if len(contents) > 0 && contents[len(contents)-1]["role"] == role {
						parts := contents[len(contents)-1]["parts"].([]map[string]interface{})
						parts[0]["text"] = parts[0]["text"].(string) + "\n\n" + m.Content
					} else {
						contents = append(contents, map[string]interface{}{
							"role":  role,
							"parts": []map[string]interface{}{{"text": m.Content}},
						})
					}
				}
				return APIRequest{
					URL:     "https://generativelanguage.googleapis.com/v1beta/models/" + cfg.Model + ":generateContent?key=" + cfg.APIKey,
					Headers: map[string]string{"Content-Type": "application/json"},
					Body: map[string]interface{}{
						"contents":         contents,
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
					Body:    map[string]interface{}{"model": cfg.Model, "messages": msgs, "temperature": 0.7},
				}
			},
		},
		"anthropic": {
			Name: "Anthropic", Type: "cloud", DefaultModel: "claude-sonnet-4-20250514",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				filtered := []Message{}
				for _, m := range msgs {
					if m.Role != "system" {
						filtered = append(filtered, m)
					}
				}
				return APIRequest{
					URL: "https://api.anthropic.com/v1/messages",
					Headers: map[string]string{
						"x-api-key": cfg.APIKey, "Content-Type": "application/json",
						"anthropic-version": "2023-06-01", "anthropic-dangerous-direct-browser-access": "true",
					},
					Body: map[string]interface{}{"model": cfg.Model, "max_tokens": 8192, "messages": filtered},
				}
			},
		},
		"mistral": {
			Name: "Mistral", Type: "cloud", DefaultModel: "mistral-large-latest",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				return APIRequest{
					URL:     "https://api.mistral.ai/v1/chat/completions",
					Headers: map[string]string{"Authorization": "Bearer " + cfg.APIKey, "Content-Type": "application/json"},
					Body:    map[string]interface{}{"model": cfg.Model, "messages": msgs, "temperature": 0.7},
				}
			},
		},
		"groq": {
			Name: "Groq", Type: "cloud", DefaultModel: "llama-3.3-70b-versatile",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				return APIRequest{
					URL:     "https://api.groq.com/openai/v1/chat/completions",
					Headers: map[string]string{"Authorization": "Bearer " + cfg.APIKey, "Content-Type": "application/json"},
					Body:    map[string]interface{}{"model": cfg.Model, "messages": msgs, "temperature": 0.7},
				}
			},
		},
		"together": {
			Name: "Together", Type: "cloud", DefaultModel: "meta-llama/Meta-Llama-3.1-70B-Instruct-Turbo",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				return APIRequest{
					URL:     "https://api.together.xyz/v1/chat/completions",
					Headers: map[string]string{"Authorization": "Bearer " + cfg.APIKey, "Content-Type": "application/json"},
					Body:    map[string]interface{}{"model": cfg.Model, "messages": msgs, "temperature": 0.7},
				}
			},
		},
		"openrouter": {
			Name: "OpenRouter", Type: "cloud", DefaultModel: "anthropic/claude-3.5-sonnet",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				return APIRequest{
					URL:     "https://openrouter.ai/api/v1/chat/completions",
					Headers: map[string]string{"Authorization": "Bearer " + cfg.APIKey, "Content-Type": "application/json"},
					Body:    map[string]interface{}{"model": cfg.Model, "messages": msgs},
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
				return APIRequest{URL: cfg.Endpoint, Headers: headers, Body: map[string]interface{}{"model": cfg.Model, "messages": msgs, "stream": false}}
			},
		},
		"lmstudio": {
			Name: "LM Studio", Type: "local", DefaultModel: "local-model", DefaultEndpoint: "http://localhost:1234/v1/chat/completions",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				return APIRequest{URL: cfg.Endpoint, Headers: map[string]string{"Content-Type": "application/json"}, Body: map[string]interface{}{"model": cfg.Model, "messages": msgs, "stream": false}}
			},
		},
		"llamacpp": {
			Name: "llama.cpp", Type: "local", DefaultModel: "default", DefaultEndpoint: "http://localhost:8080/v1/chat/completions",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				return APIRequest{URL: cfg.Endpoint, Headers: map[string]string{"Content-Type": "application/json"}, Body: map[string]interface{}{"model": cfg.Model, "messages": msgs, "stream": false}}
			},
		},
		"cloudflare": {
			Name: "Cloudflare", Type: "custom", DefaultModel: "@cf/meta/llama-3.1-8b-instruct",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				headers := map[string]string{"Content-Type": "application/json"}
				if cfg.APIKey != "" {
					headers["Authorization"] = "Bearer " + cfg.APIKey
				}
				return APIRequest{URL: cfg.Endpoint, Headers: headers, Body: map[string]interface{}{"model": cfg.Model, "messages": msgs}}
			},
		},
		"custom": {
			Name: "Custom", Type: "custom",
			BuildRequest: func(msgs []Message, cfg ProviderConfig) APIRequest {
				headers := map[string]string{"Content-Type": "application/json"}
				if cfg.APIKey != "" {
					headers["Authorization"] = "Bearer " + cfg.APIKey
				}
				return APIRequest{URL: cfg.Endpoint, Headers: headers, Body: map[string]interface{}{"model": cfg.Model, "messages": msgs}}
			},
		},
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// CONFIG PERSISTENCE
// ═══════════════════════════════════════════════════════════════════════════════

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
	state.bots = saved.Bots
	return nil
}

func saveConfig() error {
	os.MkdirAll(getConfigDir(), 0755)
	data, _ := json.MarshalIndent(SavedConfig{Provider: state.provider, Configs: state.configs, Bots: state.bots}, "", "  ")
	return os.WriteFile(getConfigPath(), data, 0600)
}

// ═══════════════════════════════════════════════════════════════════════════════
// BOT INTEGRATIONS (TELEGRAM, DISCORD, WHATSAPP)
// ═══════════════════════════════════════════════════════════════════════════════

func StartBots() {
	if state.bots.TelegramToken != "" {
		go runTelegramPoller()
	}
	if state.bots.ServerPort != "" {
		go startBotServer()
	}
}

func runTelegramPoller() {
	offset := 0
	for {
		url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=20", state.bots.TelegramToken, offset)
		resp, err := http.Get(url)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		var result struct {
			Ok     bool `json:"ok"`
			Result []struct {
				UpdateID int `json:"update_id"`
				Message  struct {
					Chat struct {
						ID int64 `json:"id"`
					} `json:"chat"`
					Text string `json:"text"`
				} `json:"message"`
			} `json:"result"`
		}
		json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()

		for _, u := range result.Result {
			offset = max(offset, u.UpdateID+1)
			if u.Message.Text != "" {
				go sendMessageFromPlatform("telegram", fmt.Sprintf("%d", u.Message.Chat.ID), u.Message.Text)
			}
		}
	}
}

func startBotServer() {
	port := state.bots.ServerPort
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/discord", handleDiscordWebhook)
	http.HandleFunc("/whatsapp", handleWhatsAppWebhook)
	go http.ListenAndServe(":"+port, nil)
}

func handleDiscordWebhook(w http.ResponseWriter, r *http.Request) {
	sigHex := r.Header.Get("X-Signature-Ed25519")
	ts := r.Header.Get("X-Signature-Timestamp")

	if sigHex == "" || ts == "" {
		http.Error(w, "missing headers", 401)
		return
	}

	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	pubKey, err1 := hex.DecodeString(state.bots.DiscordPublicKey)
	sig, err2 := hex.DecodeString(sigHex)

	if err1 != nil || err2 != nil || len(pubKey) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		http.Error(w, "invalid crypto", 401)
		return
	}

	msg := append([]byte(ts), body...)
	if !ed25519.Verify(pubKey, msg, sig) {
		http.Error(w, "invalid signature", 401)
		return
	}

	var payload map[string]interface{}
	json.Unmarshal(body, &payload)

	if t, _ := payload["type"].(float64); t == 1 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":1}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"type":5}`)) // Defer response

	go func() {
		token, _ := payload["token"].(string)
		data, _ := payload["data"].(map[string]interface{})
		options, _ := data["options"].([]interface{})
		text := ""
		if len(options) > 0 {
			opt := options[0].(map[string]interface{})
			text, _ = opt["value"].(string)
		}
		if text != "" {
			sendMessageFromPlatform("discord", token, text)
		}
	}()
}

func handleWhatsAppWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		q := r.URL.Query()
		if q.Get("hub.verify_token") == state.bots.WhatsAppVerify {
			w.Write([]byte(q.Get("hub.challenge")))
		} else {
			http.Error(w, "invalid verify token", 401)
		}
		return
	}

	var payload map[string]interface{}
	json.NewDecoder(r.Body).Decode(&payload)

	var text, from string
	entries, _ := payload["entry"].([]interface{})
	if len(entries) > 0 {
		if entry, ok := entries[0].(map[string]interface{}); ok {
			if changes, _ := entry["changes"].([]interface{}); len(changes) > 0 {
				if change, ok := changes[0].(map[string]interface{}); ok {
					if val, ok := change["value"].(map[string]interface{}); ok {
						if msgs, _ := val["messages"].([]interface{}); len(msgs) > 0 {
							if msg, ok := msgs[0].(map[string]interface{}); ok {
								from, _ = msg["from"].(string)
								if txtObj, ok := msg["text"].(map[string]interface{}); ok {
									text, _ = txtObj["body"].(string)
								}
							}
						}
					}
				}
			}
		}
	}

	if text != "" && from != "" {
		go sendMessageFromPlatform("whatsapp", from, text)
	}
	w.WriteHeader(200)
}

func sendPlatformReply(platform, chatID, text string) {
	if len(text) > 2000 {
		text = text[:1996] + "..." // Truncate safely
	}

	switch platform {
	case "telegram":
		url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", state.bots.TelegramToken)
		payload := map[string]interface{}{"chat_id": chatID, "text": text}
		b, _ := json.Marshal(payload)
		http.Post(url, "application/json", bytes.NewBuffer(b))

	case "whatsapp":
		url := fmt.Sprintf("https://graph.facebook.com/v17.0/%s/messages", state.bots.WhatsAppPhoneID)
		payload := map[string]interface{}{
			"messaging_product": "whatsapp",
			"to":                chatID,
			"type":              "text",
			"text":              map[string]string{"body": text},
		}
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest("POST", url, bytes.NewBuffer(b))
		req.Header.Set("Authorization", "Bearer "+state.bots.WhatsAppToken)
		req.Header.Set("Content-Type", "application/json")
		http.DefaultClient.Do(req)

	case "discord":
		var url string
		var req *http.Request
		if len(chatID) > 30 {
			url = fmt.Sprintf("https://discord.com/api/v10/webhooks/%s/%s/messages/@original", state.bots.DiscordAppID, chatID)
			payload := map[string]interface{}{"content": text}
			b, _ := json.Marshal(payload)
			req, _ = http.NewRequest("PATCH", url, bytes.NewBuffer(b))
		} else {
			url = fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages", chatID)
			payload := map[string]interface{}{"content": text}
			b, _ := json.Marshal(payload)
			req, _ = http.NewRequest("POST", url, bytes.NewBuffer(b))
			req.Header.Set("Authorization", "Bot "+state.bots.DiscordBotToken)
		}
		req.Header.Set("Content-Type", "application/json")
		http.DefaultClient.Do(req)
	}
}

func sendMessageFromPlatform(platform, chatID, text string) {
	aiMutex.Lock()
	defer aiMutex.Unlock()

	fmt.Printf("\r%s\n  %s %s[%s]%s %s\n", ClearLine, Cyan, White, platform, Reset, text)

	if len(state.history) == 0 {
		state.history = append(state.history, Message{Role: "user", Content: "SYSTEM: " + getSystemPrompt()})
		state.history = append(state.history, Message{Role: "assistant", Content: "Ready!"})
	}

	state.history = append(state.history, Message{Role: "user", Content: text})
	runAI(platform, chatID)

	fmt.Printf("%s❯%s ", Cyan, Reset)
}

// ═══════════════════════════════════════════════════════════════════════════════
// TERMINAL UI COMPONENTS
// ═══════════════════════════════════════════════════════════════════════════════

func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

func getTermWidth() int {
	return 80 // Default
}

func stripAnsi(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAllString(s, "")
}

func renderMarkdown(text string) string {
	codeBlockRe := regexp.MustCompile("(?s)```(\\w*)\\n(.*?)```")
	text = codeBlockRe.ReplaceAllStringFunc(text, func(match string) string {
		parts := codeBlockRe.FindStringSubmatch(match)
		lang := parts[1]
		code := parts[2]
		return renderCodeBlock(code, lang)
	})

	inlineCodeRe := regexp.MustCompile("`([^`]+)`")
	text = inlineCodeRe.ReplaceAllString(text, BgDarkGray+Cyan+"$1"+Reset)

	boldRe := regexp.MustCompile(`\*\*([^*]+)\*\*`)
	text = boldRe.ReplaceAllString(text, Bold+"$1"+Reset)

	italicRe := regexp.MustCompile(`\*([^*]+)\*`)
	text = italicRe.ReplaceAllString(text, Italic+"$1"+Reset)

	bulletRe := regexp.MustCompile(`(?m)^(\s*)[-*] (.+)$`)
	text = bulletRe.ReplaceAllString(text, "$1"+Cyan+"•"+Reset+" $2")

	numRe := regexp.MustCompile(`(?m)^(\s*)(\d+)\. (.+)$`)
	text = numRe.ReplaceAllString(text, "$1"+Cyan+"$2."+Reset+" $3")

	return text
}

func highlightSyntax(line, lang string) string {
	if lang == "" {
		return line
	}

	keywords := map[string][]string{
		"go":     {"func", "package", "import", "return", "if", "else", "for", "range", "var", "const", "type", "struct", "interface", "map", "chan", "go", "defer", "select", "case", "default", "break", "continue", "switch", "nil", "true", "false"},
		"js":     {"function", "const", "let", "var", "return", "if", "else", "for", "while", "class", "extends", "import", "export", "default", "async", "await", "try", "catch", "throw", "new", "this", "null", "undefined", "true", "false"},
		"python": {"def", "class", "import", "from", "return", "if", "elif", "else", "for", "while", "try", "except", "raise", "with", "as", "None", "True", "False", "and", "or", "not", "in", "is", "lambda", "yield", "async", "await"},
		"rust":   {"fn", "let", "mut", "const", "if", "else", "for", "while", "loop", "match", "struct", "enum", "impl", "trait", "pub", "use", "mod", "return", "self", "Self", "true", "false", "None", "Some", "Ok", "Err"},
	}

	result := line

	if strings.Contains(line, "//") {
		idx := strings.Index(line, "//")
		result = line[:idx] + SynComment + line[idx:] + Reset
		return result
	}
	if strings.HasPrefix(strings.TrimSpace(line), "#") && lang == "python" {
		return SynComment + line + Reset
	}

	stringRe := regexp.MustCompile(`"[^"]*"|'[^']*'` + "`[^`]*`")
	result = stringRe.ReplaceAllString(result, SynString+"$0"+Reset)

	numRe := regexp.MustCompile(`\b(\d+\.?\d*)\b`)
	result = numRe.ReplaceAllString(result, SynNumber+"$1"+Reset)

	if kws, ok := keywords[lang]; ok {
		for _, kw := range kws {
			kwRe := regexp.MustCompile(`\b(` + kw + `)\b`)
			result = kwRe.ReplaceAllString(result, SynKeyword+"$1"+Reset)
		}
	}

	return result
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
		highlighted := highlightSyntax(line, lang)
		result.WriteString(Gray + "  │ " + Reset + highlighted + "\n")
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
	fmt.Println(Gray + "   AI-Powered Code Editor & Bot" + Reset)
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

	elapsed := time.Since(state.startTime).Truncate(time.Second)

	left := fmt.Sprintf(" %s%s%s", Cyan, cwd, Reset)
	
	right := fmt.Sprintf("%s%s●%s %s", providerColor, Bold, Reset, providerName)
	if model != "" {
		right += fmt.Sprintf(" %s│%s %s", Gray, Reset, model)
	}
	right += fmt.Sprintf(" %s│%s %s", Gray, Reset, elapsed)
	if state.toolCalls > 0 {
		right += fmt.Sprintf(" %s│%s %d tools", Gray, Reset, state.toolCalls)
	}
	right += " "

	leftLen := len(stripAnsi(left))
	rightLen := len(stripAnsi(right))
	padding := width - leftLen - rightLen
	if padding < 0 {
		padding = 0
	}

	fmt.Printf("%s%s%s%s%s%s\n", BgGray, left, strings.Repeat(" ", padding), right, Reset, "")
}

func printDivider() {
	fmt.Println(Gray + strings.Repeat("─", getTermWidth()) + Reset)
}

func printFileDiff(filename string, oldLines, newLines []string, startLine int) {
	fmt.Printf("\n  %s%s─── %s%s\n", Gray, Bold, filename, Reset)
	maxOld := 3
	maxNew := 5

	shown := 0
	for i, line := range oldLines {
		if shown >= maxOld {
			fmt.Printf("  %s- ... +%d lines removed%s\n", Red+Dim, len(oldLines)-maxOld, Reset)
			break
		}
		lineNum := startLine + i
		displayLine := line
		if len(displayLine) > 60 {
			displayLine = displayLine[:57] + "..."
		}
		fmt.Printf("  %s%d │- %s%s\n", Red+Dim, lineNum, displayLine, Reset)
		shown++
	}

	shown = 0
	for i, line := range newLines {
		if shown >= maxNew {
			fmt.Printf("  %s+ ... +%d lines added%s\n", Green+Dim, len(newLines)-maxNew, Reset)
			break
		}
		lineNum := startLine + i
		displayLine := line
		if len(displayLine) > 60 {
			displayLine = displayLine[:57] + "..."
		}
		fmt.Printf("  %s%d │+ %s%s\n", Green, lineNum, displayLine, Reset)
		shown++
	}
}

func printToolCall(name string, args map[string]interface{}, result map[string]interface{}) {
	icon := "●"
	color := Yellow
	switch name {
	case "read_file":
		icon = "◉"
		color = Blue
	case "write_file":
		icon = "◈"
		color = Green
	case "edit_lines":
		icon = "◇"
		color = Magenta
	case "search_file":
		icon = "◎"
		color = Cyan
	case "list_files":
		icon = "◐"
		color = Blue
	case "shell":
		icon = "▶"
		color = Yellow
	case "send_message":
		icon = "✉"
		color = Magenta
	}

	fmt.Printf("\n  %s%s%s %s%s%s", color, icon, Reset, Bold, name, Reset)

	if filename, ok := args["filename"].(string); ok {
		fmt.Printf(" %s%s%s", Gray, filename, Reset)
	}
	if cmd, ok := args["command"].(string); ok {
		displayCmd := cmd
		if len(displayCmd) > 40 {
			displayCmd = displayCmd[:37] + "..."
		}
		fmt.Printf(" %s%s%s", Gray, displayCmd, Reset)
	}

	if _, ok := result["error"]; ok {
		errMsg, _ := result["error"].(string)
		if len(errMsg) > 50 {
			errMsg = errMsg[:47] + "..."
		}
		fmt.Printf(" %s✗ %s%s\n", Red, errMsg, Reset)
	} else if _, ok := result["success"]; ok {
		fmt.Printf(" %s✓%s\n", Green, Reset)
	} else if files, ok := result["files"].([]string); ok {
		fmt.Printf(" %s→ %d files%s\n", Gray, len(files), Reset)
		for i, f := range files {
			if i >= 5 {
				fmt.Printf("    %s... +%d more%s\n", Dim, len(files)-5, Reset)
				break
			}
			fmt.Printf("    %s%s%s\n", Dim, f, Reset)
		}
	} else if content, ok := result["content"].(string); ok {
		lines := strings.Split(content, "\n")
		fmt.Printf(" %s→ %d lines%s\n", Gray, len(lines), Reset)
	} else if matches, ok := result["matches"].(string); ok {
		count := strings.Count(matches, "--- Match")
		fmt.Printf(" %s→ %d matches%s\n", Gray, count, Reset)
	} else if stdout, ok := result["stdout"].(string); ok {
		stdout = strings.TrimSpace(stdout)
		if stdout == "" {
			fmt.Printf(" %s✓%s\n", Green, Reset)
		} else {
			lines := strings.Split(stdout, "\n")
			fmt.Printf(" %s→ %d lines output%s\n", Gray, len(lines), Reset)
			for i, line := range lines {
				if i >= 3 {
					fmt.Printf("    %s... +%d more%s\n", Dim, len(lines)-3, Reset)
					break
				}
				if len(line) > 60 {
					line = line[:57] + "..."
				}
				fmt.Printf("    %s%s%s\n", Dim, line, Reset)
			}
		}
	} else {
		fmt.Printf(" %s✓%s\n", Green, Reset)
	}
}

func printAssistantMessage(content string) {
	fmt.Println()
	rendered := renderMarkdown(content)
	lines := strings.Split(rendered, "\n")
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "│") || strings.HasPrefix(strings.TrimSpace(line), "┌") || strings.HasPrefix(strings.TrimSpace(line), "└") {
			fmt.Println(line)
			continue
		}
		if len(stripAnsi(line)) > 75 {
			words := strings.Fields(line)
			current := ""
			for _, word := range words {
				testLen := len(stripAnsi(current)) + len(stripAnsi(word)) + 1
				if testLen > 75 && current != "" {
					fmt.Println("  " + current)
					current = word
				} else {
					if current != "" {
						current += " "
					}
					current += word
				}
			}
			if current != "" {
				fmt.Println("  " + current)
			}
		} else if line == "" {
			fmt.Println()
		} else {
			fmt.Println("  " + line)
		}
	}
	fmt.Println()
}

func printUserMessage(text string) {
	fmt.Printf("\n  %s%s❯%s %s\n", Cyan, Bold, Reset, text)
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
// CORE AI LOGIC
// ═══════════════════════════════════════════════════════════════════════════════

func parseAIResponse(data map[string]interface{}, rawString string) string {
	// Root-level handling for Cloudflare / Custom providers that wrap things deeply
	if result, ok := data["result"].(map[string]interface{}); ok {
		if _, hasChoices := result["choices"]; hasChoices {
			data = result
		} else if respContent, hasResp := result["response"].(string); hasResp {
			return respContent
		}
	}

	// OpenAI / Standard Format
	if choices, ok := data["choices"].([]interface{}); ok && len(choices) > 0 {
		choice := choices[0].(map[string]interface{})
		if message, ok := choice["message"].(map[string]interface{}); ok {
			
			// Tool calls logic
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
			reasoning, _ := message["reasoning_content"].(string)
			if reasoning == "" {
				reasoning, _ = message["reasoning"].(string)
			}

			if reasoning != "" || content != "" {
				out := ""
				if reasoning != "" {
					out += "*thinking...*\n" + reasoning + "\n\n"
				}
				if content != "" {
					out += content
				}
				return strings.TrimSpace(out)
			}
		}
	}

	// Gemini Format
	if cands, ok := data["candidates"].([]interface{}); ok && len(cands) > 0 {
		if content, ok := cands[0].(map[string]interface{})["content"].(map[string]interface{}); ok {
			if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
				if text, ok := parts[0].(map[string]interface{})["text"].(string); ok {
					return text
				}
			}
		}
	}

	// Root level fallbacks (crucial for Cloudflare's raw response models)
	if resp, ok := data["response"].(string); ok {
		return resp
	}
	if text, ok := data["text"].(string); ok {
		return text
	}

	return rawString
}

func runTool(name string, args map[string]interface{}) map[string]interface{} {
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
		if v, ok := args[k].(int); ok {
			return v
		}
		if v, ok := args[k].(string); ok {
			if i, err := strconv.Atoi(v); err == nil {
				return i
			}
		}
		return -1
	}

	switch name {
	case "list_files":
		var files []string
		filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() && info.Name() != "." && (strings.HasPrefix(info.Name(), ".") || info.Name() == "node_modules" || info.Name() == "vendor" || info.Name() == "__pycache__") {
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
			result["error"] = "File not found: " + filename
			break
		}
		lines := strings.Split(string(content), "\n")
		var results []string
		for i, line := range lines {
			if strings.Contains(strings.ToLower(line), query) {
				s := max(0, i-2)
				e := min(len(lines)-1, i+2)
				results = append(results, fmt.Sprintf("--- Match around line %d ---", i+1))
				for j := s; j <= e; j++ {
					results = append(results, fmt.Sprintf("%d | %s", j+1, lines[j]))
				}
			}
		}
		if len(results) == 0 {
			result["error"] = fmt.Sprintf("No matches for '%s'", query)
		} else {
			result["matches"] = strings.Join(results, "\n")
		}

	case "read_file":
		filename := getString("filename")
		start := getInt("start_line")
		end := getInt("end_line")

		content, err := os.ReadFile(filename)
		if err != nil {
			result["error"] = "File not found: " + filename
			break
		}
		lines := strings.Split(string(content), "\n")

		if start < 1 {
			start = 1
		}
		if end < 1 || end > len(lines) {
			end = len(lines)
		}
		if start-1 >= end {
			result["error"] = "Invalid line range"
			break
		}

		var numbered []string
		for i := start - 1; i < end; i++ {
			numbered = append(numbered, fmt.Sprintf("%d | %s", i+1, lines[i]))
		}
		result["total_lines"] = len(lines)
		result["range"] = fmt.Sprintf("%d-%d", start, end)
		result["content"] = strings.Join(numbered, "\n")

	case "write_file":
		filename := getString("filename")
		content := getString("content")

		dir := filepath.Dir(filename)
		if dir != "." && dir != "" {
			os.MkdirAll(dir, 0755)
		}

		err := os.WriteFile(filename, []byte(content), 0644)
		if err != nil {
			result["error"] = err.Error()
		} else {
			result["success"] = true
			result["bytes"] = len(content)
		}

	case "edit_lines":
		filename := getString("filename")
		startLine := getInt("start_line")
		endLine := getInt("end_line")
		replaceWith := getString("replace_with")

		content, err := os.ReadFile(filename)
		if err != nil {
			result["error"] = "File not found: " + filename
			break
		}
		lines := strings.Split(string(content), "\n")

		if startLine < 1 || endLine < 1 {
			result["error"] = "Invalid line numbers"
			break
		}
		start := startLine - 1
		end := endLine

		if start > end || start >= len(lines) {
			result["error"] = "Invalid range"
			break
		}
		if end > len(lines) {
			end = len(lines)
		}

		oldLines := lines[start:end]
		originalIndent := ""
		if start < len(lines) {
			originalIndent = regexp.MustCompile(`^\s*`).FindString(lines[start])
		}
		replacementLines := strings.Split(replaceWith, "\n")

		if len(originalIndent) > 0 && len(replacementLines) > 0 && !regexp.MustCompile(`^\s+`).MatchString(replacementLines[0]) {
			for i, l := range replacementLines {
				if strings.TrimSpace(l) != "" {
					replacementLines[i] = originalIndent + l
				}
			}
		}

		newContent := append(lines[:start], append(replacementLines, lines[end:]...)...)
		err = os.WriteFile(filename, []byte(strings.Join(newContent, "\n")), 0644)
		if err != nil {
			result["error"] = err.Error()
		} else {
			result["success"] = true
			result["old_lines"] = oldLines
			result["new_lines"] = replacementLines
			result["start_line"] = startLine
			result["filename"] = filename
		}

	case "shell":
		command := getString("command")
		if command == "" {
			result["error"] = "No command provided"
			break
		}

		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/C", command)
		} else {
			cmd = exec.Command("sh", "-c", command)
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
			if exitErr, ok := err.(*exec.ExitError); ok {
				result["exit_code"] = exitErr.ExitCode()
			}
		} else {
			result["exit_code"] = 0
			result["success"] = true
		}

	case "send_message":
		platform := getString("platform")
		chatID := getString("chat_id")
		text := getString("text")
		if platform != "" && chatID != "" && text != "" {
			sendPlatformReply(platform, chatID, text)
			result["success"] = true
			result["delivered_to"] = platform
		} else {
			result["error"] = "Missing parameters"
		}

	default:
		result["error"] = "Unknown tool: " + name
	}

	return result
}

func runAI(source string, chatID string) {
	maxIter := 25
	p, ok := PROVIDERS[state.provider]
	if !ok {
		printError("Unknown provider")
		return
	}
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

	for iter := 0; iter < maxIter; iter++ {
		done := make(chan bool)
		go printSpinner(done, "thinking...")

		reqData := p.BuildRequest(state.history, fullCfg)

		bodyBytes, _ := json.Marshal(reqData.Body)
		req, _ := http.NewRequest("POST", reqData.URL, bytes.NewBuffer(bodyBytes))
		for k, v := range reqData.Headers {
			req.Header.Set(k, v)
		}

		client := &http.Client{Timeout: 120 * time.Second}
		resp, err := client.Do(req)

		done <- true
		time.Sleep(50 * time.Millisecond)

		if err != nil {
			printError("Request failed: " + err.Error())
			break
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		state.tokens += len(string(respBody)) / 4

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errMsg := string(respBody)
			if len(errMsg) > 150 {
				errMsg = errMsg[:150] + "..."
			}
			printError(fmt.Sprintf("API %d: %s", resp.StatusCode, errMsg))
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
			} else if strings.Contains(content, "\"tool\"") && strings.Contains(content, "{") && strings.Contains(content, "}") {
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
					fmMatch := regexp.MustCompile(`"filename"\s*:\s*"([^"]+)"`).FindStringSubmatch(jsonStr)
					filename := "file.txt"
					if len(fmMatch) > 1 {
						filename = fmMatch[1]
					}

					unescapeStr := func(str string) string {
						str = strings.ReplaceAll(str, "\\n", "\n")
						str = strings.ReplaceAll(str, "\\\"", "\"")
						return str
					}

					if strings.Contains(jsonStr, `"write_file"`) {
						cIdx := strings.Index(jsonStr, `"content"`)
						if cIdx > -1 {
							sQ := strings.Index(jsonStr[cIdx+9:], `"`) + cIdx + 9
							eQ := strings.LastIndex(jsonStr, `"`)
							if sQ > -1 && eQ > sQ {
								toolCmd = map[string]interface{}{"tool": "write_file", "args": map[string]interface{}{"filename": filename, "content": unescapeStr(jsonStr[sQ+1 : eQ])}}
							}
						}
					} else if strings.Contains(jsonStr, `"edit_lines"`) {
						slMatch := regexp.MustCompile(`"start_line"\s*:\s*(\d+)`).FindStringSubmatch(jsonStr)
						elMatch := regexp.MustCompile(`"end_line"\s*:\s*(\d+)`).FindStringSubmatch(jsonStr)
						rIdx := strings.Index(jsonStr, `"replace_with"`)
						if len(slMatch) > 1 && len(elMatch) > 1 && rIdx > -1 {
							sQ := strings.Index(jsonStr[rIdx+14:], `"`) + rIdx + 14
							eQ := strings.LastIndex(jsonStr, `"`)
							if sQ > -1 && eQ > sQ {
								sl, _ := strconv.Atoi(slMatch[1])
								el, _ := strconv.Atoi(elMatch[1])
								toolCmd = map[string]interface{}{"tool": "edit_lines", "args": map[string]interface{}{"filename": filename, "start_line": sl, "end_line": el, "replace_with": unescapeStr(jsonStr[sQ+1 : eQ])}}
							}
						}
					} else if strings.Contains(jsonStr, `"shell"`) {
						cmdMatch := regexp.MustCompile(`"command"\s*:\s*"([^"]+)"`).FindStringSubmatch(jsonStr)
						if len(cmdMatch) > 1 {
							toolCmd = map[string]interface{}{"tool": "shell", "args": map[string]interface{}{"command": unescapeStr(cmdMatch[1])}}
						}
					} else if strings.Contains(jsonStr, `"search_file"`) {
						qm := regexp.MustCompile(`"query"\s*:\s*"([^"]+)"`).FindStringSubmatch(jsonStr)
						if len(qm) > 1 {
							toolCmd = map[string]interface{}{"tool": "search_file", "args": map[string]interface{}{"filename": filename, "query": qm[1]}}
						}
					} else if strings.Contains(jsonStr, `"read_file"`) {
						slMatch := regexp.MustCompile(`"start_line"\s*:\s*(\d+)`).FindStringSubmatch(jsonStr)
						elMatch := regexp.MustCompile(`"end_line"\s*:\s*(\d+)`).FindStringSubmatch(jsonStr)
						args := map[string]interface{}{"filename": filename}
						if len(slMatch) > 1 {
							sl, _ := strconv.Atoi(slMatch[1])
							args["start_line"] = sl
						}
						if len(elMatch) > 1 {
							el, _ := strconv.Atoi(elMatch[1])
							args["end_line"] = el
						}
						toolCmd = map[string]interface{}{"tool": "read_file", "args": args}
					} else if strings.Contains(jsonStr, `"list_files"`) {
						toolCmd = map[string]interface{}{"tool": "list_files", "args": map[string]interface{}{}}
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
						result := runTool(tName, argsMap)
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

						resBytes, _ := json.Marshal(result)
						state.history = append(state.history, Message{Role: "assistant", Content: content})
						state.history = append(state.history, Message{Role: "user", Content: fmt.Sprintf(`Tool result: %s

Continue if more work needed, or summarize what you did.`, string(resBytes))})
						continue
					}
				}
			}

			printAssistantMessage(content)
			state.history = append(state.history, Message{Role: "assistant", Content: content})

			if source != "cli" && chatID != "" {
				sendPlatformReply(source, chatID, content)
			}
		} else {
			printError("Empty response from API")
		}
		break
	}
}


func getSystemPrompt() string {
	cwd, _ := os.Getwd()
	return fmt.Sprintf(`You are Axiom, an elite, highly capable AI agent.
WORKSPACE: %s
OS: %s

CRITICAL DIRECTIVES:
1. YOU HAVE FULL INTERNET ACCESS. The 'shell' tool executes directly on the user's host machine.
2. If the user asks for live data (prices, weather, etc.), DO NOT REFUSE. Immediately use the 'shell' tool with 'curl', 'wget', or a python script to fetch the data.
3. DO NOT claim you are in a sandbox. DO NOT claim you cannot access the internet.
4. DO NOT ask for permission to run commands. Just run them.
5. NO YAPPING. Keep your responses brutally concise and direct.

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
7. **send_message** (Proactively message user on telegram/whatsapp/discord when a long task is finished)
   `+"```json\n"+`{"tool": "send_message", "args": {"platform": "telegram", "chat_id": "123", "text": "I finished the deployment!"}}`+"\n```", cwd, runtime.GOOS)
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

	printUserMessage(text)
	state.history = append(state.history, Message{Role: "user", Content: text})
	runAI("cli", "")
}

// ═══════════════════════════════════════════════════════════════════════════════
// COMMAND HANDLERS
// ═══════════════════════════════════════════════════════════════════════════════

func handleConfig(reader *bufio.Reader) {
	fmt.Println()
	fmt.Printf("  %sPROVIDERS%s\n\n", Bold, Reset)
	fmt.Printf("  %sCloud:%s    google openai anthropic mistral groq together openrouter\n", Dim, Reset)
	fmt.Printf("  %sLocal:%s    ollama lmstudio llamacpp\n", Dim, Reset)
	fmt.Printf("  %sCustom:%s   cloudflare custom\n", Dim, Reset)
	fmt.Println()

	fmt.Printf("  %sProvider%s [%s]: ", Bold, Reset, state.provider)
	p, _ := reader.ReadString('\n')
	p = strings.TrimSpace(p)
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
		fmt.Printf("  %sEndpoint%s [%s]: ", Bold, Reset, defaultEp)
		ep, _ := reader.ReadString('\n')
		ep = strings.TrimSpace(ep)
		if ep != "" {
			cfg.Endpoint = ep
		} else if cfg.Endpoint == "" {
			cfg.Endpoint = prov.DefaultEndpoint
		}
	}

	if prov.Type != "local" || p == "ollama" {
		keyHint := "not set"
		if cfg.APIKey != "" {
			keyHint = "****" + cfg.APIKey[max(0, len(cfg.APIKey)-4):]
		}
		fmt.Printf("  %sAPI Key%s [%s]: ", Bold, Reset, keyHint)
		k, _ := reader.ReadString('\n')
		k = strings.TrimSpace(k)
		if k != "" {
			cfg.APIKey = k
		}
	}

	defaultModel := prov.DefaultModel
	if cfg.Model != "" {
		defaultModel = cfg.Model
	}
	fmt.Printf("  %sModel%s [%s]: ", Bold, Reset, defaultModel)
	m, _ := reader.ReadString('\n')
	m = strings.TrimSpace(m)
	if m != "" {
		cfg.Model = m
	} else if cfg.Model == "" {
		cfg.Model = prov.DefaultModel
	}

	state.configs[p] = cfg
	state.connected = true
	saveConfig()
	printSuccess(fmt.Sprintf("Connected to %s (%s)", prov.Name, cfg.Model))
}

func handleBots(reader *bufio.Reader) {
	fmt.Println("\n  " + Bold + "BOT INTEGRATIONS" + Reset)
	fmt.Println("  " + Dim + "Leave blank to keep current, type 'none' to clear." + Reset + "\n")

	ask := func(label, current string) string {
		display := current
		if display != "" {
			display = "********" + display[max(0, len(display)-4):]
		}
		fmt.Printf("  %s%s%s [%s]: ", Bold, label, Reset, display)
		val, _ := reader.ReadString('\n')
		val = strings.TrimSpace(val)
		if val == "none" {
			return ""
		}
		if val == "" {
			return current
		}
		return val
	}

	state.bots.TelegramToken = ask("Telegram Bot Token", state.bots.TelegramToken)
	state.bots.DiscordAppID = ask("Discord App ID", state.bots.DiscordAppID)
	state.bots.DiscordPublicKey = ask("Discord Public Key", state.bots.DiscordPublicKey)
	state.bots.DiscordBotToken = ask("Discord Bot Token", state.bots.DiscordBotToken)
	state.bots.WhatsAppToken = ask("WhatsApp Token", state.bots.WhatsAppToken)
	state.bots.WhatsAppPhoneID = ask("WhatsApp Phone ID", state.bots.WhatsAppPhoneID)
	state.bots.WhatsAppVerify = ask("WhatsApp Verify Token", state.bots.WhatsAppVerify)
	state.bots.ServerPort = ask("Webhook Port (default 8080)", state.bots.ServerPort)

	saveConfig()
	printSuccess("Bot integrations updated. Restart Axiom to apply network changes.")
}

func handleModel(reader *bufio.Reader) {
	prov := PROVIDERS[state.provider]
	cfg := state.configs[state.provider]
	current := cfg.Model
	if current == "" {
		current = prov.DefaultModel
	}

	fmt.Printf("\n  Current: %s%s%s\n", Cyan, current, Reset)
	fmt.Printf("  %sNew model:%s ", Bold, Reset)
	m, _ := reader.ReadString('\n')
	m = strings.TrimSpace(m)
	if m != "" {
		cfg.Model = m
		state.configs[state.provider] = cfg
		saveConfig()
		printSuccess("Model → " + m)
	}
}

func handleClear() {
	state.history = []Message{}
	state.toolCalls = 0
	state.tokens = 0
	printSuccess("Conversation cleared")
}

func handleStatus() {
	fmt.Println()
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
	cwd, _ := os.Getwd()
	elapsed := time.Since(state.startTime).Truncate(time.Second)

	fmt.Printf("  %sProvider%s   %s%s%s\n", Dim, Reset, Green, prov.Name, Reset)
	fmt.Printf("  %sModel%s      %s\n", Dim, Reset, model)
	fmt.Printf("  %sWorkspace%s  %s\n", Dim, Reset, cwd)
	fmt.Printf("  %sSession%s    %s\n", Dim, Reset, elapsed)
	fmt.Printf("  %sMessages%s   %d\n", Dim, Reset, len(state.history))
	fmt.Println()
}

func handleFiles() {
	var files []string
	filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && info.Name() != "." && (strings.HasPrefix(info.Name(), ".") || info.Name() == "node_modules" || info.Name() == "vendor" || info.Name() == "__pycache__") {
			return filepath.SkipDir
		}
		if !info.IsDir() && !strings.HasPrefix(info.Name(), ".") {
			files = append(files, path)
		}
		return nil
	})

	fmt.Printf("\n  %s%d files%s\n\n", Dim, len(files), Reset)
	for _, f := range files {
		fmt.Printf("  %s·%s %s\n", Cyan, Reset, f)
	}
	fmt.Println()
}

func printHelp() {
	fmt.Println()
	fmt.Printf("  %s%sCOMMANDS%s\n\n", Bold, Cyan, Reset)
	commands := [][]string{
		{"/config", "Configure AI provider"},
		{"/bots", "Configure Telegram/Discord/WhatsApp"},
		{"/model", "Change model"},
		{"/clear", "Clear conversation"},
		{"/status", "Show session info"},
		{"/files", "List project files"},
		{"/help", "Show this help"},
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
		p := PROVIDERS[state.provider]
		fmt.Printf("  %sConnected to %s%s%s\n", Dim, Reset+Green, p.Name, Reset+Dim)
	} else {
		fmt.Printf("  %sType %s/config%s to get started.%s\n", Dim, Yellow, Dim, Reset)
	}
	fmt.Println()
	printDivider()
}

// ═══════════════════════════════════════════════════════════════════════════════
// MAIN
// ═══════════════════════════════════════════════════════════════════════════════

func main() {
	loadConfig()
	StartBots()
	printWelcome()

	reader := bufio.NewReader(os.Stdin)

	for {
		printStatusBar()
		fmt.Printf("%s❯%s ", Cyan, Reset)

		input, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		input = strings.TrimSpace(input)

		if input == "" {
			continue
		}

		switch {
		case input == "/exit", input == "/quit", input == "/q":
			fmt.Println("\n  " + Dim + "Goodbye!" + Reset + "\n")
			return
		case input == "/help", input == "/?":
			printHelp()
		case input == "/config":
			handleConfig(reader)
		case input == "/bots":
			handleBots(reader)
		case input == "/model":
			handleModel(reader)
		case input == "/clear", input == "/reset":
			handleClear()
		case input == "/status":
			handleStatus()
		case input == "/files", input == "/ls":
			handleFiles()
		case strings.HasPrefix(input, "/"):
			printError("Unknown command. Try /help")
		default:
			sendMessage(input)
		}
	}
}
