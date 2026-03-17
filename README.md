# Axiom

> **A beautiful, minimalist AI-powered code editor for your terminal**

Axiom brings the power of modern AI coding assistants (Claude, GPT-4, Gemini, local models) directly to your command line with a clean, developer-focused interface.

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

---

## ✨ Features

- 🎨 **Beautiful Terminal UI** - Syntax highlighting, markdown rendering, clean minimal design
- 🤖 **Multi-Provider Support** - Works with 11+ AI providers (cloud & local)
- 🛠️ **Autonomous Code Editing** - AI can read, write, search, and edit files intelligently
- 💻 **Shell Integration** - Run commands and see output directly in the chat
- 💾 **Persistent Config** - Save your API keys and preferences locally
- 🎯 **Smart Tool Usage** - AI knows when to use tools vs. just chat naturally
- 📦 **Zero Dependencies** - Single binary, works anywhere Go runs

---

## 🚀 Quick Start

### Installation

**Via Go:**
```bash
go install github.com/k8o5/axiom
```

**From Source:**
```bash
git clone https://github.com/k8o5/axiom.git
cd axiom
go build -o axiom
./axiom
```

**Pre-built Binaries:**
Download from [Releases](https://github.com/k8o5/axiom/releases)

### First Run

```bash
axiom
```

On first launch, configure your AI provider:

```
/config
```

Select a provider (e.g., `google` for Gemini), enter your API key, and you're ready!

---

## 🎯 Usage

### Basic Commands

| Command | Description |
|---------|-------------|
| `/config` | Configure AI provider and API keys |
| `/model` | Switch to a different model |
| `/clear` | Clear conversation history |
| `/status` | View session info and stats |
| `/files` | List all files in current directory |
| `/help` | Show all available commands |
| `/exit` | Quit Axiom |

### Natural Conversation

Just type naturally - Axiom understands both casual chat and coding requests:

```
❯ hi there!
  
  Hello! I'm Axiom, your AI coding assistant. Ready to help with any
  programming tasks. What would you like to work on?

❯ create a simple web server in go

  ◈ write_file main.go ✓

  I've created a basic HTTP web server in Go. Here's what it does:

  • Serves on port 8080
  • Has a simple "/" route that returns "Hello, World!"
  • Includes proper error handling

  Run it with: go run main.go
```

### Code Editing Workflow

Axiom follows a smart workflow:
1. **Search** → Find the right code location
2. **Read** → Get context with line numbers
3. **Edit** → Make precise surgical changes
4. **Verify** → Run tests or commands

**Example:**
```
❯ add error logging to the login function

  ◎ search_file auth.go "func login" → 2 matches
  ◉ read_file auth.go 45-60 → 15 lines
  ◇ edit_lines auth.go 52-53 ✓

  ─── auth.go
  52 │- return nil
  53 │+ if err != nil {
  54 │+     log.Printf("Login failed: %v", err)
  55 │+ }
  56 │+ return nil

  I've added error logging to the login function. It will now log any
  authentication failures to help with debugging.
```

---

## 🤖 Supported Providers

### Cloud Providers

| Provider | Models | API Key Required |
|----------|--------|------------------|
| **Google Gemini** | `gemini-2.0-flash-exp`, `gemini-pro` | [Get Key](https://makersuite.google.com/app/apikey) |
| **OpenAI** | `gpt-4o`, `gpt-4-turbo`, `gpt-3.5-turbo` | [Get Key](https://platform.openai.com/api-keys) |
| **Anthropic** | `claude-sonnet-4`, `claude-3.5-sonnet` | [Get Key](https://console.anthropic.com/) |
| **Groq** | `llama-3.3-70b`, `mixtral-8x7b` | [Get Key](https://console.groq.com/) |
| **Mistral** | `mistral-large`, `mixtral-8x7b` | [Get Key](https://console.mistral.ai/) |
| **Together** | `llama-3.1-70b`, `qwen-2.5-72b` | [Get Key](https://api.together.xyz/) |
| **OpenRouter** | 100+ models | [Get Key](https://openrouter.ai/keys) |

### Local Providers

| Provider | Default Port | Setup Guide |
|----------|-------------|-------------|
| **Ollama** | `11434` | [ollama.ai](https://ollama.ai) |
| **LM Studio** | `1234` | [lmstudio.ai](https://lmstudio.ai) |
| **llama.cpp** | `8080` | [GitHub](https://github.com/ggerganov/llama.cpp) |

### Custom Providers

| Provider | Use Case |
|----------|----------|
| **Cloudflare Workers AI** | Serverless AI with Workers |
| **Custom Endpoint** | Any OpenAI-compatible API |

---

## 🛠️ Available Tools

Axiom's AI can use these tools autonomously:

### File Operations

**list_files** - List all project files
```json
{"tool": "list_files", "args": {}}
```

**read_file** - Read file contents (with optional line range)
```json
{"tool": "read_file", "args": {
  "filename": "main.go",
  "start_line": 1,
  "end_line": 50
}}
```

**search_file** - Find text in a file (shows context)
```json
{"tool": "search_file", "args": {
  "filename": "main.go",
  "query": "func main"
}}
```

**write_file** - Create or overwrite a file
```json
{"tool": "write_file", "args": {
  "filename": "config.json",
  "content": "{\"key\": \"value\"}"
}}
```

**edit_lines** - Precisely edit specific lines
```json
{"tool": "edit_lines", "args": {
  "filename": "main.go",
  "start_line": 10,
  "end_line": 12,
  "replace_with": "newCode();"
}}
```

### System Operations

**shell** - Execute shell commands
```json
{"tool": "shell", "args": {
  "command": "go test ./..."
}}
```

> **Safety Note:** Dangerous commands (e.g., `rm -rf /`) are automatically blocked.

---

## 🎨 UI Features

### Syntax Highlighting

Axiom automatically highlights code in:
- Go, JavaScript/TypeScript, Python, Rust
- JSON, YAML, Markdown
- Shell scripts

### Markdown Rendering

Full markdown support in AI responses:
- **Bold**, *italic*, `inline code`
- Code blocks with syntax highlighting
- Bullet points and numbered lists
- Headers and links

### Smart File Diffs

When editing files, see exactly what changed:

```diff
  ─── config.js
  10 │- const API_URL = "localhost";
  11 │+ const API_URL = "https://api.example.com";
```

### Compact Tool Output

```
  ◉ read_file auth.go → 127 lines
  ◎ search_file main.go "handler" → 3 matches
  ◇ edit_lines server.go 45-47 ✓
  ▶ shell go test ./... ✓
```

---

## ⚙️ Configuration

### Config File Location

- **Linux/Mac:** `~/.axiom/config.json`
- **Windows:** `%USERPROFILE%\.axiom\config.json`

### Example Config

```json
{
  "provider": "google",
  "configs": {
    "google": {
      "apiKey": "AIza...",
      "model": "gemini-2.0-flash-exp"
    },
    "openai": {
      "apiKey": "sk-...",
      "model": "gpt-4o"
    },
    "ollama": {
      "endpoint": "http://localhost:11434/v1/chat/completions",
      "model": "llama3.2"
    }
  }
}
```

### Environment Variables

You can also use environment variables:

```bash
export AXIOM_PROVIDER="google"
export AXIOM_API_KEY="your-api-key"
export AXIOM_MODEL="gemini-2.0-flash-exp"
```

---

## 📖 Examples

### Create a REST API

```
❯ create a REST API in go with user CRUD operations

  ◈ write_file main.go ✓
  ◈ write_file handlers.go ✓
  ◈ write_file models.go ✓

  I've created a complete REST API with:
  - User model with CRUD operations
  - HTTP handlers for GET, POST, PUT, DELETE
  - In-memory storage (replace with DB later)
  - Proper error handling and JSON responses

  Start with: go run *.go
```

### Fix a Bug

```
❯ users aren't being saved to the database

  ◎ search_file handlers.go "SaveUser" → 1 match
  ◉ read_file handlers.go 34-48 → 14 lines
  ◇ edit_lines handlers.go 42-42 ✓

  ─── handlers.go
  42 │- db.Create(user)
  43 │+ if err := db.Create(user).Error; err != nil {
  44 │+     return err
  45 │+ }

  Fixed! The database Create() call wasn't checking for errors. Now it
  properly handles and returns database errors.
```

### Refactor Code

```
❯ extract the validation logic into a separate function

  ◉ read_file validator.go 1-50 → 50 lines
  ◇ edit_lines validator.go 15-30 ✓
  ◈ write_file validate.go ✓

  I've refactored the validation:
  1. Created validate.go with ValidateUser()
  2. Updated validator.go to use the new function
  3. Added proper error messages for each validation rule

  The code is now more maintainable and testable.
```

### Write Tests

```
❯ write unit tests for the user service

  ◉ read_file user_service.go 1-100 → 100 lines
  ◈ write_file user_service_test.go ✓
  ▶ shell go test -v ./... → 5 passed

  Created comprehensive tests covering:
  • User creation with valid data
  • Validation errors
  • Duplicate email handling
  • User retrieval and updates
  • Edge cases

  All tests passing ✓
```

---

## 🔒 Security & Privacy

- **API Keys:** Stored locally in `~/.axiom/config.json` with `0600` permissions
- **No Telemetry:** Zero tracking, analytics, or data collection
- **Local First:** All configuration and history stays on your machine
- **Shell Safety:** Dangerous commands are automatically blocked
- **Code Context:** Only files you explicitly work with are sent to AI providers

---

## 🤝 Contributing

Contributions are welcome! Here's how:

1. **Fork** the repository
2. **Create** a feature branch (`git checkout -b feature/amazing`)
3. **Commit** your changes (`git commit -m 'Add amazing feature'`)
4. **Push** to the branch (`git push origin feature/amazing`)
5. **Open** a Pull Request

### Development Setup

```bash
git clone https://github.com/k8o5/axiom.git
cd axiom
go mod download
go run main.go
```

### Adding a New Provider

Check out [`CONTRIBUTING.md`](CONTRIBUTING.md) for a guide on adding new AI providers.

---

## 📝 License

MIT License - see [LICENSE](LICENSE) for details.

---

Built with:
- Go standard library (zero external dependencies!)
- Love for clean, minimal UIs ❤️

---

## 📞 Support

- **Issues:** [GitHub Issues](https://github.com/k8o5/axiom/issues)
- **Discussions:** [GitHub Discussions](https://github.com/k8o5/axiom/discussions)
- **Twitter:** [@yourusername](https://x.com/k8o5kebab3051)

---

<div align="center">

**Made with ❤️ by developers, for developers**

[⭐ Star on GitHub](https://github.com/yourusername/axiom) · [📖 Documentation](https://axiom.dev) · [🐛 Report Bug](https://github.com/yourusername/axiom/issues)

</div>
- Performance benchmarks
- Comparison table with similar tools
